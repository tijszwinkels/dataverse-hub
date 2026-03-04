package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dataverse/hub/object"
)

// testKeypair generates a fresh P-256 keypair for testing.
func testKeypair(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	compressed := elliptic.MarshalCompressed(elliptic.P256(), priv.PublicKey.X, priv.PublicKey.Y)
	pubkeyStr := base64.RawURLEncoding.EncodeToString(compressed)
	return priv, pubkeyStr
}

// signChallenge signs a challenge string with the private key, returning base64 signature.
func signChallenge(t *testing.T, priv *ecdsa.PrivateKey, challenge string) string {
	t.Helper()
	hash := sha256.Sum256([]byte(challenge))
	r, s, err := ecdsa.Sign(rand.Reader, priv, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	der, err := asn1.Marshal(struct{ R, S *big.Int }{r, s})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func TestChallengeGeneration(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	handler := http.HandlerFunc(auth.HandleChallenge)

	req := httptest.NewRequest(http.MethodGet, "/auth/challenge", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Challenge string `json:"challenge"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if resp.Challenge == "" {
		t.Error("expected non-empty challenge")
	}
	if resp.ExpiresAt == "" {
		t.Error("expected non-empty expires_at")
	}

	raw, err := base64.RawURLEncoding.DecodeString(resp.Challenge)
	if err != nil {
		t.Fatalf("challenge is not valid base64url: %v", err)
	}
	if len(raw) < 32 {
		t.Errorf("challenge should be at least 32 bytes, got %d", len(raw))
	}

	expiry, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		t.Fatalf("invalid expires_at: %v", err)
	}
	if expiry.Before(time.Now()) {
		t.Error("expires_at should be in the future")
	}
}

func TestChallengeUniqueness(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	handler := http.HandlerFunc(auth.HandleChallenge)
	challenges := make(map[string]bool)

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/auth/challenge", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		var resp struct {
			Challenge string `json:"challenge"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if challenges[resp.Challenge] {
			t.Fatalf("duplicate challenge on attempt %d", i)
		}
		challenges[resp.Challenge] = true
	}
}

func TestTokenExchangeValid(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	priv, pubkey := testKeypair(t)

	req := httptest.NewRequest(http.MethodGet, "/auth/challenge", nil)
	w := httptest.NewRecorder()
	auth.HandleChallenge(w, req)

	var challengeResp struct {
		Challenge string `json:"challenge"`
	}
	json.Unmarshal(w.Body.Bytes(), &challengeResp)

	sig := signChallenge(t, priv, challengeResp.Challenge)
	body := `{"pubkey":"` + pubkey + `","challenge":"` + challengeResp.Challenge + `","signature":"` + sig + `"}`
	req = httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	auth.HandleToken(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var tokenResp struct {
		Token     string `json:"token"`
		Pubkey    string `json:"pubkey"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tokenResp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if tokenResp.Token == "" {
		t.Error("expected non-empty token")
	}
	if tokenResp.Pubkey != pubkey {
		t.Errorf("expected pubkey %q, got %q", pubkey, tokenResp.Pubkey)
	}
	if tokenResp.ExpiresAt == "" {
		t.Error("expected non-empty expires_at")
	}

	gotPubkey, ok := auth.ValidateToken(tokenResp.Token)
	if !ok {
		t.Fatal("token should be valid")
	}
	if gotPubkey != pubkey {
		t.Errorf("ValidateToken returned %q, want %q", gotPubkey, pubkey)
	}
}

func TestTokenExchangeInvalidSignature(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	_, pubkey := testKeypair(t)

	req := httptest.NewRequest(http.MethodGet, "/auth/challenge", nil)
	w := httptest.NewRecorder()
	auth.HandleChallenge(w, req)

	var challengeResp struct {
		Challenge string `json:"challenge"`
	}
	json.Unmarshal(w.Body.Bytes(), &challengeResp)

	body := `{"pubkey":"` + pubkey + `","challenge":"` + challengeResp.Challenge + `","signature":"AAAA"}`
	req = httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	auth.HandleToken(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}

	var errResp object.APIError
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Code != "INVALID_SIGNATURE" {
		t.Errorf("expected error code INVALID_SIGNATURE, got %q", errResp.Code)
	}
}

func TestTokenExchangeExpiredChallenge(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	priv, pubkey := testKeypair(t)

	challenge := "expired-challenge-test"
	auth.mu.Lock()
	auth.challenges[challenge] = challengeEntry{expiresAt: time.Now().Add(-1 * time.Minute)}
	auth.mu.Unlock()

	sig := signChallenge(t, priv, challenge)
	body := `{"pubkey":"` + pubkey + `","challenge":"` + challenge + `","signature":"` + sig + `"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	auth.HandleToken(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}

	var errResp object.APIError
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Code != "CHALLENGE_EXPIRED" {
		t.Errorf("expected error code CHALLENGE_EXPIRED, got %q", errResp.Code)
	}
}

func TestTokenExchangeUnknownChallenge(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	priv, pubkey := testKeypair(t)

	sig := signChallenge(t, priv, "unknown-challenge")
	body := `{"pubkey":"` + pubkey + `","challenge":"unknown-challenge","signature":"` + sig + `"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	auth.HandleToken(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}

	var errResp object.APIError
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Code != "CHALLENGE_EXPIRED" {
		t.Errorf("expected error code CHALLENGE_EXPIRED, got %q", errResp.Code)
	}
}

func TestTokenExchangeReplayPrevention(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	priv, pubkey := testKeypair(t)

	req := httptest.NewRequest(http.MethodGet, "/auth/challenge", nil)
	w := httptest.NewRecorder()
	auth.HandleChallenge(w, req)

	var challengeResp struct {
		Challenge string `json:"challenge"`
	}
	json.Unmarshal(w.Body.Bytes(), &challengeResp)

	sig := signChallenge(t, priv, challengeResp.Challenge)
	body := `{"pubkey":"` + pubkey + `","challenge":"` + challengeResp.Challenge + `","signature":"` + sig + `"}`
	req = httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	auth.HandleToken(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("first exchange: expected 200, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	auth.HandleToken(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("replay: expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTokenValidation(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	_, ok := auth.ValidateToken("nonexistent")
	if ok {
		t.Error("unknown token should not be valid")
	}

	auth.mu.Lock()
	auth.tokens["expired-token"] = tokenEntry{pubkey: "test", expiresAt: time.Now().Add(-1 * time.Minute)}
	auth.mu.Unlock()

	_, ok = auth.ValidateToken("expired-token")
	if ok {
		t.Error("expired token should not be valid")
	}
}

func TestCleanup(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	auth.mu.Lock()
	auth.challenges["expired-c"] = challengeEntry{expiresAt: time.Now().Add(-1 * time.Minute)}
	auth.challenges["valid-c"] = challengeEntry{expiresAt: time.Now().Add(5 * time.Minute)}
	auth.tokens["expired-t"] = tokenEntry{pubkey: "test", expiresAt: time.Now().Add(-1 * time.Minute)}
	auth.tokens["valid-t"] = tokenEntry{pubkey: "test", expiresAt: time.Now().Add(5 * time.Minute)}
	auth.mu.Unlock()

	auth.cleanup()

	auth.mu.Lock()
	defer auth.mu.Unlock()
	if _, ok := auth.challenges["expired-c"]; ok {
		t.Error("expired challenge should be cleaned up")
	}
	if _, ok := auth.challenges["valid-c"]; !ok {
		t.Error("valid challenge should not be cleaned up")
	}
	if _, ok := auth.tokens["expired-t"]; ok {
		t.Error("expired token should be cleaned up")
	}
	if _, ok := auth.tokens["valid-t"]; !ok {
		t.Error("valid token should not be cleaned up")
	}
}

func TestAuthMiddleware(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	auth.mu.Lock()
	auth.tokens["test-token"] = tokenEntry{pubkey: "test-pubkey", expiresAt: time.Now().Add(1 * time.Hour)}
	auth.mu.Unlock()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pk := AuthPubkey(r)
		w.Write([]byte(pk))
	})

	handler := auth.Middleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "" {
		t.Errorf("expected empty pubkey, got %q", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Body.String() != "test-pubkey" {
		t.Errorf("expected 'test-pubkey', got %q", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Body.String() != "" {
		t.Errorf("expected empty pubkey for invalid token, got %q", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Basic abc123")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Body.String() != "" {
		t.Errorf("expected empty pubkey for non-Bearer auth, got %q", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.AddCookie(&http.Cookie{Name: "dv_session", Value: "test-token"})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Body.String() != "test-pubkey" {
		t.Errorf("expected 'test-pubkey' from cookie, got %q", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.AddCookie(&http.Cookie{Name: "dv_session", Value: "invalid-token"})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Body.String() != "" {
		t.Errorf("expected empty pubkey for invalid cookie, got %q", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.AddCookie(&http.Cookie{Name: "dv_session", Value: "invalid-token"})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Body.String() != "test-pubkey" {
		t.Errorf("expected bearer to take precedence, got %q", w.Body.String())
	}
}

func TestHandleLogout(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	priv, pubkey := testKeypair(t)

	req := httptest.NewRequest(http.MethodGet, "/auth/challenge", nil)
	w := httptest.NewRecorder()
	auth.HandleChallenge(w, req)

	var challengeResp struct {
		Challenge string `json:"challenge"`
	}
	json.Unmarshal(w.Body.Bytes(), &challengeResp)

	sig := signChallenge(t, priv, challengeResp.Challenge)
	body := `{"pubkey":"` + pubkey + `","challenge":"` + challengeResp.Challenge + `","signature":"` + sig + `"}`
	req = httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	auth.HandleToken(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("token exchange: expected 200, got %d", w.Code)
	}

	var tokenResp struct {
		Token string `json:"token"`
	}
	json.Unmarshal(w.Body.Bytes(), &tokenResp)

	if _, ok := auth.ValidateToken(tokenResp.Token); !ok {
		t.Fatal("token should be valid before logout")
	}

	req = httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+tokenResp.Token)
	w = httptest.NewRecorder()
	auth.HandleLogout(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("logout: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if _, ok := auth.ValidateToken(tokenResp.Token); ok {
		t.Error("token should be invalid after logout")
	}

	cookies := w.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "dv_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected dv_session cookie to be cleared")
	}
	if sessionCookie.MaxAge != -1 {
		t.Errorf("cookie MaxAge should be -1 to clear, got %d", sessionCookie.MaxAge)
	}
}

func TestHandleLogoutWithCookie(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	auth.mu.Lock()
	auth.tokens["cookie-token"] = tokenEntry{pubkey: "test-pk", expiresAt: time.Now().Add(1 * time.Hour)}
	auth.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "dv_session", Value: "cookie-token"})
	w := httptest.NewRecorder()
	auth.HandleLogout(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if _, ok := auth.ValidateToken("cookie-token"); ok {
		t.Error("token should be invalid after logout via cookie")
	}
}

func TestHandleLogoutUnauthenticated(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	w := httptest.NewRecorder()
	auth.HandleLogout(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for no-op logout, got %d: %s", w.Code, w.Body.String())
	}

	cookies := w.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "dv_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected dv_session cookie to be cleared even for unauthenticated request")
	}
}

func TestTokenExchangeSetsCookie(t *testing.T) {
	auth := NewAuthStore(168 * time.Hour)
	defer auth.Stop()

	priv, pubkey := testKeypair(t)

	req := httptest.NewRequest(http.MethodGet, "/auth/challenge", nil)
	w := httptest.NewRecorder()
	auth.HandleChallenge(w, req)

	var challengeResp struct {
		Challenge string `json:"challenge"`
	}
	json.Unmarshal(w.Body.Bytes(), &challengeResp)

	sig := signChallenge(t, priv, challengeResp.Challenge)
	body := `{"pubkey":"` + pubkey + `","challenge":"` + challengeResp.Challenge + `","signature":"` + sig + `"}`
	req = httptest.NewRequest(http.MethodPost, "/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	auth.HandleToken(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	cookies := w.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "dv_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected dv_session cookie to be set")
	}
	if !sessionCookie.HttpOnly {
		t.Error("cookie should be HttpOnly")
	}
	if !sessionCookie.Secure {
		t.Error("cookie should be Secure")
	}
	if sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite should be Lax, got %v", sessionCookie.SameSite)
	}
	if sessionCookie.MaxAge != int((168 * time.Hour).Seconds()) {
		t.Errorf("cookie MaxAge should be %d, got %d", int((168*time.Hour).Seconds()), sessionCookie.MaxAge)
	}

	var tokenResp struct {
		Token string `json:"token"`
	}
	json.Unmarshal(w.Body.Bytes(), &tokenResp)
	if sessionCookie.Value != tokenResp.Token {
		t.Errorf("cookie value %q should match token %q", sessionCookie.Value, tokenResp.Token)
	}

	gotPubkey, ok := auth.ValidateToken(sessionCookie.Value)
	if !ok {
		t.Fatal("cookie token should be valid")
	}
	if gotPubkey != pubkey {
		t.Errorf("cookie token pubkey: got %q, want %q", gotPubkey, pubkey)
	}
}
