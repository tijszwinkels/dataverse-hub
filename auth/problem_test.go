package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/object"
)

func assertAuthProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, wantStatus, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != object.ProblemMediaType {
		t.Errorf("Content-Type = %q, want %q", ct, object.ProblemMediaType)
	}
	var p object.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body not JSON: %v (body: %s)", err, rec.Body.String())
	}
	if p.Title == "" || p.Detail == "" || p.NextAction == "" {
		t.Errorf("%s: incomplete problem: %+v", wantCode, p)
	}
	if p.Code != wantCode {
		t.Errorf("code = %q, want %q", p.Code, wantCode)
	}
	if p.Status != wantStatus {
		t.Errorf("problem.status = %d, want %d", p.Status, wantStatus)
	}
}

// TestAuthTokenProblemResponses drives POST /auth/token into each error class
// and asserts an RFC 9457 problem body.
func TestAuthTokenProblemResponses(t *testing.T) {
	_, pubkey := testKeypair(t)

	cases := []struct {
		name       string
		body       string
		seed       string // challenge to seed as valid before the call, if any
		wantStatus int
		wantCode   string
	}{
		{
			name:       "invalid JSON body",
			body:       "this is not json",
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_REQUEST",
		},
		{
			name:       "missing fields",
			body:       `{"pubkey":""}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_REQUEST",
		},
		{
			name:       "unknown challenge",
			body:       `{"pubkey":"` + pubkey + `","challenge":"nope","signature":"AAAA"}`,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "CHALLENGE_EXPIRED",
		},
		{
			name:       "bad signature",
			body:       `{"pubkey":"` + pubkey + `","challenge":"seeded","signature":"AAAA"}`,
			seed:       "seeded",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "INVALID_SIGNATURE",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := NewAuthStore(168 * time.Hour)
			defer a.Stop()
			if c.seed != "" {
				a.mu.Lock()
				a.challenges[c.seed] = challengeEntry{expiresAt: time.Now().Add(time.Minute)}
				a.mu.Unlock()
			}
			req := httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			a.HandleToken(rec, req)
			assertAuthProblem(t, rec, c.wantStatus, c.wantCode)
		})
	}
}
