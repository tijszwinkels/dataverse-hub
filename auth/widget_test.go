package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWidgetHandler_MatchingHost(t *testing.T) {
	cfg := WidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	handler := WidgetHandler(cfg)

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

func TestWidgetHandler_WrongHost(t *testing.T) {
	cfg := WidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	handler := WidgetHandler(cfg)

	req := httptest.NewRequest("GET", "/widget", nil)
	req.Host = "dataverse001.net"
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for wrong host, got %d", w.Code)
	}
}

func TestWidgetHandler_HostWithPort(t *testing.T) {
	cfg := WidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	handler := WidgetHandler(cfg)

	req := httptest.NewRequest("GET", "/widget", nil)
	req.Host = "auth.dataverse001.net:443"
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for host with port, got %d", w.Code)
	}
}

func TestCORSMiddleware_AllowedOrigin(t *testing.T) {
	cfg := WidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	mw := CORSMiddleware(cfg)

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
	cfg := WidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	mw := CORSMiddleware(cfg)

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
	cfg := WidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	mw := CORSMiddleware(cfg)

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

func TestCORSMiddleware_CredentialsHeader(t *testing.T) {
	cfg := WidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	mw := CORSMiddleware(cfg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := mw(inner)

	req := httptest.NewRequest("GET", "/auth/token", nil)
	req.Header.Set("Origin", "https://dataverse001.net")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("expected Access-Control-Allow-Credentials: true, got %q", w.Header().Get("Access-Control-Allow-Credentials"))
	}

	req = httptest.NewRequest("OPTIONS", "/auth/token", nil)
	req.Header.Set("Origin", "https://dataverse001.net")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("expected Access-Control-Allow-Credentials: true on preflight, got %q", w.Header().Get("Access-Control-Allow-Credentials"))
	}

	req = httptest.NewRequest("GET", "/auth/token", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("disallowed origin should not get credentials header")
	}
}

func TestCORSMiddleware_AuthOriginAllowed(t *testing.T) {
	cfg := WidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	mw := CORSMiddleware(cfg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := mw(inner)

	req := httptest.NewRequest("GET", "/some-ref", nil)
	req.Header.Set("Origin", "https://auth.dataverse001.net")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "https://auth.dataverse001.net" {
		t.Fatalf("expected auth origin to be allowed, got %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
}
