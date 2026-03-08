package main

import (
	"bytes"
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
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/serving"
)

// signedObject creates a properly signed dataverse001 envelope for testing.
func signedObject(t *testing.T, priv *ecdsa.PrivateKey, pubkey string, id string, realms []string, objType string) []byte {
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

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// testHubWithAuth creates a Hub with auth support for integration testing.
func testHubWithAuth(t *testing.T) (*httptest.Server, *auth.AuthStore, func()) {
	t.Helper()

	dir := t.TempDir()
	store, err := storage.NewStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}

	index := storage.NewIndex()
	limiter := auth.NewRateLimiter(1000, 100000)
	auth := auth.NewAuthStore(168 * time.Hour)
	hub := serving.NewHub(store, index, limiter, auth, "")

	ts := httptest.NewServer(hub.Router())
	return ts, auth, func() {
		ts.Close()
		limiter.Stop()
		auth.Stop()
	}
}

// authenticateAs gets a bearer token for the given keypair.
func authenticateAs(t *testing.T, ts *httptest.Server, priv *ecdsa.PrivateKey, pubkey string) string {
	t.Helper()

	// GET challenge
	resp, err := http.Get(ts.URL + "/auth/challenge")
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
	json.NewDecoder(resp.Body).Decode(&challengeResp)

	// Sign challenge
	sig := signChallenge(t, priv, challengeResp.Challenge)

	// POST token
	tokenBody := mustMarshal(t, map[string]string{
		"pubkey":    pubkey,
		"challenge": challengeResp.Challenge,
		"signature": sig,
	})
	resp2, err := http.Post(ts.URL+"/auth/token", "application/json", bytes.NewReader(tokenBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("token exchange expected 200, got %d: %s", resp2.StatusCode, body)
	}

	var tokenResp struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp2.Body).Decode(&tokenResp)
	return tokenResp.Token
}

func doGetWithToken(t *testing.T, ts *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// --- Integration Tests ---

func TestPutPrivateObjectMatchingPubkey(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "11111111-1111-4111-8111-111111111111"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")

	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT private object: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPutPrivateObjectWrongPubkey(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	_, otherPubkey := testKeypair(t)

	id := "22222222-2222-4222-8222-222222222222"
	ref := pubkey + "." + id
	// in array contains OTHER pubkey (not item.pubkey) — should be rejected
	data := signedObject(t, priv, pubkey, id, []string{otherPubkey}, "NOTE")

	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT wrong pubkey-realm: expected 403, got %d: %s", resp.StatusCode, body)
	}
	var apiErr object.APIError
	json.NewDecoder(resp.Body).Decode(&apiErr)
	resp.Body.Close()

	if apiErr.Code != "REALM_FORBIDDEN" {
		t.Errorf("expected error code REALM_FORBIDDEN, got %q", apiErr.Code)
	}
}

func TestPutPublicAndPrivateObject(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "33333333-3333-4333-8333-333333333333"
	ref := pubkey + "." + id
	// Both dataverse001 and pubkey-realm — should succeed and be public
	data := signedObject(t, priv, pubkey, id, []string{"dataverse001", pubkey}, "NOTE")

	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT public+private: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPutNoValidRealm(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "44444444-4444-4444-8444-444444444444"
	ref := pubkey + "." + id
	// No dataverse001, no pubkey-realm — should be rejected
	data := signedObject(t, priv, pubkey, id, []string{"other_realm"}, "NOTE")

	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT no valid realm: expected 400, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestGetPrivateObjectWithoutAuth(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "55555555-5555-4555-8555-555555555555"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")

	// PUT
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// GET without auth — should return 404 (not 403)
	resp = doGet(t, ts, "/"+ref)
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET private without auth: expected 404, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestGetPrivateObjectWithValidAuth(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "66666666-6666-4666-8666-666666666666"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")

	// PUT
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Authenticate
	token := authenticateAs(t, ts, priv, pubkey)

	// GET with auth — should return 200
	resp = doGetWithToken(t, ts, "/"+ref, token)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET private with auth: expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestGetPrivateObjectWithWrongAuth(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	otherPriv, otherPubkey := testKeypair(t)
	_ = otherPubkey

	id := "77777777-7777-4777-8777-777777777777"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")

	// PUT
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Authenticate as different user
	wrongToken := authenticateAs(t, ts, otherPriv, otherPubkey)

	// GET with wrong auth — should return 404
	resp = doGetWithToken(t, ts, "/"+ref, wrongToken)
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET private with wrong auth: expected 404, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestSearchExcludesPrivateObjects(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// Create a public object
	pubID := "88888888-8888-4888-8888-888888888881"
	pubRef := pubkey + "." + pubID
	pubData := signedObject(t, priv, pubkey, pubID, []string{"dataverse001"}, "NOTE")
	resp := doPut(t, ts, pubRef, pubData)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT public: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Create a private object
	privID := "88888888-8888-4888-8888-888888888882"
	privRef := pubkey + "." + privID
	privData := signedObject(t, priv, pubkey, privID, []string{pubkey}, "NOTE")
	resp = doPut(t, ts, privRef, privData)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT private: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Unauthenticated search — should only see public object
	resp = doGet(t, ts, "/search")
	var list object.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	if len(list.Items) != 1 {
		t.Fatalf("unauthenticated search: expected 1 item (public only), got %d", len(list.Items))
	}

	// Authenticated search — should see both
	token := authenticateAs(t, ts, priv, pubkey)
	resp = doGetWithToken(t, ts, "/search", token)
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	if len(list.Items) != 2 {
		t.Fatalf("authenticated search: expected 2 items, got %d", len(list.Items))
	}
}

func TestSearchByPubkeyIncludesPrivateForOwner(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// Create private object
	id := "99999999-9999-4999-8999-999999999999"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unauthenticated search by pubkey — should NOT see private
	resp = doGet(t, ts, "/search?by="+pubkey)
	var list object.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 0 {
		t.Fatalf("unauthenticated search by pubkey: expected 0 items, got %d", len(list.Items))
	}

	// Authenticated search by pubkey — should see private
	token := authenticateAs(t, ts, priv, pubkey)
	resp = doGetWithToken(t, ts, "/search?by="+pubkey, token)
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 1 {
		t.Fatalf("authenticated search by pubkey: expected 1 item, got %d", len(list.Items))
	}
}

func TestFullAuthFlow(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// 1. Create a private object
	id := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// 2. Unauthenticated GET — 404
	resp = doGet(t, ts, "/"+ref)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("step 2: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 3. Get challenge
	resp, err := http.Get(ts.URL + "/auth/challenge")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 3: expected 200, got %d", resp.StatusCode)
	}
	var challengeResp struct {
		Challenge string `json:"challenge"`
		ExpiresAt string `json:"expires_at"`
	}
	json.NewDecoder(resp.Body).Decode(&challengeResp)
	resp.Body.Close()

	if challengeResp.Challenge == "" || challengeResp.ExpiresAt == "" {
		t.Fatal("step 3: empty challenge or expires_at")
	}

	// 4. Sign and exchange for token
	hash := sha256.Sum256([]byte(challengeResp.Challenge))
	r, s, err := ecdsa.Sign(rand.Reader, priv, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	der, _ := asn1.Marshal(struct{ R, S *big.Int }{r, s})
	sig := base64.StdEncoding.EncodeToString(der)

	tokenBody, _ := json.Marshal(map[string]string{
		"pubkey":    pubkey,
		"challenge": challengeResp.Challenge,
		"signature": sig,
	})
	resp, err = http.Post(ts.URL+"/auth/token", "application/json", bytes.NewReader(tokenBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("step 4: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var tokenResp struct {
		Token     string `json:"token"`
		Pubkey    string `json:"pubkey"`
		ExpiresAt string `json:"expires_at"`
	}
	json.NewDecoder(resp.Body).Decode(&tokenResp)
	resp.Body.Close()

	if tokenResp.Token == "" || tokenResp.Pubkey != pubkey {
		t.Fatalf("step 4: invalid token response: token=%q pubkey=%q", tokenResp.Token, tokenResp.Pubkey)
	}

	// 5. Authenticated GET — 200
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/"+ref, nil)
	req.Header.Set("Authorization", "Bearer "+tokenResp.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("step 5: expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPublicObjectWithPubkeyRealmIsVisible(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// Object with both dataverse001 AND pubkey-realm = public
	id := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{"dataverse001", pubkey}, "NOTE")

	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Unauthenticated GET — should return 200 (public)
	resp = doGet(t, ts, "/"+ref)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET public: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

