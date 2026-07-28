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
		writeError(w, r, http.StatusNotFound, "unknown host", "NOT_FOUND")
		return

	default:
		if normalizeVhostMode(h.VhostMode) == VhostModeRedirect {
			http.Redirect(w, r, pageRedirectTarget(h.VhostMode, h.Vhost, r, resolved, resolved), http.StatusFound)
			return
		}
		if meta, found := h.index.GetMeta(resolved); found && !meta.IsPublic {
			authPK := auth.AuthPubkey(r)
			if !realm.CanRead(meta.Realms, authPK, h.index.Resolver()) {
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
			writeError(w, r, http.StatusNotFound, "page not found", "NOT_FOUND")
			return
		}
		html := pageOwnHTML(data)
		if html == "" {
			writeError(w, r, http.StatusNotFound, "page has no HTML", "NOT_FOUND")
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
		writeError(w, r, http.StatusNotFound, "no root object", "NOT_FOUND")
		return
	}
	http.Redirect(w, r, "/"+metas[0].Ref, http.StatusFound)
}

// handleGetObject serves GET /{ref}
func (h *Hub) handleGetObject(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")

	// Ref-shape gate: reject scanner traffic (/.env, /wp-config.php, …)
	// without touching the index or the store. Cuts log spam.
	if !object.IsValidRef(ref) {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}

	// Use the index for existence and access control before reading the object.
	meta, found := h.index.GetMeta(ref)
	if !found {
		// Not in index — check disk (race condition or index lag)
		data, err := h.store.Read(ref)
		if err != nil {
			log.Printf("ERROR: GET /%s: %v", ref, err)
			writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
			return
		}
		if data == nil {
			writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
			return
		}
		// Serve directly (rare fallback)
		h.serveObject(w, r, ref, data)
		return
	}

	// Private object access control: return 404 (not 403) to avoid leaking existence.
	// HTML viewers retain the existing isolated-origin login flow, using the
	// same centralized viewer resolution as the authorized response path.
	if !meta.IsPublic {
		authPK := auth.AuthPubkey(r)
		if !realm.CanRead(meta.Realms, authPK, h.index.Resolver()) {
			if data, err := h.store.Read(ref); err == nil && data != nil {
				resolver := newViewerResolver(h.store, h.index, ref, data, authPK)
				if servePrivateViewerLogin(w, r, resolver, h.Vhost, h.VhostMode, ref) {
					return
				}
			}
			writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
			return
		}
	}
	if etag, ok := indexedNonHTMLValidator(r, meta); ok {
		w.Header().Set("Vary", "Accept")
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	// Read the requested object before selecting HTML so viewer resolution and
	// its validator are derived from the exact representation being served.
	data, err := h.store.Read(ref)
	if err != nil {
		log.Printf("ERROR: GET /%s: %v", ref, err)
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	if data == nil {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}

	h.serveObject(w, r, ref, data)
}

// serveObject selects, validates, and writes GET /{ref}'s representation.
func (h *Hub) serveObject(w http.ResponseWriter, r *http.Request, ref string, data []byte) {
	_, item, err := object.ParseEnvelope(data)
	if err != nil {
		log.Printf("ERROR: GET /%s: parse stored object: %v", ref, err)
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	resolver := newViewerResolver(h.store, h.index, ref, data, auth.AuthPubkey(r))
	selected := selectNegotiatedRepresentation(r, resolver, h.defaultViewerRef)
	selected.setCacheHeaders(w)
	if pageRef := selected.isolatedPageRef(); pageRef != "" &&
		vhostRedirect(w, r, h.Vhost, h.VhostMode, ref, pageRef, "") {
		return
	}
	etag := selected.etag(item.Revision)
	w.Header().Set("Vary", "Accept")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	selected.write(w, data, h.baseDomain())
}

// handlePutObject serves PUT /{ref}
func (h *Hub) handlePutObject(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")

	// Ref-shape gate — short-circuits before ECDSA verification on garbage URLs.
	if !object.IsValidRef(ref) {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "failed to read body", "INVALID_OBJECT")
		return
	}
	if len(body) > maxBodySize {
		writeError(w, r, http.StatusRequestEntityTooLarge, "body too large (max 10MB)", "INVALID_OBJECT")
		return
	}

	// Parse envelope and item
	env, item, err := object.ParseEnvelope(body)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err.Error(), "INVALID_OBJECT")
		return
	}

	// Resolve realms (supports both old and new format)
	realms := object.ResolveIn(env, item)

	// Validate pubkey-realms: each must match item.pubkey
	for _, realm := range realms {
		if object.IsPubkeyRealm(realm) && realm != item.Pubkey {
			writeError(w, r, http.StatusForbidden,
				"pubkey-realm does not match item pubkey", "REALM_FORBIDDEN")
			return
		}
	}

	// Object must belong to dataverse001, a self-owned pubkey-realm, or a configured shared realm.
	// A valid SHARED_REALM always includes "dataverse001" in item.in (decision 6,
	// enforced by ParseSharedRealm below), so it passes via the dataverse001 branch.
	if !realm.ValidateRealmsForPut(realms, item.Pubkey, h.index.Resolver()) {
		writeError(w, r, http.StatusBadRequest,
			"object must belong to dataverse001, server-public, a self-owned pubkey-realm, or a configured shared realm",
			"INVALID_OBJECT")
		return
	}

	// Enforce the full SHARED_REALM type contract (decisions 3, 4, 6):
	// owner-prefixed realm, signer owns it, id == RealmID(realm), and
	// dataverse001 present in item.in.
	if item.Type == realm.TypeSharedRealm {
		if _, _, err := realm.ParseSharedRealm(item); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid SHARED_REALM object: "+err.Error(), "INVALID_SHARED_REALM")
			return
		}
	}

	// Check ref matches
	expectedRef := item.Ref()
	if ref != expectedRef {
		writeError(w, r, http.StatusBadRequest,
			fmt.Sprintf("URL ref %q does not match item %q", ref, expectedRef),
			"REF_MISMATCH")
		return
	}

	// Verify signature (CPU-heavy, before acquiring any locks)
	if err := object.VerifyEnvelope(body); err != nil {
		writeError(w, r, http.StatusBadRequest, err.Error(), "INVALID_SIGNATURE")
		return
	}

	// Canonicalize for storage (CPU work — do it before taking the per-ref lock)
	canonical, err := object.CanonicalJSON(body)
	if err != nil {
		log.Printf("ERROR: PUT /%s: canonical JSON: %v", ref, err)
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}

	ts, err := item.Timestamp()
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid timestamp: "+err.Error(), "INVALID_OBJECT")
		return
	}

	// Serialize the read-check-write for this ref so concurrent PUTs cannot both
	// observe the same prior revision and both commit (optimistic-locking CAS).
	h.writeLocks.Lock(ref)
	defer h.writeLocks.Unlock(ref)

	// Check existing revision via index (no disk I/O). If-Match (RFC 9110)
	// gates the write on the stored revision; an absent header preserves the
	// legacy revision-monotonicity behavior.
	existingMeta, isUpdate := h.index.GetMeta(ref)
	if checkConditionalWrite(w, r, r.Header.Get("If-Match"), existingMeta, isUpdate, item.Revision) {
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
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}

	// Update index (pass realms for visibility tracking)
	h.index.Update(ref, item, ts, realms)

	// Update vhost hash map for PAGE objects
	if h.Vhost != nil && item.Type == "PAGE" {
		h.Vhost.AddPage(ref)
	}

	log.Printf("stored %s rev %d (%s)", ref, item.Revision, item.Type)

	// Queue the Freenet mirror. No-op when disabled; drops anything that is
	// not a public dataverse001 object; never blocks on or fails the write.
	h.Mirror.Publish(ref, item.Revision, realms, canonical)

	// Advertise the new revision so clients can chain a subsequent
	// conditional write with If-Match.
	w.Header().Set("ETag", revisionETag(item.Revision))
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

	// Ref-shape gate (see handleGetObject for rationale).
	if !object.IsValidRef(ref) {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}

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
// if it handled the response. The explicit GET /{ref}/raw path shares the same
// bytes via blobRaw/writeBlobBytes but skips the Accept gate.
func serveBlob(w http.ResponseWriter, r *http.Request, data []byte) bool {
	b, ok := blobContent(data)
	if !ok {
		return false
	}
	// Gate on Accept BEFORE decoding: a mismatched request (e.g. a browser's
	// text/html for a BLOB with no page relation) must not pay a full base64
	// decode, nor log a WARN on corrupt data it never intended to serve.
	if !acceptsMimeType(r, b.mimeType) {
		return false
	}
	raw, ok := b.decode()
	if !ok {
		return false
	}
	writeBlobBytes(w, b.mimeType, raw)
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

// writeError writes an RFC 9457 application/problem+json error response,
// content-negotiated on the request's Accept header (see object.WriteProblem).
// msg becomes the problem's detail; code selects the stable title/next_action.
func writeError(w http.ResponseWriter, r *http.Request, status int, msg, code string) {
	object.WriteProblem(w, r, status, msg, code)
}

// baseDomain returns the hub's base domain if vhosting is configured.
func (h *Hub) baseDomain() string {
	if h.Vhost != nil {
		return h.Vhost.BaseDomain()
	}
	return ""
}

// writePageHTML writes a 200 text/html response, injecting the base-domain meta.
func writePageHTML(w http.ResponseWriter, html, baseDomain string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, injectBaseDomain(html, baseDomain))
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
