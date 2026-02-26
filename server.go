package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Hub ties together the store, index, rate limiter, and auth.
type Hub struct {
	store            *Store
	index            *Index
	limiter          *RateLimiter
	auth             *AuthStore
	defaultViewerRef string
}

// NewHub creates a Hub with the given components.
func NewHub(store *Store, index *Index, limiter *RateLimiter, auth *AuthStore, defaultViewerRef string) *Hub {
	return &Hub{store: store, index: index, limiter: limiter, auth: auth, defaultViewerRef: defaultViewerRef}
}

// Router returns the chi router with all routes and middleware.
func (h *Hub) Router() http.Handler {
	return h.RouterWithAuthWidget(AuthWidgetConfig{})
}

// RouterWithAuthWidget returns the chi router with auth widget support.
// If cfg.AuthHost is empty, the widget route and CORS middleware are skipped.
func (h *Hub) RouterWithAuthWidget(cfg AuthWidgetConfig) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(h.limiter.Middleware)
	r.Use(h.auth.Middleware)
	if cfg.AuthHost != "" {
		r.Use(corsMiddleware(cfg))
	}
	r.Use(jsonContentType)

	// Auth routes
	r.Get("/auth/challenge", h.auth.HandleChallenge)
	r.Post("/auth/token", h.auth.HandleToken)
	r.Post("/auth/logout", h.auth.HandleLogout)

	if cfg.AuthHost != "" {
		r.Get("/widget", authWidgetHandler(cfg))
	}
	r.Get("/", h.handleRoot)
	r.Get("/search", h.handleListObjects)
	r.Get("/{ref}", h.handleGetObject)
	r.Put("/{ref}", h.handlePutObject)
	r.Get("/{ref}/inbound", h.handleGetInbound)

	return r
}

// jsonContentType sets the Content-Type header to application/json.
func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}
