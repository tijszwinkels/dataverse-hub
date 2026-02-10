package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Upstream is an HTTP client for talking to the root hub server.
type Upstream struct {
	baseURL   string
	client    *http.Client
	available atomic.Bool
}

// retryDelays defines the backoff between retry attempts.
// Attempt 1: immediate, Attempt 2: after 500ms, Attempt 3: after 2s.
var retryDelays = []time.Duration{0, 500 * time.Millisecond, 2 * time.Second}

// NewUpstream creates a new upstream client pointing at the given base URL.
func NewUpstream(baseURL string) *Upstream {
	u := &Upstream{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
	u.available.Store(true)
	return u
}

// Available returns whether the upstream is currently reachable.
func (u *Upstream) Available() bool {
	return u.available.Load()
}

// SetAvailable explicitly sets the availability flag.
func (u *Upstream) SetAvailable(v bool) {
	u.available.Store(v)
}

// Do executes an HTTP request with retry logic for transport errors.
// bodyBytes is used to reconstruct the request body on retries (nil for GET).
// HTTP error responses (4xx, 5xx) are returned immediately without retry.
func (u *Upstream) Do(req *http.Request, bodyBytes []byte) (*http.Response, error) {
	var lastErr error
	for i, delay := range retryDelays {
		if delay > 0 {
			time.Sleep(delay)
		}

		// Reset body for each attempt
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			req.ContentLength = int64(len(bodyBytes))
		}

		resp, err := u.client.Do(req)
		if err == nil {
			u.available.Store(true)
			log.Printf("[proxy] upstream %s %s → %d", req.Method, req.URL.Path, resp.StatusCode)
			return resp, nil
		}
		lastErr = err
		log.Printf("[proxy] WARN: upstream attempt %d/%d failed: %v", i+1, len(retryDelays), err)
	}

	u.available.Store(false)
	return nil, fmt.Errorf("upstream unreachable after %d attempts: %w", len(retryDelays), lastErr)
}

// HealthCheck performs a lightweight HEAD request to check upstream availability.
func (u *Upstream) HealthCheck() error {
	req, err := http.NewRequest(http.MethodHead, u.baseURL+"/", nil)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := u.client.Do(req)
	if err != nil {
		u.available.Store(false)
		return fmt.Errorf("health check failed: %w", err)
	}
	resp.Body.Close()
	u.available.Store(true)
	return nil
}
