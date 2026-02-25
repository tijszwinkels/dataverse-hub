package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthWidgetHandler_MatchingHost(t *testing.T) {
	cfg := AuthWidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	handler := authWidgetHandler(cfg)

	req := httptest.NewRequest("GET", "/widget", nil)
	req.Host = "auth.dataverse001.net"
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("expected Content-Type text/html, got %q", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatal("expected HTML body with doctype")
	}
}

func TestAuthWidgetHandler_WrongHost(t *testing.T) {
	cfg := AuthWidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	handler := authWidgetHandler(cfg)

	req := httptest.NewRequest("GET", "/widget", nil)
	req.Host = "dataverse001.net"
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for wrong host, got %d", w.Code)
	}
}

func TestAuthWidgetHandler_HostWithPort(t *testing.T) {
	cfg := AuthWidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	handler := authWidgetHandler(cfg)

	req := httptest.NewRequest("GET", "/widget", nil)
	req.Host = "auth.dataverse001.net:443"
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for host with port, got %d", w.Code)
	}
}

func TestCORSMiddleware_AllowedOrigin(t *testing.T) {
	cfg := AuthWidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	mw := corsMiddleware(cfg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := mw(inner)

	req := httptest.NewRequest("GET", "/some-ref", nil)
	req.Header.Set("Origin", "https://dataverse001.net")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "https://dataverse001.net" {
		t.Fatalf("expected CORS Allow-Origin header, got %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestCORSMiddleware_DisallowedOrigin(t *testing.T) {
	cfg := AuthWidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	mw := corsMiddleware(cfg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := mw(inner)

	req := httptest.NewRequest("GET", "/some-ref", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("expected no CORS header for disallowed origin, got %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestCORSMiddleware_Preflight(t *testing.T) {
	cfg := AuthWidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	mw := corsMiddleware(cfg)

	innerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerCalled = true
	})
	handler := mw(inner)

	req := httptest.NewRequest("OPTIONS", "/some-ref", nil)
	req.Header.Set("Origin", "https://dataverse001.net")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for OPTIONS preflight, got %d", w.Code)
	}
	if innerCalled {
		t.Fatal("inner handler should not be called for OPTIONS preflight")
	}
	if w.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Fatal("expected Allow-Methods header on preflight")
	}
}

func TestCORSMiddleware_AuthOriginAllowed(t *testing.T) {
	cfg := AuthWidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	mw := corsMiddleware(cfg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := mw(inner)

	// The auth origin itself should be allowed
	req := httptest.NewRequest("GET", "/some-ref", nil)
	req.Header.Set("Origin", "https://auth.dataverse001.net")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "https://auth.dataverse001.net" {
		t.Fatalf("expected auth origin to be allowed, got %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestWidgetRouteInHubRouter(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	index := NewIndex()
	limiter := NewRateLimiter(1000, 100000)
	defer limiter.Stop()

	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()
	hub := NewHub(store, index, limiter, auth, "")
	cfg := AuthWidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	handler := hub.RouterWithAuthWidget(cfg)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Widget route with matching host
	req, _ := http.NewRequest("GET", ts.URL+"/widget", nil)
	req.Host = "auth.dataverse001.net"
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /widget with auth host, got %d", resp.StatusCode)
	}

	// Widget route with wrong host — should 404
	req2, _ := http.NewRequest("GET", ts.URL+"/widget", nil)
	req2.Host = "dataverse001.net"
	resp2, err := http.DefaultTransport.RoundTrip(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for /widget with wrong host, got %d", resp2.StatusCode)
	}
}
