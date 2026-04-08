package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
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

// testHubWithVhostModeAndShared creates a Hub with vhosting and shared realms.
func testHubWithVhostModeAndShared(t *testing.T, baseDomain, mode string, dnsRecords map[string][]string, sharedConfig map[string][]string) (*httptest.Server, *serving.Hub, func()) {
	t.Helper()

	dir := t.TempDir()
	store, err := storage.NewStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}

	shared := realm.NewSharedRealms()
	if sharedConfig != nil {
		shared.Load(sharedConfig)
	}
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
	hub.VhostMode = mode

	ts := httptest.NewServer(hub.Router())
	return ts, hub, func() {
		ts.Close()
		limiter.Stop()
		authStore.Stop()
	}
}

// testHubWithVhostMode creates a Hub with vhosting enabled on baseDomain.
func testHubWithVhostMode(t *testing.T, baseDomain, mode string, dnsRecords map[string][]string) (*httptest.Server, *serving.Hub, func()) {
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
	hub.VhostMode = mode

	ts := httptest.NewServer(hub.Router())
	return ts, hub, func() {
		ts.Close()
		limiter.Stop()
		authStore.Stop()
	}
}

// testHubWithVhost creates a Hub with isolate-mode vhosting enabled on baseDomain.
func testHubWithVhost(t *testing.T, baseDomain string, dnsRecords map[string][]string) (*httptest.Server, *serving.Hub, func()) {
	t.Helper()
	return testHubWithVhostMode(t, baseDomain, serving.VhostModeIsolate, dnsRecords)
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

func doGetWithHostAndCookie(t *testing.T, ts *httptest.Server, path, host, accept string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Host = host
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func sharedRealmPageObject(t *testing.T, priv *ecdsa.PrivateKey, pubkey, id, realmName string) []byte {
	t.Helper()

	item := map[string]any{
		"in":         []string{realmName},
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-03-18T12:00:00Z",
		"updated_at": "2026-03-18T12:00:00Z",
		"revision":   1,
		"type":       "PAGE",
		"content": map[string]string{
			"html": "<!DOCTYPE html><html><body><h1>Shared Realm Page</h1></body></html>",
		},
	}
	itemJSON, err := object.CanonicalJSON(mustMarshal(t, item))
	if err != nil {
		t.Fatal(err)
	}

	hash := sha256.Sum256(itemJSON)
	r, s, err := ecdsa.Sign(rand.Reader, priv, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	der, err := asn1.Marshal(struct{ R, S *big.Int }{r, s})
	if err != nil {
		t.Fatal(err)
	}

	env := map[string]any{
		"is":        "instructionGraph001",
		"signature": base64.StdEncoding.EncodeToString(der),
		"item":      json.RawMessage(itemJSON),
	}
	result, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func privatePageObject(t *testing.T, priv *ecdsa.PrivateKey, pubkey, id string) []byte {
	t.Helper()

	item := map[string]any{
		"in":         []string{pubkey},
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-03-18T12:00:00Z",
		"updated_at": "2026-03-18T12:00:00Z",
		"revision":   1,
		"type":       "PAGE",
		"content": map[string]string{
			"html": "<!DOCTYPE html><html><body><h1>Private Page</h1></body></html>",
		},
	}
	itemJSON, err := object.CanonicalJSON(mustMarshal(t, item))
	if err != nil {
		t.Fatal(err)
	}

	hash := sha256.Sum256(itemJSON)
	r, s, err := ecdsa.Sign(rand.Reader, priv, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	der, err := asn1.Marshal(struct{ R, S *big.Int }{r, s})
	if err != nil {
		t.Fatal(err)
	}

	env := map[string]any{
		"is":        "instructionGraph001",
		"signature": base64.StdEncoding.EncodeToString(der),
		"item":      json.RawMessage(itemJSON),
	}
	result, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func authenticateHost(t *testing.T, ts *httptest.Server, host string, priv *ecdsa.PrivateKey, pubkey string) *http.Cookie {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/auth/challenge", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("challenge expected 200, got %d: %s", resp.StatusCode, body)
	}

	var challengeResp struct {
		Challenge string `json:"challenge"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&challengeResp); err != nil {
		t.Fatal(err)
	}

	body := bytes.NewBuffer(mustMarshal(t, map[string]string{
		"pubkey":    pubkey,
		"challenge": challengeResp.Challenge,
		"signature": signChallenge(t, priv, challengeResp.Challenge),
	}))
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/auth/token", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("token expected 200, got %d: %s", resp.StatusCode, respBody)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "dv_session" {
			return cookie
		}
	}
	t.Fatal("expected dv_session cookie")
	return nil
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

func TestVhost_UnknownSubdomain_WithRootObject_StillReturns404(t *testing.T) {
	ts, _, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	putFixture(t, ts, "root.json")

	resp := doGetWithHost(t, ts, "/", "unknown.example.com", "text/html")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown subdomain / with root object: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVhostRedirectMode_TXTSubdomain_RedirectsToBaseDomainPath(t *testing.T) {
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	ts, _, cleanup := testHubWithVhostMode(t, "example.com", serving.VhostModeRedirect, map[string][]string{
		"_dv.social.example.com": {"dv1-page=" + pageRef},
	})
	defer cleanup()

	putFixture(t, ts, "page.json")

	resp := doGetWithHost(t, ts, "/", "social.example.com", "text/html")
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("redirect mode TXT subdomain /: expected 302, got %d: %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	expectedLoc := fmt.Sprintf("http://example.com/%s", pageRef)
	if loc != expectedLoc {
		t.Errorf("redirect Location = %q, want %q", loc, expectedLoc)
	}
	resp.Body.Close()
}

func TestVhostRedirectMode_PageRedirect_WrongHostUsesBaseDomain(t *testing.T) {
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	ts, hub, cleanup := testHubWithVhostMode(t, "example.com", serving.VhostModeRedirect, nil)
	defer cleanup()

	putFixture(t, ts, "page.json")
	hub.Vhost.AddPage(pageRef)

	resp := doGetWithHost(t, ts, "/"+pageRef, "social.example.com", "text/html")
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("redirect mode page on pretty host: expected 302, got %d: %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	expectedLoc := fmt.Sprintf("http://example.com/%s", pageRef)
	if loc != expectedLoc {
		t.Errorf("redirect Location = %q, want %q", loc, expectedLoc)
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

func TestVhost_PrivatePageOnCanonicalHost_ShowsLoginWhenUnauthenticated(t *testing.T) {
	priv, pubkey := testKeypair(t)
	pageID := "bbbbbbbb-1111-4222-8333-cccccccccccc"
	pageRef := pubkey + "." + pageID

	ts, hub, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	resp := doPut(t, ts, pageRef, privatePageObject(t, priv, pubkey, pageID))
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT private page: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
	hub.Vhost.AddPage(pageRef)

	host := vhost.PageHash(pageRef) + ".example.com"
	resp = doGetWithHost(t, ts, "/"+pageRef, host, "text/html")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("private page unauthenticated: expected 200 login page, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("Sign In To View This Page")) {
		t.Fatalf("expected login page, got: %s", body)
	}
	if bytes.Contains(body, []byte("Private Page")) {
		t.Fatalf("unauthenticated request should not serve private page HTML: %s", body)
	}
}

func TestVhost_PrivatePageOnCanonicalHost_AfterAuthServesPage(t *testing.T) {
	priv, pubkey := testKeypair(t)
	pageID := "dddddddd-1111-4222-8333-eeeeeeeeeeee"
	pageRef := pubkey + "." + pageID

	ts, hub, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	resp := doPut(t, ts, pageRef, privatePageObject(t, priv, pubkey, pageID))
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT private page: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
	hub.Vhost.AddPage(pageRef)

	host := vhost.PageHash(pageRef) + ".example.com"
	cookie := authenticateHost(t, ts, host, priv, pubkey)

	resp = doGetWithHostAndCookie(t, ts, "/"+pageRef, host, "text/html", cookie)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("private page with auth: expected 200, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("Private Page")) {
		t.Fatalf("expected private page HTML, got: %s", body)
	}
	if bytes.Contains(body, []byte("Sign In To View This Page")) {
		t.Fatalf("authenticated request should not serve login page: %s", body)
	}
}

func TestVhost_SharedRealmPage_VhostRoot_AuthenticatedMemberServesPage(t *testing.T) {
	// Owner creates a PAGE in a shared realm; a different member authenticates
	// and should be able to access it via vhost root.
	ownerPriv, ownerPubkey := testKeypair(t)
	memberPriv, memberPubkey := testKeypair(t)

	realmName := ownerPubkey + ".TeamRealm"
	pageID := "eeeeeeee-2222-4333-9444-ffffffffffff"
	pageRef := ownerPubkey + "." + pageID

	ts, hub, cleanup := testHubWithVhostModeAndShared(t, "example.com", serving.VhostModeIsolate, nil,
		map[string][]string{
			realmName: {ownerPubkey, memberPubkey},
		})
	defer cleanup()

	// Owner PUTs a PAGE in the shared realm
	resp := doPut(t, ts, pageRef, sharedRealmPageObject(t, ownerPriv, ownerPubkey, pageID, realmName))
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT shared realm page: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
	hub.Vhost.AddPage(pageRef)

	host := vhost.PageHash(pageRef) + ".example.com"

	// Unauthenticated → should get login page
	resp = doGetWithHost(t, ts, "/", host, "text/html")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("shared realm page unauthenticated: expected 200 login page, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("Sign In To View This Page")) {
		t.Fatalf("expected login page for unauthenticated request, got: %s", body)
	}

	// Member authenticates (not the owner, but a shared-realm member)
	cookie := authenticateHost(t, ts, host, memberPriv, memberPubkey)

	// Authenticated member → should get the actual page
	resp = doGetWithHostAndCookie(t, ts, "/", host, "text/html", cookie)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("shared realm page with member auth: expected 200, got %d: %s", resp.StatusCode, body)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("Shared Realm Page")) {
		t.Fatalf("expected shared realm page HTML, got: %s", body)
	}
	if bytes.Contains(body, []byte("Sign In To View This Page")) {
		t.Fatalf("authenticated member should not see login page: %s", body)
	}
}
