package serving

import (
	"net/http"

	"github.com/dataverse/hub/auth"
	"github.com/dataverse/hub/storage"
	"github.com/dataverse/hub/vhost"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Hub ties together the store, index, rate limiter, and auth.
type Hub struct {
	store            *storage.Store
	index            *storage.Index
	limiter          *auth.RateLimiter
	auth             *auth.AuthStore
	defaultViewerRef string
	Vhost            *vhost.Resolver // nil = vhosting disabled
}

// NewHub creates a Hub with the given components.
func NewHub(store *storage.Store, index *storage.Index, limiter *auth.RateLimiter, auth *auth.AuthStore, defaultViewerRef string) *Hub {
	return &Hub{store: store, index: index, limiter: limiter, auth: auth, defaultViewerRef: defaultViewerRef}
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

	// Auth routes
	r.Get("/auth/challenge", h.auth.HandleChallenge)
	r.Post("/auth/token", h.auth.HandleToken)
	r.Post("/auth/logout", h.auth.HandleLogout)

	r.Get("/ask", TLSAskHandler(h.Vhost))
	r.Get("/", h.handleRoot)
	r.Get("/search", h.handleListObjects)
	r.Get("/{ref}", h.handleGetObject)
	r.Put("/{ref}", h.handlePutObject)
	r.Get("/{ref}/inbound", h.handleGetInbound)

	return r
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

// jsonContentType sets the Content-Type header to application/json.
func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}
