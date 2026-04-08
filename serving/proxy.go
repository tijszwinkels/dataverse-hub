package serving

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/upstream"
	"github.com/tijszwinkels/dataverse-hub/vhost"
)

// Proxy is a caching proxy that forwards requests to an upstream root hub
// while maintaining a local store for offline resilience.
type Proxy struct {
	store            *storage.Store
	index            *storage.Index
	limiter          *auth.RateLimiter
	auth             *auth.AuthStore
	defaultViewerRef string
	shared           *realm.SharedRealms
	Vhost            *vhost.Resolver // nil = vhosting disabled
	VhostMode        string

	// UpstreamPush controls which objects are forwarded to upstream on PUT.
	// "public" (default) — only dataverse001 objects are forwarded.
	// "all" — all objects are forwarded, including identity-realm and shared-realm.
	UpstreamPush string

	upstream *upstream.Client
	pending  *upstream.SyncPending
}

// NewProxy creates a Proxy with the given components.
func NewProxy(store *storage.Store, index *storage.Index, limiter *auth.RateLimiter, auth *auth.AuthStore, defaultViewerRef string, up *upstream.Client, pending *upstream.SyncPending, shared *realm.SharedRealms) *Proxy {
	return &Proxy{
		store:            store,
		index:            index,
		limiter:          limiter,
		auth:             auth,
		defaultViewerRef: defaultViewerRef,
		VhostMode:        VhostModeIsolate,
		upstream:         up,
		pending:          pending,
		shared:           shared,
	}
}

// baseDomain returns the hub's base domain if vhosting is configured.
func (p *Proxy) baseDomain() string {
	if p.Vhost != nil {
		return p.Vhost.BaseDomain()
	}
	return ""
}

// Router returns the chi router with proxy handlers and middleware.
func (p *Proxy) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(p.limiter.Middleware)
	r.Use(p.auth.Middleware)
	r.Use(jsonContentType)

	// Auth routes
	r.Get("/auth/challenge", p.auth.HandleChallenge)
	r.Post("/auth/token", p.auth.HandleToken)
	r.Post("/auth/logout", p.auth.HandleLogout)
	r.Get("/auth/realms", handleAuthRealms(p.shared))

	r.Get("/ask", TLSAskHandler(p.Vhost))
	r.Get("/", p.handleRoot)
	r.Get("/search", p.handleSearch)
	r.Get("/{ref}", p.handleGetObject)
	r.Put("/{ref}", p.handlePutObject)
	r.Get("/{ref}/inbound", p.handleInbound)

	return r
}

// handleRoot serves GET / with vhost-aware routing.
func (p *Proxy) handleRoot(w http.ResponseWriter, r *http.Request) {
	if p.Vhost == nil {
		p.handleRootLegacy(w, r)
		return
	}

	resolved := p.Vhost.Resolve(r.Host)
	switch {
	case resolved == "":
		if baseHostMatches(r.Host, p.Vhost.BaseDomain()) {
			p.handleRootLegacy(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "unknown host", "NOT_FOUND")
		return

	default:
		if normalizeVhostMode(p.VhostMode) == VhostModeRedirect {
			http.Redirect(w, r, pageRedirectTarget(p.VhostMode, p.Vhost, r, resolved, resolved), http.StatusFound)
			return
		}
		if meta, found := p.index.GetMeta(resolved); found && !meta.IsPublic {
			authPK := auth.AuthPubkey(r)
			if !realm.CanRead(meta.Realms, authPK, p.shared) {
				servePrivatePageLogin(w)
				return
			}
		}

		// ETag/304 check via index (no disk I/O)
		if meta, found := p.index.GetMeta(resolved); found {
			etag := `"` + strconv.Itoa(meta.Revision) + `-html"`
			w.Header().Set("Vary", "Accept")
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		data, err := p.store.Read(resolved)
		if err != nil || data == nil {
			log.Printf("[proxy] WARN: vhost root: page %s not found", resolved)
			writeError(w, http.StatusNotFound, "page not found", "NOT_FOUND")
			return
		}
		html := p.resolvePageHTML(resolved, data)
		if html == "" {
			writeError(w, http.StatusNotFound, "page has no HTML", "NOT_FOUND")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, injectBaseDomain(html, p.baseDomain()))
	}
}

// handleRootLegacy redirects to the ROOT object from local index.
func (p *Proxy) handleRootLegacy(w http.ResponseWriter, r *http.Request) {
	metas := p.index.GetAll("", "ROOT", "", false)
	if len(metas) == 0 {
		writeError(w, http.StatusNotFound, "no root object", "NOT_FOUND")
		return
	}
	http.Redirect(w, r, "/"+metas[0].Ref, http.StatusFound)
}

// handleGetObject proxies GET /{ref} through upstream with ETag enrichment.
func (p *Proxy) handleGetObject(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")

	// Build upstream request — ETag reflects our cache state, not the client's
	clientETag := r.Header.Get("If-None-Match")
	upstreamETag := p.buildUpstreamETag(ref)

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.upstream.BaseURL()+"/"+ref, nil)
	if err != nil {
		log.Printf("[proxy] ERROR: GET /%s: build request: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	upstreamReq.Header.Set("Accept", "application/json")
	if upstreamETag != "" {
		upstreamReq.Header.Set("If-None-Match", upstreamETag)
	}

	resp, err := p.upstream.Do(upstreamReq, nil)
	if err != nil {
		// Upstream unreachable — fall back to local cache
		log.Printf("[proxy] WARN: GET /%s: upstream unreachable, serving from cache", ref)
		p.serveFromLocalCache(w, r, ref, clientETag)
		return
	}
	defer resp.Body.Close()

	// Phase 1: Sync main object with upstream
	switch resp.StatusCode {
	case http.StatusNotModified:
		// local cache is current

	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("[proxy] ERROR: GET /%s: read upstream body: %v", ref, err)
			writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
			return
		}
		p.CacheLocally(ref, body)

	case http.StatusNotFound:
		localData, _ := p.store.Read(ref)
		if localData == nil {
			writeError(w, http.StatusNotFound, "object not found", "NOT_FOUND")
			return
		}
		log.Printf("[proxy] GET /%s: upstream 404 but found locally, serving + pushing", ref)
		go p.pushToUpstream(ref, localData)

	default:
		if upstream.IsDown(resp.StatusCode) {
			log.Printf("[proxy] WARN: GET /%s: upstream returned %d, falling back to cache", ref, resp.StatusCode)
			io.Copy(io.Discard, resp.Body)
			p.serveFromLocalCache(w, r, ref, clientETag)
			return
		}
		// Forward non-gateway upstream error (4xx, etc.)
		body, _ := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}

	// Phase 2: Sync page dependencies (if serving HTML)
	if acceptsHTML(r) {
		p.ensurePageDepsFresh(ref)
	}

	// Phase 3: Serve from local cache (no more upstream calls)
	p.serveFromLocalCache(w, r, ref, clientETag)
}

// handlePutObject proxies PUT /{ref} with local signature verification.
func (p *Proxy) handlePutObject(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body", "INVALID_OBJECT")
		return
	}
	if len(body) > maxBodySize {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large (max 10MB)", "INVALID_OBJECT")
		return
	}

	// Parse and validate locally first
	env, item, err := object.ParseEnvelope(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "INVALID_OBJECT")
		return
	}
	realms := object.ResolveIn(env, item)

	// Validate pubkey-realm ownership: each pubkey-realm must match item.pubkey
	for _, pr := range object.PubkeyRealms(realms) {
		if pr != item.Pubkey {
			writeError(w, http.StatusForbidden,
				"pubkey-realm does not match item pubkey",
				"REALM_FORBIDDEN")
			return
		}
	}

	// Object must belong to dataverse001, a self-owned pubkey-realm, or a configured shared realm
	if !realm.ValidateRealmsForPut(realms, item.Pubkey, p.shared) {
		writeError(w, http.StatusBadRequest,
			"object must belong to dataverse001, server-public, a self-owned pubkey-realm, or a configured shared realm",
			"INVALID_OBJECT")
		return
	}
	if ref != item.Ref() {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("URL ref %q does not match item %q", ref, item.Ref()),
			"REF_MISMATCH")
		return
	}

	// Local signature verification
	if err := object.VerifyEnvelope(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "INVALID_SIGNATURE")
		return
	}

	// Canonicalize
	canonical, err := object.CanonicalJSON(body)
	if err != nil {
		log.Printf("[proxy] ERROR: PUT /%s: canonical JSON: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}

	// Non-global objects (private, server-public) are stored locally only — unless upstream_push = "all"
	if !realm.IsGlobalObject(realms) && p.UpstreamPush != "all" {
		p.storePrivateLocally(w, ref, item, canonical, realms)
		return
	}

	// Forward to upstream
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPut, p.upstream.BaseURL()+"/"+ref, nil)
	if err != nil {
		log.Printf("[proxy] ERROR: PUT /%s: build request: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")

	resp, err := p.upstream.Do(upstreamReq, canonical)
	if err != nil {
		// Upstream unreachable — store locally with sync pending
		log.Printf("[proxy] WARN: PUT /%s: upstream unreachable, storing locally (sync pending)", ref)
		p.storeLocallyWithPending(w, ref, item, canonical, realms)
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		// Backup old version before caching
		if existingMeta, isUpdate := p.index.GetMeta(ref); isUpdate {
			if err := p.store.Backup(ref, existingMeta.Revision); err != nil {
				log.Printf("[proxy] WARN: PUT /%s: backup rev %d failed: %v", ref, existingMeta.Revision, err)
			}
		}
		// Cache locally
		ts, _ := item.Timestamp()
		p.store.Write(ref, canonical, ts)
		p.index.Update(ref, item, ts, realms)
		// Update vhost hash map for PAGE objects
		if p.Vhost != nil && item.Type == "PAGE" {
			p.Vhost.AddPage(ref)
		}
		log.Printf("[proxy] stored %s rev %d (%s)", ref, item.Revision, item.Type)

		w.WriteHeader(resp.StatusCode)
		w.Write(canonical)

	case http.StatusConflict:
		// Upstream has newer revision — fetch and cache it
		log.Printf("[proxy] PUT /%s: upstream conflict, fetching newer version", ref)
		go p.fetchAndCacheFromUpstream(ref)
		respBody, _ := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)

	default:
		// Forward upstream error (400, etc.)
		respBody, _ := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
	}
}

// handleSearch forwards GET /search to upstream, falls back to local.
func (p *Proxy) handleSearch(w http.ResponseWriter, r *http.Request) {
	p.forwardListEndpoint(w, r, "/search?"+r.URL.RawQuery)
}

// handleInbound forwards GET /{ref}/inbound to upstream, falls back to local.
func (p *Proxy) handleInbound(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	p.forwardListEndpoint(w, r, "/"+ref+"/inbound?"+r.URL.RawQuery)
}

// forwardListEndpoint forwards a list-type request to upstream, falling back to local on failure.
// When the user is authenticated, merges local private objects into upstream results.
func (p *Proxy) forwardListEndpoint(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.upstream.BaseURL()+upstreamPath, nil)
	if err != nil {
		log.Printf("[proxy] ERROR: forward %s: build request: %v", upstreamPath, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	upstreamReq.Header.Set("Accept", "application/json")

	resp, err := p.upstream.Do(upstreamReq, nil)
	if err != nil {
		// Fall back to local
		log.Printf("[proxy] WARN: upstream unreachable for %s, serving from local index", upstreamPath)
		p.serveLocalList(w, r)
		return
	}
	defer resp.Body.Close()

	if upstream.IsDown(resp.StatusCode) || resp.StatusCode == http.StatusNotFound {
		if resp.StatusCode != http.StatusNotFound {
			log.Printf("[proxy] WARN: upstream returned %d for %s, falling back to local index", resp.StatusCode, upstreamPath)
		}
		io.Copy(io.Discard, resp.Body)
		p.serveLocalList(w, r)
		return
	}

	body, _ := io.ReadAll(resp.Body)

	// Background-cache upstream items we don't have locally yet
	go p.cacheUpstreamListRefs(body)

	// If user is not authenticated, forward upstream response as-is (fast path)
	authPK := auth.AuthPubkey(r)
	if authPK == "" {
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}

	// User is authenticated — merge local private objects into upstream results
	var upstreamResp object.ListResponse
	if err := json.Unmarshal(body, &upstreamResp); err != nil {
		// Can't parse upstream response — forward raw
		log.Printf("[proxy] WARN: forward %s: parse upstream response: %v", upstreamPath, err)
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}

	p.mergePrivateIntoUpstream(w, r, upstreamResp, authPK)
}

// serveLocalList serves list/inbound results from the local index (fallback).
func (p *Proxy) serveLocalList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Determine if this is a search or inbound request based on path
	ref := chi.URLParam(r, "ref")
	includeInboundCounts := q.Get("include") == "inbound_counts"
	membersOnly := q.Get("members_only") != "false"

	authPK := auth.AuthPubkey(r)
	var metas []object.ObjectMeta
	if ref != "" {
		// Inbound
		filters := storage.InboundFilters{
			Relation: q.Get("relation"),
			From:     q.Get("from"),
			Type:     q.Get("type"),
		}
		metas = p.index.GetInbound(ref, filters, authPK, membersOnly)
	} else {
		// Search
		metas = p.index.GetAll(q.Get("by"), q.Get("type"), authPK, membersOnly)
	}

	limit := parseLimit(q.Get("limit"), 50, 200)
	cursor := parseCursor(q.Get("cursor"))

	items, refs, nextCursor, hasMore := paginateAndLoad(p.store, metas, cursor, limit)

	if includeInboundCounts {
		items = enrichWithInboundCounts(p.index, items, refs)
	}

	writeList(w, items, nextCursor, hasMore)
}

// mergePrivateIntoUpstream merges local private objects into an upstream list response.
// Private objects never exist on upstream, so no dedup is needed. Both sources are sorted
// by (UpdatedAt DESC, Ref), enabling a standard merge-sort.
func (p *Proxy) mergePrivateIntoUpstream(w http.ResponseWriter, r *http.Request, upstreamResp object.ListResponse, authPK string) {
	q := r.URL.Query()
	ref := chi.URLParam(r, "ref")
	includeInboundCounts := q.Get("include") == "inbound_counts"
	membersOnly := q.Get("members_only") != "false"
	limit := parseLimit(q.Get("limit"), 50, 200)
	cursor := parseCursor(q.Get("cursor"))

	// Query local index for objects visible to this user
	var metas []object.ObjectMeta
	if ref != "" {
		filters := storage.InboundFilters{
			Relation: q.Get("relation"),
			From:     q.Get("from"),
			Type:     q.Get("type"),
		}
		metas = p.index.GetInbound(ref, filters, authPK, membersOnly)
	} else {
		metas = p.index.GetAll(q.Get("by"), q.Get("type"), authPK, membersOnly)
	}

	// Filter to objects not already on upstream.
	// Private objects and server-public objects are local-only;
	// only global (dataverse001) objects exist on upstream.
	privateMetas := make([]object.ObjectMeta, 0, len(metas))
	for _, m := range metas {
		if !realm.IsGlobalObject(m.Realms) {
			privateMetas = append(privateMetas, m)
		}
	}

	// Fast path: no local private objects — forward upstream response as-is
	if len(privateMetas) == 0 {
		writeList(w, upstreamResp.Items, upstreamResp.Cursor, upstreamResp.HasMore)
		return
	}

	// Apply cursor to local private metas (skip items before cursor position)
	if cursor != nil {
		idx := 0
		for idx < len(privateMetas) {
			m := privateMetas[idx]
			if m.UpdatedAt.Before(cursor.T) || (m.UpdatedAt.Equal(cursor.T) && m.Ref < cursor.Ref) {
				break
			}
			idx++
		}
		privateMetas = privateMetas[idx:]
	}

	// Parse upstream items to extract sort keys
	type sortableItem struct {
		data      json.RawMessage
		ref       string
		updatedAt time.Time
	}

	upstreamItems := make([]sortableItem, 0, len(upstreamResp.Items))
	for _, raw := range upstreamResp.Items {
		ts, itemRef := extractSortKey(raw)
		upstreamItems = append(upstreamItems, sortableItem{data: raw, ref: itemRef, updatedAt: ts})
	}

	// Load local private objects
	localItems := make([]sortableItem, 0, len(privateMetas))
	for _, m := range privateMetas {
		data, err := p.store.Read(m.Ref)
		if err != nil || data == nil {
			continue
		}
		item := json.RawMessage(data)
		if m.Type == "BLOB" {
			item = stripBlobData(item)
		}
		localItems = append(localItems, sortableItem{data: item, ref: m.Ref, updatedAt: m.UpdatedAt})
	}

	// Merge-sort both lists by (UpdatedAt DESC, Ref DESC)
	merged := make([]sortableItem, 0, len(upstreamItems)+len(localItems))
	ui, li := 0, 0
	for ui < len(upstreamItems) && li < len(localItems) {
		u, l := upstreamItems[ui], localItems[li]
		// Pick the newer item (DESC order)
		if u.updatedAt.After(l.updatedAt) || (u.updatedAt.Equal(l.updatedAt) && u.ref >= l.ref) {
			merged = append(merged, u)
			ui++
		} else {
			merged = append(merged, l)
			li++
		}
	}
	for ; ui < len(upstreamItems); ui++ {
		merged = append(merged, upstreamItems[ui])
	}
	for ; li < len(localItems); li++ {
		merged = append(merged, localItems[li])
	}

	// Apply limit
	hasMore := upstreamResp.HasMore || len(merged) > limit
	if len(merged) > limit {
		merged = merged[:limit]
	}

	// Build result
	items := make([]json.RawMessage, len(merged))
	refs := make([]string, len(merged))
	for i, m := range merged {
		items[i] = m.data
		refs[i] = m.ref
	}

	if includeInboundCounts {
		items = enrichWithInboundCounts(p.index, items, refs)
	}

	// Generate cursor from last merged item
	var nextCursor *string
	if hasMore && len(merged) > 0 {
		last := merged[len(merged)-1]
		c := object.Cursor{T: last.updatedAt, Ref: last.ref}
		encoded, _ := json.Marshal(c)
		s := encodeBase64Cursor(encoded)
		nextCursor = &s
	}

	writeList(w, items, nextCursor, hasMore)
}

// extractSortKey extracts (UpdatedAt, Ref) from a raw JSON item for merge-sorting.
func extractSortKey(raw json.RawMessage) (time.Time, string) {
	// Parse just the fields we need from the envelope
	var env struct {
		Item struct {
			Pubkey    string `json:"pubkey"`
			ID        string `json:"id"`
			Ref       string `json:"ref"`
			UpdatedAt string `json:"updated_at"`
			CreatedAt string `json:"created_at"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return time.Time{}, ""
	}

	ref := env.Item.Ref
	if ref == "" && env.Item.Pubkey != "" && env.Item.ID != "" {
		ref = env.Item.Pubkey + "." + env.Item.ID
	}

	tsStr := env.Item.UpdatedAt
	if tsStr == "" {
		tsStr = env.Item.CreatedAt
	}
	ts, _ := time.Parse(time.RFC3339, tsStr)

	return ts, ref
}

// cacheLocally stores an object in the local store and updates the index.
// Refuses to downgrade: if local has a newer revision, pushes local to upstream instead.
func (p *Proxy) CacheLocally(ref string, data []byte) {
	_, item, err := object.ParseEnvelope(data)
	if err != nil {
		log.Printf("[proxy] WARN: cache %s: parse: %v", ref, err)
		return
	}

	if existingMeta, isUpdate := p.index.GetMeta(ref); isUpdate {
		if existingMeta.Revision > item.Revision {
			// Local is newer — push local to upstream instead of downgrading
			log.Printf("[proxy] cache %s: local rev %d > upstream rev %d, pushing local", ref, existingMeta.Revision, item.Revision)
			if localData, err := p.store.Read(ref); err == nil && localData != nil {
				go p.pushToUpstream(ref, localData)
			}
			return
		}
		if existingMeta.Revision == item.Revision {
			return // same revision, nothing to do
		}
		// Incoming is newer — backup old before overwriting
		if err := p.store.Backup(ref, existingMeta.Revision); err != nil {
			log.Printf("[proxy] WARN: cache %s: backup rev %d failed: %v", ref, existingMeta.Revision, err)
		}
	}

	ts, _ := item.Timestamp()
	if err := p.store.Write(ref, data, ts); err != nil {
		log.Printf("[proxy] WARN: cache %s: write: %v", ref, err)
		return
	}
	p.index.Update(ref, item, ts)
	// Update vhost hash map for PAGE objects
	if p.Vhost != nil && item.Type == "PAGE" {
		p.Vhost.AddPage(ref)
	}
	log.Printf("[proxy] cached %s rev %d (%s)", ref, item.Revision, item.Type)
}

// ensureFresh checks upstream for a newer version of ref and updates local cache.
// On failure, local cache is left as-is (best effort).
func (p *Proxy) ensureFresh(ref string) {
	upstreamETag := p.buildUpstreamETag(ref)

	req, err := http.NewRequest(http.MethodGet, p.upstream.BaseURL()+"/"+ref, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	if upstreamETag != "" {
		req.Header.Set("If-None-Match", upstreamETag)
	}

	resp, err := p.upstream.Do(req, nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		// already fresh
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return
		}
		p.CacheLocally(ref, body)
	case http.StatusNotFound:
		if data, err := p.store.Read(ref); err == nil && data != nil {
			go p.pushToUpstream(ref, data)
		}
	default:
		io.Copy(io.Discard, resp.Body)
	}
}

// ensurePageDepsFresh syncs the page-related objects that may be needed for
// HTML rendering. Must be called BEFORE the serve phase.
func (p *Proxy) ensurePageDepsFresh(ref string) {
	data, err := p.store.Read(ref)
	if err != nil || data == nil {
		return
	}
	_, item, err := object.ParseEnvelope(data)
	if err != nil {
		return
	}

	// Object is itself a PAGE — HTML is inline, nothing to sync
	if item.Type == "PAGE" {
		return
	}

	// Has a page relation — sync that ref
	if pageRels, ok := item.Relations["page"]; ok && len(pageRels) > 0 {
		var rel object.RelationRef
		if json.Unmarshal(pageRels[0], &rel) == nil && rel.Ref != "" {
			p.ensureFresh(rel.Ref)
			return
		}
	}

	// No page relation — try default viewer
	if p.defaultViewerRef != "" && ref != p.defaultViewerRef {
		p.ensureFresh(p.defaultViewerRef)
		// Default viewer may itself have a page relation
		p.ensurePageDepsFresh(p.defaultViewerRef)
	}
}

// --- internal helpers ---

// buildUpstreamETag returns the ETag to send to upstream based on our local
// cache state. Always uses OUR cached revision — never the client's ETag.
// The upstream question is "is my cache current?", which is independent of
// what the client has.
func (p *Proxy) buildUpstreamETag(ref string) string {
	meta, found := p.index.GetMeta(ref)
	if !found {
		return ""
	}
	return `"` + strconv.Itoa(meta.Revision) + `"`
}

// serveFromLocalCache reads from local store and serves with content negotiation.
func (p *Proxy) serveFromLocalCache(w http.ResponseWriter, r *http.Request, ref string, clientETag string) {
	meta, found := p.index.GetMeta(ref)
	if !found {
		data, err := p.store.Read(ref)
		if err != nil || data == nil {
			writeError(w, http.StatusNotFound, "object not found", "NOT_FOUND")
			return
		}
		p.serveObjectData(w, r, ref, data)
		return
	}

	// Private object access check
	if !meta.IsPublic {
		authPK := auth.AuthPubkey(r)
		if !realm.CanRead(meta.Realms, authPK, p.shared) {
			if p.Vhost != nil && acceptsHTML(r) && (meta.Type == "PAGE" || meta.HasPageRelation) {
				pageRef := ref
				if meta.HasPageRelation && meta.PageRef != "" {
					pageRef = meta.PageRef
				}
				if !canonicalPageHost(p.VhostMode, p.Vhost, r.Host, pageRef) {
					target := pageRedirectTarget(p.VhostMode, p.Vhost, r, ref, pageRef)
					if r.URL.RawQuery != "" {
						target += "?" + r.URL.RawQuery
					}
					http.Redirect(w, r, target, http.StatusFound)
					return
				}
				servePrivatePageLogin(w)
				return
			}
			writeError(w, http.StatusNotFound, "object not found", "NOT_FOUND")
			return
		}
	}

	// Build ETag and check 304 (same logic as Hub.handleGetObject)
	etag := `"` + strconv.Itoa(meta.Revision) + `"`
	isHTML := false
	if acceptsHTML(r) {
		if meta.Type == "PAGE" || meta.HasPageRelation {
			isHTML = true
		} else if p.defaultViewerRef != "" && ref != p.defaultViewerRef {
			isHTML = true
		}
	}
	// BLOB content negotiation overrides the default viewer (but not PAGE/page-relation)
	isBlob := false
	if meta.Type == "BLOB" && meta.MimeType != "" && acceptsMimeType(r, meta.MimeType) {
		isBlob = true
		isHTML = false
	}
	if isHTML {
		etag = etag[:len(etag)-1] + pageETagSuffix(p.index, meta, p.defaultViewerRef) + `"`
	} else if isBlob {
		etag = etag[:len(etag)-1] + `-blob"`
	}

	// Vhost redirect: if this is a PAGE and we're on the wrong subdomain, redirect
	if p.Vhost != nil && acceptsHTML(r) && (meta.Type == "PAGE" || meta.HasPageRelation) {
		pageRef := ref
		if meta.HasPageRelation && meta.PageRef != "" {
			pageRef = meta.PageRef
		}
		if !canonicalPageHost(p.VhostMode, p.Vhost, r.Host, pageRef) {
			target := pageRedirectTarget(p.VhostMode, p.Vhost, r, ref, pageRef)
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
	}

	w.Header().Set("Vary", "Accept")
	w.Header().Set("ETag", etag)

	if clientETag == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	data, err := p.store.Read(ref)
	if err != nil || data == nil {
		writeError(w, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}
	p.serveObjectData(w, r, ref, data)
}

// serveObjectData writes the response body with content negotiation.
func (p *Proxy) serveObjectData(w http.ResponseWriter, r *http.Request, ref string, data []byte) {
	// Set ETag from the data if not already set
	if w.Header().Get("ETag") == "" {
		_, item, err := object.ParseEnvelope(data)
		if err == nil {
			etag := `"` + strconv.Itoa(item.Revision) + `"`
			if acceptsHTML(r) {
				if meta, found := p.index.GetMeta(ref); found {
					etag = etag[:len(etag)-1] + pageETagSuffix(p.index, meta, p.defaultViewerRef) + `"`
				} else {
					etag = etag[:len(etag)-1] + `-html"`
				}
			}
			w.Header().Set("Vary", "Accept")
			w.Header().Set("ETag", etag)
		}
	}

	if serveBlob(w, r, data) {
		return
	}

	if acceptsHTML(r) {
		html := p.resolvePageHTML(ref, data)
		if html == "" && p.defaultViewerRef != "" && ref != p.defaultViewerRef {
			html = p.resolveDefaultViewerHTML(ref)
		}
		if html != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, injectBaseDomain(html, p.baseDomain()))
			return
		}
		log.Printf("[proxy] GET /%s: client accepts HTML but no PAGE found, serving JSON", ref)
	}

	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// resolvePageHTML extracts HTML from a PAGE object or follows a page relation.
// Only reads from local store — all upstream syncing must be done beforehand.
func (p *Proxy) resolvePageHTML(reqRef string, data []byte) string {
	_, item, err := object.ParseEnvelope(data)
	if err != nil {
		return ""
	}

	if item.Type == "PAGE" {
		log.Printf("[proxy] GET /%s: serving inline PAGE HTML", reqRef)
		return extractHTML(item)
	}

	pageRels, ok := item.Relations["page"]
	if !ok || len(pageRels) == 0 {
		return ""
	}
	var rel object.RelationRef
	if err := json.Unmarshal(pageRels[0], &rel); err != nil || rel.Ref == "" {
		return ""
	}
	pageData, err := p.store.Read(rel.Ref)
	if err != nil || pageData == nil {
		log.Printf("[proxy] GET /%s: page relation %s not in local store", reqRef, rel.Ref)
		return ""
	}
	_, pageItem, err := object.ParseEnvelope(pageData)
	if err != nil || pageItem.Type != "PAGE" {
		return ""
	}
	log.Printf("[proxy] GET /%s: serving HTML via page relation %s", reqRef, rel.Ref)
	return extractHTML(pageItem)
}

// resolveDefaultViewerHTML loads the default viewer PAGE HTML from local store.
func (p *Proxy) resolveDefaultViewerHTML(reqRef string) string {
	data, err := p.store.Read(p.defaultViewerRef)
	if err != nil || data == nil {
		log.Printf("[proxy] GET /%s: default viewer %s not in local store", reqRef, p.defaultViewerRef)
		return ""
	}
	log.Printf("[proxy] GET /%s: trying default viewer %s", reqRef, p.defaultViewerRef)
	return p.resolvePageHTML(reqRef, data)
}

// storePrivateLocally stores a private object locally without forwarding to upstream.
func (p *Proxy) storePrivateLocally(w http.ResponseWriter, ref string, item *object.Item, canonical []byte, realms object.InField) {
	existingMeta, isUpdate := p.index.GetMeta(ref)
	if isUpdate && existingMeta.Revision >= item.Revision {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("existing revision %d >= incoming %d", existingMeta.Revision, item.Revision),
			"REVISION_CONFLICT")
		return
	}

	ts, err := item.Timestamp()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid timestamp: "+err.Error(), "INVALID_OBJECT")
		return
	}

	if isUpdate {
		if err := p.store.Backup(ref, existingMeta.Revision); err != nil {
			log.Printf("[proxy] WARN: PUT /%s: backup rev %d failed: %v", ref, existingMeta.Revision, err)
		}
	}

	if err := p.store.Write(ref, canonical, ts); err != nil {
		log.Printf("[proxy] ERROR: PUT /%s: store write: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	p.index.Update(ref, item, ts, realms)
	// Update vhost hash map for PAGE objects
	if p.Vhost != nil && item.Type == "PAGE" {
		p.Vhost.AddPage(ref)
	}
	log.Printf("stored %s rev %d (%s) [private, local-only]", ref, item.Revision, item.Type)

	w.WriteHeader(http.StatusCreated)
	w.Write(canonical)
}

// storeLocallyWithPending stores an object locally and adds to sync pending.
func (p *Proxy) storeLocallyWithPending(w http.ResponseWriter, ref string, item *object.Item, canonical []byte, realms object.InField) {
	// Check revision against local index
	existingMeta, isUpdate := p.index.GetMeta(ref)
	if isUpdate && existingMeta.Revision >= item.Revision {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("existing revision %d >= incoming %d", existingMeta.Revision, item.Revision),
			"REVISION_CONFLICT")
		return
	}

	ts, err := item.Timestamp()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid timestamp: "+err.Error(), "INVALID_OBJECT")
		return
	}

	// Backup old version before overwriting
	if isUpdate {
		if err := p.store.Backup(ref, existingMeta.Revision); err != nil {
			log.Printf("[proxy] WARN: PUT /%s: backup rev %d failed: %v", ref, existingMeta.Revision, err)
		}
	}

	// Write to sync_pending first (crash safety)
	if err := p.pending.Add(ref, canonical); err != nil {
		log.Printf("[proxy] ERROR: PUT /%s: sync pending add: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}

	// Write to main store
	if err := p.store.Write(ref, canonical, ts); err != nil {
		log.Printf("[proxy] ERROR: PUT /%s: store write: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	p.index.Update(ref, item, ts, realms)
	// Update vhost hash map for PAGE objects
	if p.Vhost != nil && item.Type == "PAGE" {
		p.Vhost.AddPage(ref)
	}
	log.Printf("[proxy] stored %s rev %d (%s) (sync pending)", ref, item.Revision, item.Type)

	// 202 Accepted — stored locally, sync pending
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"status": "pending_sync",
		"ref":    ref,
	})
}

// pushToUpstream PUTs a local object to upstream (fire-and-forget).
// Used when we discover upstream is missing an object we have locally.
// Only global objects (dataverse001) are pushed; server-public and private objects stay local.
func (p *Proxy) pushToUpstream(ref string, data []byte) {
	// Guard: only push global objects upstream
	env, item, err := object.ParseEnvelope(data)
	if err == nil {
		realms := object.ResolveIn(env, item)
		if !realm.IsGlobalObject(realms) && p.UpstreamPush != "all" {
			log.Printf("[proxy] skip push %s: not a global object", ref)
			return
		}
	}

	req, err := http.NewRequest(http.MethodPut, p.upstream.BaseURL()+"/"+ref, nil)
	if err != nil {
		log.Printf("[proxy] WARN: push %s: build request: %v", ref, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.upstream.Do(req, data)
	if err != nil {
		log.Printf("[proxy] WARN: push %s: upstream unreachable: %v", ref, err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		log.Printf("[proxy] pushed %s to upstream (%d)", ref, resp.StatusCode)
	} else {
		log.Printf("[proxy] WARN: push %s: upstream returned %d", ref, resp.StatusCode)
	}
}

// fetchAndCacheFromUpstream GETs an object from upstream and caches it locally.
// Used after a PUT 409 conflict to get the newer version.
func (p *Proxy) fetchAndCacheFromUpstream(ref string) {
	req, err := http.NewRequest(http.MethodGet, p.upstream.BaseURL()+"/"+ref, nil)
	if err != nil {
		log.Printf("[proxy] WARN: fetch-after-conflict %s: build request: %v", ref, err)
		return
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.upstream.Do(req, nil)
	if err != nil {
		log.Printf("[proxy] WARN: fetch-after-conflict %s: %v", ref, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[proxy] WARN: fetch-after-conflict %s: read body: %v", ref, err)
		return
	}
	p.CacheLocally(ref, body)
}

// cacheUpstreamListRefs parses a list response from upstream and triggers
// background ensureFresh calls for items not yet in the local cache.
// Runs sequentially with a small delay to avoid hammering upstream.
func (p *Proxy) cacheUpstreamListRefs(body []byte) {
	var resp object.ListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return
	}

	var cached int
	for _, raw := range resp.Items {
		_, ref := extractSortKey(raw)
		if ref == "" {
			continue
		}
		if _, found := p.index.GetMeta(ref); found {
			continue // already in local cache
		}
		p.ensureFresh(ref)
		cached++
		time.Sleep(200 * time.Millisecond)
	}
	if cached > 0 {
		log.Printf("[proxy] background-cached %d/%d items from upstream list", cached, len(resp.Items))
	}
}

// encodeBase64Cursor encodes cursor bytes as base64url.
func encodeBase64Cursor(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}
