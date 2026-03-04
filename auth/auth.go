package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dataverse/hub/object"
)

// AuthStore manages challenges and bearer tokens in memory.
type AuthStore struct {
	mu          sync.Mutex
	challenges  map[string]challengeEntry
	tokens      map[string]tokenEntry
	tokenExpiry time.Duration
	stopClean   chan struct{}
}

type challengeEntry struct {
	expiresAt time.Time
}

type tokenEntry struct {
	pubkey    string
	expiresAt time.Time
}

const challengeExpiry = 5 * time.Minute

// NewAuthStore creates a new auth store with the given token expiry and starts cleanup.
func NewAuthStore(tokenExpiry time.Duration) *AuthStore {
	a := &AuthStore{
		challenges:  make(map[string]challengeEntry),
		tokens:      make(map[string]tokenEntry),
		tokenExpiry: tokenExpiry,
		stopClean:   make(chan struct{}),
	}
	go a.cleanupLoop()
	return a
}

// Stop stops the background cleanup goroutine.
func (a *AuthStore) Stop() {
	close(a.stopClean)
}

// HandleChallenge serves GET /auth/challenge.
func (a *AuthStore) HandleChallenge(w http.ResponseWriter, r *http.Request) {
	// Generate 32 random bytes
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Printf("ERROR: auth challenge: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	challenge := base64.RawURLEncoding.EncodeToString(b)
	expiresAt := time.Now().Add(challengeExpiry)

	a.mu.Lock()
	a.challenges[challenge] = challengeEntry{expiresAt: expiresAt}
	a.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"challenge":  challenge,
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}

// HandleToken serves POST /auth/token.
func (a *AuthStore) HandleToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pubkey    string `json:"pubkey"`
		Challenge string `json:"challenge"`
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", "INVALID_REQUEST")
		return
	}

	if req.Pubkey == "" || req.Challenge == "" || req.Signature == "" {
		writeError(w, http.StatusBadRequest, "missing required fields: pubkey, challenge, signature", "INVALID_REQUEST")
		return
	}

	// Check challenge exists and is not expired, then delete (single-use)
	a.mu.Lock()
	entry, ok := a.challenges[req.Challenge]
	if ok {
		delete(a.challenges, req.Challenge)
	}
	a.mu.Unlock()

	if !ok || time.Now().After(entry.expiresAt) {
		writeError(w, http.StatusUnauthorized, "challenge expired or unknown", "CHALLENGE_EXPIRED")
		return
	}

	// Decode and decompress pubkey
	pubkeyBytes, err := base64.RawURLEncoding.DecodeString(req.Pubkey)
	if err != nil || len(pubkeyBytes) != 33 {
		writeError(w, http.StatusUnauthorized, "invalid pubkey", "INVALID_SIGNATURE")
		return
	}

	pubkey, err := object.DecompressP256(pubkeyBytes)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid pubkey", "INVALID_SIGNATURE")
		return
	}

	// Decode signature (base64 standard → ASN.1 DER)
	sigBytes, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid signature encoding", "INVALID_SIGNATURE")
		return
	}

	var sig struct {
		R, S *big.Int
	}
	if _, err := asn1.Unmarshal(sigBytes, &sig); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid signature format", "INVALID_SIGNATURE")
		return
	}

	// Verify: sign the raw challenge string as UTF-8 bytes
	hash := sha256.Sum256([]byte(req.Challenge))
	if !ecdsa.Verify(pubkey, hash[:], sig.R, sig.S) {
		writeError(w, http.StatusUnauthorized, "signature verification failed", "INVALID_SIGNATURE")
		return
	}

	// Generate token
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		log.Printf("ERROR: auth token gen: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	expiresAt := time.Now().Add(a.tokenExpiry)

	a.mu.Lock()
	a.tokens[token] = tokenEntry{pubkey: req.Pubkey, expiresAt: expiresAt}
	a.mu.Unlock()

	// Set session cookie so browsers authenticate transparently
	http.SetCookie(w, &http.Cookie{
		Name:     "dv_session",
		Value:    token,
		Path:     "/",
		MaxAge:   int(a.tokenExpiry.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"token":      token,
		"pubkey":     req.Pubkey,
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}

// HandleLogout serves POST /auth/logout.
// Invalidates the token (from bearer header or cookie) and clears the session cookie.
// Always returns 200 — logout is idempotent.
func (a *AuthStore) HandleLogout(w http.ResponseWriter, r *http.Request) {
	token := extractBearerToken(r)
	if token == "" {
		token = extractCookieToken(r)
	}

	if token != "" {
		a.mu.Lock()
		delete(a.tokens, token)
		a.mu.Unlock()
	}

	// Clear the cookie regardless
	http.SetCookie(w, &http.Cookie{
		Name:     "dv_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ValidateToken returns the pubkey associated with a valid token, or ("", false).
func (a *AuthStore) ValidateToken(token string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	entry, ok := a.tokens[token]
	if !ok || time.Now().After(entry.expiresAt) {
		return "", false
	}
	return entry.pubkey, true
}

// Middleware extracts the pubkey from a bearer token and stores it in context.
// Does NOT reject unauthenticated requests — handlers decide individually.
func (a *AuthStore) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := extractBearerToken(r)
		if token == "" {
			token = extractCookieToken(r)
		}
		if token != "" {
			if pubkey, ok := a.ValidateToken(token); ok {
				ctx := context.WithValue(r.Context(), authPubkeyKey, pubkey)
				r = r.WithContext(ctx)
			}
		}
		next.ServeHTTP(w, r)
	})
}

type contextKey string

const authPubkeyKey contextKey = "auth_pubkey"

// AuthPubkey returns the authenticated pubkey from context, or "".
func AuthPubkey(r *http.Request) string {
	v, _ := r.Context().Value(authPubkeyKey).(string)
	return v
}

// extractBearerToken pulls the token from the Authorization header.
func extractBearerToken(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if !strings.HasPrefix(a, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(a, "Bearer ")
}

// extractCookieToken pulls the token from the dv_session cookie.
func extractCookieToken(r *http.Request) string {
	c, err := r.Cookie("dv_session")
	if err != nil {
		return ""
	}
	return c.Value
}

func (a *AuthStore) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.cleanup()
		case <-a.stopClean:
			return
		}
	}
}

func (a *AuthStore) cleanup() {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	for k, v := range a.challenges {
		if now.After(v.expiresAt) {
			delete(a.challenges, k)
		}
	}
	for k, v := range a.tokens {
		if now.After(v.expiresAt) {
			delete(a.tokens, k)
		}
	}
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg, code string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(object.APIError{Error: msg, Code: code})
}
