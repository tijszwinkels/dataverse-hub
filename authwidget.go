package main

import (
	"io"
	"net/http"
	"strings"
)

// AuthWidgetConfig configures the auth widget module.
type AuthWidgetConfig struct {
	// AuthHost is the hostname for the auth widget (e.g. "auth.dataverse001.net").
	AuthHost string
	// AllowedOrigins are origins that may embed the widget and call the hub API.
	// Typically ["https://dataverse001.net"].
	AllowedOrigins []string
}

// authWidgetHandler returns an http.HandlerFunc that serves the widget HTML.
// It only responds when the Host header matches AuthHost or the path is /widget.
func authWidgetHandler(cfg AuthWidgetConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := strings.Split(r.Host, ":")[0]
		if host != cfg.AuthHost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, widgetHTML)
	}
}

// corsMiddleware adds CORS headers for the auth widget origin.
// The widget on auth.dataverse001.net needs to make API calls
// (GET, PUT) back to dataverse001.net.
func corsMiddleware(cfg AuthWidgetConfig) func(http.Handler) http.Handler {
	allowed := make(map[string]bool)
	for _, o := range cfg.AllowedOrigins {
		allowed[o] = true
	}
	// Also allow the auth origin itself
	allowed["https://"+cfg.AuthHost] = true

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
			}
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
