package main

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
)

// --- helpers for the explicit representation paths (/json, /raw, /page) ---

// makeViewerPage builds a signed PAGE usable as the hub's default viewer.
// Its HTML carries the "DEFAULT-VIEWER" marker so tests can assert it rendered.
func makeViewerPage(t *testing.T) (ref string, data []byte) {
	t.Helper()
	priv, pubkey := testKeypair(t)
	id := "99999999-9999-4999-8999-999999999999"
	item := map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-03-01T00:00:00Z",
		"in":         []string{"dataverse001"},
		"type":       "PAGE",
		"content": map[string]any{
			"title": "Default Viewer",
			"html":  "<!DOCTYPE html><html><head><title>Viewer</title></head><body><div id=\"dv\">DEFAULT-VIEWER</div></body></html>",
		},
	}
	return pubkey + "." + id, buildSignedObject(t, priv, item)
}

// testHubWithViewer creates a Hub whose default viewer is a stored PAGE, so
// /page on a plain object renders the viewer. Returns the server, the viewer
// ref, and a cleanup func.
func testHubWithViewer(t *testing.T) (*httptest.Server, string, func()) {
	t.Helper()

	dir := t.TempDir()
	store, err := storage.NewStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	shared := realm.NewSharedRealms()
	index := storage.NewIndex(shared)
	limiter := auth.NewRateLimiter(1000, 100000)
	au := auth.NewAuthStore(168 * time.Hour)

	viewerRef, viewerData := makeViewerPage(t)
	hub := serving.NewHub(store, index, limiter, au, viewerRef, shared)
	ts := httptest.NewServer(hub.Router())

	if resp := doPut(t, ts, viewerRef, viewerData); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT viewer: expected 201, got %d: %s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}

	return ts, viewerRef, func() {
		ts.Close()
		limiter.Stop()
		au.Stop()
	}
}

// makePrivateBlob builds a signed private (identity-realm) text BLOB.
func makePrivateBlob(t *testing.T, priv *ecdsa.PrivateKey, pubkey, id string) (ref string, data []byte) {
	t.Helper()
	item := map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-03-02T00:00:00Z",
		"in":         []string{pubkey},
		"type":       "BLOB",
		"content": map[string]any{
			"mime_type": "text/plain",
			"text":      "secret raw bytes",
		},
	}
	return pubkey + "." + id, buildSignedObject(t, priv, item)
}

// makePrivatePage builds a signed private (identity-realm) PAGE.
func makePrivatePage(t *testing.T, priv *ecdsa.PrivateKey, pubkey, id string) (ref string, data []byte) {
	t.Helper()
	item := map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-03-02T00:00:00Z",
		"in":         []string{pubkey},
		"type":       "PAGE",
		"content": map[string]any{
			"title": "Secret",
			"html":  "<!DOCTYPE html><html><body><h1>SECRET-PAGE</h1></body></html>",
		},
	}
	return pubkey + "." + id, buildSignedObject(t, priv, item)
}

// makePageWithPageRelation builds a signed PAGE that carries its OWN html AND a
// `page` relation pointing at relTarget (so index.HasPageRelation is true even
// though the object is itself a PAGE). GET /{ref}/raw must serve the PAGE's own
// html — never the relation target — and use a non-colliding ETag suffix.
func makePageWithPageRelation(t *testing.T, relTarget string) (ref string, data []byte) {
	t.Helper()
	priv, pubkey := testKeypair(t)
	id := "77777777-8888-4999-8aaa-bbbbbbbbbbbb"
	item := map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-03-05T00:00:00Z",
		"in":         []string{"dataverse001"},
		"type":       "PAGE",
		"content": map[string]any{
			"title": "Own Page",
			"html":  "<!DOCTYPE html><html><body><h1>OWN-PAGE-HTML</h1></body></html>",
		},
		"relations": map[string]any{
			"page": []map[string]any{{"ref": relTarget}},
		},
	}
	return pubkey + "." + id, buildSignedObject(t, priv, item)
}

// makeEmptyHTMLPage builds a signed PAGE whose content.html is empty — a
// degenerate object with no raw representation (GET /{ref}/raw -> 409 NO_RAW).
func makeEmptyHTMLPage(t *testing.T) (ref string, data []byte) {
	t.Helper()
	priv, pubkey := testKeypair(t)
	id := "12121212-3434-4565-8787-909090909090"
	item := map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-03-06T00:00:00Z",
		"in":         []string{"dataverse001"},
		"type":       "PAGE",
		"content": map[string]any{
			"title": "Empty",
			"html":  "",
		},
	}
	return pubkey + "." + id, buildSignedObject(t, priv, item)
}

// makeHTMLBlob builds a signed BLOB whose content.mime_type is text/html. It is
// still a BLOB, so GET /{ref}/raw serves its bytes verbatim on the shared origin
// — the /raw origin-isolation redirect keys on the OBJECT TYPE (PAGE), not on
// content type (HTML-mime BLOBs are out of scope, tracked in issue #14).
func makeHTMLBlob(t *testing.T) (ref string, data []byte) {
	t.Helper()
	priv, pubkey := testKeypair(t)
	id := "abababab-cdcd-4e4e-8f8f-010101010101"
	item := map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-03-07T00:00:00Z",
		"in":         []string{"dataverse001"},
		"type":       "BLOB",
		"content": map[string]any{
			"mime_type": "text/html",
			"text":      "<html><body>RAW-HTML-BLOB</body></html>",
		},
	}
	return pubkey + "." + id, buildSignedObject(t, priv, item)
}

// putOK PUTs a signed object and fails the test unless the hub returns 201.
func putOK(t *testing.T, ts *httptest.Server, ref string, data []byte) {
	t.Helper()
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT %s: expected 201, got %d: %s", ref, resp.StatusCode, body)
	}
	resp.Body.Close()
}

// testHubBackdoor creates a Hub and returns direct handles to its store and
// index, so tests can simulate store/index divergence (index miss, stale meta
// mid-revision-update) that cannot be produced through the HTTP API.
func testHubBackdoor(t *testing.T) (*httptest.Server, *storage.Store, *storage.Index, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.NewStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	shared := realm.NewSharedRealms()
	index := storage.NewIndex(shared)
	limiter := auth.NewRateLimiter(1000, 100000)
	au := auth.NewAuthStore(168 * time.Hour)
	hub := serving.NewHub(store, index, limiter, au, "", shared)
	ts := httptest.NewServer(hub.Router())
	return ts, store, index, func() {
		ts.Close()
		limiter.Stop()
		au.Stop()
	}
}

// fixtureRef returns the ref of a stored fixture.
func fixtureRef(t *testing.T, name string) string {
	t.Helper()
	data := loadTestFixture(t, name)
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
	json.Unmarshal(env.Item, &item)
	return item.Ref()
}

// etagFor returns the ETag GET /{ref} sets for the given Accept header — the
// baseline the explicit representation paths must match.
func etagFor(t *testing.T, ts *httptest.Server, path, accept string) string {
	t.Helper()
	resp := doGetWithAccept(t, ts, path, accept)
	resp.Body.Close()
	return resp.Header.Get("ETag")
}

// get304 asserts a conditional GET with the given If-None-Match returns 304.
func assert304(t *testing.T, ts *httptest.Server, path, etag string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("If-None-Match", etag)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("GET %s If-None-Match %s: expected 304, got %d", path, etag, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("GET %s: expected empty 304 body, got %d bytes", path, len(body))
	}
}

// --- GET /{ref}/json ---

func TestProjectionJSON(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "root.json")
	putFixture(t, ts, "blob.json")
	rootRef := fixtureRef(t, "root.json")

	// A separate hub for the text BLOB (shares an id with page.json).
	ts2, cleanup2 := testHub(t)
	defer cleanup2()
	putFixture(t, ts2, "text_blob.json")
	textRef := fixtureRef(t, "text_blob.json")

	// A separate hub for the PAGE.
	ts3, cleanup3 := testHub(t)
	defer cleanup3()
	putFixture(t, ts3, "page.json")
	pageRef := fixtureRef(t, "page.json")

	cases := []struct {
		name   string
		ts     *httptest.Server
		ref    string
		accept string // Accept sent to json — must be ignored
	}{
		{"json object", ts, rootRef, "text/html"},
		{"binary blob", ts, blobRef, "image/png"},
		{"text blob", ts2, textRef, "*/*"},
		{"page", ts3, pageRef, "text/html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doGetWithAccept(t, tc.ts, "/"+tc.ref+"/json", tc.accept)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("expected application/json, got %q", ct)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// Always the signed envelope, regardless of Accept.
			var env object.Envelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("/json body is not a valid envelope: %v (%s)", err, body)
			}
			var item object.Item
			if err := json.Unmarshal(env.Item, &item); err != nil {
				t.Fatalf("/json envelope has no parseable item: %v", err)
			}
			if item.Ref() != tc.ref {
				t.Errorf("/json wrong ref: got %s want %s", item.Ref(), tc.ref)
			}

			// ETag must match GET /{ref} with Accept: application/json.
			want := etagFor(t, tc.ts, "/"+tc.ref, "application/json")
			if got := resp.Header.Get("ETag"); got != want {
				t.Errorf("json ETag %q != GET /{ref} JSON ETag %q", got, want)
			}
			assert304(t, tc.ts, "/"+tc.ref+"/json", want)
		})
	}
}

func TestProjectionJSONPrivate(t *testing.T) {
	ts, authStore, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "aaaa1111-1111-4111-8111-111111111111"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")
	if resp := doPut(t, ts, ref, data); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT private: expected 201, got %d: %s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}
	_ = authStore

	// Unauthenticated → 404 (don't leak existence).
	resp := doGet(t, ts, "/"+ref+"/json")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unauth json: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Authenticated owner → 200 envelope.
	token := authenticateAs(t, ts, priv, pubkey)
	resp = doGetWithToken(t, ts, "/"+ref+"/json", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth json: expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var env object.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("auth /json not an envelope: %v", err)
	}
}

// --- GET /{ref}/raw ---

func TestProjectionRawBinaryBlob(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "blob.json")

	// Accept: text/html must be ignored — /raw always serves the bytes.
	resp := doGetWithAccept(t, ts, "/"+blobRef+"/raw", "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("expected image/png, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(body) < 4 || string(body[:4]) != "\x89PNG" {
		t.Errorf("expected raw PNG bytes, got %d bytes", len(body))
	}

	want := etagFor(t, ts, "/"+blobRef, "image/png")
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("raw ETag %q != GET /{ref} blob ETag %q", got, want)
	}
	assert304(t, ts, "/"+blobRef+"/raw", want)
}

func TestProjectionRawTextBlob(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "text_blob.json")
	textRef := fixtureRef(t, "text_blob.json")

	resp := doGetWithAccept(t, ts, "/"+textRef+"/raw", "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/javascript" {
		t.Errorf("expected application/javascript, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `console.log("hello world");` {
		t.Errorf("unexpected raw text body: %q", body)
	}
}

// TestProjectionRawNoRawRepresentation covers objects with no native raw
// representation: neither a BLOB (bytes) nor a PAGE (own html) -> 409 NO_RAW.
// A PAGE is NOT included here — /raw now serves a PAGE's own html (see
// TestProjectionRawPageInline).
func TestProjectionRawNoRawRepresentation(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "root.json")
	rootRef := fixtureRef(t, "root.json")
	putFixture(t, ts, "identity.json")
	identityRef := fixtureRef(t, "identity.json")

	for _, ref := range []string{rootRef, identityRef} {
		resp := doGet(t, ts, "/"+ref+"/raw")
		assertProblem(t, resp, http.StatusConflict, "NO_RAW")
	}
}

// TestProjectionRawPageInline: a PAGE served via /raw returns its own
// content.html as text/html, regardless of Accept. With no page relation, GET
// /{ref}'s HTML representation is that same html, so the ETags match.
func TestProjectionRawPageInline(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "page.json")
	pageRef := fixtureRef(t, "page.json")

	// Accept: application/json must be ignored — /raw serves the PAGE's own html.
	resp := doGetWithAccept(t, ts, "/"+pageRef+"/raw", "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("<h1>Hello Dataverse</h1>")) {
		t.Errorf("expected inline PAGE html, got: %s", body)
	}

	// ETag parity: /raw shares GET /{ref}'s HTML ETag for an inline PAGE.
	want := etagFor(t, ts, "/"+pageRef, "text/html")
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("raw ETag %q != GET /{ref} HTML ETag %q", got, want)
	}
	assert304(t, ts, "/"+pageRef+"/raw", want)
}

// TestProjectionRawPageWithPageRelation: a PAGE that ALSO carries a page relation
// still serves its OWN html via /raw (never the relation viewer — that stays
// /page's job), and uses a distinct ETag suffix so it never collides with the
// viewer ETag GET /{ref} may serve for text/html.
func TestProjectionRawPageWithPageRelation(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "page.json") // the page-relation target
	ref, data := makePageWithPageRelation(t, pageFixtureRef)
	putOK(t, ts, ref, data)

	resp := doGetWithAccept(t, ts, "/"+ref+"/raw", "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("OWN-PAGE-HTML")) {
		t.Errorf("/raw must serve the PAGE's own html, got: %s", body)
	}
	if bytes.Contains(body, []byte("Hello Dataverse")) {
		t.Errorf("/raw must NOT follow the page relation to the target html")
	}

	rawETag := resp.Header.Get("ETag")
	if !strings.HasSuffix(rawETag, `-raw"`) {
		t.Errorf("/raw on page-relation PAGE: expected -raw ETag suffix, got %q", rawETag)
	}
	if viewerETag := etagFor(t, ts, "/"+ref, "text/html"); rawETag == viewerETag {
		t.Errorf("/raw ETag %q must not collide with GET /{ref} HTML ETag %q", rawETag, viewerETag)
	}
	assert304(t, ts, "/"+ref+"/raw", rawETag)
}

// TestProjectionRawPageEmptyHTML: a degenerate PAGE with empty content.html has
// no raw representation -> 409 NO_RAW.
func TestProjectionRawPageEmptyHTML(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	ref, data := makeEmptyHTMLPage(t)
	putOK(t, ts, ref, data)

	resp := doGet(t, ts, "/"+ref+"/raw")
	assertProblem(t, resp, http.StatusConflict, "NO_RAW")
}

// TestProjectionRawPagePrivate: a private PAGE via /raw is 404 unauthenticated
// (existence not leaked) and serves its own html to the authenticated owner.
func TestProjectionRawPagePrivate(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "dddd7777-7777-4777-8777-777777777777"
	ref, data := makePrivatePage(t, priv, pubkey, id)
	putOK(t, ts, ref, data)

	resp := doGet(t, ts, "/"+ref+"/raw")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unauth raw page: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	token := authenticateAs(t, ts, priv, pubkey)
	resp = doGetWithToken(t, ts, "/"+ref+"/raw", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth raw page: expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("SECRET-PAGE")) {
		t.Errorf("auth raw page: expected own html, got %s", body)
	}
}

// TestProjectionRawIndexMissPageFailsClosed: /raw's serve decision must never
// outrun the meta the security checks used. On an index miss (found=false),
// authorizeProjection cannot evaluate realm auth (the realms live in the index)
// and rawVhostRedirect cannot fire — so a PAGE that exists only on disk must
// NEVER leave /raw as HTML, not even to its owner. Fail closed: 409 NO_RAW.
// The PAGE here is private (identity-realm) and the request unauthenticated,
// making the leak this guards against concrete.
func TestProjectionRawIndexMissPageFailsClosed(t *testing.T) {
	ts, store, _, cleanup := testHubBackdoor(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	ref, data := makePrivatePage(t, priv, pubkey, "eeee8888-8888-4888-8888-888888888888")
	// Write to disk WITHOUT updating the index — the index-miss race.
	if err := store.Write(ref, data, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Even an HTML-accepting client must not receive the html.
	resp := doGetWithAccept(t, ts, "/"+ref+"/raw", "text/html")
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("index-miss PAGE served as HTML on /raw (Content-Type %q): %s", ct, body)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A JSON-accepting client gets the structured NO_RAW problem.
	assertProblem(t, doGet(t, ts, "/"+ref+"/raw"), http.StatusConflict, "NO_RAW")
}

// TestProjectionRawStaleBlobMetaPageOnDisk: stale index mid BLOB→PAGE revision
// update. The index still says BLOB — so rawETagSuffix chose "-blob" and
// rawVhostRedirect did NOT fire — while the store already holds the new PAGE
// revision. Serving that PAGE's html would put author-controlled HTML on the
// shared origin with the redirect bypassed; /raw must fail closed: 409 NO_RAW,
// never text/html.
func TestProjectionRawStaleBlobMetaPageOnDisk(t *testing.T) {
	ts, store, index, cleanup := testHubBackdoor(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "ffff9999-aaaa-4bbb-8ccc-dddddddddddd"
	ref := pubkey + "." + id

	// Index: the old BLOB revision (what the auth/redirect checks will see).
	blobData := buildSignedObject(t, priv, map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-03-08T00:00:00Z",
		"in":         []string{"dataverse001"},
		"type":       "BLOB",
		"revision":   1,
		"content": map[string]any{
			"mime_type": "text/plain",
			"text":      "old blob revision",
		},
	})
	blobEnv, blobItem, err := object.ParseEnvelope(blobData)
	if err != nil {
		t.Fatal(err)
	}
	index.Update(ref, blobItem, time.Now(), object.ResolveIn(blobEnv, blobItem))

	// Disk: the new PAGE revision (index not yet updated).
	pageData := buildSignedObject(t, priv, map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-03-08T00:01:00Z",
		"in":         []string{"dataverse001"},
		"type":       "PAGE",
		"revision":   2,
		"content": map[string]any{
			"title": "Race Page",
			"html":  "<!DOCTYPE html><html><body><h1>STALE-RACE-PAGE</h1></body></html>",
		},
	})
	if err := store.Write(ref, pageData, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Even an HTML-accepting client must not receive the html.
	resp := doGetWithAccept(t, ts, "/"+ref+"/raw", "text/html")
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("stale-BLOB-meta PAGE served as HTML on /raw (Content-Type %q): %s", ct, body)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A JSON-accepting client gets the structured NO_RAW problem.
	assertProblem(t, doGet(t, ts, "/"+ref+"/raw"), http.StatusConflict, "NO_RAW")
}

func TestProjectionRawPrivate(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "bbbb2222-2222-4222-8222-222222222222"
	ref, data := makePrivateBlob(t, priv, pubkey, id)
	if resp := doPut(t, ts, ref, data); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT private blob: expected 201, got %d: %s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}

	resp := doGet(t, ts, "/"+ref+"/raw")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unauth raw: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	token := authenticateAs(t, ts, priv, pubkey)
	resp = doGetWithToken(t, ts, "/"+ref+"/raw", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth raw: expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "secret raw bytes" {
		t.Errorf("auth raw: unexpected body %q", body)
	}
}

// --- GET /{ref}/page ---

func TestProjectionPageInlinePAGE(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "page.json")
	pageRef := fixtureRef(t, "page.json")

	// Accept: application/json must be ignored — /page always renders HTML.
	resp := doGetWithAccept(t, ts, "/"+pageRef+"/page", "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("<h1>Hello Dataverse</h1>")) {
		t.Errorf("expected inline PAGE HTML, got: %s", body)
	}

	want := etagFor(t, ts, "/"+pageRef, "text/html")
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("page ETag %q != GET /{ref} HTML ETag %q", got, want)
	}
	assert304(t, ts, "/"+pageRef+"/page", want)
}

func TestProjectionPageViaRelation(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "page.json")
	putFixture(t, ts, "app_with_page.json")
	appRef := fixtureRef(t, "app_with_page.json")

	resp := doGet(t, ts, "/"+appRef+"/page")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("<h1>Hello Dataverse</h1>")) {
		t.Errorf("expected page-relation HTML, got: %s", body)
	}

	want := etagFor(t, ts, "/"+appRef, "text/html")
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("page ETag %q != GET /{ref} HTML ETag %q", got, want)
	}
}

func TestProjectionPageDefaultViewer(t *testing.T) {
	ts, _, cleanup := testHubWithViewer(t)
	defer cleanup()

	// A plain JSON object (no page relation) renders through the default viewer.
	putFixture(t, ts, "root.json")
	rootRef := fixtureRef(t, "root.json")

	resp := doGet(t, ts, "/"+rootRef+"/page")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("DEFAULT-VIEWER")) {
		t.Errorf("expected default-viewer HTML, got: %s", body)
	}

	want := etagFor(t, ts, "/"+rootRef, "text/html")
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("page ETag %q != GET /{ref} HTML ETag %q", got, want)
	}
	assert304(t, ts, "/"+rootRef+"/page", want)
}

func TestProjectionPageNoRepresentation(t *testing.T) {
	// No default viewer configured: a plain JSON object has no HTML → 409.
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "root.json")
	rootRef := fixtureRef(t, "root.json")

	resp := doGet(t, ts, "/"+rootRef+"/page")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("page with no viewer: expected 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestProjectionPagePrivate(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "cccc3333-3333-4333-8333-333333333333"
	ref, data := makePrivatePage(t, priv, pubkey, id)
	if resp := doPut(t, ts, ref, data); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT private page: expected 201, got %d: %s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}

	resp := doGet(t, ts, "/"+ref+"/page")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unauth page: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	token := authenticateAs(t, ts, priv, pubkey)
	resp = doGetWithToken(t, ts, "/"+ref+"/page", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth page: expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("SECRET-PAGE")) {
		t.Errorf("auth page: expected HTML, got %s", body)
	}
}

// TestProjectionBlobWithPageRelationPinsEachRepresentation is the crux of the
// de-magicking: a BLOB carrying a page relation is negotiated differently by
// GET /{ref} depending on Accept (browsers get the viewer, curl gets raw bytes).
// The explicit paths must each pin ONE representation regardless of that.
func TestProjectionBlobWithPageRelationPinsEachRepresentation(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "page.json")
	ref, data := makeBlobWithPageRelation(t, pageFixtureRef)
	if resp := doPut(t, ts, ref, data); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT blob: expected 201, got %d: %s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}

	browser := "text/html,application/xhtml+xml,image/avif,*/*;q=0.8"

	// /raw — always the bytes, even for a browser Accept.
	resp := doGetWithAccept(t, ts, "/"+ref+"/raw", browser)
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("/raw: expected image/png, got %q", ct)
	}
	rawBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(rawBody) < 4 || string(rawBody[:4]) != "\x89PNG" {
		t.Errorf("/raw: expected PNG bytes")
	}
	if et := resp.Header.Get("ETag"); !strings.HasSuffix(et, `-blob"`) {
		t.Errorf("/raw: expected -blob ETag, got %q", et)
	}

	// /page — always the viewer, even for Accept: application/json.
	resp = doGetWithAccept(t, ts, "/"+ref+"/page", "application/json")
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("/page: expected text/html, got %q", ct)
	}
	pageBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(pageBody, []byte("<h1>Hello Dataverse</h1>")) {
		t.Errorf("/page: expected viewer HTML, got %s", pageBody)
	}
	if et := resp.Header.Get("ETag"); !strings.Contains(et, "-html") {
		t.Errorf("/page: expected -html ETag, got %q", et)
	}

	// /json — always the envelope, plain "<rev>" ETag.
	resp = doGetWithAccept(t, ts, "/"+ref+"/json", browser)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("/json: expected application/json, got %q", ct)
	}
	jsonBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var env object.Envelope
	if err := json.Unmarshal(jsonBody, &env); err != nil {
		t.Errorf("/json: not an envelope: %v", err)
	}
	if et := resp.Header.Get("ETag"); strings.Contains(et, "-blob") || strings.Contains(et, "-html") {
		t.Errorf("/json: expected plain ETag, got %q", et)
	}
}

// TestProjectionPrivateWrongIdentity hardens the realm-auth gate: a caller
// authenticated as a DIFFERENT identity (not a realm member) must get 404 on
// every projection path — same as an unauthenticated caller, never leaking the
// object to a non-owner who merely holds a valid token.
func TestProjectionPrivateWrongIdentity(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	owner, ownerPub := testKeypair(t)
	intruder, intruderPub := testKeypair(t)

	noteRef := ownerPub + ".aaaa9999-9999-4999-8999-999999999999"
	note := signedObject(t, owner, ownerPub, "aaaa9999-9999-4999-8999-999999999999", []string{ownerPub}, "NOTE")
	blobRefP, blobData := makePrivateBlob(t, owner, ownerPub, "bbbb9999-9999-4999-8999-999999999999")
	pageRefP, pageData := makePrivatePage(t, owner, ownerPub, "cccc9999-9999-4999-8999-999999999999")
	for _, pv := range []struct {
		ref  string
		data []byte
	}{{noteRef, note}, {blobRefP, blobData}, {pageRefP, pageData}} {
		if resp := doPut(t, ts, pv.ref, pv.data); resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("PUT %s: expected 201, got %d: %s", pv.ref, resp.StatusCode, body)
		} else {
			resp.Body.Close()
		}
	}

	// Token for the intruder — valid auth, but not a member of the owner's realm.
	token := authenticateAs(t, ts, intruder, intruderPub)

	paths := []string{noteRef + "/json", blobRefP + "/raw", pageRefP + "/page"}
	for _, p := range paths {
		resp := doGetWithToken(t, ts, "/"+p, token)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("wrong-identity GET /%s: expected 404, got %d", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// --- shared: invalid / missing refs ---

func TestProjectionInvalidAndMissingRefs(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	missing := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.00000000-0000-4000-8000-000000000001"
	for _, suffix := range []string{"json", "raw", "page"} {
		// Garbage ref (scanner shape) → 404.
		resp := doGet(t, ts, "/not-a-ref/"+suffix)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("invalid ref /%s: expected 404, got %d", suffix, resp.StatusCode)
		}
		resp.Body.Close()

		// Well-formed but absent ref → 404.
		resp = doGet(t, ts, "/"+missing+"/"+suffix)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("missing ref /%s: expected 404, got %d", suffix, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
