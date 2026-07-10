package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/tijszwinkels/dataverse-hub/object"
)

// The proxy must serve the explicit representation paths just like the hub,
// fetching from upstream into the local cache first. These tests PUT objects
// directly to the *root* hub (not the proxy) so a passing GET through the proxy
// proves it synced upstream before projecting.

func TestProxyProjectionJSON(t *testing.T) {
	proxySrv, rootSrv, cleanup := testRootAndProxy(t)
	defer cleanup()

	// Store only on the root — the proxy must fetch it.
	data := loadTestFixture(t, "root.json")
	ref := fixtureRef(t, "root.json")
	if resp := doPut(t, rootSrv, ref, data); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT root: expected 201, got %d: %s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}

	// Accept: text/html must be ignored — json is always the envelope.
	resp := doGetWithAccept(t, proxySrv, "/"+ref+"/json", "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var env object.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("/json not a valid envelope: %v", err)
	}

	want := etagFor(t, proxySrv, "/"+ref, "application/json")
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("proxy json ETag %q != GET /{ref} JSON ETag %q", got, want)
	}
	assert304(t, proxySrv, "/"+ref+"/json", want)
}

func TestProxyProjectionRaw(t *testing.T) {
	proxySrv, rootSrv, cleanup := testRootAndProxy(t)
	defer cleanup()

	blobData := loadTestFixture(t, "blob.json")
	if resp := doPut(t, rootSrv, blobRef, blobData); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT blob to root failed: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	resp := doGetWithAccept(t, proxySrv, "/"+blobRef+"/raw", "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("expected image/png, got %q", ct)
	}
	rawBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(rawBody) < 4 || string(rawBody[:4]) != "\x89PNG" {
		t.Errorf("expected raw PNG bytes, got %d", len(rawBody))
	}

	want := etagFor(t, proxySrv, "/"+blobRef, "image/png")
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("proxy raw ETag %q != GET /{ref} blob ETag %q", got, want)
	}
	assert304(t, proxySrv, "/"+blobRef+"/raw", want)

	// A non-BLOB projected as /raw -> 409.
	rootData := loadTestFixture(t, "root.json")
	rootRef := fixtureRef(t, "root.json")
	if resp := doPut(t, rootSrv, rootRef, rootData); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT root.json to root failed: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	resp = doGet(t, proxySrv, "/"+rootRef+"/raw")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("proxy raw on non-BLOB: expected 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestProxyProjectionRawPage: a PAGE projected via /raw through the proxy serves
// its own html (synced from upstream, no page-relation deps needed), with ETag
// parity against GET /{ref}'s HTML representation.
func TestProxyProjectionRawPage(t *testing.T) {
	proxySrv, rootSrv, cleanup := testRootAndProxy(t)
	defer cleanup()

	// Store the PAGE only on the root — the proxy must sync it before serving.
	pageData := loadTestFixture(t, "page.json")
	pageRef := fixtureRef(t, "page.json")
	if resp := doPut(t, rootSrv, pageRef, pageData); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT page to root failed: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Accept: application/json must be ignored — /raw serves the PAGE's own html.
	resp := doGetWithAccept(t, proxySrv, "/"+pageRef+"/raw", "application/json")
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

	want := etagFor(t, proxySrv, "/"+pageRef, "text/html")
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("proxy raw ETag %q != GET /{ref} HTML ETag %q", got, want)
	}
	assert304(t, proxySrv, "/"+pageRef+"/raw", want)
}

func TestProxyProjectionPageInline(t *testing.T) {
	proxySrv, rootSrv, cleanup := testRootAndProxy(t)
	defer cleanup()

	pageData := loadTestFixture(t, "page.json")
	pageRef := fixtureRef(t, "page.json")
	if resp := doPut(t, rootSrv, pageRef, pageData); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT page to root failed: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Accept: application/json must be ignored — /page always renders HTML.
	resp := doGetWithAccept(t, proxySrv, "/"+pageRef+"/page", "application/json")
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

	want := etagFor(t, proxySrv, "/"+pageRef, "text/html")
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("proxy page ETag %q != GET /{ref} HTML ETag %q", got, want)
	}
	assert304(t, proxySrv, "/"+pageRef+"/page", want)
}

func TestProxyProjectionPageViaRelation(t *testing.T) {
	proxySrv, rootSrv, cleanup := testRootAndProxy(t)
	defer cleanup()

	// Store the app and its page only on the root — the proxy must sync both
	// (object + page-relation dependency) to render.
	pageData := loadTestFixture(t, "page.json")
	pageRef := fixtureRef(t, "page.json")
	appData := loadTestFixture(t, "app_with_page.json")
	appRef := fixtureRef(t, "app_with_page.json")
	for _, pv := range []struct {
		ref  string
		data []byte
	}{{pageRef, pageData}, {appRef, appData}} {
		if resp := doPut(t, rootSrv, pv.ref, pv.data); resp.StatusCode != http.StatusCreated {
			t.Fatalf("PUT %s to root failed: %d", pv.ref, resp.StatusCode)
		} else {
			resp.Body.Close()
		}
	}

	resp := doGet(t, proxySrv, "/"+appRef+"/page")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("<h1>Hello Dataverse</h1>")) {
		t.Errorf("expected page-relation HTML, got: %s", body)
	}
}

func TestProxyProjectionPrivate(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// Private objects are stored on the proxy locally and never forwarded upstream.
	note := signedObject(t, priv, pubkey, "dddd4444-4444-4444-8444-444444444444", []string{pubkey}, "NOTE")
	noteRef := pubkey + ".dddd4444-4444-4444-8444-444444444444"
	blobRefP, blobDataP := makePrivateBlob(t, priv, pubkey, "eeee5555-5555-4555-8555-555555555555")
	pageRefP, pageDataP := makePrivatePage(t, priv, pubkey, "ffff6666-6666-4666-8666-666666666666")

	for _, pv := range []struct {
		ref  string
		data []byte
	}{{noteRef, note}, {blobRefP, blobDataP}, {pageRefP, pageDataP}} {
		if resp := doPut(t, proxySrv, pv.ref, pv.data); resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("PUT private %s: expected 201, got %d: %s", pv.ref, resp.StatusCode, body)
		} else {
			resp.Body.Close()
		}
	}

	// Unauthenticated projections -> 404.
	for _, p := range []string{"/" + noteRef + "/json", "/" + blobRefP + "/raw", "/" + pageRefP + "/page"} {
		resp := doGet(t, proxySrv, p)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("unauth %s: expected 404, got %d", p, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Authenticated owner -> served.
	token := authenticateAs(t, proxySrv, priv, pubkey)

	resp := doGetWithToken(t, proxySrv, "/"+noteRef+"/json", token)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("auth json: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doGetWithToken(t, proxySrv, "/"+blobRefP+"/raw", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth raw: expected 200, got %d", resp.StatusCode)
	}
	rawBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(rawBody) != "secret raw bytes" {
		t.Errorf("auth raw: unexpected body %q", rawBody)
	}

	resp = doGetWithToken(t, proxySrv, "/"+pageRefP+"/page", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth page: expected 200, got %d", resp.StatusCode)
	}
	pageBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(pageBody, []byte("SECRET-PAGE")) {
		t.Errorf("auth page: expected HTML, got %s", pageBody)
	}
}
