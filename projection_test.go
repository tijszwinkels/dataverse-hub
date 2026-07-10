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

func TestProjectionRawNotABlob(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "root.json")
	rootRef := fixtureRef(t, "root.json")
	putFixture(t, ts, "page.json")
	pageRef := fixtureRef(t, "page.json")

	for _, ref := range []string{rootRef, pageRef} {
		resp := doGet(t, ts, "/"+ref+"/raw")
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("raw on non-BLOB %s: expected 409, got %d", ref, resp.StatusCode)
		}
		resp.Body.Close()
	}
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
