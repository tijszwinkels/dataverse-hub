package serving

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/events"
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

	Events               *events.Log // nil = events disabled (/events → 404)
	EventsMaxSubscribers int         // 0 = default (256)
	eventSubs            atomic.Int64
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

	// Auth routes
	r.Get("/auth/challenge", h.auth.HandleChallenge)
	r.Post("/auth/token", h.auth.HandleToken)
	r.Post("/auth/logout", h.auth.HandleLogout)
	r.Get("/auth/realms", handleAuthRealms(h.index.Resolver()))

	r.Get("/ask", TLSAskHandler(h.Vhost))
	r.Get("/", h.handleRoot)
	r.Get("/search", h.handleListObjects)
	r.Get("/events", h.handleEvents)
	r.Get("/{ref}", h.handleGetObject)
	r.Put("/{ref}", h.handlePutObject)
	r.Get("/{ref}/inbound", h.handleGetInbound)

	return r
}

// handleEvents serves the change feed (SSE + JSON replay). Uses the same
// merged realm resolver as GET so "receives an event" ⇔ "could GET the ref".
func (h *Hub) handleEvents(w http.ResponseWriter, r *http.Request) {
	serveEvents(w, r, h.Events, h.index.Resolver(), h.auth, &h.eventSubs, h.EventsMaxSubscribers)
}

// handleAuthRealms returns a handler for GET /auth/realms.
// Returns the shared realms the authenticated user belongs to.
func handleAuthRealms(resolver realm.RealmResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authPK := auth.AuthPubkey(r)
		if authPK == "" {
			writeError(w, http.StatusUnauthorized, "authentication required", "UNAUTHORIZED")
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

// jsonContentType sets the Content-Type header to application/json.
func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}
