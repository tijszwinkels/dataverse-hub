package main

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/tijszwinkels/dataverse-hub/object"
)

// TestProxyPutSuccessReturnsETag: a global object PUT through the proxy returns
// the new revision ETag.
func TestProxyPutSuccessReturnsETag(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "2f000000-0000-4000-8000-000000000001"
	ref := pubkey + "." + id

	data := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	resp := doPut(t, proxySrv, ref, data)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: expected 201, got %d: %s", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("ETag"), `"1"`; got != want {
		t.Errorf("ETag on proxy create: got %q, want %q", got, want)
	}
}

// TestProxyIfMatchForwardedMismatch: for a global object, the proxy forwards
// If-Match to upstream, which fails the precondition; the proxy relays 412.
func TestProxyIfMatchForwardedMismatch(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "2f000000-0000-4000-8000-000000000002"
	ref := pubkey + "." + id

	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	doPut(t, proxySrv, ref, data1).Body.Close()

	data2 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 2)
	resp := doPutWithHeaders(t, proxySrv, ref, data2, map[string]string{"If-Match": `"5"`})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy If-Match mismatch: expected 412, got %d: %s", resp.StatusCode, body)
	}
}

// TestProxyIfMatchForwardedMatch: a matching If-Match proceeds through the proxy
// and returns the new revision ETag.
func TestProxyIfMatchForwardedMatch(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "2f000000-0000-4000-8000-000000000003"
	ref := pubkey + "." + id

	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	doPut(t, proxySrv, ref, data1).Body.Close()

	data2 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 2)
	resp := doPutWithHeaders(t, proxySrv, ref, data2, map[string]string{"If-Match": `"1"`})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy If-Match match: expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("ETag"), `"2"`; got != want {
		t.Errorf("ETag after proxy update: got %q, want %q", got, want)
	}
}

// TestProxyIfMatchStarNotExisting: If-Match: * on a global object that does not
// yet exist is forwarded and rejected 412.
func TestProxyIfMatchStarNotExisting(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "2f000000-0000-4000-8000-000000000004"
	ref := pubkey + "." + id

	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	resp := doPutWithHeaders(t, proxySrv, ref, data1, map[string]string{"If-Match": "*"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy If-Match * on new object: expected 412, got %d: %s", resp.StatusCode, body)
	}
}

// TestProxyPrivateIfMatch: private (identity-realm) objects are stored locally
// by the proxy, which enforces If-Match against its own store.
func TestProxyPrivateIfMatch(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "2f000000-0000-4000-8000-000000000005"
	ref := pubkey + "." + id

	// Private object: realm is the owner's pubkey → stored locally, not forwarded.
	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{pubkey}, "NOTE", 1)
	resp := doPut(t, proxySrv, ref, data1)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("private PUT: expected 201, got %d: %s", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("ETag"), `"1"`; got != want {
		t.Errorf("ETag on private create: got %q, want %q", got, want)
	}
	resp.Body.Close()

	data2 := signedObjectWithRevision(t, priv, pubkey, id, []string{pubkey}, "NOTE", 2)

	// Wrong If-Match → 412, object must stay at revision 1.
	resp = doPutWithHeaders(t, proxySrv, ref, data2, map[string]string{"If-Match": `"5"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("private If-Match mismatch: expected 412, got %d: %s", resp.StatusCode, body)
	}
	var apiErr object.APIError
	json.NewDecoder(resp.Body).Decode(&apiErr)
	resp.Body.Close()
	if apiErr.Code != "PRECONDITION_FAILED" {
		t.Errorf("expected code PRECONDITION_FAILED, got %q", apiErr.Code)
	}

	// Correct If-Match → proves the object was untouched at revision 1. The
	// proxy's local-store path returns 201 for both create and update (existing
	// behavior, preserved).
	resp = doPutWithHeaders(t, proxySrv, ref, data2, map[string]string{"If-Match": `"1"`})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("private If-Match match: expected 201, got %d: %s", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("ETag"), `"2"`; got != want {
		t.Errorf("ETag after private update: got %q, want %q", got, want)
	}
}
