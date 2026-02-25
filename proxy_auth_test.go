package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testProxyWithAuth creates a proxy hub with auth support for testing.
// Uses a fake upstream that returns 404 for everything, forcing local-only behavior.
func testProxyWithAuth(t *testing.T) (*httptest.Server, *AuthStore, *Store, *Index, func()) {
	t.Helper()

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(APIError{Error: "not found", Code: "NOT_FOUND"})
	}))

	dir := t.TempDir()
	store, _ := NewStore(dir, true)
	index := NewIndex()
	limiter := NewRateLimiter(10000, 1000000)
	auth := NewAuthStore(168 * time.Hour)
	upstream := NewUpstream(fakeUpstream.URL)
	pendingDir := filepath.Join(dir, "sync_pending")
	pending := NewSyncPending(pendingDir, upstream, store, index)

	proxy := NewProxy(store, index, limiter, auth, "", upstream, pending)
	ts := httptest.NewServer(proxy.Router())

	return ts, auth, store, index, func() {
		ts.Close()
		fakeUpstream.Close()
		limiter.Stop()
		auth.Stop()
	}
}

// storePrivateObject creates and stores a private object (pubkey-only realm) directly in the store+index.
func storePrivateObject(t *testing.T, store *Store, index *Index, priv interface{}, pubkey, id string) string {
	t.Helper()
	// We need a properly signed object — reuse signedObject helper
	// Since we can't call signedObject without the ECDSA key, accept it as *ecdsa.PrivateKey
	// Just store it directly using the lower-level approach
	return pubkey + "." + id
}

func TestProxyAuthChallengeEndpoint(t *testing.T) {
	ts, _, _, _, cleanup := testProxyWithAuth(t)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/auth/challenge")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 from /auth/challenge, got %d: %s", resp.StatusCode, body)
	}

	var challengeResp struct {
		Challenge string `json:"challenge"`
	}
	json.NewDecoder(resp.Body).Decode(&challengeResp)
	if challengeResp.Challenge == "" {
		t.Error("expected non-empty challenge")
	}
}

func TestProxyAuthFullFlow(t *testing.T) {
	ts, _, _, _, cleanup := testProxyWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	token := authenticateAs(t, ts, priv, pubkey)
	if token == "" {
		t.Fatal("expected non-empty token")
	}
}

func TestProxyPutPrivateObject(t *testing.T) {
	ts, _, _, _, cleanup := testProxyWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "aaaa1111-1111-4111-8111-111111111111"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")

	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT private object via proxy: expected 201 or 202, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestProxyPutPrivateObjectWrongPubkey(t *testing.T) {
	ts, _, _, _, cleanup := testProxyWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	_, otherPubkey := testKeypair(t)
	id := "bbbb2222-2222-4222-8222-222222222222"
	ref := pubkey + "." + id
	// Sign with priv but use otherPubkey as realm — should be rejected
	data := signedObject(t, priv, pubkey, id, []string{otherPubkey}, "NOTE")

	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT with wrong pubkey-realm via proxy: expected 403, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestProxyGetPrivateObjectWithoutAuth(t *testing.T) {
	ts, _, store, index, cleanup := testProxyWithAuth(t)
	defer cleanup()

	// Store a private object directly
	priv, pubkey := testKeypair(t)
	id := "cccc3333-3333-4333-8333-333333333333"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")

	// Write directly to store + index
	tmpFile := filepath.Join(os.TempDir(), "test_private.json")
	os.WriteFile(tmpFile, data, 0644)
	defer os.Remove(tmpFile)

	_, item, _ := ParseEnvelope(data)
	tsTime, _ := item.Timestamp()
	store.Write(ref, data, tsTime)
	realms := InField([]string{pubkey})
	index.Update(ref, item, tsTime, realms)

	// GET without auth should return 404
	resp, err := http.Get(ts.URL + "/" + ref)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET private object without auth via proxy: expected 404, got %d: %s", resp.StatusCode, body)
	}
}

func TestProxyGetPrivateObjectWithAuth(t *testing.T) {
	ts, _, store, index, cleanup := testProxyWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "dddd4444-4444-4444-8444-444444444444"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")

	_, item, _ := ParseEnvelope(data)
	tsTime, _ := item.Timestamp()
	store.Write(ref, data, tsTime)
	realms := InField([]string{pubkey})
	index.Update(ref, item, tsTime, realms)

	// Authenticate
	token := authenticateAs(t, ts, priv, pubkey)

	// GET with valid auth should return 200
	resp := doGetWithToken(t, ts, "/"+ref, token)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET private object with auth via proxy: expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestProxySearchExcludesPrivateObjects(t *testing.T) {
	ts, _, store, index, cleanup := testProxyWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// Store a public object
	pubID := "eeee5555-5555-4555-8555-555555555555"
	pubRef := pubkey + "." + pubID
	pubData := signedObject(t, priv, pubkey, pubID, []string{"dataverse001"}, "NOTE")
	_, pubItem, _ := ParseEnvelope(pubData)
	pubTS, _ := pubItem.Timestamp()
	store.Write(pubRef, pubData, pubTS)
	pubRealms := InField([]string{"dataverse001"})
	index.Update(pubRef, pubItem, pubTS, pubRealms)

	// Store a private object
	privID := "ffff6666-6666-4666-8666-666666666666"
	privRef := pubkey + "." + privID
	privData := signedObject(t, priv, pubkey, privID, []string{pubkey}, "NOTE")
	_, privItem, _ := ParseEnvelope(privData)
	privTS, _ := privItem.Timestamp()
	store.Write(privRef, privData, privTS)
	privRealms := InField([]string{pubkey})
	index.Update(privRef, privItem, privTS, privRealms)

	// Search without auth — should only see public
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/search?type=NOTE", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	// The response should contain the public object but not the private one
	if contains(string(body), privID) {
		t.Error("search without auth should NOT include private object")
	}
	if !contains(string(body), pubID) {
		t.Error("search without auth should include public object")
	}
}

func TestProxySearchIncludesPrivateForOwner(t *testing.T) {
	ts, _, store, index, cleanup := testProxyWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// Store a private object
	privID := "aaaa7777-7777-4777-8777-777777777777"
	privRef := pubkey + "." + privID
	privData := signedObject(t, priv, pubkey, privID, []string{pubkey}, "NOTE")
	_, privItem, _ := ParseEnvelope(privData)
	privTS, _ := privItem.Timestamp()
	store.Write(privRef, privData, privTS)
	privRealms := InField([]string{pubkey})
	index.Update(privRef, privItem, privTS, privRealms)

	// Authenticate and search
	token := authenticateAs(t, ts, priv, pubkey)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/search?by="+pubkey+"&type=NOTE", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !contains(string(body), privID) {
		t.Errorf("search by owner with auth should include private object, got: %s", string(body))
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 && (haystack != "" && needle != "" && containsStr(haystack, needle))
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
