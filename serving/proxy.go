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
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/freenet"
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
	Mirror           *freenet.Mirror // nil = Freenet mirroring disabled

	// UpstreamPush controls which objects are forwarded to upstream on PUT.
	// "public" (default) — only dataverse001 objects are forwarded.
	// "all" — all objects are forwarded, including identity-realm and shared-realm.
	UpstreamPush string

	upstream   *upstream.Client
	pending    *upstream.SyncPending
	writeLocks *keyedMutex // per-ref serialization of local read-check-write
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
		writeLocks:       newKeyedMutex(),
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

	r.NotFound(problemNotFound)
	r.MethodNotAllowed(methodNotAllowedProblem(r))

	// Auth routes
	r.Get("/auth/challenge", p.auth.HandleChallenge)
	r.Post("/auth/token", p.auth.HandleToken)
	r.Post("/auth/logout", p.auth.HandleLogout)
	r.Get("/auth/realms", handleAuthRealms(p.index.Resolver()))

	r.Get("/ask", TLSAskHandler(p.Vhost))
	r.Get("/freenet/status", func(w http.ResponseWriter, r *http.Request) {
		handleFreenetStatus(w, r, p.Mirror)
	})
	r.Get("/", p.handleRoot)
	// Root representation aliases (see resolveRootTarget / Hub.Router).
	r.Get("/json", p.handleRootJSON)
	r.Get("/raw", p.handleRootRaw)
	r.Get("/page", p.handleRootPage)
	r.Get("/search", p.handleSearch)
	r.Get("/{ref}", p.handleGetObject)
	r.Put("/{ref}", p.handlePutObject)
	r.Get("/{ref}/inbound", p.handleInbound)
	r.Get("/{ref}/json", p.handleGetJSON)
	r.Get("/{ref}/raw", p.handleGetRaw)
	r.Get("/{ref}/page", p.handleGetPage)

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
		writeError(w, r, http.StatusNotFound, "unknown host", "NOT_FOUND")
		return

	default:
		if normalizeVhostMode(p.VhostMode) == VhostModeRedirect {
			http.Redirect(w, r, pageRedirectTarget(p.VhostMode, p.Vhost, r, resolved, resolved), http.StatusFound)
			return
		}
		if meta, found := p.index.GetMeta(resolved); found && !meta.IsPublic {
			authPK := auth.AuthPubkey(r)
			if !realm.CanRead(meta.Realms, authPK, p.index.Resolver()) {
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
		io.WriteString(w, injectBaseDomain(html, p.baseDomain()))
	}
}

// handleRootLegacy redirects to the ROOT object from local index.
func (p *Proxy) handleRootLegacy(w http.ResponseWriter, r *http.Request) {
	metas := p.index.GetAll("", "ROOT", "", false)
	if len(metas) == 0 {
		writeError(w, r, http.StatusNotFound, "no root object", "NOT_FOUND")
		return
	}
	http.Redirect(w, r, "/"+metas[0].Ref, http.StatusFound)
}

// handleGetObject proxies GET /{ref} through upstream with ETag enrichment.
func (p *Proxy) handleGetObject(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")

	// Ref-shape gate: refs that can't possibly be valid (scanner traffic for
	// /.env, /wp-config.php, /robots.txt, …) get 404'd without an upstream
	// call. This protects upstream's per-IP rate limit, since from upstream's
	// perspective every proxy-forwarded request comes from a single IP.
	if !object.IsValidRef(ref) {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}

	// The upstream sync uses OUR cache state, independent of the client's ETag.
	// Serving HTML also refreshes viewer dependencies before local selection.
	if !p.syncFromUpstream(w, r, ref, acceptsHTML(r)) {
		return
	}
	p.serveFromLocalCache(w, r, ref)
}

// syncFromUpstream refreshes the local cache for ref from upstream, then
// optionally its page-relation / default-viewer dependencies (needed only when
// an HTML representation may be served). It returns ok=true when the caller
// should proceed to serve from the local cache, and ok=false when it has
// already written a terminal response (404 for a missing object, a forwarded
// non-gateway upstream error, or a 500). On a transient upstream failure
// (unreachable or gateway-down) it returns ok=true so the caller falls back to
// the local cache. The upstream conditional request uses OUR cached revision,
// never the client's ETag.
func (p *Proxy) syncFromUpstream(w http.ResponseWriter, r *http.Request, ref string, syncPageDeps bool) bool {
	upstreamETag := p.buildUpstreamETag(ref)

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.upstream.BaseURL()+"/"+ref, nil)
	if err != nil {
		log.Printf("[proxy] ERROR: GET /%s: build request: %v", ref, err)
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
		return false
	}
	upstreamReq.Header.Set("Accept", "application/json")
	if upstreamETag != "" {
		upstreamReq.Header.Set("If-None-Match", upstreamETag)
	}

	resp, err := p.upstream.Do(upstreamReq, nil)
	if err != nil {
		// Upstream unreachable — fall back to local cache
		log.Printf("[proxy] WARN: GET /%s: upstream unreachable, serving from cache", ref)
		return true
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
			writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
			return false
		}
		p.CacheLocally(ref, body)

	case http.StatusNotFound:
		localData, _ := p.store.Read(ref)
		if localData == nil {
			writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
			return false
		}
		log.Printf("[proxy] GET /%s: upstream 404 but found locally, serving + pushing", ref)
		go p.pushToUpstream(ref, localData)

	default:
		if upstream.IsDown(resp.StatusCode) {
			log.Printf("[proxy] WARN: GET /%s: upstream returned %d, falling back to cache", ref, resp.StatusCode)
			io.Copy(io.Discard, resp.Body)
			return true
		}
		// Forward non-gateway upstream error (4xx, etc.), preserving its
		// Content-Type so an upgraded upstream's problem+json passes through.
		body, _ := io.ReadAll(resp.Body)
		forwardResponse(w, r, resp, body)
		return false
	}

	// Phase 2: Sync page dependencies (if an HTML representation may be served)
	if syncPageDeps {
		p.ensurePageDepsFresh(ref)
	}
	return true
}

// handlePutObject proxies PUT /{ref} with local signature verification.
func (p *Proxy) handlePutObject(w http.ResponseWriter, r *http.Request) {
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

	// Parse and validate locally first
	env, item, err := object.ParseEnvelope(body)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err.Error(), "INVALID_OBJECT")
		return
	}
	realms := object.ResolveIn(env, item)

	// Validate pubkey-realm ownership: each pubkey-realm must match item.pubkey
	for _, pr := range object.PubkeyRealms(realms) {
		if pr != item.Pubkey {
			writeError(w, r, http.StatusForbidden,
				"pubkey-realm does not match item pubkey",
				"REALM_FORBIDDEN")
			return
		}
	}

	// Object must belong to dataverse001, a self-owned pubkey-realm, or a configured shared realm.
	// A valid SHARED_REALM always includes "dataverse001" in item.in (decision 6,
	// enforced by ParseSharedRealm below), so it passes via the dataverse001 branch.
	if !realm.ValidateRealmsForPut(realms, item.Pubkey, p.index.Resolver()) {
		writeError(w, r, http.StatusBadRequest,
			"object must belong to dataverse001, server-public, a self-owned pubkey-realm, or a configured shared realm",
			"INVALID_OBJECT")
		return
	}
	// Enforce the full SHARED_REALM type contract (decisions 3, 4, 6).
	if item.Type == realm.TypeSharedRealm {
		if _, _, err := realm.ParseSharedRealm(item); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid SHARED_REALM object: "+err.Error(), "INVALID_SHARED_REALM")
			return
		}
	}
	if ref != item.Ref() {
		writeError(w, r, http.StatusBadRequest,
			fmt.Sprintf("URL ref %q does not match item %q", ref, item.Ref()),
			"REF_MISMATCH")
		return
	}

	// Local signature verification
	if err := object.VerifyEnvelope(body); err != nil {
		writeError(w, r, http.StatusBadRequest, err.Error(), "INVALID_SIGNATURE")
		return
	}

	// Canonicalize
	canonical, err := object.CanonicalJSON(body)
	if err != nil {
		log.Printf("[proxy] ERROR: PUT /%s: canonical JSON: %v", ref, err)
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}

	ifMatch := r.Header.Get("If-Match")

	// Non-global objects (private, server-public) are stored locally only — unless upstream_push = "all"
	if !realm.IsGlobalObject(realms) && p.UpstreamPush != "all" {
		p.storePrivateLocally(w, r, ref, item, canonical, realms, ifMatch)
		return
	}

	// Forward to upstream
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPut, p.upstream.BaseURL()+"/"+ref, nil)
	if err != nil {
		log.Printf("[proxy] ERROR: PUT /%s: build request: %v", ref, err)
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	// Upstream is the authority for global objects, so let it enforce the
	// If-Match precondition; a 412 comes back and is relayed below.
	if ifMatch != "" {
		upstreamReq.Header.Set("If-Match", ifMatch)
	}

	resp, err := p.upstream.Do(upstreamReq, canonical)
	if err != nil {
		// Upstream unreachable — store locally with sync pending
		log.Printf("[proxy] WARN: PUT /%s: upstream unreachable, storing locally (sync pending)", ref)
		p.storeLocallyWithPending(w, r, ref, item, canonical, realms, ifMatch)
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		// Cache the upstream-accepted object. Lock per ref so the
		// backup→write→index sequence cannot interleave with another writer.
		// Wrapped in a closure so the lock is released via defer even if a
		// store/index call panics (chi's Recoverer would otherwise swallow the
		// panic and leak the lock, deadlocking future PUTs to this ref).
		func() {
			p.writeLocks.Lock(ref)
			defer p.writeLocks.Unlock(ref)
			if existingMeta, isUpdate := p.index.GetMeta(ref); isUpdate {
				if err := p.store.Backup(ref, existingMeta.Revision); err != nil {
					log.Printf("[proxy] WARN: PUT /%s: backup rev %d failed: %v", ref, existingMeta.Revision, err)
				}
			}
			ts, _ := item.Timestamp()
			p.store.Write(ref, canonical, ts)
			p.index.Update(ref, item, ts, realms)
		}()
		// Update vhost hash map for PAGE objects
		if p.Vhost != nil && item.Type == "PAGE" {
			p.Vhost.AddPage(ref)
		}
		log.Printf("[proxy] stored %s rev %d (%s)", ref, item.Revision, item.Type)

		// Queue the Freenet mirror (see Hub.handlePutObject for the contract).
		p.Mirror.Publish(ref, item.Revision, realms, canonical)

		// Advertise the new revision so clients can chain a conditional write.
		w.Header().Set("ETag", revisionETag(item.Revision))
		w.WriteHeader(resp.StatusCode)
		w.Write(canonical)

	case http.StatusConflict:
		// Upstream has newer revision — fetch and cache it
		log.Printf("[proxy] PUT /%s: upstream conflict, fetching newer version", ref)
		go p.fetchAndCacheFromUpstream(ref)
		respBody, _ := io.ReadAll(resp.Body)
		forwardResponse(w, r, resp, respBody)

	default:
		// Forward upstream error (400, etc.), preserving its Content-Type so an
		// upgraded upstream's problem+json passes through unchanged.
		respBody, _ := io.ReadAll(resp.Body)
		forwardResponse(w, r, resp, respBody)
	}
}

// forwardResponse relays an upstream response's status and body to the client.
//
// The proxy always queries upstream with Accept: application/json (so it can
// cache the JSON object), so an upstream problem+json error was negotiated on
// the proxy's Accept, not the client's. Re-negotiate it here on the *client's*
// Accept via the shared writer, so an HTML-only client keeps the legacy body
// and proxy mode matches hub mode. Non-problem bodies are relayed verbatim with
// the upstream Content-Type preserved.
func forwardResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, body []byte) {
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, object.ProblemMediaType) {
		var p object.Problem
		if json.Unmarshal(body, &p) == nil {
			object.WriteProblem(w, r, resp.StatusCode, p.Detail, p.Code)
			return
		}
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// handleSearch forwards GET /search to upstream, falls back to local.
func (p *Proxy) handleSearch(w http.ResponseWriter, r *http.Request) {
	p.forwardListEndpoint(w, r, "/search?"+r.URL.RawQuery)
}

// handleInbound forwards GET /{ref}/inbound to upstream, falls back to local.
func (p *Proxy) handleInbound(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	// Ref-shape gate (see handleGetObject for rationale).
	if !object.IsValidRef(ref) {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}
	p.forwardListEndpoint(w, r, "/"+ref+"/inbound?"+r.URL.RawQuery)
}

// forwardListEndpoint forwards a list-type request to upstream, falling back to local on failure.
// When the user is authenticated, merges local private objects into upstream results.
func (p *Proxy) forwardListEndpoint(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.upstream.BaseURL()+upstreamPath, nil)
	if err != nil {
		log.Printf("[proxy] ERROR: forward %s: build request: %v", upstreamPath, err)
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
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

	authPK := auth.AuthPubkey(r)

	// Merge local-only objects (server-public, private) into upstream results.
	// Even unauthenticated users may see server-public objects that only exist locally.
	var upstreamResp object.ListResponse
	if err := json.Unmarshal(body, &upstreamResp); err != nil {
		// Can't parse upstream response — forward raw
		log.Printf("[proxy] WARN: forward %s: parse upstream response: %v", upstreamPath, err)
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}

	p.mergeLocalIntoUpstream(w, r, upstreamResp, authPK)
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

// mergeLocalIntoUpstream merges local-only objects (server-public, private) into an upstream list response.
// Private objects never exist on upstream, so no dedup is needed. Both sources are sorted
// by (UpdatedAt DESC, Ref), enabling a standard merge-sort.
func (p *Proxy) mergeLocalIntoUpstream(w http.ResponseWriter, r *http.Request, upstreamResp object.ListResponse, authPK string) {
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

// ensureViewerDependencyFresh refreshes globally replicated dependencies but
// leaves private/server-local viewer objects under the proxy's local authority.
// In particular, an upstream 404 must never cause a private viewer to be pushed.
func (p *Proxy) ensureViewerDependencyFresh(ref string) {
	if meta, found := p.index.GetMeta(ref); found {
		if !realm.IsGlobalObject(object.InField(meta.Realms)) {
			return
		}
	} else if data, err := p.store.Read(ref); err == nil && data != nil {
		if env, item, err := object.ParseEnvelope(data); err == nil && !realm.IsGlobalObject(object.ResolveIn(env, item)) {
			return
		}
	}
	p.ensureFresh(ref)
}

// ensurePageDepsFresh syncs every bounded dependency that may participate in
// HTML selection: direct PAGE, one TYPE hop plus its PAGE, and the configured
// generic viewer. It never recursively follows type_def.
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

	if pageRef := firstRelationRef(item, "page", ref); pageRef != "" && pageRef != ref {
		p.ensureViewerDependencyFresh(pageRef)
	}

	if typeRef := firstRelationRef(item, "type_def", ref); typeRef != "" && typeRef != ref {
		p.ensureViewerDependencyFresh(typeRef)
		typeData, err := p.store.Read(typeRef)
		if err == nil && typeData != nil {
			_, typeItem, err := object.ParseEnvelope(typeData)
			if err == nil && typeItem.Type == "TYPE" {
				if pageRef := firstRelationRef(typeItem, "page", typeRef); pageRef != "" && pageRef != ref && pageRef != typeRef {
					p.ensureViewerDependencyFresh(pageRef)
				}
			}
		}
	}

	if p.defaultViewerRef != "" && ref != p.defaultViewerRef {
		p.ensureViewerDependencyFresh(p.defaultViewerRef)
		defaultData, err := p.store.Read(p.defaultViewerRef)
		if err == nil && defaultData != nil {
			_, defaultItem, err := object.ParseEnvelope(defaultData)
			if err == nil && defaultItem.Type != "PAGE" {
				if pageRef := firstRelationRef(defaultItem, "page", p.defaultViewerRef); pageRef != "" && pageRef != ref && pageRef != p.defaultViewerRef {
					p.ensureViewerDependencyFresh(pageRef)
				}
			}
		}
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
func (p *Proxy) serveFromLocalCache(w http.ResponseWriter, r *http.Request, ref string) {
	meta, found := p.index.GetMeta(ref)
	if !found {
		data, err := p.store.Read(ref)
		if err != nil || data == nil {
			writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
			return
		}
		p.serveObjectData(w, r, ref, data)
		return
	}

	// Private object access check
	if !meta.IsPublic {
		authPK := auth.AuthPubkey(r)
		if !realm.CanRead(meta.Realms, authPK, p.index.Resolver()) {
			if data, err := p.store.Read(ref); err == nil && data != nil {
				resolver := newViewerResolver(p.store, p.index, ref, data, authPK)
				if servePrivateViewerLogin(w, r, resolver, p.Vhost, p.VhostMode, ref) {
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

	data, err := p.store.Read(ref)
	if err != nil || data == nil {
		writeError(w, r, http.StatusNotFound, "object not found", "NOT_FOUND")
		return
	}
	p.serveObjectData(w, r, ref, data)
}

// serveObjectData selects, validates, and writes GET /{ref}'s representation.
func (p *Proxy) serveObjectData(w http.ResponseWriter, r *http.Request, ref string, data []byte) {
	_, item, err := object.ParseEnvelope(data)
	if err != nil {
		log.Printf("[proxy] ERROR: GET /%s: parse stored object: %v", ref, err)
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	resolver := newViewerResolver(p.store, p.index, ref, data, auth.AuthPubkey(r))
	selected := selectNegotiatedRepresentation(r, resolver, p.defaultViewerRef)
	selected.setCacheHeaders(w)
	if pageRef := selected.isolatedPageRef(); pageRef != "" &&
		vhostRedirect(w, r, p.Vhost, p.VhostMode, ref, pageRef, "") {
		return
	}
	etag := selected.etag(item.Revision)
	w.Header().Set("Vary", "Accept")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	selected.write(w, data, p.baseDomain())
}

// storePrivateLocally stores a private object locally without forwarding to upstream.
func (p *Proxy) storePrivateLocally(w http.ResponseWriter, r *http.Request, ref string, item *object.Item, canonical []byte, realms object.InField, ifMatch string) {
	// The proxy is the authority for private objects, so enforce the read-check-
	// write atomically per ref (see keyedMutex).
	p.writeLocks.Lock(ref)
	defer p.writeLocks.Unlock(ref)

	existingMeta, isUpdate := p.index.GetMeta(ref)
	if checkConditionalWrite(w, r, ifMatch, existingMeta, isUpdate, item.Revision) {
		return
	}

	ts, err := item.Timestamp()
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid timestamp: "+err.Error(), "INVALID_OBJECT")
		return
	}

	if isUpdate {
		if err := p.store.Backup(ref, existingMeta.Revision); err != nil {
			log.Printf("[proxy] WARN: PUT /%s: backup rev %d failed: %v", ref, existingMeta.Revision, err)
		}
	}

	if err := p.store.Write(ref, canonical, ts); err != nil {
		log.Printf("[proxy] ERROR: PUT /%s: store write: %v", ref, err)
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	p.index.Update(ref, item, ts, realms)
	// Update vhost hash map for PAGE objects
	if p.Vhost != nil && item.Type == "PAGE" {
		p.Vhost.AddPage(ref)
	}
	log.Printf("stored %s rev %d (%s) [private, local-only]", ref, item.Revision, item.Type)

	w.Header().Set("ETag", revisionETag(item.Revision))
	w.WriteHeader(http.StatusCreated)
	w.Write(canonical)
}

// storeLocallyWithPending stores an object locally and adds to sync pending.
func (p *Proxy) storeLocallyWithPending(w http.ResponseWriter, r *http.Request, ref string, item *object.Item, canonical []byte, realms object.InField, ifMatch string) {
	// Upstream is unreachable, so the proxy is temporarily the authority: apply
	// the same atomic read-check-write and If-Match precondition as elsewhere.
	p.writeLocks.Lock(ref)
	defer p.writeLocks.Unlock(ref)

	// Check revision against local index
	existingMeta, isUpdate := p.index.GetMeta(ref)
	if checkConditionalWrite(w, r, ifMatch, existingMeta, isUpdate, item.Revision) {
		return
	}

	ts, err := item.Timestamp()
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid timestamp: "+err.Error(), "INVALID_OBJECT")
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
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}

	// Write to main store
	if err := p.store.Write(ref, canonical, ts); err != nil {
		log.Printf("[proxy] ERROR: PUT /%s: store write: %v", ref, err)
		writeError(w, r, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	p.index.Update(ref, item, ts, realms)
	// Update vhost hash map for PAGE objects
	if p.Vhost != nil && item.Type == "PAGE" {
		p.Vhost.AddPage(ref)
	}
	log.Printf("[proxy] stored %s rev %d (%s) (sync pending)", ref, item.Revision, item.Type)

	// Queue the Freenet mirror. Upstream is down, but this hub has accepted
	// and durably stored the object, so it is mirrorable now; the snapshot is
	// signed and the publish is idempotent, so a later upstream conflict
	// costs nothing.
	p.Mirror.Publish(ref, item.Revision, realms, canonical)

	// 202 Accepted — stored locally, sync pending
	w.Header().Set("ETag", revisionETag(item.Revision))
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
