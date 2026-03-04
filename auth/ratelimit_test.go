package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimitRefund(t *testing.T) {
	rl := NewRateLimiter(5, 1000)
	defer rl.Stop()

	ip := "1.2.3.4"

	for i := 0; i < 4; i++ {
		ok, _ := rl.Allow(ip)
		if !ok {
			t.Fatalf("Allow should succeed on attempt %d", i+1)
		}
	}

	rl.Refund(ip)
	rl.Refund(ip)

	minRem, _ := rl.Remaining(ip)
	if minRem != 3 {
		t.Errorf("expected 3 remaining after refund, got %d", minRem)
	}
}

func TestRateLimitMiddleware304DoesNotConsume(t *testing.T) {
	rl := NewRateLimiter(3, 1000)
	defer rl.Stop()

	handler304 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})

	handler200 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	mw304 := rl.Middleware(handler304)
	mw200 := rl.Middleware(handler200)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "5.6.7.8"
		w := httptest.NewRecorder()
		mw304.ServeHTTP(w, req)
		if w.Code != http.StatusNotModified {
			t.Fatalf("expected 304, got %d", w.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "5.6.7.8"
	w := httptest.NewRecorder()
	mw200.ServeHTTP(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Fatal("304 responses should not consume rate limit tokens")
	}
}
