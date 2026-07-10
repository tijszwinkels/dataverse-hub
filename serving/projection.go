package serving

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/vhost"
)

// Explicit representation paths de-magic the Accept-header negotiation on
// GET /{ref}: each URL pins one representation regardless of Accept.
//
//	GET /{ref}/json — the signed JSON envelope, always.
//	GET /{ref}/raw  — a BLOB's bytes with its content.mime_type; 409 if not a BLOB.
//	GET /{ref}/page — an HTML view (inline PAGE, page-relation viewer, or the
//	                  configured default viewer); 409 if no HTML representation.
//
// All three share GET /{ref}'s realm auth (unauthorized private object -> 404,
// not 403, to avoid leaking existence) and ETag/If-None-Match semantics: the
// ETag equals the one GET /{ref} sets for that same representation, so a client
// can revalidate either URL interchangeably. The Accept-negotiated GET /{ref}
// stays unchanged as sugar. Because the representation is fixed, these responses
// do not carry Vary: Accept.

// authorizeProjection enforces the realm-auth decision for a projection path,
// mirroring GET /{ref}. It returns the indexed meta (found=false only in the
// rare index-miss race, where the caller falls back to a direct disk read) and
// ok=false when it has already written a 404 for an unauthorized private object.
func authorizeProjection(w http.ResponseWriter, r *http.Request, ref string, index *storage.Index) (meta object.ObjectMeta, found, ok bool) {
	meta, found = index.GetMeta(ref)
	if found && !meta.IsPublic {
		if !realm.CanRead(meta.Realms, auth.AuthPubkey(r), index.Resolver()) {
			writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
			return meta, found, false
		}
	}
	return meta, found, true
}

// readForProjection reads an object's bytes for a projection response, writing
// the appropriate error (404 missing, 500 read failure) and returning ok=false
// on failure.
func readForProjection(w http.ResponseWriter, r *http.Request, store *storage.Store, ref string) ([]byte, bool) {
	data, err := store.Read(ref)
	if err != nil {
		log.Printf("ERROR: GET /%s: %v", ref, err)
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
		return nil, false
	}
	if data == nil {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return nil, false
	}
	return data, true
}

// serveJSON writes the JSON-envelope representation: the stored object bytes
// verbatim. ETag "<rev>" and If-None-Match/304 match GET /{ref}'s JSON.
func serveJSON(w http.ResponseWriter, r *http.Request, store *storage.Store, ref string, meta object.ObjectMeta, found bool) {
	if found {
		etag := `"` + strconv.Itoa(meta.Revision) + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	data, ok := readForProjection(w, r, store, ref)
	if !ok {
		return
	}
	// Content-Type is already application/json via jsonContentType middleware.
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// serveRaw writes a BLOB's raw bytes with its content.mime_type, regardless of
// Accept. 409 for a non-BLOB or a BLOB without servable content. ETag
// "<rev>-blob" and 304 match GET /{ref}'s raw-BLOB representation.
func serveRaw(w http.ResponseWriter, r *http.Request, store *storage.Store, ref string, meta object.ObjectMeta, found bool) {
	if found {
		if meta.Type != "BLOB" || meta.MimeType == "" {
			writeError(w, r, http.StatusConflict, "object is not a BLOB", "NOT_BLOB")
			return
		}
		etag := `"` + strconv.Itoa(meta.Revision) + `-blob"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	data, ok := readForProjection(w, r, store, ref)
	if !ok {
		return
	}
	mime, raw, isBlob := blobRaw(data)
	if !isBlob {
		writeError(w, r, http.StatusConflict, "object is not a BLOB", "NOT_BLOB")
		return
	}
	writeBlobBytes(w, mime, raw)
}

// serveProjectionPage forces the HTML representation. renderPage resolves an
// inline PAGE or page-relation viewer; renderDefault resolves the configured
// default viewer (only consulted when the object is not itself page-viewable).
// 409 when no HTML representation is available. ETag "<rev>-...-html" and 304
// match GET /{ref}'s HTML representation.
func serveProjectionPage(w http.ResponseWriter, r *http.Request, store *storage.Store, index *storage.Index, ref string, meta object.ObjectMeta, found bool, defaultViewerRef, baseDomain string, renderPage func([]byte) string, renderDefault func() string) {
	hasDefault := defaultViewerRef != "" && ref != defaultViewerRef
	if found {
		if !pageViewable(index, meta) && !hasDefault {
			writeError(w, r, http.StatusConflict, "object has no page representation", "NO_PAGE")
			return
		}
		etag := `"` + strconv.Itoa(meta.Revision) + pageETagSuffix(index, meta, defaultViewerRef) + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	data, ok := readForProjection(w, r, store, ref)
	if !ok {
		return
	}
	if html := renderPage(data); html != "" {
		writePageHTML(w, html, baseDomain)
		return
	}
	if hasDefault {
		if html := renderDefault(); html != "" {
			writePageHTML(w, html, baseDomain)
			return
		}
	}
	writeError(w, r, http.StatusConflict, "object has no page representation", "NO_PAGE")
}

// pageVhostRedirect enforces per-app origin isolation for GET /{ref}/page. When
// the object is a PAGE (or resolves one via a page relation) and the request hit
// a non-canonical host, it 302s to the canonical page host — the same check
// GET /{ref} makes, minus the acceptsHTML gate (/page always serves HTML, so the
// shared-origin exposure exists for every Accept). The redirect preserves the
// /page suffix and query string so the target still pins the HTML representation.
// Returns true if it redirected; the caller must then stop.
func pageVhostRedirect(w http.ResponseWriter, r *http.Request, resolver *vhost.Resolver, vhostMode, ref string, meta object.ObjectMeta) bool {
	if resolver == nil || !(meta.Type == "PAGE" || meta.HasPageRelation) {
		return false
	}
	pageRef := ref
	if meta.HasPageRelation && meta.PageRef != "" {
		pageRef = meta.PageRef
	}
	if canonicalPageHost(vhostMode, resolver, r.Host, pageRef) {
		return false
	}
	target := pageRedirectTargetPath(vhostMode, resolver, r, ref, pageRef, "/page")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusFound)
	return true
}

// blobPayload is a BLOB's servable content, parsed but not yet base64-decoded.
// Splitting parse from decode lets the Accept-gated serveBlob skip a wasted
// decode (and a spurious WARN on corrupt data) when the mime type isn't accepted,
// while GET /{ref}/raw decodes unconditionally.
type blobPayload struct {
	ref      string
	mimeType string
	dataB64  string
	text     string
}

// blobContent parses a BLOB's servable content without decoding base64. ok=false
// if data is not a BLOB, has no mime_type, or carries neither content.data nor
// content.text.
func blobContent(data []byte) (blobPayload, bool) {
	_, item, err := object.ParseEnvelope(data)
	if err != nil || item.Type != "BLOB" || item.Content == nil {
		return blobPayload{}, false
	}
	var content struct {
		MimeType string `json:"mime_type"`
		Data     string `json:"data"`
		Text     string `json:"text"`
	}
	if err := json.Unmarshal(item.Content, &content); err != nil || content.MimeType == "" {
		return blobPayload{}, false
	}
	if content.Data == "" && content.Text == "" {
		return blobPayload{}, false
	}
	return blobPayload{ref: item.Ref(), mimeType: content.MimeType, dataB64: content.Data, text: content.Text}, true
}

// decode returns the raw servable bytes: text as-is, or base64-decoded data.
// ok=false only if the base64 payload is corrupt.
func (b blobPayload) decode() ([]byte, bool) {
	if b.text != "" {
		return []byte(b.text), true
	}
	raw, err := base64.StdEncoding.DecodeString(b.dataB64)
	if err != nil {
		log.Printf("WARN: blob %s: base64 decode: %v", b.ref, err)
		return nil, false
	}
	return raw, true
}

// blobRaw extracts a BLOB's mime type and decoded bytes. ok=false if data is not
// a raw-servable BLOB or its base64 payload is corrupt.
func blobRaw(data []byte) (mimeType string, raw []byte, ok bool) {
	b, ok := blobContent(data)
	if !ok {
		return "", nil, false
	}
	raw, ok = b.decode()
	if !ok {
		return "", nil, false
	}
	return b.mimeType, raw, true
}

// writeBlobBytes writes raw BLOB bytes with the correct Content-Type and cache
// headers (200 OK).
func writeBlobBytes(w http.ResponseWriter, mimeType string, raw []byte) {
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

// --- Hub handlers ---

// handleGetJSON serves GET /{ref}/json.
func (h *Hub) handleGetJSON(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	if !object.IsValidRef(ref) {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}
	meta, found, ok := authorizeProjection(w, r, ref, h.index)
	if !ok {
		return
	}
	serveJSON(w, r, h.store, ref, meta, found)
}

// handleGetRaw serves GET /{ref}/raw.
func (h *Hub) handleGetRaw(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	if !object.IsValidRef(ref) {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}
	meta, found, ok := authorizeProjection(w, r, ref, h.index)
	if !ok {
		return
	}
	serveRaw(w, r, h.store, ref, meta, found)
}

// handleGetPage serves GET /{ref}/page.
func (h *Hub) handleGetPage(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	if !object.IsValidRef(ref) {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}
	meta, found, ok := authorizeProjection(w, r, ref, h.index)
	if !ok {
		return
	}
	if pageVhostRedirect(w, r, h.Vhost, h.VhostMode, ref, meta) {
		return
	}
	serveProjectionPage(w, r, h.store, h.index, ref, meta, found, h.defaultViewerRef, h.baseDomain(),
		func(data []byte) string { return h.resolvePageHTML(data) },
		h.resolveDefaultViewerHTML)
}

// --- Proxy handlers ---
//
// Each mirrors its Hub counterpart but first syncs the object (and, for /page,
// its page dependencies) from upstream into the local cache, then projects from
// the local store. Realm auth runs against the post-sync local index.

// handleGetJSON serves GET /{ref}/json in proxy mode.
func (p *Proxy) handleGetJSON(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	if !object.IsValidRef(ref) {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}
	if !p.syncFromUpstream(w, r, ref, false) {
		return
	}
	meta, found, ok := authorizeProjection(w, r, ref, p.index)
	if !ok {
		return
	}
	serveJSON(w, r, p.store, ref, meta, found)
}

// handleGetRaw serves GET /{ref}/raw in proxy mode.
func (p *Proxy) handleGetRaw(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	if !object.IsValidRef(ref) {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}
	if !p.syncFromUpstream(w, r, ref, false) {
		return
	}
	meta, found, ok := authorizeProjection(w, r, ref, p.index)
	if !ok {
		return
	}
	serveRaw(w, r, p.store, ref, meta, found)
}

// handleGetPage serves GET /{ref}/page in proxy mode.
func (p *Proxy) handleGetPage(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	if !object.IsValidRef(ref) {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}
	if !p.syncFromUpstream(w, r, ref, true) {
		return
	}
	meta, found, ok := authorizeProjection(w, r, ref, p.index)
	if !ok {
		return
	}
	if pageVhostRedirect(w, r, p.Vhost, p.VhostMode, ref, meta) {
		return
	}
	serveProjectionPage(w, r, p.store, p.index, ref, meta, found, p.defaultViewerRef, p.baseDomain(),
		func(data []byte) string { return p.resolvePageHTML(ref, data) },
		func() string { return p.resolveDefaultViewerHTML(ref) })
}
