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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
)

// signedObjectWithRevision creates a signed object with configurable revision and timestamps.
func signedObjectWithRevision(t *testing.T, priv *ecdsa.PrivateKey, pubkey string, id string, realms []string, objType string, revision int) []byte {
	t.Helper()

	ts := time.Date(2026, 2, 24, 12, 0, 0, 0, time.UTC).Add(time.Duration(revision) * time.Hour)
	item := map[string]any{
		"in":         realms,
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-02-24T12:00:00Z",
		"updated_at": ts.Format(time.RFC3339),
		"revision":   revision,
		"type":       objType,
		"content":    map[string]string{"title": fmt.Sprintf("Test Object %s rev %d", id, revision)},
	}
	itemJSON, err := object.CanonicalJSON(mustMarshalHelper(t, item))
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

func mustMarshalHelper(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// --- Revision Update Tests ---

func TestPutHigherRevisionSucceeds(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "10000001-1111-4111-8111-111111111111"
	ref := pubkey + "." + id

	// PUT revision 1
	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	resp := doPut(t, ts, ref, data1)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT rev 1: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// PUT revision 2 — should succeed with 200
	data2 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 2)
	resp = doPut(t, ts, ref, data2)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT rev 2: expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// GET should return revision 2
	resp = doGet(t, ts, "/"+ref)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var env object.Envelope
	json.Unmarshal(body, &env)
	var item object.Item
	json.Unmarshal(env.Item, &item)
	if item.Revision != 2 {
		t.Errorf("expected revision 2, got %d", item.Revision)
	}
}

func TestPutLowerRevisionRejected(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "10000002-2222-4222-8222-222222222222"
	ref := pubkey + "." + id

	// PUT revision 5
	data5 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 5)
	resp := doPut(t, ts, ref, data5)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT rev 5: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// PUT revision 3 — should be rejected with 409
	data3 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 3)
	resp = doPut(t, ts, ref, data3)
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT rev 3 over rev 5: expected 409, got %d: %s", resp.StatusCode, body)
	}
	var apiErr object.APIError
	json.NewDecoder(resp.Body).Decode(&apiErr)
	resp.Body.Close()

	if apiErr.Code != "REVISION_CONFLICT" {
		t.Errorf("expected error code REVISION_CONFLICT, got %q", apiErr.Code)
	}
}

func TestPutRevisionUpdateCreatesBackup(t *testing.T) {
	// Use a known store directory so we can check for backup files.
	dir := t.TempDir()
	ts, cleanup := testHubWithDir(t, dir)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "10000003-3333-4333-8333-333333333333"
	ref := pubkey + "." + id

	// PUT revision 1
	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	resp := doPut(t, ts, ref, data1)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT rev 1: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// PUT revision 2 (triggers backup of rev 1)
	data2 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 2)
	resp = doPut(t, ts, ref, data2)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT rev 2: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Check backup file exists
	backupPath := filepath.Join(dir, "bk", ref+".r1.json")
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		t.Errorf("expected backup file at %s", backupPath)
	}
}

func TestPutMultipleRevisionUpdates(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "10000004-4444-4444-8444-444444444444"
	ref := pubkey + "." + id

	// Sequential updates: 1 → 2 → 3 → 4
	for rev := 1; rev <= 4; rev++ {
		data := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", rev)
		resp := doPut(t, ts, ref, data)

		expected := http.StatusOK
		if rev == 1 {
			expected = http.StatusCreated
		}
		if resp.StatusCode != expected {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("PUT rev %d: expected %d, got %d: %s", rev, expected, resp.StatusCode, body)
		}
		resp.Body.Close()
	}

	// Final GET should return revision 4
	resp := doGet(t, ts, "/"+ref)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var env object.Envelope
	json.Unmarshal(body, &env)
	var item object.Item
	json.Unmarshal(env.Item, &item)
	if item.Revision != 4 {
		t.Errorf("expected revision 4, got %d", item.Revision)
	}
}

// --- Logout + Token Invalidation Tests ---

func TestLogoutInvalidatesToken(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// Store a private object
	id := "20000001-1111-4111-8111-111111111111"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Authenticate
	token := authenticateAs(t, ts, priv, pubkey)

	// Verify token works
	resp = doGetWithToken(t, ts, "/"+ref, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with valid token: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Logout
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Token should no longer work — private object returns 404
	resp = doGetWithToken(t, ts, "/"+ref, token)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after logout: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLogoutIdempotent(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	// Logout without any token — should still return 200
	resp, err := http.Post(ts.URL+"/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout without token: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLogoutClearsCookie(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	_ = authenticateAs(t, ts, priv, pubkey)

	// Logout and check Set-Cookie clears dv_session
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/auth/logout", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	found := false
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "dv_session" {
			found = true
			if cookie.MaxAge >= 0 {
				t.Errorf("expected MaxAge < 0 (clear cookie), got %d", cookie.MaxAge)
			}
		}
	}
	if !found {
		t.Error("expected Set-Cookie header for dv_session on logout")
	}
}

// --- Cookie-Based Auth Tests ---

func TestCookieAuth(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// Store a private object
	id := "30000001-1111-4111-8111-111111111111"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Get challenge
	resp, err := http.Get(ts.URL + "/auth/challenge")
	if err != nil {
		t.Fatal(err)
	}
	var challengeResp struct {
		Challenge string `json:"challenge"`
	}
	json.NewDecoder(resp.Body).Decode(&challengeResp)
	resp.Body.Close()

	// Exchange for token — extract cookie value from Set-Cookie header
	sig := signChallenge(t, priv, challengeResp.Challenge)
	tokenBody := mustMarshalHelper(t, map[string]string{
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
		t.Fatalf("token exchange: expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Find the dv_session cookie value from the response
	var cookieValue string
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "dv_session" {
			cookieValue = cookie.Value
		}
	}
	if cookieValue == "" {
		t.Fatal("expected dv_session cookie in token exchange response")
	}

	// GET private object with cookie header (no Authorization header)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/"+ref, nil)
	req.AddCookie(&http.Cookie{Name: "dv_session", Value: cookieValue})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET with cookie: expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestTokenExchangeSetsCookie(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// Get challenge
	resp, err := http.Get(ts.URL + "/auth/challenge")
	if err != nil {
		t.Fatal(err)
	}
	var challengeResp struct {
		Challenge string `json:"challenge"`
	}
	json.NewDecoder(resp.Body).Decode(&challengeResp)
	resp.Body.Close()

	// Exchange token and check Set-Cookie
	sig := signChallenge(t, priv, challengeResp.Challenge)
	tokenBody := mustMarshalHelper(t, map[string]string{
		"pubkey":    pubkey,
		"challenge": challengeResp.Challenge,
		"signature": sig,
	})
	resp, err = http.Post(ts.URL+"/auth/token", "application/json", bytes.NewReader(tokenBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	found := false
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "dv_session" {
			found = true
			if cookie.Value == "" {
				t.Error("dv_session cookie should have a value")
			}
			if !cookie.HttpOnly {
				t.Error("dv_session cookie should be HttpOnly")
			}
		}
	}
	if !found {
		t.Error("expected dv_session cookie in token exchange response")
	}
}

func TestChallengeSingleUse(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)

	// Get challenge
	resp, err := http.Get(ts.URL + "/auth/challenge")
	if err != nil {
		t.Fatal(err)
	}
	var challengeResp struct {
		Challenge string `json:"challenge"`
	}
	json.NewDecoder(resp.Body).Decode(&challengeResp)
	resp.Body.Close()

	sig := signChallenge(t, priv, challengeResp.Challenge)
	tokenBody := mustMarshalHelper(t, map[string]string{
		"pubkey":    pubkey,
		"challenge": challengeResp.Challenge,
		"signature": sig,
	})

	// First exchange — should succeed
	resp, err = http.Post(ts.URL+"/auth/token", "application/json", bytes.NewReader(tokenBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first exchange: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Second exchange with same challenge — should fail (single-use)
	resp, err = http.Post(ts.URL+"/auth/token", "application/json", bytes.NewReader(tokenBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay exchange: expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Concurrent Request Tests ---

func TestConcurrentPutDifferentRefs(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	const n = 20
	var wg sync.WaitGroup
	errors := make(chan string, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("c0000000-0000-4000-8000-%012d", idx)
			ref := pubkey + "." + id
			data := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)

			resp := doPut(t, ts, ref, data)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				body, _ := io.ReadAll(resp.Body)
				errors <- fmt.Sprintf("PUT %s: expected 201, got %d: %s", ref, resp.StatusCode, body)
			}
		}(i)
	}
	wg.Wait()
	close(errors)

	for e := range errors {
		t.Error(e)
	}
}

func TestConcurrentGetSameRef(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	// Store one object
	data := loadTestFixture(t, "root.json")
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Read it concurrently
	const n = 50
	var wg sync.WaitGroup
	errors := make(chan string, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := doGet(t, ts, "/"+ref)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errors <- fmt.Sprintf("GET: expected 200, got %d", resp.StatusCode)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			var gotEnv object.Envelope
			if err := json.Unmarshal(body, &gotEnv); err != nil {
				errors <- fmt.Sprintf("GET returned invalid JSON: %v", err)
			}
		}()
	}
	wg.Wait()
	close(errors)

	for e := range errors {
		t.Error(e)
	}
}

func TestConcurrentSearchWithPuts(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	var wg sync.WaitGroup
	errors := make(chan string, 100)

	// Concurrent PUTs
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("d0000000-0000-4000-8000-%012d", idx)
			ref := pubkey + "." + id
			data := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
			resp := doPut(t, ts, ref, data)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				body, _ := io.ReadAll(resp.Body)
				errors <- fmt.Sprintf("PUT %d: expected 201, got %d: %s", idx, resp.StatusCode, body)
			}
		}(i)
	}

	// Concurrent searches while PUTs are happening
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := doGet(t, ts, "/search")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errors <- fmt.Sprintf("search: expected 200, got %d", resp.StatusCode)
				return
			}
			var list object.ListResponse
			if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
				errors <- fmt.Sprintf("search: invalid JSON: %v", err)
			}
		}()
	}

	wg.Wait()
	close(errors)

	for e := range errors {
		t.Error(e)
	}
}

func TestConcurrentAuthAndAccess(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	const n = 10
	var wg sync.WaitGroup
	errors := make(chan string, n*2)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			priv, pubkey := testKeypair(t)

			// Store a private object
			id := fmt.Sprintf("e0000000-0000-4000-8000-%012d", idx)
			ref := pubkey + "." + id
			data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")
			resp := doPut(t, ts, ref, data)
			if resp.StatusCode != http.StatusCreated {
				body, _ := io.ReadAll(resp.Body)
				errors <- fmt.Sprintf("PUT %d: expected 201, got %d: %s", idx, resp.StatusCode, body)
				resp.Body.Close()
				return
			}
			resp.Body.Close()

			// Authenticate
			token := authenticateAs(t, ts, priv, pubkey)

			// Access private object
			resp = doGetWithToken(t, ts, "/"+ref, token)
			if resp.StatusCode != http.StatusOK {
				errors <- fmt.Sprintf("GET %d: expected 200, got %d", idx, resp.StatusCode)
			}
			resp.Body.Close()
		}(i)
	}

	wg.Wait()
	close(errors)

	for e := range errors {
		t.Error(e)
	}
}

// --- Additional Auth Edge Cases ---

func TestTokenExchangeInvalidSignature(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	_, pubkey := testKeypair(t)
	otherPriv, _ := testKeypair(t)

	// Get challenge
	resp, err := http.Get(ts.URL + "/auth/challenge")
	if err != nil {
		t.Fatal(err)
	}
	var challengeResp struct {
		Challenge string `json:"challenge"`
	}
	json.NewDecoder(resp.Body).Decode(&challengeResp)
	resp.Body.Close()

	// Sign challenge with WRONG key
	sig := signChallenge(t, otherPriv, challengeResp.Challenge)
	tokenBody := mustMarshalHelper(t, map[string]string{
		"pubkey":    pubkey,
		"challenge": challengeResp.Challenge,
		"signature": sig,
	})

	resp, err = http.Post(ts.URL+"/auth/token", "application/json", bytes.NewReader(tokenBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("wrong-key exchange: expected 401, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestTokenExchangeMissingFields(t *testing.T) {
	ts, _, cleanup := testHubWithAuth(t)
	defer cleanup()

	// Missing all fields
	resp, err := http.Post(ts.URL+"/auth/token", "application/json",
		strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty fields: expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Invalid JSON
	resp, err = http.Post(ts.URL+"/auth/token", "application/json",
		strings.NewReader(`not json`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid JSON: expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- testHubWithDir helper ---

func testHubWithDir(t *testing.T, dir string) (*httptest.Server, func()) {
	t.Helper()

	store, err := storage.NewStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}

	shared := realm.NewSharedRealms()
	index := storage.NewIndex(shared)
	limiter := auth.NewRateLimiter(1000, 100000)
	authSt := auth.NewAuthStore(168 * time.Hour)
	hub := serving.NewHub(store, index, limiter, authSt, "", shared)

	server := httptest.NewServer(hub.Router())
	return server, func() {
		server.Close()
		limiter.Stop()
		authSt.Stop()
	}
}
