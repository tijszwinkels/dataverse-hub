package serving

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
	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/vhost"
)

const maxBodySize = 10 << 20 // 10 MB

// handleRoot serves GET / — with vhosting, resolves Host to a PAGE.
// Without vhosting (legacy), redirects to the ROOT object.
func (h *Hub) handleRoot(w http.ResponseWriter, r *http.Request) {
	if h.Vhost == nil {
		h.handleRootLegacy(w, r)
		return
	}

	resolved := h.Vhost.Resolve(r.Host)
	switch {
	case resolved == "":
		if baseHostMatches(r.Host, h.Vhost.BaseDomain()) {
			h.handleRootLegacy(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "unknown host", "NOT_FOUND")
		return

	default:
		if normalizeVhostMode(h.VhostMode) == VhostModeRedirect {
			http.Redirect(w, r, pageRedirectTarget(h.VhostMode, h.Vhost, r, resolved, resolved), http.StatusFound)
			return
		}
		if meta, found := h.index.GetMeta(resolved); found && !meta.IsPublic {
			authPK := auth.AuthPubkey(r)
			if !realm.CanRead(meta.Realms, authPK, h.shared) {
				servePrivatePageLogin(w)
				return
			}
		}

		// ETag/304 check via index (no disk I/O)
		if meta, found := h.index.GetMeta(resolved); found {
			etag := `"` + strconv.Itoa(meta.Revision) + `-html"`
			w.Header().Set("Vary", "Accept")
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		// Serve PAGE from resolved ref
		data, err := h.store.Read(resolved)
		if err != nil || data == nil {
			log.Printf("WARN: vhost root: page %s not found", resolved)
			writeError(w, http.StatusNotFound, "page not found", "NOT_FOUND")
			return
		}
		html := h.resolvePageHTML(data)
		if html == "" {
			writeError(w, http.StatusNotFound, "page has no HTML", "NOT_FOUND")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, injectBaseDomain(html, h.baseDomain()))
	}
}

// handleRootLegacy is the original root handler: redirect to ROOT object.
func (h *Hub) handleRootLegacy(w http.ResponseWriter, r *http.Request) {
	metas := h.index.GetAll("", "ROOT", "", false)
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

	// Private object access control: return 404 (not 403) to avoid leaking existence
	if !meta.IsPublic {
		authPK := auth.AuthPubkey(r)
		if !realm.CanRead(meta.Realms, authPK, h.shared) {
			if h.Vhost != nil && acceptsHTML(r) && (meta.Type == "PAGE" || meta.HasPageRelation) {
				pageRef := ref
				if meta.HasPageRelation && meta.PageRef != "" {
					pageRef = meta.PageRef
				}
				if !canonicalPageHost(h.VhostMode, h.Vhost, r.Host, pageRef) {
					target := pageRedirectTarget(h.VhostMode, h.Vhost, r, ref, pageRef)
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
	// BLOB content negotiation overrides the default viewer (but not PAGE/page-relation)
	isBlob := false
	if meta.Type == "BLOB" && meta.MimeType != "" && acceptsMimeType(r, meta.MimeType) {
		isBlob = true
		isHTML = false
	}

	if isHTML {
		etag = etag[:len(etag)-1] + pageETagSuffix(h.index, meta, h.defaultViewerRef) + `"`
	} else if isBlob {
		etag = etag[:len(etag)-1] + `-blob"`
	}

	// Vhost redirect: if this is a PAGE and we're on the wrong subdomain, redirect
	if h.Vhost != nil && acceptsHTML(r) && (meta.Type == "PAGE" || meta.HasPageRelation) {
		pageRef := ref
		if meta.HasPageRelation && meta.PageRef != "" {
			pageRef = meta.PageRef
		}
		if !canonicalPageHost(h.VhostMode, h.Vhost, r.Host, pageRef) {
			target := pageRedirectTarget(h.VhostMode, h.Vhost, r, ref, pageRef)
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
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
// BLOB content negotiation runs first — it only fires for type BLOB, so
// PAGE objects and page-relation HTML are unaffected. This ensures BLOBs
// take priority over the default viewer (which would otherwise intercept
// browser requests that include text/html in Accept).
func (h *Hub) serveObject(w http.ResponseWriter, r *http.Request, ref string, data []byte) {
	if serveBlob(w, r, data) {
		return
	}

	if acceptsHTML(r) {
		html := h.resolvePageHTML(data)
		if html == "" && h.defaultViewerRef != "" && ref != h.defaultViewerRef {
			html = h.resolveDefaultViewerHTML()
		}
		if html != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, injectBaseDomain(html, h.baseDomain()))
			return
		}
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
	env, item, err := object.ParseEnvelope(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "INVALID_OBJECT")
		return
	}

	// Resolve realms (supports both old and new format)
	realms := object.ResolveIn(env, item)

	// Validate pubkey-realms: each must match item.pubkey
	for _, realm := range realms {
		if object.IsPubkeyRealm(realm) && realm != item.Pubkey {
			writeError(w, http.StatusForbidden,
				"pubkey-realm does not match item pubkey", "REALM_FORBIDDEN")
			return
		}
	}

	// Object must belong to dataverse001, a self-owned pubkey-realm, or a configured shared realm
	if !realm.ValidateRealmsForPut(realms, item.Pubkey, h.shared) {
		writeError(w, http.StatusBadRequest,
			"object must belong to dataverse001, server-public, a self-owned pubkey-realm, or a configured shared realm",
			"INVALID_OBJECT")
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
	if err := object.VerifyEnvelope(body); err != nil {
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
	canonical, err := object.CanonicalJSON(body)
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

	// Update index (pass realms for visibility tracking)
	h.index.Update(ref, item, ts, realms)

	// Update vhost hash map for PAGE objects
	if h.Vhost != nil && item.Type == "PAGE" {
		h.Vhost.AddPage(ref)
	}

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
	membersOnly := q.Get("members_only") != "false" // default true

	authPK := auth.AuthPubkey(r)
	metas := h.index.GetAll(pubkey, typeFilter, authPK, membersOnly)
	items, refs, nextCursor, hasMore := paginateAndLoad(h.store, metas, cursor, limit)

	if includeInboundCounts {
		items = enrichWithInboundCounts(h.index, items, refs)
	}

	writeList(w, items, nextCursor, hasMore)
}

// handleGetInbound serves GET /{ref}/inbound
func (h *Hub) handleGetInbound(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	q := r.URL.Query()

	filters := storage.InboundFilters{
		Relation: q.Get("relation"),
		From:     q.Get("from"),
		Type:     q.Get("type"),
	}
	limit := parseLimit(q.Get("limit"), 50, 200)
	cursor := parseCursor(q.Get("cursor"))
	includeInboundCounts := q.Get("include") == "inbound_counts"
	membersOnly := q.Get("members_only") != "false"

	authPK := auth.AuthPubkey(r)
	metas := h.index.GetInbound(ref, filters, authPK, membersOnly)
	items, refs, nextCursor, hasMore := paginateAndLoad(h.store, metas, cursor, limit)

	if includeInboundCounts {
		items = enrichWithInboundCounts(h.index, items, refs)
	}

	writeList(w, items, nextCursor, hasMore)
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
// MIME type exactly, via a wildcard subtype (e.g. image/* matches image/png),
// or via */* (client accepts anything — serve the BLOB's native content).
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
		if mt == mimeType || mt == wildcard || mt == "*/*" {
			return true
		}
	}
	return false
}

// serveBlob checks if data is a BLOB object whose mime_type matches the
// request's Accept header. If so, it serves the raw content with the correct
// Content-Type and cache headers. Supports both binary BLOBs (content.data,
// base64-encoded) and text BLOBs (content.text, plain string). Returns true
// if it handled the response.
func serveBlob(w http.ResponseWriter, r *http.Request, data []byte) bool {
	_, item, err := object.ParseEnvelope(data)
	if err != nil || item.Type != "BLOB" || item.Content == nil {
		return false
	}

	var content struct {
		MimeType string `json:"mime_type"`
		Data     string `json:"data"`
		Text     string `json:"text"`
	}
	if err := json.Unmarshal(item.Content, &content); err != nil || content.MimeType == "" {
		return false
	}

	// Need either data (base64) or text (plain)
	if content.Data == "" && content.Text == "" {
		return false
	}

	if !acceptsMimeType(r, content.MimeType) {
		return false
	}

	var raw []byte
	if content.Text != "" {
		// Text BLOB: serve plain string directly
		raw = []byte(content.Text)
	} else {
		// Binary BLOB: decode base64
		raw, err = base64.StdEncoding.DecodeString(content.Data)
		if err != nil {
			log.Printf("WARN: serveBlob %s: base64 decode: %v", item.Ref(), err)
			return false
		}
	}

	w.Header().Set("Content-Type", content.MimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
	return true
}

// stripBlobData removes the content.data and content.text fields from a BLOB
// object's JSON representation, keeping metadata (mime_type, size, sha256,
// filename). Used in list responses to avoid sending large payloads.
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
	delete(content, "text")
	item["content"], _ = json.Marshal(content)
	obj["item"], _ = json.Marshal(item)
	result, _ := json.Marshal(obj)
	return result
}

// resolvePageHTML extracts HTML content from a PAGE object, or follows a `page`
// relation to find one. Returns empty string if no HTML can be resolved.
func (h *Hub) resolvePageHTML(data []byte) string {
	env, item, err := object.ParseEnvelope(data)
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
	var rel object.RelationRef
	if err := json.Unmarshal(pageRels[0], &rel); err != nil || rel.Ref == "" {
		log.Printf("WARN: resolvePageHTML: invalid page relation: %v", err)
		return ""
	}
	pageData, err := h.store.Read(rel.Ref)
	if err != nil || pageData == nil {
		log.Printf("WARN: resolvePageHTML: page ref %s not found: %v", rel.Ref, err)
		return ""
	}
	_, pageItem, err := object.ParseEnvelope(pageData)
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

// pageETagSuffix returns the ETag suffix for HTML representations.
// Includes the page/viewer revision so browser caches invalidate when the PAGE changes.
func pageETagSuffix(index *storage.Index, meta object.ObjectMeta, defaultViewerRef string) string {
	if meta.Type == "PAGE" {
		return "-html" // own revision tracks HTML changes
	}
	pageRef := meta.PageRef
	if pageRef == "" && defaultViewerRef != "" && meta.Ref != defaultViewerRef {
		pageRef = defaultViewerRef
	}
	if pageRef == "" {
		return "-html"
	}
	pageMeta, found := index.GetMeta(pageRef)
	if !found {
		return "-html"
	}
	return fmt.Sprintf("-p%d-html", pageMeta.Revision)
}

// extractHTML pulls the html string from item.content.html.
func extractHTML(item *object.Item) string {
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

func parseCursor(s string) *object.Cursor {
	if s == "" {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	var c object.Cursor
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	return &c
}

func writeError(w http.ResponseWriter, status int, msg, code string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(object.APIError{Error: msg, Code: code})
}

// baseDomain returns the hub's base domain if vhosting is configured.
func (h *Hub) baseDomain() string {
	if h.Vhost != nil {
		return h.Vhost.BaseDomain()
	}
	return ""
}

// injectBaseDomain inserts a <meta name="dv-base-domain"> tag into PAGE HTML.
// If baseDomain is empty, returns html unchanged.
func injectBaseDomain(html, baseDomain string) string {
	if baseDomain == "" {
		return html
	}
	tag := `<meta name="dv-base-domain" content="` + baseDomain + `">`
	idx := strings.Index(strings.ToLower(html), "<head")
	if idx >= 0 {
		if close := strings.IndexByte(html[idx:], '>'); close >= 0 {
			pos := idx + close + 1
			return html[:pos] + "\n" + tag + html[pos:]
		}
	}
	return tag + "\n" + html
}

// TLSAskHandler returns an http.HandlerFunc for Caddy's on-demand TLS "ask"
// endpoint. It validates the requested domain against the vhost resolver:
// known PAGE hash subdomains and custom domains with _dv. TXT records are
// approved (200), everything else is rejected (403).
func TLSAskHandler(resolver *vhost.Resolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		domain := r.URL.Query().Get("domain")
		if domain == "" {
			http.Error(w, "missing domain param", http.StatusBadRequest)
			return
		}
		if resolver == nil {
			log.Printf("TLS ask: rejected %q (vhosting disabled)", domain)
			http.Error(w, "vhosting disabled", http.StatusForbidden)
			return
		}
		if resolver.Resolve(domain) != "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		log.Printf("TLS ask: rejected %q", domain)
		http.Error(w, "unknown domain", http.StatusForbidden)
	}
}

func writeList(w http.ResponseWriter, items []json.RawMessage, cursor *string, hasMore bool) {
	if items == nil {
		items = []json.RawMessage{}
	}
	resp := object.ListResponse{
		Items:   items,
		Cursor:  cursor,
		HasMore: hasMore,
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}
