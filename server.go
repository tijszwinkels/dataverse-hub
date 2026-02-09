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
	verbose bool
}

// NewHub creates a Hub with the given components.
func NewHub(store *Store, index *Index, limiter *RateLimiter, verbose bool) *Hub {
	return &Hub{store: store, index: index, limiter: limiter, verbose: verbose}
}

// Router returns the chi router with all routes and middleware.
func (h *Hub) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(h.requestLogger)
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

// requestLogger logs method, path, status, and duration. With verbose, also logs user-agent.
func (h *Hub) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		if h.verbose {
			log.Printf("%s %s %d %s ua=%q", r.Method, r.RequestURI, ww.Status(), time.Since(start), r.UserAgent())
		} else {
			log.Printf("%s %s %d %s", r.Method, r.RequestURI, ww.Status(), time.Since(start))
		}
	})
}

// jsonContentType sets the Content-Type header to application/json.
func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}
