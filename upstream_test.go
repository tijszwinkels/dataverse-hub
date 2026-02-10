package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamDoSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	u := NewUpstream(srv.URL)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/test", nil)
	resp, err := u.Do(req, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if !u.Available() {
		t.Error("upstream should be marked available after success")
	}
}

func TestUpstreamDoHTTPErrorNoRetry(t *testing.T) {
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad"}`))
	}))
	defer srv.Close()

	u := NewUpstream(srv.URL)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/test", nil)
	resp, err := u.Do(req, nil)
	if err != nil {
		t.Fatalf("HTTP errors should not return err: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	if callCount.Load() != 1 {
		t.Errorf("HTTP errors should not retry, got %d calls", callCount.Load())
	}
}

func TestUpstreamDoRetriesOnTransportError(t *testing.T) {
	// Point to a server that's already closed — transport error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	u := NewUpstream(srv.URL)
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/test", nil)
	_, err := u.Do(req, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for closed server")
	}
	if !u.Available() == true {
		// should be marked unavailable
	}
	if u.Available() {
		t.Error("upstream should be marked unavailable after all retries fail")
	}
	// Should have waited at least 500ms (second retry delay)
	if elapsed < 400*time.Millisecond {
		t.Errorf("expected at least 400ms for retries, got %v", elapsed)
	}
}

func TestUpstreamDoPUTRetryPreservesBody(t *testing.T) {
	var callCount atomic.Int32
	var lastBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		lastBody = body
		if n < 3 {
			// Simulate connection close on first 2 attempts by closing hijacked conn
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	u := NewUpstream(srv.URL)
	bodyBytes := []byte(`{"in":"dataverse001","item":{"id":"test"}}`)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/test", nil)
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.Do(req, bodyBytes)
	if err != nil {
		t.Fatalf("expected success on 3rd attempt: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}
	if string(lastBody) != string(bodyBytes) {
		t.Errorf("body not preserved on retry: got %q", lastBody)
	}
}

func TestUpstreamHealthCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("health check should use HEAD, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u := NewUpstream(srv.URL)
	u.SetAvailable(false)

	err := u.HealthCheck()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !u.Available() {
		t.Error("should be available after successful health check")
	}
}

func TestUpstreamHealthCheckFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	u := NewUpstream(srv.URL)
	err := u.HealthCheck()
	if err == nil {
		t.Fatal("expected error for closed server")
	}
	if u.Available() {
		t.Error("should be unavailable after failed health check")
	}
}
