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
//	GET /{ref}/raw  — the object's content in its native media type: a BLOB's
//	                  bytes with its content.mime_type, or a PAGE's OWN
//	                  content.html as text/html (never a page-relation or default
//	                  viewer — that composition is /page's job). 409 if the object
//	                  has no such native representation.
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

// serveRaw writes an object's content in its native media type, regardless of
// Accept: a BLOB's decoded bytes with its content.mime_type, or a PAGE's OWN
// content.html as text/html. It never follows a page relation or the default
// viewer — that composition is /page's job — so a PAGE with a page relation
// still serves its own html here. 409 NO_RAW for anything else (a non-BLOB
// non-PAGE, a BLOB without servable content, or a degenerate PAGE with empty
// html). ETag/304 match GET /{ref}'s representation of the same bytes; see
// rawETagSuffix for the exact suffixes and the deliberate page-relation
// divergence.
//
// Fail-closed invariant: the serve decision must never outrun the INDEX meta
// the security checks used. The caller's authorizeProjection (realm auth) and
// rawVhostRedirect (origin isolation) both act on meta, so author-controlled
// HTML may leave /raw only when that same meta said PAGE. On an index miss
// (found=false — no realm auth was possible) or a meta/disk disagreement (a
// stale BLOB meta chose the "-blob" ETag and skipped the redirect while disk
// already holds a PAGE revision) the PAGE branch is skipped and the response
// falls through to 409 NO_RAW. BLOB bytes carry no such gate: they are safe to
// serve in either divergence direction.
func serveRaw(w http.ResponseWriter, r *http.Request, store *storage.Store, ref string, meta object.ObjectMeta, found bool, baseDomain string) {
	if found {
		suffix, ok := rawETagSuffix(meta)
		if !ok {
			writeError(w, r, http.StatusConflict, "object has no raw representation", "NO_RAW")
			return
		}
		etag := `"` + strconv.Itoa(meta.Revision) + suffix + `"`
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
	// BLOB: bytes with content.mime_type — byte-for-byte unchanged.
	if mime, raw, isBlob := blobRaw(data); isBlob {
		writeBlobBytes(w, mime, raw)
		return
	}
	// PAGE: its OWN html, injected with the base-domain meta like every other
	// HTML-serving path (so it matches GET /{ref}'s HTML body exactly for the
	// no-page-relation case, keeping the shared ETag honest). Gated on the
	// INDEX meta — not just the disk bytes — per the fail-closed invariant above.
	if found && meta.Type == "PAGE" {
		if html := pageOwnHTML(data); html != "" {
			writePageHTML(w, html, baseDomain)
			return
		}
	}
	writeError(w, r, http.StatusConflict, "object has no raw representation", "NO_RAW")
}

// rawETagSuffix returns the ETag suffix GET /{ref}/raw uses for meta, and
// ok=false when meta has no native raw representation (409 NO_RAW).
//
//   - BLOB with a mime_type → "-blob", matching GET /{ref}'s raw-BLOB ETag.
//   - inline PAGE (no page relation) → "-html". GET /{ref}'s HTML representation
//     is that same content.html, so reusing "-html" keeps revalidation across
//     the two URLs interchangeable (the parity contract).
//   - PAGE WITH a page relation → "-raw", a DELIBERATELY distinct suffix. /raw
//     serves the PAGE's own html, but GET /{ref}/page composes the page-relation
//     viewer (a different body /raw never serves); a shared ETag could then hand
//     a client a false 304 across the two representations. The distinct suffix
//     also future-proofs the invariant if resolvePageHTML ever starts following
//     a PAGE's own page relation.
func rawETagSuffix(meta object.ObjectMeta) (string, bool) {
	switch {
	case meta.Type == "BLOB" && meta.MimeType != "":
		return "-blob", true
	case meta.Type == "PAGE":
		if meta.HasPageRelation {
			return "-raw", true
		}
		return "-html", true
	default:
		return "", false
	}
}

// pageOwnHTML returns a PAGE object's OWN content.html, or "" if data is not a
// PAGE or its html is empty (a degenerate object). It never follows a page
// relation — unlike resolvePageHTML — because /raw pins the object's own content.
func pageOwnHTML(data []byte) string {
	_, item, err := object.ParseEnvelope(data)
	if err != nil || item.Type != "PAGE" {
		return ""
	}
	return extractHTML(item)
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

// vhostRedirect issues the per-app origin-isolation 302 for an author-controlled
// HTML representation. It 302s to the canonical page host for pageRef when the
// request hit a non-canonical host, preserving the given path suffix (e.g.
// "/page", "/raw") and the query string so the target still pins that
// representation. Returns true if it redirected; the caller must then stop.
// The pageVhostRedirect / rawVhostRedirect wrappers decide whether to redirect
// and pick pageRef, because /page and /raw canonicalize differently (see each).
func vhostRedirect(w http.ResponseWriter, r *http.Request, resolver *vhost.Resolver, vhostMode, ref, pageRef, pathSuffix string) bool {
	if resolver == nil {
		return false
	}
	if canonicalPageHost(vhostMode, resolver, r.Host, pageRef) {
		return false
	}
	target := pageRedirectTargetPath(vhostMode, resolver, r, ref, pageRef, pathSuffix)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusFound)
	return true
}

// pageVhostRedirect enforces per-app origin isolation for GET /{ref}/page. When
// the object is a PAGE (or resolves one via a page relation) and the request hit
// a non-canonical host, it 302s to the canonical page host — the same check
// GET /{ref} makes, minus the acceptsHTML gate (/page always serves HTML, so the
// shared-origin exposure exists for every Accept). It canonicalizes on the
// resolved PAGE ref: the page-relation target when there is one, else the
// object's own ref.
func pageVhostRedirect(w http.ResponseWriter, r *http.Request, resolver *vhost.Resolver, vhostMode, ref string, meta object.ObjectMeta) bool {
	if !(meta.Type == "PAGE" || meta.HasPageRelation) {
		return false
	}
	pageRef := ref
	if meta.HasPageRelation && meta.PageRef != "" {
		pageRef = meta.PageRef
	}
	return vhostRedirect(w, r, resolver, vhostMode, ref, pageRef, "/page")
}

// rawVhostRedirect enforces per-app origin isolation for GET /{ref}/raw. /raw on
// a PAGE serves the PAGE's OWN author-controlled html, so it needs the same
// canonical-host 302 as /page — but keyed strictly on the OBJECT being a PAGE,
// canonicalized on the PAGE's OWN ref (never a page-relation target, which /raw
// ignores). A non-PAGE object — including one carrying a page relation, or an
// HTML-mime BLOB (out of scope, issue #14) — is served on the shared origin and
// must NOT redirect (a non-PAGE with a page relation 409s on /raw instead).
func rawVhostRedirect(w http.ResponseWriter, r *http.Request, resolver *vhost.Resolver, vhostMode, ref string, meta object.ObjectMeta) bool {
	if meta.Type != "PAGE" {
		return false
	}
	return vhostRedirect(w, r, resolver, vhostMode, ref, ref, "/raw")
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

// --- Root representation aliases ---
//
// GET /json, /raw, /page serve the representation of whatever GET / resolves to
// on this host — DIRECTLY (200), not via a redirect. This powers a redirect-less
// agent bootstrap ("GET https://dataverse001.net/json …"): the dumbest client
// must succeed without following a 302. resolveRootTarget mirrors handleRoot's
// host resolution exactly, then the alias delegates to the identical
// /{ref}/<repr> pipeline via serve<Repr>ForRef.

// resolveRootTarget resolves the ref that GET / serves on this host, for a root
// representation alias. It mirrors handleRoot / handleRootLegacy:
//   - Vhost==nil, or a request to the base domain → the ROOT object's ref (the
//     same ref handleRootLegacy redirects to). The caller serves it directly.
//   - a vhost page-host in isolate mode → the resolved page ref.
//   - a vhost page-host in VhostModeRedirect → a 302 to the base domain, mirroring
//     handleRoot, but with pathSuffix (/json, /raw, /page) appended so the
//     representation survives the redirect; handled=true.
//   - an unknown host → 404 NOT_FOUND; handled=true.
//   - no ROOT object present → 404 NOT_FOUND; handled=true.
//
// When handled=true a terminal response is already written and the caller must
// stop. Otherwise ref is the target and the caller runs the full /{ref}/<repr>
// pipeline (auth, ETag/304, vhost redirect, projection) unchanged.
func resolveRootTarget(w http.ResponseWriter, r *http.Request, index *storage.Index, resolver *vhost.Resolver, vhostMode, pathSuffix string) (ref string, handled bool) {
	if resolver == nil {
		return rootObjectRef(w, r, index)
	}
	resolved := resolver.Resolve(r.Host)
	if resolved == "" {
		if baseHostMatches(r.Host, resolver.BaseDomain()) {
			return rootObjectRef(w, r, index)
		}
		writeError(w, r, http.StatusNotFound, "unknown host", "NOT_FOUND")
		return "", true
	}
	if normalizeVhostMode(vhostMode) == VhostModeRedirect {
		http.Redirect(w, r, pageRedirectTargetPath(vhostMode, resolver, r, resolved, resolved, pathSuffix), http.StatusFound)
		return "", true
	}
	return resolved, false
}

// rootObjectRef returns the ROOT object's ref — the same ref handleRootLegacy
// redirects GET / to — or writes 404 NOT_FOUND (handled=true) when there is no
// ROOT object.
func rootObjectRef(w http.ResponseWriter, r *http.Request, index *storage.Index) (ref string, handled bool) {
	metas := index.GetAll("", "ROOT", "", false)
	if len(metas) == 0 {
		writeError(w, r, http.StatusNotFound, "no root object", "NOT_FOUND")
		return "", true
	}
	return metas[0].Ref, false
}

// --- Hub handlers ---

// handleGetJSON serves GET /{ref}/json.
func (h *Hub) handleGetJSON(w http.ResponseWriter, r *http.Request) {
	h.serveJSONForRef(w, r, chi.URLParam(r, "ref"))
}

// handleRootJSON serves GET /json — the JSON envelope of GET /'s target.
func (h *Hub) handleRootJSON(w http.ResponseWriter, r *http.Request) {
	ref, handled := resolveRootTarget(w, r, h.index, h.Vhost, h.VhostMode, "/json")
	if handled {
		return
	}
	h.serveJSONForRef(w, r, ref)
}

// serveJSONForRef runs the /{ref}/json pipeline for an explicit ref.
func (h *Hub) serveJSONForRef(w http.ResponseWriter, r *http.Request, ref string) {
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
	h.serveRawForRef(w, r, chi.URLParam(r, "ref"))
}

// handleRootRaw serves GET /raw — the native content of GET /'s target.
func (h *Hub) handleRootRaw(w http.ResponseWriter, r *http.Request) {
	ref, handled := resolveRootTarget(w, r, h.index, h.Vhost, h.VhostMode, "/raw")
	if handled {
		return
	}
	h.serveRawForRef(w, r, ref)
}

// serveRawForRef runs the /{ref}/raw pipeline for an explicit ref.
func (h *Hub) serveRawForRef(w http.ResponseWriter, r *http.Request, ref string) {
	if !object.IsValidRef(ref) {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}
	meta, found, ok := authorizeProjection(w, r, ref, h.index)
	if !ok {
		return
	}
	// Auth (authorizeProjection) runs BEFORE the redirect so an unauthorized
	// private object 404s and never leaks existence via a Location header.
	if rawVhostRedirect(w, r, h.Vhost, h.VhostMode, ref, meta) {
		return
	}
	serveRaw(w, r, h.store, ref, meta, found, h.baseDomain())
}

// handleGetPage serves GET /{ref}/page.
func (h *Hub) handleGetPage(w http.ResponseWriter, r *http.Request) {
	h.servePageForRef(w, r, chi.URLParam(r, "ref"))
}

// handleRootPage serves GET /page — the HTML view of GET /'s target.
func (h *Hub) handleRootPage(w http.ResponseWriter, r *http.Request) {
	ref, handled := resolveRootTarget(w, r, h.index, h.Vhost, h.VhostMode, "/page")
	if handled {
		return
	}
	h.servePageForRef(w, r, ref)
}

// servePageForRef runs the /{ref}/page pipeline for an explicit ref.
func (h *Hub) servePageForRef(w http.ResponseWriter, r *http.Request, ref string) {
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
	p.serveJSONForRef(w, r, chi.URLParam(r, "ref"))
}

// handleRootJSON serves GET /json — the JSON envelope of GET /'s target.
func (p *Proxy) handleRootJSON(w http.ResponseWriter, r *http.Request) {
	ref, handled := resolveRootTarget(w, r, p.index, p.Vhost, p.VhostMode, "/json")
	if handled {
		return
	}
	p.serveJSONForRef(w, r, ref)
}

// serveJSONForRef runs the /{ref}/json proxy pipeline for an explicit ref.
func (p *Proxy) serveJSONForRef(w http.ResponseWriter, r *http.Request, ref string) {
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

// handleGetRaw serves GET /{ref}/raw in proxy mode. syncPageDeps stays false:
// /raw on a PAGE serves its own html, so no page-relation dependencies are needed.
func (p *Proxy) handleGetRaw(w http.ResponseWriter, r *http.Request) {
	p.serveRawForRef(w, r, chi.URLParam(r, "ref"))
}

// handleRootRaw serves GET /raw — the native content of GET /'s target.
func (p *Proxy) handleRootRaw(w http.ResponseWriter, r *http.Request) {
	ref, handled := resolveRootTarget(w, r, p.index, p.Vhost, p.VhostMode, "/raw")
	if handled {
		return
	}
	p.serveRawForRef(w, r, ref)
}

// serveRawForRef runs the /{ref}/raw proxy pipeline for an explicit ref.
func (p *Proxy) serveRawForRef(w http.ResponseWriter, r *http.Request, ref string) {
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
	// Auth runs BEFORE the redirect (see Hub.handleGetRaw) so existence is never
	// leaked for an unauthorized private object.
	if rawVhostRedirect(w, r, p.Vhost, p.VhostMode, ref, meta) {
		return
	}
	serveRaw(w, r, p.store, ref, meta, found, p.baseDomain())
}

// handleGetPage serves GET /{ref}/page in proxy mode.
func (p *Proxy) handleGetPage(w http.ResponseWriter, r *http.Request) {
	p.servePageForRef(w, r, chi.URLParam(r, "ref"))
}

// handleRootPage serves GET /page — the HTML view of GET /'s target.
func (p *Proxy) handleRootPage(w http.ResponseWriter, r *http.Request) {
	ref, handled := resolveRootTarget(w, r, p.index, p.Vhost, p.VhostMode, "/page")
	if handled {
		return
	}
	p.servePageForRef(w, r, ref)
}

// servePageForRef runs the /{ref}/page proxy pipeline for an explicit ref.
func (p *Proxy) servePageForRef(w http.ResponseWriter, r *http.Request, ref string) {
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
