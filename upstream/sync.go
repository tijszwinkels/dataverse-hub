package upstream

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/storage"
)

// SyncPending manages a folder of objects that failed to push to upstream
// and need to be retried when connectivity is restored.
type SyncPending struct {
	dir      string
	upstream *Client
	store    *storage.Store
	index    *storage.Index

	stop chan struct{}
	wg   sync.WaitGroup
	mu   sync.Mutex // protects queue replacement against acknowledgements
}

// NewSyncPending creates a sync pending manager. Creates the directory if needed.
func NewSyncPending(dir string, upstream *Client, store *storage.Store, index *storage.Index) *SyncPending {
	os.MkdirAll(dir, 0755)
	return &SyncPending{
		dir:      dir,
		upstream: upstream,
		store:    store,
		index:    index,
		stop:     make(chan struct{}),
	}
}

// Add writes an object file to the sync_pending directory (atomic write).
func (sp *SyncPending) Add(ref string, data []byte) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	target := filepath.Join(sp.dir, ref+".json")

	tmp, err := os.CreateTemp(sp.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("syncpending add temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("syncpending add write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncpending add sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("syncpending add close: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("syncpending add rename: %w", err)
	}
	return nil
}

// Remove deletes a pending object file.
func (sp *SyncPending) Remove(ref string) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return os.Remove(filepath.Join(sp.dir, ref+".json"))
}

// acknowledge removes only the version actually sent, never a newer queued edit.
func (sp *SyncPending) acknowledge(ref string, sent []byte) bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	path := filepath.Join(sp.dir, ref+".json")
	current, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		log.Printf("[proxy] ERROR: sync acknowledge %s: %v", ref, err)
		return false
	}
	if !bytes.Equal(current, sent) {
		return true
	}
	if err := os.Remove(path); err != nil {
		log.Printf("[proxy] ERROR: sync acknowledge remove %s: %v", ref, err)
		return false
	}
	return true
}

func (sp *SyncPending) preserve(ref string, data []byte, folder string) bool {
	path, err := storage.PreserveConflict(filepath.Join(filepath.Dir(sp.dir), folder), data)
	if err != nil {
		log.Printf("[proxy] ERROR: preserve rejected edit %s: %v; keeping retry pending", ref, err)
		return false
	}
	log.Printf("[proxy] WARN: rejected edit %s preserved at %s; resolve explicitly with a higher revision", ref, path)
	return sp.acknowledge(ref, data)
}

// List returns all refs in the sync_pending directory.
func (sp *SyncPending) List() ([]string, error) {
	entries, err := os.ReadDir(sp.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var refs []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		refs = append(refs, strings.TrimSuffix(name, ".json"))
	}
	return refs, nil
}

// Start launches the background drain goroutine.
func (sp *SyncPending) Start() {
	sp.wg.Add(1)
	go sp.drain()
}

// Stop signals the drain goroutine to stop and waits for it to finish.
func (sp *SyncPending) Stop() {
	close(sp.stop)
	sp.wg.Wait()
}

func (sp *SyncPending) drain() {
	defer sp.wg.Done()

	for {
		select {
		case <-sp.stop:
			return
		default:
		}

		// If upstream is unavailable, probe and wait
		if sp.upstream == nil || !sp.upstream.Available() {
			if sp.upstream != nil {
				sp.upstream.HealthCheck()
			}
			if sp.upstream == nil || !sp.upstream.Available() {
				if !sp.sleepOrStop(10 * time.Second) {
					return
				}
				continue
			}
		}

		refs, err := sp.List()
		if err != nil {
			log.Printf("[proxy] WARN: sync pending list: %v", err)
			if !sp.sleepOrStop(5 * time.Second) {
				return
			}
			continue
		}

		if len(refs) == 0 {
			if !sp.sleepOrStop(5 * time.Second) {
				return
			}
			continue
		}

		for _, ref := range refs {
			if !sp.pushOne(ref) {
				break // error or upstream down, back to outer loop
			}
			if !sp.sleepOrStop(1 * time.Second) {
				return
			}
		}
	}
}

// pushOne attempts to push a single pending object to upstream.
// Returns true if processing should continue, false to break the loop.
func (sp *SyncPending) pushOne(ref string) bool {
	path := filepath.Join(sp.dir, ref+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[proxy] WARN: sync pending read %s: %v", ref, err)
		return true // skip this file, continue with others
	}

	url := sp.upstream.baseURL + "/" + ref
	req, err := http.NewRequest(http.MethodPut, url, nil)
	if err != nil {
		log.Printf("[proxy] ERROR: sync pending build request %s: %v", ref, err)
		return true
	}
	req.Header.Set("Content-Type", "application/json")

	// Single attempt — drain is background, no rush
	if data != nil {
		req.Body = io.NopCloser(bytes.NewReader(data))
		req.ContentLength = int64(len(data))
	}
	resp, err := sp.upstream.client.Do(req)
	if err != nil {
		log.Printf("[proxy] WARN: sync pending push %s failed: %v", ref, err)
		sp.upstream.SetAvailable(false)
		return false // upstream down, stop draining
	}
	defer resp.Body.Close()

	pending, _ := sp.List()
	remaining := len(pending) - 1
	if remaining < 0 {
		remaining = 0
	}

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		log.Printf("[proxy] sync: pushed %s (pending: %d remaining)", ref, remaining)
		return sp.acknowledge(ref, data)

	case resp.StatusCode == http.StatusConflict:
		// 409 can mean an identical replay, a concurrent edit, or a newer
		// upstream revision. None permits silently replacing the local edit.
		remote, err := sp.fetchObject(ref)
		if err != nil {
			log.Printf("[proxy] WARN: sync conflict fetch %s: %v; keeping retry pending", ref, err)
			return false
		}
		same, err := object.SameItem(data, remote)
		if err != nil {
			log.Printf("[proxy] ERROR: compare sync conflict %s: %v", ref, err)
			return false
		}
		if same {
			return sp.acknowledge(ref, data)
		}
		return sp.preserve(ref, data, "sync_conflicts")

	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// Permanent client error — our data is bad, won't succeed on retry
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[proxy] WARN: sync pending push %s rejected (%d): %s", ref, resp.StatusCode, body)
		return sp.preserve(ref, data, "sync_rejected")

	default:
		// Server error (5xx) — upstream is struggling, stop and retry later
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[proxy] WARN: sync pending push %s: upstream returned %d: %s", ref, resp.StatusCode, body)
		return false
	}
}

// fetchObject reads and verifies the upstream candidate without mutating local storage.
func (sp *SyncPending) fetchObject(ref string) ([]byte, error) {
	url := sp.upstream.baseURL + "/" + ref
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := sp.upstream.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := object.VerifyEnvelope(data); err != nil {
		return nil, err
	}

	_, item, err := object.ParseEnvelope(data)
	if err != nil {
		return nil, err
	}
	if item.Ref() != ref {
		return nil, fmt.Errorf("upstream returned a different ref")
	}
	return data, nil
}

// sleepOrStop waits for the given duration or returns false if stop was signaled.
func (sp *SyncPending) sleepOrStop(d time.Duration) bool {
	select {
	case <-sp.stop:
		return false
	case <-time.After(d):
		return true
	}
}
