package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dataverse/hub/auth"
	"github.com/dataverse/hub/storage"
	"github.com/dataverse/hub/serving"
)

func TestWidgetRouteInHubRouter(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewStore(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	index := storage.NewIndex()
	limiter := auth.NewRateLimiter(1000, 100000)
	defer limiter.Stop()

	authStore := auth.NewAuthStore(168 * time.Hour)
	defer authStore.Stop()
	hub := serving.NewHub(store, index, limiter, authStore, "")
	cfg := auth.WidgetConfig{
		AuthHost:       "auth.dataverse001.net",
		AllowedOrigins: []string{"https://dataverse001.net"},
	}
	handler := hub.RouterWithAuthWidget(cfg)
	ts := httptest.NewServer(handler)
	defer ts.Close()

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
