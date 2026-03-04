package upstream

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientDoSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	u := NewClient(srv.URL)
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

func TestClientDoHTTPErrorNoRetry(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad"}`))
	}))
	defer srv.Close()

	u := NewClient(srv.URL)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/test", nil)
	resp, err := u.Do(req, nil)
	if err != nil {
		t.Fatalf("HTTP errors should not return err: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 call, got %d", callCount)
	}
}

func TestClientDoFastFailWhenUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach upstream when marked unavailable")
	}))
	defer srv.Close()

	u := NewClient(srv.URL)
	u.SetAvailable(false)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/test", nil)
	start := time.Now()
	_, err := u.Do(req, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error when upstream unavailable")
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("fast-fail took %v, expected <50ms", elapsed)
	}
}

func TestClientDoTransportErrorMarksUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	u := NewClient(srv.URL)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/test", nil)
	_, err := u.Do(req, nil)

	if err == nil {
		t.Fatal("expected error for closed server")
	}
	if u.Available() {
		t.Error("upstream should be marked unavailable after transport error")
	}
}

func TestClientDoPUTPreservesBody(t *testing.T) {
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		lastBody = body
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	u := NewClient(srv.URL)
	bodyBytes := []byte(`{"in":"dataverse001","item":{"id":"test"}}`)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/test", nil)
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.Do(req, bodyBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}
	if string(lastBody) != string(bodyBytes) {
		t.Errorf("body not set correctly: got %q", lastBody)
	}
}

func TestClientHealthCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("health check should use HEAD, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u := NewClient(srv.URL)
	u.SetAvailable(false)

	err := u.HealthCheck()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !u.Available() {
		t.Error("should be available after successful health check")
	}
}

func TestClientHealthCheckFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	u := NewClient(srv.URL)
	err := u.HealthCheck()
	if err == nil {
		t.Fatal("expected error for closed server")
	}
	if u.Available() {
		t.Error("should be unavailable after failed health check")
	}
}

func TestClientHealthCheck502MarksUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	u := NewClient(srv.URL)
	err := u.HealthCheck()
	if err == nil {
		t.Fatal("expected error for 502 response")
	}
	if u.Available() {
		t.Error("should be unavailable after 502 health check")
	}
}

func TestClientHealthCheckerRecovery(t *testing.T) {
	// Start with server down
	var available atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u := NewClient(srv.URL)
	u.SetAvailable(false)

	u.StartHealthChecker(50 * time.Millisecond)
	defer u.Stop()

	// Should stay unavailable while server returns 502
	time.Sleep(120 * time.Millisecond)
	if u.Available() {
		t.Fatal("should still be unavailable while server returns 502")
	}

	// Bring server back
	available.Store(true)
	time.Sleep(120 * time.Millisecond)
	if !u.Available() {
		t.Error("should be available after server recovery")
	}
}

func TestIsDown(t *testing.T) {
	if !IsDown(http.StatusBadGateway) {
		t.Error("502 should be down")
	}
	if !IsDown(http.StatusServiceUnavailable) {
		t.Error("503 should be down")
	}
	if !IsDown(http.StatusGatewayTimeout) {
		t.Error("504 should be down")
	}
	if IsDown(http.StatusOK) {
		t.Error("200 should not be down")
	}
	if IsDown(http.StatusInternalServerError) {
		t.Error("500 should not be down")
	}
}
