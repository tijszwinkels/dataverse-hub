package main

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
)

// SyncPending manages a folder of objects that failed to push to upstream
// and need to be retried when connectivity is restored.
type SyncPending struct {
	dir      string
	upstream *Upstream
	store    *Store
	index    *Index

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewSyncPending creates a sync pending manager. Creates the directory if needed.
func NewSyncPending(dir string, upstream *Upstream, store *Store, index *Index) *SyncPending {
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
	return os.Remove(filepath.Join(sp.dir, ref+".json"))
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
		sp.Remove(ref)
		return true

	case resp.StatusCode == http.StatusConflict:
		// Upstream has newer revision — fetch it and cache locally
		log.Printf("[proxy] sync: %s conflict (upstream has newer), fetching", ref)
		sp.Remove(ref)
		sp.fetchAndCache(ref)
		return true

	default:
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[proxy] WARN: sync pending push %s: upstream returned %d: %s", ref, resp.StatusCode, body)
		return false // unexpected error, stop draining
	}
}

// fetchAndCache fetches an object from upstream and caches it locally.
func (sp *SyncPending) fetchAndCache(ref string) {
	if sp.store == nil {
		return
	}

	url := sp.upstream.baseURL + "/" + ref
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/json")

	resp, err := sp.upstream.client.Do(req)
	if err != nil {
		log.Printf("[proxy] WARN: sync fetch %s: %v", ref, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[proxy] WARN: sync fetch read %s: %v", ref, err)
		return
	}

	_, item, err := ParseEnvelope(data)
	if err != nil {
		log.Printf("[proxy] WARN: sync fetch parse %s: %v", ref, err)
		return
	}

	ts, _ := item.Timestamp()
	if err := sp.store.Write(ref, data, ts); err != nil {
		log.Printf("[proxy] WARN: sync fetch store %s: %v", ref, err)
		return
	}
	if sp.index != nil {
		sp.index.Update(ref, item, ts)
	}
	log.Printf("[proxy] sync: fetched newer %s from upstream", ref)
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
