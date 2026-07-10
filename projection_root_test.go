package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/upstream"
	"github.com/tijszwinkels/dataverse-hub/vhost"
)

// The root representation aliases (GET /json, /raw, /page) serve the
// representation of whatever GET / resolves to on this host — directly (200),
// not via a redirect, so a redirect-less agent bootstrap ("GET /json …")
// succeeds. Host resolution mirrors handleRoot; the projection pipeline
// (auth, ETag/304, vhost redirects, 409) is identical to /{ref}/<repr>.

// assertEnvelopeRef parses a JSON body as a signed envelope and checks its ref.
func assertEnvelopeRef(t *testing.T, body []byte, wantRef string) {
	t.Helper()
	var env object.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("/json body is not a valid envelope: %v (%s)", err, body)
	}
	var item object.Item
	if err := json.Unmarshal(env.Item, &item); err != nil {
		t.Fatalf("/json envelope has no parseable item: %v", err)
	}
	if item.Ref() != wantRef {
		t.Errorf("/json wrong ref: got %s want %s", item.Ref(), wantRef)
	}
}

// --- Hub: base domain (Vhost == nil) ---

func TestRootReprJSON_HubBaseDomain(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "root.json")
	rootRef := fixtureRef(t, "root.json")

	// Accept: text/html must be ignored — /json is always the envelope.
	resp := doGetWithAccept(t, ts, "/json", "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /json: expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assertEnvelopeRef(t, body, rootRef)

	// ETag parity with GET /{rootRef}/json, and If-None-Match → 304.
	want := etagFor(t, ts, "/"+rootRef+"/json", "application/json")
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("GET /json ETag %q != GET /{rootRef}/json ETag %q", got, want)
	}
	assert304(t, ts, "/json", want)
}

func TestRootReprRaw_HubBaseDomain(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "root.json")

	// The genesis/root object is neither BLOB nor PAGE → 409 NO_RAW (intended).
	resp := doGet(t, ts, "/raw")
	assertProblem(t, resp, http.StatusConflict, "NO_RAW")
}

func TestRootReprPage_HubBaseDomainDefaultViewer(t *testing.T) {
	ts, _, cleanup := testHubWithViewer(t)
	defer cleanup()

	putFixture(t, ts, "root.json")
	rootRef := fixtureRef(t, "root.json")

	// /page composes the default viewer, exactly like GET /{rootRef}/page does.
	resp := doGetWithAccept(t, ts, "/page", "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /page: expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("DEFAULT-VIEWER")) {
		t.Errorf("expected default-viewer HTML, got: %s", body)
	}

	want := etagFor(t, ts, "/"+rootRef+"/page", "text/html")
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("GET /page ETag %q != GET /{rootRef}/page ETag %q", got, want)
	}
	assert304(t, ts, "/page", want)
}

func TestRootReprPage_HubBaseDomainNoViewer(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "root.json")

	// No default viewer configured: the root object has no HTML view → 409.
	resp := doGet(t, ts, "/page")
	assertProblem(t, resp, http.StatusConflict, "NO_PAGE")
}

// --- Hub: base domain with vhosting enabled (request hits the base host) ---

func TestRootReprJSON_HubVhostBaseHost(t *testing.T) {
	ts, _, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "root.json")
	rootRef := fixtureRef(t, "root.json")

	// On the base domain, /json resolves to the ROOT ref (handleRootLegacy's target).
	resp := doGetWithHost(t, ts, "/json", "example.com", "text/html")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /json on base host: expected 200, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assertEnvelopeRef(t, body, rootRef)
}

// --- Hub: vhost page-host (isolate mode) ---

func TestRootReprJSON_HubVhostPageHost(t *testing.T) {
	pageRef := fixtureRef(t, "page.json")

	ts, hub, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "page.json")
	hub.Vhost.AddPage(pageRef)

	hash := vhost.PageHash(pageRef)

	// On the page host, GET / resolves to pageRef; /json serves its envelope.
	resp := doGetWithHost(t, ts, "/json", hash+".example.com", "text/html")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /json on page host: expected 200, got %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	gotETag := resp.Header.Get("ETag")
	resp.Body.Close()
	assertEnvelopeRef(t, body, pageRef)

	// ETag parity: /json on the page host equals GET /{pageRef}/json's JSON ETag.
	want := etagFor(t, ts, "/"+pageRef+"/json", "application/json")
	if gotETag != want {
		t.Errorf("GET /json ETag %q != GET /{pageRef}/json ETag %q", gotETag, want)
	}
}

// --- Hub: unknown host → 404 ---

func TestRootReprUnknownHost_Hub(t *testing.T) {
	ts, _, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "root.json")

	for _, suffix := range []string{"/json", "/raw", "/page"} {
		resp := doGetWithHost(t, ts, suffix, "unknown.test", "text/html")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s on unknown host: expected 404, got %d", suffix, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// --- Hub: VhostModeRedirect → 302 to base domain, suffix preserved ---

func TestRootReprRedirectMode_Hub(t *testing.T) {
	pageRef := fixtureRef(t, "page.json")

	ts, hub, cleanup := testHubWithVhostMode(t, "example.com", serving.VhostModeRedirect, nil)
	defer cleanup()

	putFixture(t, ts, "page.json")
	hub.Vhost.AddPage(pageRef)

	hash := vhost.PageHash(pageRef)
	for _, suffix := range []string{"/json", "/raw", "/page"} {
		resp := doGetWithHost(t, ts, suffix, hash+".example.com", "text/html")
		if resp.StatusCode != http.StatusFound {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("GET %s on page host (redirect mode): expected 302, got %d: %s", suffix, resp.StatusCode, body)
		}
		want := fmt.Sprintf("http://example.com/%s%s", pageRef, suffix)
		if loc := resp.Header.Get("Location"); loc != want {
			t.Errorf("GET %s redirect Location = %q, want %q", suffix, loc, want)
		}
		resp.Body.Close()
	}
}

// --- Hub: existing /{ref}/... routes and /{ref} routing must be unaffected ---

func TestRootReprRoutingUnaffected_Hub(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "root.json")
	rootRef := fixtureRef(t, "root.json")

	// /{ref} still serves the object.
	resp := doGet(t, ts, "/"+rootRef)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /{ref}: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// /{ref}/json still serves the envelope.
	resp = doGet(t, ts, "/"+rootRef+"/json")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /{ref}/json: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A non-ref single segment (scanner traffic) still 404s via /{ref}.
	resp = doGet(t, ts, "/wp-login.php")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /wp-login.php: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Proxy: base domain ---

func TestRootReprJSON_ProxyBaseDomain(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	// PUT through the proxy so the ROOT object is in the local index (root-ref
	// resolution is local, mirroring handleRootLegacy).
	putFixture(t, proxySrv, "root.json")
	rootRef := fixtureRef(t, "root.json")

	resp := doGetWithAccept(t, proxySrv, "/json", "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy GET /json: expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assertEnvelopeRef(t, body, rootRef)

	want := etagFor(t, proxySrv, "/"+rootRef+"/json", "application/json")
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("proxy GET /json ETag %q != GET /{rootRef}/json ETag %q", got, want)
	}
	assert304(t, proxySrv, "/json", want)
}

func TestRootReprRaw_ProxyBaseDomain(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	putFixture(t, proxySrv, "root.json")

	resp := doGet(t, proxySrv, "/raw")
	assertProblem(t, resp, http.StatusConflict, "NO_RAW")
}

// --- Proxy: vhost page-host (isolate mode) ---

func TestRootReprJSON_ProxyVhostPageHost(t *testing.T) {
	pageRef := fixtureRef(t, "page.json")

	proxySrv, cleanup := testRootAndVhostProxy(t, "example.com")
	defer cleanup()

	// PUT through the proxy — caches locally, registers the page host.
	pageData := loadTestFixture(t, "page.json")
	if resp := doPut(t, proxySrv, pageRef, pageData); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT page via proxy: expected 201, got %d: %s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}

	hash := vhost.PageHash(pageRef)
	resp := doGetWithHost(t, proxySrv, "/json", hash+".example.com", "text/html")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy GET /json on page host: expected 200, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assertEnvelopeRef(t, body, pageRef)
}

// --- Proxy: unknown host → 404 ---

func TestRootReprUnknownHost_Proxy(t *testing.T) {
	proxySrv, cleanup := testRootAndVhostProxy(t, "example.com")
	defer cleanup()

	resp := doGetWithHost(t, proxySrv, "/json", "unknown.test", "text/html")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("proxy GET /json on unknown host: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Proxy: VhostModeRedirect → 302 to base domain, suffix preserved ---

func TestRootReprRedirectMode_Proxy(t *testing.T) {
	pageRef := fixtureRef(t, "page.json")

	proxySrv, cleanup := testRootAndVhostProxyMode(t, "example.com", serving.VhostModeRedirect)
	defer cleanup()

	pageData := loadTestFixture(t, "page.json")
	if resp := doPut(t, proxySrv, pageRef, pageData); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT page via proxy: expected 201, got %d: %s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}

	hash := vhost.PageHash(pageRef)
	for _, suffix := range []string{"/json", "/raw", "/page"} {
		resp := doGetWithHost(t, proxySrv, suffix, hash+".example.com", "text/html")
		if resp.StatusCode != http.StatusFound {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("proxy GET %s on page host (redirect mode): expected 302, got %d: %s", suffix, resp.StatusCode, body)
		}
		want := fmt.Sprintf("http://example.com/%s%s", pageRef, suffix)
		if loc := resp.Header.Get("Location"); loc != want {
			t.Errorf("proxy GET %s redirect Location = %q, want %q", suffix, loc, want)
		}
		resp.Body.Close()
	}
}

// testRootAndVhostProxyMode builds a root hub + a vhost proxy in the given mode.
func testRootAndVhostProxyMode(t *testing.T, baseDomain, mode string) (*httptest.Server, func()) {
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
	proxy.VhostMode = mode

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
