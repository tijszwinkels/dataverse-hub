package main

import (
	"encoding/json"
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
)

// testHubWithLimiter is like testHub but with explicit rate-limit values, so
// the 429 (RATE_LIMITED) path can be exercised deterministically.
func testHubWithLimiter(t *testing.T, perMin, perHour int) (*httptest.Server, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.NewStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	shared := realm.NewSharedRealms()
	index := storage.NewIndex(shared)
	limiter := auth.NewRateLimiter(perMin, perHour)
	authStore := auth.NewAuthStore(168 * time.Hour)
	hub := serving.NewHub(store, index, limiter, authStore, "", shared)
	ts := httptest.NewServer(hub.Router())
	return ts, func() {
		ts.Close()
		limiter.Stop()
		authStore.Stop()
	}
}

// assertProblem verifies an RFC 9457 problem+json error response: preserved
// status, problem media type, and a non-empty title/detail/next_action with the
// expected machine code. Returns the decoded problem for further assertions.
func assertProblem(t *testing.T, resp *http.Response, wantStatus int, wantCode string) object.Problem {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, wantStatus, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != object.ProblemMediaType {
		t.Errorf("Content-Type = %q, want %q (body: %s)", ct, object.ProblemMediaType, body)
	}
	var p object.Problem
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("body is not JSON: %v (body: %s)", err, body)
	}
	if p.Title == "" {
		t.Errorf("%s: empty title (body: %s)", wantCode, body)
	}
	if p.Detail == "" {
		t.Errorf("%s: empty detail (body: %s)", wantCode, body)
	}
	if p.NextAction == "" {
		t.Errorf("%s: empty next_action (body: %s)", wantCode, body)
	}
	if p.Code != wantCode {
		t.Errorf("code = %q, want %q", p.Code, wantCode)
	}
	if p.Status != wantStatus {
		t.Errorf("problem.status = %d, want %d", p.Status, wantStatus)
	}
	return p
}

func noteItem(pubkey, id string, realms []string) map[string]any {
	return map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-02-11T18:00:00+01:00",
		"in":         realms,
		"type":       "NOTE",
		"content":    map[string]any{"text": "hello"},
	}
}

// TestHubProblemResponses drives each Hub-mode endpoint into a specific error
// class and asserts an RFC 9457 problem body. Table-driven per endpoint × class.
func TestHubProblemResponses(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	priv2, _ := testKeypair(t)
	_, otherPubkey := testKeypair(t) // a valid pubkey-realm distinct from pubkey

	// Pre-store an object so we have a real ref for a ref-mismatch PUT target.
	id := "aaaaaaaa-1111-4111-8111-111111111111"
	ref := pubkey + "." + id
	stored := buildSignedObject(t, priv, noteItem(pubkey, id, []string{"dataverse001"}))
	if resp := doPut(t, ts, ref, stored); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup PUT: expected 201, got %d: %s", resp.StatusCode, body)
	}

	cases := []struct {
		name       string
		method     string
		path       string // for GET
		putRef     string // for PUT
		body       []byte
		wantStatus int
		wantCode   string
	}{
		{
			name:       "get not-found (valid ref, absent)",
			method:     http.MethodGet,
			path:       "/" + pubkey + ".bbbbbbbb-2222-4222-8222-222222222222",
			wantStatus: http.StatusNotFound,
			wantCode:   "NOT_FOUND",
		},
		{
			name:       "get malformed ref",
			method:     http.MethodGet,
			path:       "/not-a-valid-ref",
			wantStatus: http.StatusNotFound,
			wantCode:   "NOT_FOUND",
		},
		{
			name:       "inbound malformed ref",
			method:     http.MethodGet,
			path:       "/.env/inbound",
			wantStatus: http.StatusNotFound,
			wantCode:   "NOT_FOUND",
		},
		{
			name:       "put invalid envelope",
			method:     http.MethodPut,
			putRef:     pubkey + ".cccccccc-3333-4333-8333-333333333333",
			body:       []byte("{ this is not valid json"),
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_OBJECT",
		},
		{
			name:       "put unauthorized realm",
			method:     http.MethodPut,
			putRef:     pubkey + ".dddddddd-4444-4444-8444-444444444444",
			body:       buildSignedObject(t, priv, noteItem(pubkey, "dddddddd-4444-4444-8444-444444444444", []string{otherPubkey})),
			wantStatus: http.StatusForbidden,
			wantCode:   "REALM_FORBIDDEN",
		},
		{
			name:       "put ref mismatch",
			method:     http.MethodPut,
			putRef:     pubkey + ".eeeeeeee-5555-4555-8555-555555555555",
			body:       stored, // body's ref is `ref`, not the URL above
			wantStatus: http.StatusBadRequest,
			wantCode:   "REF_MISMATCH",
		},
		{
			name:       "put bad signature",
			method:     http.MethodPut,
			putRef:     pubkey + ".ffffffff-6666-4666-8666-666666666666",
			body:       buildSignedObject(t, priv2, noteItem(pubkey, "ffffffff-6666-4666-8666-666666666666", []string{"dataverse001"})),
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_SIGNATURE",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var resp *http.Response
			switch c.method {
			case http.MethodGet:
				resp = doGet(t, ts, c.path)
			case http.MethodPut:
				resp = doPut(t, ts, c.putRef, c.body)
			}
			assertProblem(t, resp, c.wantStatus, c.wantCode)
		})
	}
}

// TestHubRevisionConflictProblem covers the 409 class (needs a prior PUT).
func TestHubRevisionConflictProblem(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "10000009-2222-4222-8222-222222222222"
	ref := pubkey + "." + id

	data5 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 5)
	if resp := doPut(t, ts, ref, data5); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT rev5: got %d", resp.StatusCode)
	}
	data3 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 3)
	resp := doPut(t, ts, ref, data3)
	assertProblem(t, resp, http.StatusConflict, "REVISION_CONFLICT")
}

// TestHubAuthRealmsUnauthorizedProblem covers /auth/realms without a token.
func TestHubAuthRealmsUnauthorizedProblem(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	resp := doGet(t, ts, "/auth/realms")
	assertProblem(t, resp, http.StatusUnauthorized, "UNAUTHORIZED")
}

// TestHubProblemContentNegotiation: JSON/wildcard clients get problem+json;
// an HTML-only client keeps the legacy {error, code} body unchanged.
func TestHubProblemContentNegotiation(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	_, pubkey := testKeypair(t)
	absent := "/" + pubkey + ".99999999-2222-4222-8222-222222222222"

	// JSON client → problem+json
	resp := doGetWithAccept(t, ts, absent, "application/json")
	assertProblem(t, resp, http.StatusNotFound, "NOT_FOUND")

	// HTML-only client → legacy body, application/json, no problem fields
	resp = doGetWithAccept(t, ts, absent, "text/html")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("HTML-only Content-Type = %q, want application/json", ct)
	}
	var legacy object.APIError
	if err := json.Unmarshal(body, &legacy); err != nil {
		t.Fatalf("legacy body not JSON: %v", err)
	}
	if legacy.Code != "NOT_FOUND" || legacy.Error == "" {
		t.Errorf("legacy body wrong: %+v", legacy)
	}
	var p object.Problem
	json.Unmarshal(body, &p)
	if p.NextAction != "" {
		t.Errorf("HTML-only client must not receive next_action, got %q", p.NextAction)
	}
}

// TestHubRateLimitedProblem covers the 429 class emitted by the rate limiter.
func TestHubRateLimitedProblem(t *testing.T) {
	ts, cleanup := testHubWithLimiter(t, 2, 1000)
	defer cleanup()

	_, pubkey := testKeypair(t)
	path := "/" + pubkey + ".77777777-2222-4222-8222-222222222222"

	// A fixed X-Forwarded-For gives the limiter a stable per-IP key (httptest
	// otherwise varies the source port each request).
	var got *http.Response
	for i := 0; i < 6; i++ {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			got = resp
			break
		}
		resp.Body.Close()
	}
	if got == nil {
		t.Fatal("never got 429 from rate limiter")
	}
	if ra := got.Header.Get("Retry-After"); ra == "" {
		t.Errorf("429 response missing Retry-After header")
	}
	assertProblem(t, got, http.StatusTooManyRequests, "RATE_LIMITED")
}
