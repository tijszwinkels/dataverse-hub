package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/vhost"
)

// testHubWithVhost creates a Hub with vhosting enabled on baseDomain.
func testHubWithVhost(t *testing.T, baseDomain string, dnsRecords map[string][]string) (*httptest.Server, *serving.Hub, func()) {
	t.Helper()

	dir := t.TempDir()
	store, err := storage.NewStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}

	shared := realm.NewSharedRealms()
	index := storage.NewIndex(shared)
	limiter := auth.NewRateLimiter(1000, 100000)
	authStore := auth.NewAuthStore(168 * time.Hour)

	dns := func(host string) ([]string, error) {
		if r, ok := dnsRecords[host]; ok {
			return r, nil
		}
		return nil, fmt.Errorf("no such host")
	}
	resolver := vhost.NewResolver(baseDomain, 5*time.Minute, dns)

	hub := serving.NewHub(store, index, limiter, authStore, "", shared)
	hub.Vhost = resolver

	ts := httptest.NewServer(hub.Router())
	return ts, hub, func() {
		ts.Close()
		limiter.Stop()
		authStore.Stop()
	}
}

func doGetWithHost(t *testing.T, ts *httptest.Server, path, host, accept string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Host = host
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	// Don't follow redirects
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestVhost_BareDomain_LegacyBehavior(t *testing.T) {
	ts, _, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	// PUT root object so handleRootLegacy has something to redirect to
	data := loadTestFixture(t, "root.json")
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
	json.Unmarshal(env.Item, &item)
	resp := doPut(t, ts, item.Ref(), data)
	resp.Body.Close()

	// GET / on bare domain should redirect to ROOT object (legacy behavior)
	resp = doGetWithHost(t, ts, "/", "example.com", "")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("bare domain /: expected 302, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("expected Location header")
	}
	resp.Body.Close()
}

func TestVhost_HashSubdomain_ServesPage(t *testing.T) {
	ts, hub, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	// PUT the page fixture
	putFixture(t, ts, "page.json")
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	// Register the page in the resolver
	hub.Vhost.AddPage(pageRef)
	hash := vhost.PageHash(pageRef)

	// GET / on hash subdomain should serve the PAGE HTML
	resp := doGetWithHost(t, ts, "/", hash+".example.com", "text/html")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("hash subdomain /: expected 200, got %d: %s", resp.StatusCode, body)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("<h1>Hello Dataverse</h1>")) {
		t.Errorf("expected page HTML content, got: %s", body)
	}
}

func TestVhost_TXTSubdomain_ServesPage(t *testing.T) {
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	ts, _, cleanup := testHubWithVhost(t, "example.com", map[string][]string{
		"_dv.social.example.com": {"dv1-page=" + pageRef},
	})
	defer cleanup()

	putFixture(t, ts, "page.json")

	// GET / on TXT-mapped subdomain should serve the PAGE HTML
	resp := doGetWithHost(t, ts, "/", "social.example.com", "text/html")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("TXT subdomain /: expected 200, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("<h1>Hello Dataverse</h1>")) {
		t.Errorf("expected page HTML content")
	}
}

func TestVhost_AuthSubdomain_NoSpecialCase(t *testing.T) {
	// "auth" is no longer a special-cased subdomain — treated like any unknown subdomain
	ts, _, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	resp := doGetWithHost(t, ts, "/", "auth.example.com", "text/html")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("auth subdomain /: expected 404 (no longer special), got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVhost_PageRedirect_WrongSubdomain(t *testing.T) {
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	ts, hub, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "page.json")
	hub.Vhost.AddPage(pageRef)

	// Request PAGE from bare domain with Accept: text/html → should redirect
	resp := doGetWithHost(t, ts, "/"+pageRef, "example.com", "text/html")
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("page on wrong host: expected 302, got %d: %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	expectedHash := vhost.PageHash(pageRef)
	expectedLoc := fmt.Sprintf("http://%s.example.com/%s", expectedHash, pageRef)
	if loc != expectedLoc {
		t.Errorf("redirect Location = %q, want %q", loc, expectedLoc)
	}
	resp.Body.Close()
}

func TestVhost_PageRedirect_PreservesPort(t *testing.T) {
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	ts, hub, cleanup := testHubWithVhost(t, "localhost", nil)
	defer cleanup()

	putFixture(t, ts, "page.json")
	hub.Vhost.AddPage(pageRef)

	// Request PAGE from localhost:5678 → redirect should preserve port
	resp := doGetWithHost(t, ts, "/"+pageRef, "localhost:5678", "text/html")
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 302, got %d: %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	expectedHash := vhost.PageHash(pageRef)
	expectedLoc := fmt.Sprintf("http://%s.localhost:5678/%s", expectedHash, pageRef)
	if loc != expectedLoc {
		t.Errorf("redirect Location = %q, want %q", loc, expectedLoc)
	}
	resp.Body.Close()
}

func TestVhost_PageRedirect_CorrectSubdomain_NoRedirect(t *testing.T) {
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"
	hash := vhost.PageHash(pageRef)

	ts, hub, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "page.json")
	hub.Vhost.AddPage(pageRef)

	// Request PAGE from correct hash subdomain → should serve directly
	resp := doGetWithHost(t, ts, "/"+pageRef, hash+".example.com", "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page on correct host: expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %q", ct)
	}
	resp.Body.Close()
}

func TestVhost_NonPageObject_NoRedirect(t *testing.T) {
	ts, _, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "root.json")
	rootRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.00000000-0000-0000-0000-000000000000"

	// Non-PAGE object with Accept: application/json on any subdomain → no redirect
	resp := doGetWithHost(t, ts, "/"+rootRef, "random.example.com", "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-page on random host: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVhost_JSONRequest_NoRedirect(t *testing.T) {
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	ts, hub, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "page.json")
	hub.Vhost.AddPage(pageRef)

	// JSON request for a PAGE on wrong host → no redirect, serve JSON
	resp := doGetWithHost(t, ts, "/"+pageRef, "example.com", "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page JSON on wrong host: expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
	resp.Body.Close()
}

func TestVhost_AppWithPageRelation_Redirect(t *testing.T) {
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"
	appRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.bbbbbbbb-cccc-4ddd-eeee-ffffffffffff"

	ts, hub, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "page.json")
	putFixture(t, ts, "app_with_page.json")
	hub.Vhost.AddPage(pageRef)

	// APP with page relation on bare domain → redirect to PAGE's subdomain
	resp := doGetWithHost(t, ts, "/"+appRef, "example.com", "text/html")
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("app on wrong host: expected 302, got %d: %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	expectedHash := vhost.PageHash(pageRef)
	expectedLoc := fmt.Sprintf("http://%s.example.com/%s", expectedHash, appRef)
	if loc != expectedLoc {
		t.Errorf("redirect Location = %q, want %q", loc, expectedLoc)
	}
	resp.Body.Close()
}

func TestVhost_UnknownSubdomain_RootReturns404(t *testing.T) {
	ts, _, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	resp := doGetWithHost(t, ts, "/", "unknown.example.com", "text/html")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown subdomain /: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVhost_APIWorksOnAnySubdomain(t *testing.T) {
	ts, _, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "root.json")

	// Search should work on any subdomain
	resp := doGetWithHost(t, ts, "/search", "random.example.com", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search on random subdomain: expected 200, got %d", resp.StatusCode)
	}
	var list object.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 1 {
		t.Errorf("expected 1 item, got %d", len(list.Items))
	}
}

func TestVhost_PutUpdatesHashMap(t *testing.T) {
	ts, hub, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "page.json")
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	// After PUT, the hash map should be updated automatically
	hash := vhost.PageHash(pageRef)
	resolved := hub.Vhost.Resolve(hash + ".example.com")
	if resolved != pageRef {
		t.Errorf("after PUT, resolver should map hash to page ref, got %q", resolved)
	}
}
