package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/upstream"
)

// testProxyWithAuth creates a proxy hub with auth support for testing.
// Uses a fake upstream that returns 404 for everything, forcing local-only behavior.
func testProxyWithAuth(t *testing.T) (*httptest.Server, *auth.AuthStore, *storage.Store, *storage.Index, func()) {
	t.Helper()

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(object.APIError{Error: "not found", Code: "NOT_FOUND"})
	}))

	dir := t.TempDir()
	store, _ := storage.NewStore(dir, true)
	index := storage.NewIndex()
	limiter := auth.NewRateLimiter(10000, 1000000)
	auth := auth.NewAuthStore(168 * time.Hour)
	up := upstream.NewClient(fakeUpstream.URL)
	pendingDir := filepath.Join(dir, "sync_pending")
	pending := upstream.NewSyncPending(pendingDir, up, store, index)

	proxy := serving.NewProxy(store, index, limiter, auth, "", up, pending)
	ts := httptest.NewServer(proxy.Router())

	return ts, auth, store, index, func() {
		ts.Close()
		fakeUpstream.Close()
		limiter.Stop()
		auth.Stop()
	}
}

// testProxyWithRealUpstream creates a proxy backed by a real Hub (not a 404 fake).
// Returns the proxy server, upstream Hub server, proxy store+index, and cleanup func.
func testProxyWithRealUpstream(t *testing.T) (proxy *httptest.Server, upstreamSrv *httptest.Server, proxyStore *storage.Store, proxyIndex *storage.Index, cleanup func()) {
	t.Helper()

	// Create a real upstream hub
	upstreamDir := t.TempDir()
	upstreamStore, _ := storage.NewStore(upstreamDir, true)
	upstreamIndex := storage.NewIndex()
	upstreamLimiter := auth.NewRateLimiter(10000, 1000000)
	upstreamAuth := auth.NewAuthStore(168 * time.Hour)
	upstreamHub := serving.NewHub(upstreamStore, upstreamIndex, upstreamLimiter, upstreamAuth, "")
	upstreamSrv = httptest.NewServer(upstreamHub.Router())

	// Create proxy pointing at real upstream
	proxyDir := t.TempDir()
	proxyStore, _ = storage.NewStore(proxyDir, true)
	proxyIndex = storage.NewIndex()
	proxyLimiter := auth.NewRateLimiter(10000, 1000000)
	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	up := upstream.NewClient(upstreamSrv.URL)
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := upstream.NewSyncPending(pendingDir, up, proxyStore, proxyIndex)

	p := serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", up, pending)
	proxy = httptest.NewServer(p.Router())

	cleanup = func() {
		proxy.Close()
		upstreamSrv.Close()
		upstreamLimiter.Stop()
		upstreamAuth.Stop()
		proxyLimiter.Stop()
		proxyAuth.Stop()
	}
	return
}

// signedObjectWithRelation creates a signed object with a named relation to a target.
func signedObjectWithRelation(t *testing.T, priv *ecdsa.PrivateKey, pubkey string, id string, realms []string, objType, relName, relTargetRef string) []byte {
	t.Helper()

	item := map[string]any{
		"in":         realms,
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-02-24T12:00:00Z",
		"updated_at": "2026-02-24T12:00:00Z",
		"revision":   1,
		"type":       objType,
		"content":    map[string]string{"title": "Test Object " + id},
		"relations": map[string]any{
			relName: []map[string]string{{"ref": relTargetRef}},
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
	sig := base64.StdEncoding.EncodeToString(der)

	env := map[string]any{
		"is":        "instructionGraph001",
		"signature": sig,
		"item":      json.RawMessage(itemJSON),
	}

	result, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return result
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

	_, item, _ := object.ParseEnvelope(data)
	tsTime, _ := item.Timestamp()
	store.Write(ref, data, tsTime)
	realms := object.InField([]string{pubkey})
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

	_, item, _ := object.ParseEnvelope(data)
	tsTime, _ := item.Timestamp()
	store.Write(ref, data, tsTime)
	realms := object.InField([]string{pubkey})
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
	_, pubItem, _ := object.ParseEnvelope(pubData)
	pubTS, _ := pubItem.Timestamp()
	store.Write(pubRef, pubData, pubTS)
	pubRealms := object.InField([]string{"dataverse001"})
	index.Update(pubRef, pubItem, pubTS, pubRealms)

	// Store a private object
	privID := "ffff6666-6666-4666-8666-666666666666"
	privRef := pubkey + "." + privID
	privData := signedObject(t, priv, pubkey, privID, []string{pubkey}, "NOTE")
	_, privItem, _ := object.ParseEnvelope(privData)
	privTS, _ := privItem.Timestamp()
	store.Write(privRef, privData, privTS)
	privRealms := object.InField([]string{pubkey})
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
	_, privItem, _ := object.ParseEnvelope(privData)
	privTS, _ := privItem.Timestamp()
	store.Write(privRef, privData, privTS)
	privRealms := object.InField([]string{pubkey})
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

// --- Proxy Search Merge Tests ---
// These tests verify that when upstream returns public objects and the proxy has
// local private objects, authenticated search results merge both sources.

func TestProxySearchMergesPrivateWithUpstream(t *testing.T) {
	proxy, upstream, proxyStore, proxyIndex, cleanup := testProxyWithRealUpstream(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// 1. PUT a public object to the upstream hub directly
	pubID := "aaaa8888-8888-4888-8888-888888888881"
	pubRef := pubkey + "." + pubID
	pubData := signedObject(t, priv, pubkey, pubID, []string{"dataverse001"}, "NOTE")
	resp := doPut(t, upstream, pubRef, pubData)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT public to upstream: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// 2. Store a private object locally on the proxy (never goes to upstream)
	privID := "aaaa8888-8888-4888-8888-888888888882"
	privRef := pubkey + "." + privID
	privData := signedObject(t, priv, pubkey, privID, []string{pubkey}, "NOTE")
	_, privItem, _ := object.ParseEnvelope(privData)
	privTS, _ := privItem.Timestamp()
	proxyStore.Write(privRef, privData, privTS)
	proxyIndex.Update(privRef, privItem, privTS, object.InField([]string{pubkey}))

	// 3. Authenticate with the proxy
	token := authenticateAs(t, proxy, priv, pubkey)

	// 4. Search on the proxy with auth — should see BOTH public (from upstream) and private (local)
	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/search?by="+pubkey+"&type=NOTE", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var list object.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)

	foundPub := false
	foundPriv := false
	for _, raw := range list.Items {
		s := string(raw)
		if contains(s, pubID) {
			foundPub = true
		}
		if contains(s, privID) {
			foundPriv = true
		}
	}

	if !foundPub {
		t.Errorf("proxy search should include public object from upstream (id=%s)", pubID)
	}
	if !foundPriv {
		t.Errorf("proxy search should include private object from local store (id=%s)", privID)
	}
	if len(list.Items) != 2 {
		t.Errorf("expected 2 items in merged results, got %d", len(list.Items))
	}
}

func TestProxySearchMergeExcludesPrivateForUnauthenticated(t *testing.T) {
	proxy, upstream, proxyStore, proxyIndex, cleanup := testProxyWithRealUpstream(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// PUT public object to upstream
	pubID := "aaaa9999-9999-4999-8999-999999999991"
	pubRef := pubkey + "." + pubID
	pubData := signedObject(t, priv, pubkey, pubID, []string{"dataverse001"}, "NOTE")
	resp := doPut(t, upstream, pubRef, pubData)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT public to upstream: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Store private object locally on proxy
	privID := "aaaa9999-9999-4999-8999-999999999992"
	privRef := pubkey + "." + privID
	privData := signedObject(t, priv, pubkey, privID, []string{pubkey}, "NOTE")
	_, privItem, _ := object.ParseEnvelope(privData)
	privTS, _ := privItem.Timestamp()
	proxyStore.Write(privRef, privData, privTS)
	proxyIndex.Update(privRef, privItem, privTS, object.InField([]string{pubkey}))

	// Search WITHOUT auth — should only see public from upstream
	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/search?by="+pubkey+"&type=NOTE", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var list object.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)

	if len(list.Items) != 1 {
		t.Errorf("unauthenticated search should return 1 item (public only), got %d", len(list.Items))
	}
	bodyStr := ""
	for _, raw := range list.Items {
		bodyStr += string(raw)
	}
	if contains(bodyStr, privID) {
		t.Error("unauthenticated search should NOT include private object")
	}
	if !contains(bodyStr, pubID) {
		t.Error("unauthenticated search should include public object from upstream")
	}
}

func TestProxyInboundMergesPrivateWithUpstream(t *testing.T) {
	proxy, upstream, proxyStore, proxyIndex, cleanup := testProxyWithRealUpstream(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// Create a target object (public, on upstream)
	targetID := "bbbb0000-0000-4000-8000-000000000001"
	targetRef := pubkey + "." + targetID
	targetData := signedObject(t, priv, pubkey, targetID, []string{"dataverse001"}, "NOTE")
	resp := doPut(t, upstream, targetRef, targetData)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT target: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Create a private comment pointing at the target
	privID := "bbbb0000-0000-4000-8000-000000000002"
	privRef := pubkey + "." + privID
	privData := signedObjectWithRelation(t, priv, pubkey, privID, []string{pubkey}, "COMMENT", "comments_on", targetRef)
	_, privItem, _ := object.ParseEnvelope(privData)
	privTS, _ := privItem.Timestamp()
	proxyStore.Write(privRef, privData, privTS)
	proxyIndex.Update(privRef, privItem, privTS, object.InField([]string{pubkey}))

	// Also store the target on proxy so inbound query works locally
	proxyStore.Write(targetRef, targetData, privTS)
	_, targetItem, _ := object.ParseEnvelope(targetData)
	proxyIndex.Update(targetRef, targetItem, privTS, object.InField([]string{"dataverse001"}))

	// Authenticate and query inbound
	token := authenticateAs(t, proxy, priv, pubkey)
	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/"+targetRef+"/inbound?relation=comments_on", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var list object.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)

	foundPriv := false
	for _, raw := range list.Items {
		if contains(string(raw), privID) {
			foundPriv = true
		}
	}
	if !foundPriv {
		t.Errorf("inbound query should include private comment from local store (id=%s), items=%d", privID, len(list.Items))
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
