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
	"github.com/go-chi/chi/v5/middleware"
)

// Proxy is a caching proxy that forwards requests to an upstream root hub
// while maintaining a local store for offline resilience.
type Proxy struct {
	store            *Store
	index            *Index
	limiter          *RateLimiter
	defaultViewerRef string

	upstream *Upstream
	pending  *SyncPending
}

// NewProxy creates a Proxy with the given components.
func NewProxy(store *Store, index *Index, limiter *RateLimiter, defaultViewerRef string, upstream *Upstream, pending *SyncPending) *Proxy {
	return &Proxy{
		store:            store,
		index:            index,
		limiter:          limiter,
		defaultViewerRef: defaultViewerRef,
		upstream:         upstream,
		pending:          pending,
	}
}

// Router returns the chi router with proxy handlers and middleware.
func (p *Proxy) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(p.limiter.Middleware)
	r.Use(jsonContentType)

	r.Get("/", p.handleRoot)
	r.Get("/search", p.handleSearch)
	r.Get("/{ref}", p.handleGetObject)
	r.Put("/{ref}", p.handlePutObject)
	r.Get("/{ref}/inbound", p.handleInbound)

	return r
}

// handleRoot redirects to the ROOT object from local index.
func (p *Proxy) handleRoot(w http.ResponseWriter, r *http.Request) {
	metas := p.index.GetAll("", "ROOT")
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

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.upstream.baseURL+"/"+ref, nil)
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

	switch resp.StatusCode {
	case http.StatusNotModified:
		p.serveFromLocalCache(w, r, ref, clientETag)

	case http.StatusOK:
		// Upstream has data — cache locally and serve
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("[proxy] ERROR: GET /%s: read upstream body: %v", ref, err)
			writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
			return
		}
		p.cacheLocally(ref, body)
		p.serveObjectData(w, r, ref, body)

	case http.StatusNotFound:
		writeError(w, http.StatusNotFound, "object not found", "NOT_FOUND")

	default:
		// Forward upstream error
		body, _ := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
	}
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
		writeError(w, http.StatusRequestEntityTooLarge, "body too large (max 1MB)", "INVALID_OBJECT")
		return
	}

	// Parse and validate locally first
	env, item, err := ParseEnvelope(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "INVALID_OBJECT")
		return
	}
	if env.In != "dataverse001" {
		writeError(w, http.StatusBadRequest, "missing or wrong 'in' marker", "INVALID_OBJECT")
		return
	}
	if ref != item.Ref() {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("URL ref %q does not match item %q", ref, item.Ref()),
			"REF_MISMATCH")
		return
	}

	// Local signature verification
	if err := VerifyEnvelope(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "INVALID_SIGNATURE")
		return
	}

	// Canonicalize
	canonical, err := canonicalJSON(body)
	if err != nil {
		log.Printf("[proxy] ERROR: PUT /%s: canonical JSON: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}

	// Forward to upstream
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPut, p.upstream.baseURL+"/"+ref, nil)
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
		p.storeLocallyWithPending(w, ref, item, canonical)
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		// Cache locally
		ts, _ := item.Timestamp()
		p.store.Write(ref, canonical, ts)
		p.index.Update(ref, item, ts)
		log.Printf("[proxy] stored %s rev %d (%s)", ref, item.Revision, item.Type)

		w.WriteHeader(resp.StatusCode)
		w.Write(canonical)

	default:
		// Forward upstream error (409, 400, etc.)
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
func (p *Proxy) forwardListEndpoint(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.upstream.baseURL+upstreamPath, nil)
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

	// Forward upstream response directly
	body, _ := io.ReadAll(resp.Body)
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// serveLocalList serves list/inbound results from the local index (fallback).
func (p *Proxy) serveLocalList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Determine if this is a search or inbound request based on path
	ref := chi.URLParam(r, "ref")
	includeInboundCounts := q.Get("include") == "inbound_counts"

	var metas []ObjectMeta
	if ref != "" {
		// Inbound
		filters := InboundFilters{
			Relation: q.Get("relation"),
			From:     q.Get("from"),
			Type:     q.Get("type"),
		}
		metas = p.index.GetInbound(ref, filters)
	} else {
		// Search
		metas = p.index.GetAll(q.Get("by"), q.Get("type"))
	}

	limit := parseLimit(q.Get("limit"), 50, 200)
	cursor := parseCursor(q.Get("cursor"))

	items, refs, nextCursor, hasMore := p.paginateAndLoad(metas, cursor, limit)

	if includeInboundCounts {
		items = p.enrichWithInboundCounts(items, refs)
	}

	writeList(w, items, nextCursor, hasMore)
}

// paginateAndLoad applies cursor/limit to sorted metas, then loads the actual objects.
func (p *Proxy) paginateAndLoad(metas []ObjectMeta, cursor *Cursor, limit int) ([]json.RawMessage, []string, *string, bool) {
	// Reuse the same logic as Hub — identical pagination
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
		data, err := p.store.Read(m.Ref)
		if err != nil || data == nil {
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
		s := encodeBase64Cursor(encoded)
		nextCursor = &s
	}

	return items, refs, nextCursor, hasMore
}

// enrichWithInboundCounts adds _inbound_counts to each item.
func (p *Proxy) enrichWithInboundCounts(items []json.RawMessage, refs []string) []json.RawMessage {
	enriched := make([]json.RawMessage, len(items))
	for i, item := range items {
		counts := p.index.GetInboundCounts(refs[i])
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			enriched[i] = item
			continue
		}
		countsJSON, _ := json.Marshal(counts)
		obj["_inbound_counts"] = countsJSON
		result, _ := json.Marshal(obj)
		enriched[i] = result
	}
	return enriched
}

// cacheLocally stores an object in the local store and updates the index.
func (p *Proxy) cacheLocally(ref string, data []byte) {
	_, item, err := ParseEnvelope(data)
	if err != nil {
		log.Printf("[proxy] WARN: cache %s: parse: %v", ref, err)
		return
	}
	ts, _ := item.Timestamp()
	if err := p.store.Write(ref, data, ts); err != nil {
		log.Printf("[proxy] WARN: cache %s: write: %v", ref, err)
		return
	}
	p.index.Update(ref, item, ts)
	log.Printf("[proxy] cached %s rev %d (%s)", ref, item.Revision, item.Type)
}

// readOrFetch reads an object from local store, falling back to an upstream
// fetch (and local cache) if not found locally.
func (p *Proxy) readOrFetch(ref string) ([]byte, error) {
	data, err := p.store.Read(ref)
	if err == nil && data != nil {
		return data, nil
	}

	req, err := http.NewRequest(http.MethodGet, p.upstream.baseURL+"/"+ref, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.upstream.Do(req, nil)
	if err != nil {
		return nil, fmt.Errorf("upstream unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	p.cacheLocally(ref, body)
	return body, nil
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
	if isHTML {
		etag = etag[:len(etag)-1] + `-html"`
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
		_, item, err := ParseEnvelope(data)
		if err == nil {
			etag := `"` + strconv.Itoa(item.Revision) + `"`
			if acceptsHTML(r) {
				etag = etag[:len(etag)-1] + `-html"`
			}
			w.Header().Set("Vary", "Accept")
			w.Header().Set("ETag", etag)
		}
	}

	if acceptsHTML(r) {
		html := p.resolvePageHTML(ref, data)
		if html == "" && p.defaultViewerRef != "" && ref != p.defaultViewerRef {
			html = p.resolveDefaultViewerHTML(ref)
		}
		if html != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, html)
			return
		}
		log.Printf("[proxy] GET /%s: client accepts HTML but no PAGE found, serving JSON", ref)
	}

	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// resolvePageHTML extracts HTML from a PAGE object or follows a page relation.
// reqRef is the originally requested ref (for logging).
func (p *Proxy) resolvePageHTML(reqRef string, data []byte) string {
	_, item, err := ParseEnvelope(data)
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
	var rel RelationRef
	if err := json.Unmarshal(pageRels[0], &rel); err != nil || rel.Ref == "" {
		return ""
	}
	pageData, err := p.readOrFetch(rel.Ref)
	if err != nil {
		log.Printf("[proxy] GET /%s: page relation %s fetch failed: %v", reqRef, rel.Ref, err)
		return ""
	}
	if pageData == nil {
		return ""
	}
	_, pageItem, err := ParseEnvelope(pageData)
	if err != nil || pageItem.Type != "PAGE" {
		return ""
	}
	log.Printf("[proxy] GET /%s: serving HTML via page relation %s", reqRef, rel.Ref)
	return extractHTML(pageItem)
}

// resolveDefaultViewerHTML loads the default viewer PAGE HTML.
// reqRef is the originally requested ref (for logging).
func (p *Proxy) resolveDefaultViewerHTML(reqRef string) string {
	data, err := p.readOrFetch(p.defaultViewerRef)
	if err != nil {
		log.Printf("[proxy] GET /%s: default viewer %s fetch failed: %v", reqRef, p.defaultViewerRef, err)
		return ""
	}
	if data == nil {
		return ""
	}
	// resolvePageHTML logs which PAGE it found; we just log that we're using the default viewer
	log.Printf("[proxy] GET /%s: trying default viewer %s", reqRef, p.defaultViewerRef)
	return p.resolvePageHTML(reqRef, data)
}

// storeLocallyWithPending stores an object locally and adds to sync pending.
func (p *Proxy) storeLocallyWithPending(w http.ResponseWriter, ref string, item *Item, canonical []byte) {
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
	p.index.Update(ref, item, ts)
	log.Printf("[proxy] stored %s rev %d (%s) (sync pending)", ref, item.Revision, item.Type)

	// 202 Accepted — stored locally, sync pending
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"status": "pending_sync",
		"ref":    ref,
	})
}

// encodeBase64Cursor encodes cursor bytes as base64url.
func encodeBase64Cursor(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}
