package auth

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimiter tracks per-IP request counts with two windows (per-minute, per-day).
type RateLimiter struct {
	mu        sync.Mutex
	perMin    map[string]*window
	perDay    map[string]*window
	maxPerMin int
	maxPerDay int
	stopClean chan struct{}
}

type window struct {
	count   int
	resetAt time.Time
}

// NewRateLimiter creates a rate limiter with the given limits.
func NewRateLimiter(perMin, perDay int) *RateLimiter {
	rl := &RateLimiter{
		perMin:    make(map[string]*window),
		perDay:    make(map[string]*window),
		maxPerMin: perMin,
		maxPerDay: perDay,
		stopClean: make(chan struct{}),
	}
	go rl.cleanupLoop()
	return rl
}

// Stop stops the background cleanup goroutine.
func (rl *RateLimiter) Stop() {
	close(rl.stopClean)
}

// Allow checks if the IP is within rate limits. Returns (allowed, retryAfter).
func (rl *RateLimiter) Allow(ip string) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	// Check per-minute window
	wm := rl.getOrCreate(rl.perMin, ip, now, time.Minute)
	if wm.count >= rl.maxPerMin {
		return false, time.Until(wm.resetAt)
	}

	// Check per-day window
	wd := rl.getOrCreate(rl.perDay, ip, now, 24*time.Hour)
	if wd.count >= rl.maxPerDay {
		return false, time.Until(wd.resetAt)
	}

	wm.count++
	wd.count++
	return true, 0
}

// Remaining returns (minuteRemaining, dayRemaining) for an IP.
func (rl *RateLimiter) Remaining(ip string) (int, int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	wm := rl.getOrCreate(rl.perMin, ip, now, time.Minute)
	wd := rl.getOrCreate(rl.perDay, ip, now, 24*time.Hour)

	minRem := rl.maxPerMin - wm.count
	if minRem < 0 {
		minRem = 0
	}
	dayRem := rl.maxPerDay - wd.count
	if dayRem < 0 {
		dayRem = 0
	}
	return minRem, dayRem
}

func (rl *RateLimiter) getOrCreate(m map[string]*window, ip string, now time.Time, duration time.Duration) *window {
	w, ok := m[ip]
	if !ok || now.After(w.resetAt) {
		w = &window{count: 0, resetAt: now.Add(duration)}
		m[ip] = w
	}
	return w
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rl.cleanup()
		case <-rl.stopClean:
			return
		}
	}
}

func (rl *RateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	for ip, w := range rl.perMin {
		if now.After(w.resetAt) {
			delete(rl.perMin, ip)
		}
	}
	for ip, w := range rl.perDay {
		if now.After(w.resetAt) {
			delete(rl.perDay, ip)
		}
	}
}

// Refund gives back one token to both per-minute and per-day windows.
// Used to exempt 304 Not Modified responses from rate limiting.
func (rl *RateLimiter) Refund(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	if wm, ok := rl.perMin[ip]; ok && now.Before(wm.resetAt) && wm.count > 0 {
		wm.count--
	}
	if wd, ok := rl.perDay[ip]; ok && now.Before(wd.resetAt) && wd.count > 0 {
		wd.count--
	}
}

// statusCapture wraps http.ResponseWriter to capture the status code.
type statusCapture struct {
	http.ResponseWriter
	status int
}

func (sc *statusCapture) WriteHeader(code int) {
	sc.status = code
	sc.ResponseWriter.WriteHeader(code)
}

// Middleware returns an HTTP middleware that enforces rate limits.
// 304 Not Modified responses are refunded and do not consume tokens.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr // chi's RealIP middleware sets this

		allowed, retryAfter := rl.Allow(ip)

		if !allowed {
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rl.maxPerMin))
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(retryAfter).Unix(), 10))
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
			http.Error(w, `{"error":"rate limit exceeded","code":"RATE_LIMITED"}`, http.StatusTooManyRequests)
			return
		}

		// Set rate-limit headers before handler (handler calls WriteHeader)
		minRem, _ := rl.Remaining(ip)
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rl.maxPerMin))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(minRem))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))

		sc := &statusCapture{ResponseWriter: w, status: 200}
		next.ServeHTTP(sc, r)

		// Refund if 304 — these shouldn't count against rate limits
		if sc.status == http.StatusNotModified {
			rl.Refund(ip)
		}
	})
}
