package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/upstream"
	"github.com/tijszwinkels/dataverse-hub/vhost"
)

// testRootAndVhostProxy builds a root hub + an isolate-mode vhost proxy pointing
// at it. Returns the proxy server and cleanup.
func testRootAndVhostProxy(t *testing.T, baseDomain string) (*httptest.Server, func()) {
	t.Helper()

	rootDir := t.TempDir()
	rootStore, _ := storage.NewStore(rootDir, true)
	rootShared := realm.NewSharedRealms()
	rootIndex := storage.NewIndex(rootShared)
	rootLimiter := auth.NewRateLimiter(10000, 1000000)
	rootAuth := auth.NewAuthStore(168 * time.Hour)
	rootHub := serving.NewHub(rootStore, rootIndex, rootLimiter, rootAuth, "", rootShared)
	rootSrv := httptest.NewServer(rootHub.Router())

	proxyDir := t.TempDir()
	proxyStore, _ := storage.NewStore(proxyDir, true)
	proxyShared := realm.NewSharedRealms()
	proxyIndex := storage.NewIndex(proxyShared)
	proxyLimiter := auth.NewRateLimiter(10000, 1000000)
	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	up := upstream.NewClient(rootSrv.URL)
	pending := upstream.NewSyncPending(filepath.Join(proxyDir, "sync_pending"), up, proxyStore, proxyIndex)
	proxy := serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", up, pending, proxyShared)

	dns := func(host string) ([]string, error) { return nil, fmt.Errorf("no such host") }
	proxy.Vhost = vhost.NewResolver(baseDomain, 5*time.Minute, dns)
	proxy.VhostMode = serving.VhostModeIsolate

	proxySrv := httptest.NewServer(proxy.Router())
	return proxySrv, func() {
		proxySrv.Close()
		rootSrv.Close()
		rootLimiter.Stop()
		proxyLimiter.Stop()
		rootAuth.Stop()
		proxyAuth.Stop()
	}
}

// GET /{ref}/page must honor per-app origin isolation exactly like GET /{ref}:
// a PAGE requested on the wrong (base) host redirects to its canonical page host
// — and the redirect must KEEP the /page suffix so the target pins HTML (no
// Accept re-negotiation on the isolated origin).
func TestProjectionPageVhostRedirect_Hub(t *testing.T) {
	pageRef := fixtureRef(t, "page.json")

	ts, hub, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "page.json")
	hub.Vhost.AddPage(pageRef)

	// Wrong host (base domain) → 302 to the canonical page host, /page preserved.
	resp := doGetWithHost(t, ts, "/"+pageRef+"/page", "example.com", "text/html")
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("/page on base host: expected 302, got %d: %s", resp.StatusCode, body)
	}
	hash := vhost.PageHash(pageRef)
	want := fmt.Sprintf("http://%s.example.com/%s/page", hash, pageRef)
	if loc := resp.Header.Get("Location"); loc != want {
		t.Errorf("redirect Location = %q, want %q", loc, want)
	}
	resp.Body.Close()

	// The redirect must fire even without an Accept: text/html hint — /page always
	// serves HTML, so the shared-origin exposure exists regardless of Accept.
	resp = doGetWithHost(t, ts, "/"+pageRef+"/page", "example.com", "application/json")
	if resp.StatusCode != http.StatusFound {
		t.Errorf("/page on base host (json Accept): expected 302, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Correct hash subdomain → serve directly, no redirect.
	resp = doGetWithHost(t, ts, "/"+pageRef+"/page", hash+".example.com", "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/page on canonical host: expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %q", ct)
	}
	resp.Body.Close()
}

func TestProjectionPageVhostRedirect_Proxy(t *testing.T) {
	pageRef := fixtureRef(t, "page.json")

	proxySrv, cleanup := testRootAndVhostProxy(t, "example.com")
	defer cleanup()

	// PUT through the proxy (forwards to root, caches locally, registers the page).
	pageData := loadTestFixture(t, "page.json")
	if resp := doPut(t, proxySrv, pageRef, pageData); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT page via proxy: expected 201, got %d: %s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}

	resp := doGetWithHost(t, proxySrv, "/"+pageRef+"/page", "example.com", "text/html")
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy /page on base host: expected 302, got %d: %s", resp.StatusCode, body)
	}
	hash := vhost.PageHash(pageRef)
	want := fmt.Sprintf("http://%s.example.com/%s/page", hash, pageRef)
	if loc := resp.Header.Get("Location"); loc != want {
		t.Errorf("proxy redirect Location = %q, want %q", loc, want)
	}
	resp.Body.Close()
}
