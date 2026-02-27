package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// Index is an in-memory index for fast relation lookups and filtering.
type Index struct {
	mu       sync.RWMutex
	inbound  map[string][]RelationEntry // target_ref -> sources pointing at it
	byPubkey map[string][]string        // pubkey -> refs owned by that key
	meta     map[string]ObjectMeta      // ref -> metadata
}

// NewIndex creates an empty index.
func NewIndex() *Index {
	return &Index{
		inbound:  make(map[string][]RelationEntry),
		byPubkey: make(map[string][]string),
		meta:     make(map[string]ObjectMeta),
	}
}

// InboundFilters are the optional filters for inbound queries.
type InboundFilters struct {
	Relation string // filter by relation type
	From     string // filter by source pubkey
	Type     string // filter by source object type
}

// Rebuild scans the store and populates the index. Returns object count and duration.
func (idx *Index) Rebuild(store *Store) (int, time.Duration, error) {
	start := time.Now()

	refs, err := store.Scan()
	if err != nil {
		return 0, 0, fmt.Errorf("index rebuild scan: %w", err)
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Reset
	idx.inbound = make(map[string][]RelationEntry)
	idx.byPubkey = make(map[string][]string)
	idx.meta = make(map[string]ObjectMeta)

	count := 0
	for _, ref := range refs {
		data, err := store.Read(ref)
		if err != nil {
			log.Printf("WARN: index rebuild skip %s: %v", ref, err)
			continue
		}
		if data == nil {
			continue
		}

		env, item, err := ParseEnvelope(data)
		if err != nil {
			log.Printf("WARN: index rebuild parse %s: %v", ref, err)
			continue
		}

		ts, err := item.Timestamp()
		if err != nil {
			log.Printf("WARN: index rebuild timestamp %s: %v", ref, err)
			ts = time.Time{}
		}

		realms := ResolveIn(env, item)
		idx.addLocked(ref, item, ts, realms)
		count++
	}

	return count, time.Since(start), nil
}

// Add adds or updates an object in the index. Thread-safe.
func (idx *Index) Add(ref string, item *Item, ts time.Time, realms ...InField) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.addLocked(ref, item, ts, realms...)
}

// Remove removes an object from the index. Thread-safe.
func (idx *Index) Remove(ref string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.removeLocked(ref)
}

// Update removes old entries and adds new ones. Thread-safe.
func (idx *Index) Update(ref string, item *Item, ts time.Time, realms ...InField) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.removeLocked(ref)
	idx.addLocked(ref, item, ts, realms...)
}

// GetInbound returns metadata of objects pointing at targetRef, filtered.
// authPubkey controls visibility of private objects (empty = public only).
func (idx *Index) GetInbound(targetRef string, filters InboundFilters, authPubkey string) []ObjectMeta {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	entries := idx.inbound[targetRef]
	var result []ObjectMeta
	for _, e := range entries {
		if filters.Relation != "" && e.RelationType != filters.Relation {
			continue
		}
		m, ok := idx.meta[e.SourceRef]
		if !ok {
			continue
		}
		if !m.IsPublic && !HasMatchingRealm(m.Realms, authPubkey) {
			continue
		}
		if filters.From != "" && m.Pubkey != filters.From {
			continue
		}
		if filters.Type != "" && m.Type != filters.Type {
			continue
		}
		result = append(result, m)
	}

	sortMetaDesc(result)
	return result
}

// GetByPubkey returns metadata of objects owned by the given pubkey, optionally filtered by type.
// authPubkey controls visibility of private objects (empty = public only).
func (idx *Index) GetByPubkey(pubkey, typeFilter, authPubkey string) []ObjectMeta {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	refs := idx.byPubkey[pubkey]
	var result []ObjectMeta
	for _, ref := range refs {
		m, ok := idx.meta[ref]
		if !ok {
			continue
		}
		if !m.IsPublic && !HasMatchingRealm(m.Realms, authPubkey) {
			continue
		}
		if typeFilter != "" && m.Type != typeFilter {
			continue
		}
		result = append(result, m)
	}

	sortMetaDesc(result)
	return result
}

// GetAll returns all object metadata, optionally filtered by pubkey and/or type.
// authPubkey controls visibility of private objects (empty = public only).
func (idx *Index) GetAll(pubkey, typeFilter, authPubkey string) []ObjectMeta {
	if pubkey != "" {
		return idx.GetByPubkey(pubkey, typeFilter, authPubkey)
	}

	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var result []ObjectMeta
	for _, m := range idx.meta {
		if !m.IsPublic && !HasMatchingRealm(m.Realms, authPubkey) {
			continue
		}
		if typeFilter != "" && m.Type != typeFilter {
			continue
		}
		result = append(result, m)
	}

	sortMetaDesc(result)
	return result
}

// GetMeta returns metadata for a single ref.
func (idx *Index) GetMeta(ref string) (ObjectMeta, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	m, ok := idx.meta[ref]
	return m, ok
}

// GetInboundCounts returns a map of relation_type -> count for all inbound relations to targetRef.
func (idx *Index) GetInboundCounts(targetRef string) map[string]int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	counts := make(map[string]int)
	for _, e := range idx.inbound[targetRef] {
		counts[e.RelationType]++
	}
	return counts
}

// addLocked adds to all index maps. Caller must hold write lock.
// realms is the resolved InField (from envelope+item); if nil, falls back to item.In.
func (idx *Index) addLocked(ref string, item *Item, ts time.Time, realms ...InField) {
	// Extract mime_type for BLOB objects (used for content negotiation)
	var mimeType string
	if item.Type == "BLOB" && item.Content != nil {
		var content struct {
			MimeType string `json:"mime_type"`
		}
		json.Unmarshal(item.Content, &content)
		mimeType = content.MimeType
	}

	// Extract first page relation ref
	var pageRef string
	if pageRels, ok := item.Relations["page"]; ok && len(pageRels) > 0 {
		var rr RelationRef
		if json.Unmarshal(pageRels[0], &rr) == nil && rr.Ref != "" {
			pageRef = rr.Ref
		}
	}

	// Resolve realms for visibility
	var resolved InField
	if len(realms) > 0 && len(realms[0]) > 0 {
		resolved = realms[0]
	} else {
		resolved = item.In
	}

	// Meta
	idx.meta[ref] = ObjectMeta{
		Ref:             ref,
		Pubkey:          item.Pubkey,
		Type:            item.Type,
		Revision:        item.Revision,
		HasPageRelation: pageRef != "",
		PageRef:         pageRef,
		MimeType:        mimeType,
		IsPublic:        IsPublicObject(resolved),
		Realms:          []string(resolved),
		UpdatedAt:       ts,
	}

	// byPubkey
	idx.byPubkey[item.Pubkey] = append(idx.byPubkey[item.Pubkey], ref)

	// Inbound relations: for each relation, extract target refs
	for relType, entries := range item.Relations {
		for _, raw := range entries {
			var rr RelationRef
			if err := json.Unmarshal(raw, &rr); err != nil || rr.Ref == "" {
				continue
			}
			idx.inbound[rr.Ref] = append(idx.inbound[rr.Ref], RelationEntry{
				SourceRef:    ref,
				RelationType: relType,
			})
		}
	}
}

// removeLocked removes from all index maps. Caller must hold write lock.
func (idx *Index) removeLocked(ref string) {
	m, ok := idx.meta[ref]
	if !ok {
		return
	}

	// Remove from byPubkey
	refs := idx.byPubkey[m.Pubkey]
	for i, r := range refs {
		if r == ref {
			idx.byPubkey[m.Pubkey] = append(refs[:i], refs[i+1:]...)
			break
		}
	}

	// Remove from inbound: need to scan all targets this ref pointed at
	// We don't track forward relations, so scan all inbound entries
	for target, entries := range idx.inbound {
		filtered := entries[:0]
		for _, e := range entries {
			if e.SourceRef != ref {
				filtered = append(filtered, e)
			}
		}
		if len(filtered) == 0 {
			delete(idx.inbound, target)
		} else {
			idx.inbound[target] = filtered
		}
	}

	delete(idx.meta, ref)
}

// sortMetaDesc sorts by (UpdatedAt DESC, Ref DESC).
func sortMetaDesc(metas []ObjectMeta) {
	sort.Slice(metas, func(i, j int) bool {
		if !metas[i].UpdatedAt.Equal(metas[j].UpdatedAt) {
			return metas[i].UpdatedAt.After(metas[j].UpdatedAt)
		}
		return metas[i].Ref > metas[j].Ref
	})
}
