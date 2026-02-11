package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

const maxBodySize = 10 << 20 // 10 MB

// handleRoot serves GET / — redirects to the ROOT object.
func (h *Hub) handleRoot(w http.ResponseWriter, r *http.Request) {
	metas := h.index.GetAll("", "ROOT")
	if len(metas) == 0 {
		writeError(w, http.StatusNotFound, "no root object", "NOT_FOUND")
		return
	}
	http.Redirect(w, r, "/"+metas[0].Ref, http.StatusFound)
}

// handleGetObject serves GET /{ref}
func (h *Hub) handleGetObject(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")

	// Fast path: use index to build ETag and check 304 without disk I/O
	meta, found := h.index.GetMeta(ref)
	if !found {
		// Not in index — check disk (race condition or index lag)
		data, err := h.store.Read(ref)
		if err != nil {
			log.Printf("ERROR: GET /%s: %v", ref, err)
			writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
			return
		}
		if data == nil {
			writeError(w, http.StatusNotFound, "object not found", "NOT_FOUND")
			return
		}
		// Serve directly (rare fallback)
		h.serveObject(w, r, ref, data)
		return
	}

	// Build ETag from indexed revision
	etag := `"` + strconv.Itoa(meta.Revision) + `"`

	// Determine representation from index data (no disk I/O)
	isHTML := false
	if acceptsHTML(r) {
		if meta.Type == "PAGE" || meta.HasPageRelation {
			isHTML = true
		} else if h.defaultViewerRef != "" && ref != h.defaultViewerRef {
			isHTML = true
		}
	}
	isBlob := false
	if !isHTML && meta.Type == "BLOB" && meta.MimeType != "" && acceptsMimeType(r, meta.MimeType) {
		isBlob = true
	}

	if isHTML {
		etag = etag[:len(etag)-1] + `-html"`
	} else if isBlob {
		etag = etag[:len(etag)-1] + `-blob"`
	}

	w.Header().Set("Vary", "Accept")
	w.Header().Set("ETag", etag)

	// 304 Not Modified — zero disk I/O
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Cache miss — read file for the response body
	data, err := h.store.Read(ref)
	if err != nil {
		log.Printf("ERROR: GET /%s: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	if data == nil {
		writeError(w, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}

	h.serveObject(w, r, ref, data)
}

// serveObject writes the response body for a GET that isn't 304.
// ETag/Vary headers must already be set by the caller.
func (h *Hub) serveObject(w http.ResponseWriter, r *http.Request, ref string, data []byte) {
	if acceptsHTML(r) {
		html := h.resolvePageHTML(data)
		if html == "" && h.defaultViewerRef != "" && ref != h.defaultViewerRef {
			html = h.resolveDefaultViewerHTML()
		}
		if html != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, html)
			return
		}
	}

	if serveBlob(w, r, data) {
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// handlePutObject serves PUT /{ref}
func (h *Hub) handlePutObject(w http.ResponseWriter, r *http.Request) {
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

	// Check existing revision via index (no disk I/O)
	existingMeta, isUpdate := h.index.GetMeta(ref)
	if isUpdate && existingMeta.Revision >= item.Revision {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("existing revision %d >= incoming %d", existingMeta.Revision, item.Revision),
			"REVISION_CONFLICT")
		return
	}

	// Canonicalize for storage
	canonical, err := canonicalJSON(body)
	if err != nil {
		log.Printf("ERROR: PUT /%s: canonical JSON: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}

	ts, err := item.Timestamp()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid timestamp: "+err.Error(), "INVALID_OBJECT")
		return
	}

	// Backup old version before overwriting
	if isUpdate {
		if err := h.store.Backup(ref, existingMeta.Revision); err != nil {
			log.Printf("WARN: PUT /%s: backup rev %d failed: %v", ref, existingMeta.Revision, err)
		}
	}

	// Write to store
	if err := h.store.Write(ref, canonical, ts); err != nil {
		log.Printf("ERROR: PUT /%s: write: %v", ref, err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}

	// Update index
	h.index.Update(ref, item, ts)
	log.Printf("stored %s rev %d (%s)", ref, item.Revision, item.Type)

	if isUpdate {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
	w.Write(canonical)
}

// handleListObjects serves GET /search
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

// handleGetInbound serves GET /{ref}/inbound
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
		item := json.RawMessage(data)
		if m.Type == "BLOB" {
			item = stripBlobData(item)
		}
		items = append(items, item)
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

// acceptsHTML returns true if the request Accept header includes text/html.
func acceptsHTML(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if mt == "text/html" {
			return true
		}
	}
	return false
}

// acceptsMimeType returns true if the request Accept header matches the given
// MIME type exactly or via a wildcard subtype (e.g. image/* matches image/png).
func acceptsMimeType(r *http.Request, mimeType string) bool {
	if mimeType == "" {
		return false
	}
	mainType, _, ok := strings.Cut(mimeType, "/")
	if !ok {
		return false
	}
	wildcard := mainType + "/*"
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if mt == mimeType || mt == wildcard {
			return true
		}
	}
	return false
}

// serveBlob checks if data is a BLOB object whose mime_type matches the
// request's Accept header. If so, it decodes content.data and writes raw
// bytes with the correct Content-Type and cache headers. Returns true if
// it handled the response.
func serveBlob(w http.ResponseWriter, r *http.Request, data []byte) bool {
	_, item, err := ParseEnvelope(data)
	if err != nil || item.Type != "BLOB" || item.Content == nil {
		return false
	}

	var content struct {
		MimeType string `json:"mime_type"`
		Data     string `json:"data"`
		Size     int    `json:"size"`
	}
	if err := json.Unmarshal(item.Content, &content); err != nil || content.MimeType == "" || content.Data == "" {
		return false
	}

	if !acceptsMimeType(r, content.MimeType) {
		return false
	}

	raw, err := base64.StdEncoding.DecodeString(content.Data)
	if err != nil {
		log.Printf("WARN: serveBlob %s: base64 decode: %v", item.Ref(), err)
		return false
	}

	w.Header().Set("Content-Type", content.MimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
	return true
}

// stripBlobData removes the content.data field from a BLOB object's JSON
// representation, keeping all other fields (mime_type, size, sha256, filename).
// Used in list responses to avoid sending large payloads.
func stripBlobData(data json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return data
	}
	itemRaw, ok := obj["item"]
	if !ok {
		return data
	}
	var item map[string]json.RawMessage
	if json.Unmarshal(itemRaw, &item) != nil {
		return data
	}
	contentRaw, ok := item["content"]
	if !ok {
		return data
	}
	var content map[string]json.RawMessage
	if json.Unmarshal(contentRaw, &content) != nil {
		return data
	}
	delete(content, "data")
	item["content"], _ = json.Marshal(content)
	obj["item"], _ = json.Marshal(item)
	result, _ := json.Marshal(obj)
	return result
}

// resolvePageHTML extracts HTML content from a PAGE object, or follows a `page`
// relation to find one. Returns empty string if no HTML can be resolved.
func (h *Hub) resolvePageHTML(data []byte) string {
	env, item, err := ParseEnvelope(data)
	if err != nil {
		return ""
	}
	_ = env

	// Case 1: object itself is a PAGE
	if item.Type == "PAGE" {
		return extractHTML(item)
	}

	// Case 2: object has a `page` relation — follow the first ref
	pageRels, ok := item.Relations["page"]
	if !ok || len(pageRels) == 0 {
		return ""
	}
	var rel RelationRef
	if err := json.Unmarshal(pageRels[0], &rel); err != nil || rel.Ref == "" {
		log.Printf("WARN: resolvePageHTML: invalid page relation: %v", err)
		return ""
	}
	pageData, err := h.store.Read(rel.Ref)
	if err != nil || pageData == nil {
		log.Printf("WARN: resolvePageHTML: page ref %s not found: %v", rel.Ref, err)
		return ""
	}
	_, pageItem, err := ParseEnvelope(pageData)
	if err != nil {
		log.Printf("WARN: resolvePageHTML: failed to parse page %s: %v", rel.Ref, err)
		return ""
	}
	if pageItem.Type != "PAGE" {
		log.Printf("WARN: resolvePageHTML: page ref %s is type %q, not PAGE", rel.Ref, pageItem.Type)
		return ""
	}
	return extractHTML(pageItem)
}

// resolveDefaultViewerHTML loads and caches the default viewer PAGE's HTML.
func (h *Hub) resolveDefaultViewerHTML() string {
	data, err := h.store.Read(h.defaultViewerRef)
	if err != nil || data == nil {
		log.Printf("WARN: default viewer %s not found: %v", h.defaultViewerRef, err)
		return ""
	}
	return h.resolvePageHTML(data)
}

// extractHTML pulls the html string from item.content.html.
func extractHTML(item *Item) string {
	if item.Content == nil {
		return ""
	}
	var content struct {
		HTML string `json:"html"`
	}
	if err := json.Unmarshal(item.Content, &content); err != nil {
		return ""
	}
	return content.HTML
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
