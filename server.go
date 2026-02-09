package main

import (
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Hub ties together the store, index, and rate limiter.
type Hub struct {
	store   *Store
	index   *Index
	limiter *RateLimiter
}

// NewHub creates a Hub with the given components.
func NewHub(store *Store, index *Index, limiter *RateLimiter) *Hub {
	return &Hub{store: store, index: index, limiter: limiter}
}

// Router returns the chi router with all routes and middleware.
func (h *Hub) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(h.limiter.Middleware)
	r.Use(jsonContentType)

	r.Get("/", h.handleRoot)

	r.Route("/v1", func(r chi.Router) {
		r.Get("/objects", h.handleListObjects)
		r.Get("/objects/{ref}", h.handleGetObject)
		r.Put("/objects/{ref}", h.handlePutObject)
		r.Get("/objects/{ref}/inbound", h.handleGetInbound)
	})

	return r
}

// requestLogger logs method, path, status, duration, and user-agent.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		log.Printf("%s %s %d %s ua=%q", r.Method, r.RequestURI, ww.Status(), time.Since(start).Round(time.Millisecond), r.UserAgent())
	})
}

// jsonContentType sets the Content-Type header to application/json.
func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}
