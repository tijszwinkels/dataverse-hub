package serving

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/vhost"
)

// Hub ties together the store, index, rate limiter, and auth.
type Hub struct {
	store            *storage.Store
	index            *storage.Index
	limiter          *auth.RateLimiter
	auth             *auth.AuthStore
	defaultViewerRef string
	shared           *realm.SharedRealms
	Vhost            *vhost.Resolver // nil = vhosting disabled
	VhostMode        string
	writeLocks       *keyedMutex // per-ref serialization of PUT read-check-write
}

// NewHub creates a Hub with the given components.
func NewHub(store *storage.Store, index *storage.Index, limiter *auth.RateLimiter, auth *auth.AuthStore, defaultViewerRef string, shared *realm.SharedRealms) *Hub {
	return &Hub{
		store:            store,
		index:            index,
		limiter:          limiter,
		auth:             auth,
		defaultViewerRef: defaultViewerRef,
		shared:           shared,
		VhostMode:        VhostModeIsolate,
		writeLocks:       newKeyedMutex(),
	}
}

// Router returns the chi router with all routes and middleware.
func (h *Hub) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(h.limiter.Middleware)
	r.Use(h.auth.Middleware)
	r.Use(jsonContentType)

	r.NotFound(problemNotFound)
	r.MethodNotAllowed(methodNotAllowedProblem(r))

	// Auth routes
	r.Get("/auth/challenge", h.auth.HandleChallenge)
	r.Post("/auth/token", h.auth.HandleToken)
	r.Post("/auth/logout", h.auth.HandleLogout)
	r.Get("/auth/realms", handleAuthRealms(h.index.Resolver()))

	r.Get("/ask", TLSAskHandler(h.Vhost))
	r.Get("/", h.handleRoot)
	// Root representation aliases: the representation of whatever GET / resolves
	// to on this host, served directly (see resolveRootTarget). Static routes,
	// so they take precedence over /{ref} (refs are pubkey.uuid, never bare words).
	r.Get("/json", h.handleRootJSON)
	r.Get("/raw", h.handleRootRaw)
	r.Get("/page", h.handleRootPage)
	r.Get("/search", h.handleListObjects)
	r.Get("/{ref}", h.handleGetObject)
	r.Put("/{ref}", h.handlePutObject)
	r.Get("/{ref}/inbound", h.handleGetInbound)
	r.Get("/{ref}/json", h.handleGetJSON)
	r.Get("/{ref}/raw", h.handleGetRaw)
	r.Get("/{ref}/page", h.handleGetPage)

	return r
}

// handleAuthRealms returns a handler for GET /auth/realms.
// Returns the shared realms the authenticated user belongs to.
func handleAuthRealms(resolver realm.RealmResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authPK := auth.AuthPubkey(r)
		if authPK == "" {
			writeError(w, r, http.StatusUnauthorized, "authentication required", "UNAUTHORIZED")
			return
		}
		var realms []string
		if resolver != nil {
			realms = resolver.RealmsForPubkey(authPK)
		}
		if realms == nil {
			realms = []string{}
		}
		json.NewEncoder(w).Encode(map[string][]string{
			"realms": realms,
		})
	}
}

// requestScheme returns "https" or "http" based on X-Forwarded-Proto or TLS state.
func requestScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// requestPort returns the port suffix (e.g. ":5678") from the Host header,
// or "" if it's the default port for the scheme (or absent).
func requestPort(r *http.Request) string {
	host := r.Host
	i := strings.LastIndex(host, ":")
	if i == -1 {
		return ""
	}
	// Avoid matching IPv6 bracket notation
	if strings.Contains(host, "]") {
		if bi := strings.LastIndex(host, "]"); bi > i {
			return ""
		}
	}
	port := host[i:] // includes the colon
	// Omit default ports
	if port == ":80" || port == ":443" {
		return ""
	}
	return port
}

// problemNotFound is the router's fallback for unmatched paths — it returns an
// RFC 9457 problem instead of chi's plain-text "404 page not found".
func problemNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, "no route matches this path", "NOT_FOUND")
}

// methodNotAllowedProblem returns the router's 405 fallback. chi sets the RFC
// 7231 Allow header for its default handler but drops it for a custom one, so
// we reconstruct it by probing the router for the methods registered on the
// path, then return an RFC 9457 problem instead of an empty body.
func methodNotAllowedProblem(router *chi.Mux) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if allow := allowedMethods(router, r.URL.Path); allow != "" {
			w.Header().Set("Allow", allow)
		}
		writeError(w, r, http.StatusMethodNotAllowed, "method not allowed for this endpoint", "METHOD_NOT_ALLOWED")
	}
}

// allowedMethods probes the router for the HTTP methods registered on path,
// returning them as an RFC 7231 comma-separated Allow value (e.g. "GET, PUT").
func allowedMethods(router *chi.Mux, path string) string {
	candidates := []string{
		http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions,
	}
	var allowed []string
	for _, m := range candidates {
		if router.Match(chi.NewRouteContext(), m, path) {
			allowed = append(allowed, m)
		}
	}
	return strings.Join(allowed, ", ")
}

// jsonContentType sets the Content-Type header to application/json.
func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}
