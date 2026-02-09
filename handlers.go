package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

const maxBodySize = 1 << 20 // 1 MB

// handleRoot serves GET / — returns the ROOT object.
func (h *Hub) handleRoot(w http.ResponseWriter, r *http.Request) {
	metas := h.index.GetAll("", "ROOT")
	if len(metas) == 0 {
		writeError(w, http.StatusNotFound, "no root object", "NOT_FOUND")
		return
	}
	data, err := h.store.Read(metas[0].Ref)
	if err != nil || data == nil {
		log.Printf("ERROR: GET /: read root %s: %v", metas[0].Ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// handleGetObject serves GET /v1/objects/{ref}
func (h *Hub) handleGetObject(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")

	data, err := h.store.Read(ref)
	if err != nil {
		log.Printf("ERROR: GET /objects/%s: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	if data == nil {
		writeError(w, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}

	// ETag = revision number (objects are immutable at a given revision)
	meta, _ := h.index.GetMeta(ref)
	etag := `"0"`
	if meta.Ref != "" {
		// Parse revision from stored data to get exact value
		var env Envelope
		var item Item
		if err := json.Unmarshal(data, &env); err != nil {
			log.Printf("WARN: GET /objects/%s: failed to parse envelope for ETag: %v", ref, err)
		} else if err := json.Unmarshal(env.Item, &item); err != nil {
			log.Printf("WARN: GET /objects/%s: failed to parse item for ETag: %v", ref, err)
		} else {
			etag = `"` + strconv.Itoa(item.Revision) + `"`
		}
	}
	w.Header().Set("ETag", etag)

	// 304 Not Modified if client has this revision
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// handlePutObject serves PUT /v1/objects/{ref}
func (h *Hub) handlePutObject(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body", "INVALID_OBJECT")
		return
	}
	if len(body) > maxBodySize {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large (max 1MB)", "INVALID_OBJECT")
		return
	}

	// Parse envelope and item
	env, item, err := ParseEnvelope(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "INVALID_OBJECT")
		return
	}

	// Validate magic marker
	if env.In != "dataverse001" {
		writeError(w, http.StatusBadRequest, "missing or wrong 'in' marker", "INVALID_OBJECT")
		return
	}

	// Check ref matches
	expectedRef := item.Ref()
	if ref != expectedRef {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("URL ref %q does not match item %q", ref, expectedRef),
			"REF_MISMATCH")
		return
	}

	// Verify signature (CPU-heavy, before acquiring any locks)
	if err := VerifyEnvelope(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "INVALID_SIGNATURE")
		return
	}

	// Check existing revision
	existing, _ := h.store.Read(ref)
	if existing != nil {
		_, existingItem, err := ParseEnvelope(existing)
		if err == nil && existingItem.Revision >= item.Revision {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("existing revision %d >= incoming %d", existingItem.Revision, item.Revision),
				"REVISION_CONFLICT")
			return
		}
	}

	// Canonicalize for storage
	canonical, err := canonicalJSON(body)
	if err != nil {
		log.Printf("ERROR: PUT /objects/%s: canonical JSON: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}

	ts, err := item.Timestamp()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid timestamp: "+err.Error(), "INVALID_OBJECT")
		return
	}

	// Write to store
	if err := h.store.Write(ref, canonical, ts); err != nil {
		log.Printf("ERROR: PUT /objects/%s: write: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}

	// Update index
	h.index.Update(ref, item, ts)

	if existing != nil {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
	w.Write(canonical)
}

// handleListObjects serves GET /v1/objects
func (h *Hub) handleListObjects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pubkey := q.Get("by")
	typeFilter := q.Get("type")
	limit := parseLimit(q.Get("limit"), 50, 200)
	cursor := parseCursor(q.Get("cursor"))
	includeInboundCounts := q.Get("include") == "inbound_counts"

	metas := h.index.GetAll(pubkey, typeFilter)
	items, refs, nextCursor, hasMore := h.paginateAndLoad(metas, cursor, limit)

	if includeInboundCounts {
		items = h.enrichWithInboundCounts(items, refs)
	}

	writeList(w, items, nextCursor, hasMore)
}

// handleGetInbound serves GET /v1/objects/{ref}/inbound
func (h *Hub) handleGetInbound(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	q := r.URL.Query()

	filters := InboundFilters{
		Relation: q.Get("relation"),
		From:     q.Get("from"),
		Type:     q.Get("type"),
	}
	limit := parseLimit(q.Get("limit"), 50, 200)
	cursor := parseCursor(q.Get("cursor"))
	includeInboundCounts := q.Get("include") == "inbound_counts"

	metas := h.index.GetInbound(ref, filters)
	items, refs, nextCursor, hasMore := h.paginateAndLoad(metas, cursor, limit)

	if includeInboundCounts {
		items = h.enrichWithInboundCounts(items, refs)
	}

	writeList(w, items, nextCursor, hasMore)
}

// paginateAndLoad applies cursor/limit to sorted metas, then loads the actual objects.
// Returns items, their refs (parallel arrays), cursor, and hasMore.
func (h *Hub) paginateAndLoad(metas []ObjectMeta, cursor *Cursor, limit int) ([]json.RawMessage, []string, *string, bool) {
	// Apply cursor: skip items until we pass the cursor position
	if cursor != nil {
		idx := 0
		for idx < len(metas) {
			m := metas[idx]
			if m.UpdatedAt.Before(cursor.T) || (m.UpdatedAt.Equal(cursor.T) && m.Ref < cursor.Ref) {
				break
			}
			idx++
		}
		metas = metas[idx:]
	}

	hasMore := len(metas) > limit
	if hasMore {
		metas = metas[:limit]
	}

	var items []json.RawMessage
	var refs []string
	for _, m := range metas {
		data, err := h.store.Read(m.Ref)
		if err != nil || data == nil {
			log.Printf("WARN: paginate skip %s: read error or missing", m.Ref)
			continue
		}
		items = append(items, json.RawMessage(data))
		refs = append(refs, m.Ref)
	}

	var nextCursor *string
	if hasMore && len(metas) > 0 {
		last := metas[len(metas)-1]
		c := Cursor{T: last.UpdatedAt, Ref: last.Ref}
		encoded, _ := json.Marshal(c)
		s := base64.RawURLEncoding.EncodeToString(encoded)
		nextCursor = &s
	}

	return items, refs, nextCursor, hasMore
}

// enrichWithInboundCounts adds _inbound_counts to each item in the list.
func (h *Hub) enrichWithInboundCounts(items []json.RawMessage, refs []string) []json.RawMessage {
	enriched := make([]json.RawMessage, len(items))
	for i, item := range items {
		counts := h.index.GetInboundCounts(refs[i])
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			log.Printf("WARN: enrich skip %s: unmarshal: %v", refs[i], err)
			enriched[i] = item
			continue
		}
		countsJSON, _ := json.Marshal(counts)
		obj["_inbound_counts"] = countsJSON
		result, err := json.Marshal(obj)
		if err != nil {
			log.Printf("WARN: enrich skip %s: marshal: %v", refs[i], err)
			enriched[i] = item
			continue
		}
		enriched[i] = result
	}
	return enriched
}

func parseLimit(s string, defaultVal, maxVal int) int {
	if s == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return defaultVal
	}
	if n > maxVal {
		return maxVal
	}
	return n
}

func parseCursor(s string) *Cursor {
	if s == "" {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	var c Cursor
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	return &c
}

func writeError(w http.ResponseWriter, status int, msg, code string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIError{Error: msg, Code: code})
}

func writeList(w http.ResponseWriter, items []json.RawMessage, cursor *string, hasMore bool) {
	if items == nil {
		items = []json.RawMessage{}
	}
	resp := ListResponse{
		Items:   items,
		Cursor:  cursor,
		HasMore: hasMore,
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}
