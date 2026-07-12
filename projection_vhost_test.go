package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/vhost"
)

// testRootAndVhostProxy builds a root hub + an isolate-mode vhost proxy pointing
// at it. Returns the proxy server and cleanup. See testRootAndVhostProxyMode for
// the mode-parameterized variant.
func testRootAndVhostProxy(t *testing.T, baseDomain string) (*httptest.Server, func()) {
	t.Helper()
	return testRootAndVhostProxyMode(t, baseDomain, serving.VhostModeIsolate)
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

// GET /{ref}/raw on a PAGE serves author-controlled HTML, so it must honor
// per-app origin isolation exactly like /page: a PAGE on the base host redirects
// to its canonical page host with the /raw suffix preserved.
func TestProjectionRawVhostRedirect_Hub(t *testing.T) {
	pageRef := fixtureRef(t, "page.json")

	ts, hub, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "page.json")
	hub.Vhost.AddPage(pageRef)

	// Wrong host (base domain) → 302 to the canonical page host, /raw preserved.
	resp := doGetWithHost(t, ts, "/"+pageRef+"/raw", "example.com", "text/html")
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("/raw on base host: expected 302, got %d: %s", resp.StatusCode, body)
	}
	hash := vhost.PageHash(pageRef)
	want := fmt.Sprintf("http://%s.example.com/%s/raw", hash, pageRef)
	if loc := resp.Header.Get("Location"); loc != want {
		t.Errorf("redirect Location = %q, want %q", loc, want)
	}
	resp.Body.Close()

	// The redirect fires regardless of Accept — /raw on a PAGE is always HTML.
	resp = doGetWithHost(t, ts, "/"+pageRef+"/raw", "example.com", "application/json")
	if resp.StatusCode != http.StatusFound {
		t.Errorf("/raw on base host (json Accept): expected 302, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Correct hash subdomain → serve the PAGE's own html directly, no redirect.
	resp = doGetWithHost(t, ts, "/"+pageRef+"/raw", hash+".example.com", "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/raw on canonical host: expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %q", ct)
	}
	resp.Body.Close()
}

// TestProjectionRawVhostScoping hardens the /raw redirect's key: it triggers on
// the OBJECT TYPE being PAGE, never on content type or a bare page relation.
func TestProjectionRawVhostScoping(t *testing.T) {
	pageRef := fixtureRef(t, "page.json")

	ts, hub, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "page.json")
	hub.Vhost.AddPage(pageRef)

	// (a) An HTML-mime BLOB stays served byte-for-byte on the shared origin —
	// no redirect (HTML-mime BLOBs are out of scope, tracked in issue #14).
	blobRef, blobData := makeHTMLBlob(t)
	putOK(t, ts, blobRef, blobData)
	resp := doGetWithHost(t, ts, "/"+blobRef+"/raw", "example.com", "text/html")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTML BLOB /raw on base host: expected 200 (no redirect), got %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html" {
		t.Errorf("HTML BLOB /raw: expected verbatim mime text/html, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("RAW-HTML-BLOB")) {
		t.Errorf("HTML BLOB /raw: unexpected body %s", body)
	}

	// (b) A non-PAGE object carrying a page relation 409s on /raw and must NOT
	// redirect — only actual PAGEs redirect on /raw.
	putFixture(t, ts, "app_with_page.json")
	appRef := fixtureRef(t, "app_with_page.json")
	resp = doGetWithHost(t, ts, "/"+appRef+"/raw", "example.com", "text/html")
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("app-with-page /raw: expected 409 (no redirect), got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestProjectionRawVhostRedirect_Proxy(t *testing.T) {
	pageRef := fixtureRef(t, "page.json")

	proxySrv, cleanup := testRootAndVhostProxy(t, "example.com")
	defer cleanup()

	pageData := loadTestFixture(t, "page.json")
	if resp := doPut(t, proxySrv, pageRef, pageData); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT page via proxy: expected 201, got %d: %s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}

	resp := doGetWithHost(t, proxySrv, "/"+pageRef+"/raw", "example.com", "text/html")
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy /raw on base host: expected 302, got %d: %s", resp.StatusCode, body)
	}
	hash := vhost.PageHash(pageRef)
	want := fmt.Sprintf("http://%s.example.com/%s/raw", hash, pageRef)
	if loc := resp.Header.Get("Location"); loc != want {
		t.Errorf("proxy redirect Location = %q, want %q", loc, want)
	}
	resp.Body.Close()

	// Canonical host → 200 with the PAGE's own html.
	resp = doGetWithHost(t, proxySrv, "/"+pageRef+"/raw", hash+".example.com", "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy /raw on canonical host: expected 200, got %d", resp.StatusCode)
	}
	pageBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(pageBody, []byte("<h1>Hello Dataverse</h1>")) {
		t.Errorf("proxy /raw canonical: expected own html, got %s", pageBody)
	}
}
