package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSyncPendingAddListRemove(t *testing.T) {
	dir := t.TempDir()
	sp := NewSyncPending(dir, nil, nil, nil)

	ref := "AxyU5_test.00000000-0000-0000-0000-000000000001"
	data := []byte(`{"in":"dataverse001"}`)

	// Add
	if err := sp.Add(ref, data); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Verify file exists
	path := filepath.Join(dir, ref+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}

	// List
	refs, err := sp.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0] != ref {
		t.Errorf("expected [%s], got %v", ref, refs)
	}

	// Remove
	if err := sp.Remove(ref); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	refs, _ = sp.List()
	if len(refs) != 0 {
		t.Errorf("expected empty list after remove, got %v", refs)
	}
}

func TestSyncPendingOverwrite(t *testing.T) {
	dir := t.TempDir()
	sp := NewSyncPending(dir, nil, nil, nil)

	ref := "AxyU5_test.00000000-0000-0000-0000-000000000002"

	// Add rev 1
	if err := sp.Add(ref, []byte(`{"rev":1}`)); err != nil {
		t.Fatal(err)
	}
	// Add rev 2 — should overwrite
	if err := sp.Add(ref, []byte(`{"rev":2}`)); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ref+".json"))
	if string(data) != `{"rev":2}` {
		t.Errorf("expected rev 2, got %s", data)
	}

	// Only one file
	refs, _ := sp.List()
	if len(refs) != 1 {
		t.Errorf("expected 1 pending, got %d", len(refs))
	}
}

func TestSyncPendingDrainPushesAndRemoves(t *testing.T) {
	// Set up a mock upstream that accepts PUTs
	var pushCount atomic.Int32
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		pushCount.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer mockUpstream.Close()

	dir := t.TempDir()
	upstream := NewUpstream(mockUpstream.URL)

	storeDir := t.TempDir()
	store, _ := NewStore(storeDir, true)
	index := NewIndex()

	sp := NewSyncPending(dir, upstream, store, index)

	// Add a pending object
	ref := "AxyU5_test.00000000-0000-0000-0000-000000000003"
	obj := createMinimalObject(ref)
	sp.Add(ref, obj)

	// Start drain
	sp.Start()
	defer sp.Stop()

	// Wait for drain to process
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		refs, _ := sp.List()
		if len(refs) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	refs, _ := sp.List()
	if len(refs) != 0 {
		t.Errorf("expected 0 pending after drain, got %d", len(refs))
	}
	if pushCount.Load() < 1 {
		t.Errorf("expected at least 1 push, got %d", pushCount.Load())
	}
}

func TestSyncPendingStopIsClean(t *testing.T) {
	dir := t.TempDir()
	sp := NewSyncPending(dir, nil, nil, nil)
	sp.Start()

	// Stop should not hang
	done := make(chan struct{})
	go func() {
		sp.Stop()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() did not return within 3s")
	}
}

// createMinimalObject creates a minimal valid-looking JSON object for sync pending tests.
func createMinimalObject(ref string) []byte {
	obj := map[string]any{
		"in":        "dataverse001",
		"signature": "test",
		"item": map[string]any{
			"id":         ref[len(ref)-36:],
			"pubkey":     ref[:len(ref)-37],
			"created_at": "2026-02-10T00:00:00Z",
		},
	}
	data, _ := json.Marshal(obj)
	return data
}
