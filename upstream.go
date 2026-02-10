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
// Tracks availability: when the upstream is marked down, Do() fast-fails
// and a background health-checker probes periodically to detect recovery.
type Upstream struct {
	baseURL   string
	client    *http.Client
	available atomic.Bool

	stopCh chan struct{}
	doneCh chan struct{}
}

// NewUpstream creates a new upstream client pointing at the given base URL.
func NewUpstream(baseURL string) *Upstream {
	u := &Upstream{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
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

// Do executes an HTTP request against the upstream.
// If the upstream is marked unavailable, returns immediately with an error.
// On transport errors, marks the upstream as unavailable (the background
// health-checker will detect recovery).
// HTTP error responses (4xx, 5xx) are returned as-is without retry.
func (u *Upstream) Do(req *http.Request, bodyBytes []byte) (*http.Response, error) {
	if !u.available.Load() {
		return nil, fmt.Errorf("upstream unavailable (fast-fail)")
	}

	if bodyBytes != nil {
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		req.ContentLength = int64(len(bodyBytes))
	}

	resp, err := u.client.Do(req)
	if err != nil {
		u.available.Store(false)
		log.Printf("[proxy] WARN: upstream %s %s failed: %v (marked unavailable)", req.Method, req.URL.Path, err)
		return nil, fmt.Errorf("upstream unreachable: %w", err)
	}

	u.available.Store(true)
	log.Printf("[proxy] upstream %s %s → %d", req.Method, req.URL.Path, resp.StatusCode)
	return resp, nil
}

// HealthCheck performs a lightweight HEAD request to check upstream availability.
// Returns an error if the upstream is unreachable or returns a gateway error (502/503/504).
func (u *Upstream) HealthCheck() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.baseURL+"/", nil)
	if err != nil {
		return err
	}

	resp, err := u.client.Do(req)
	if err != nil {
		u.available.Store(false)
		return fmt.Errorf("health check failed: %w", err)
	}
	resp.Body.Close()

	if isUpstreamDown(resp.StatusCode) {
		u.available.Store(false)
		return fmt.Errorf("health check: upstream returned %d", resp.StatusCode)
	}

	u.available.Store(true)
	return nil
}

// StartHealthChecker runs a background goroutine that probes the upstream
// every interval. Call Stop() to terminate it.
func (u *Upstream) StartHealthChecker(interval time.Duration) {
	go func() {
		defer close(u.doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-u.stopCh:
				return
			case <-ticker.C:
				wasAvailable := u.available.Load()
				err := u.HealthCheck()
				nowAvailable := u.available.Load()

				if !wasAvailable && nowAvailable {
					log.Printf("[proxy] upstream %s is back (was down)", u.baseURL)
				} else if wasAvailable && !nowAvailable {
					log.Printf("[proxy] upstream %s went down: %v", u.baseURL, err)
				}
			}
		}
	}()
}

// Stop terminates the background health-checker and waits for it to finish.
func (u *Upstream) Stop() {
	select {
	case <-u.stopCh:
		// already closed
	default:
		close(u.stopCh)
	}
	<-u.doneCh
}
