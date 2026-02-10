package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// testRootAndProxy creates a root hub server and a proxy pointing at it.
// Returns (proxyServer, rootServer, cleanup).
func testRootAndProxy(t *testing.T) (*httptest.Server, *httptest.Server, func()) {
	t.Helper()

	// Root hub
	rootDir := t.TempDir()
	rootStore, _ := NewStore(rootDir, true)
	rootIndex := NewIndex()
	rootLimiter := NewRateLimiter(10000, 1000000)
	rootHub := NewHub(rootStore, rootIndex, rootLimiter, "")
	rootSrv := httptest.NewServer(rootHub.Router())

	// Proxy
	proxyDir := t.TempDir()
	proxyStore, _ := NewStore(proxyDir, true)
	proxyIndex := NewIndex()
	proxyLimiter := NewRateLimiter(10000, 1000000)
	upstream := NewUpstream(rootSrv.URL)
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := NewSyncPending(pendingDir, upstream, proxyStore, proxyIndex)

	proxy := NewProxy(proxyStore, proxyIndex, proxyLimiter, "", upstream, pending)
	proxySrv := httptest.NewServer(proxy.Router())

	return proxySrv, rootSrv, func() {
		proxySrv.Close()
		rootSrv.Close()
		rootLimiter.Stop()
		proxyLimiter.Stop()
	}
}

func TestProxyPutAndGet(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	var env Envelope
	json.Unmarshal(data, &env)
	var item Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	// PUT through proxy
	resp := doPut(t, proxySrv, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// GET through proxy
	resp = doGet(t, proxySrv, "/"+ref)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var gotEnv Envelope
	json.Unmarshal(body, &gotEnv)
	var gotItem Item
	json.Unmarshal(gotEnv.Item, &gotItem)
	if gotItem.Ref() != ref {
		t.Errorf("expected ref %s, got %s", ref, gotItem.Ref())
	}
}

func TestProxyGetCachesLocally(t *testing.T) {
	proxySrv, rootSrv, cleanup := testRootAndProxy(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	var env Envelope
	json.Unmarshal(data, &env)
	var item Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	// PUT directly to root
	resp := doPut(t, rootSrv, ref, data)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("root PUT expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GET through proxy — should fetch from root and cache
	resp = doGet(t, proxySrv, "/"+ref)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy GET expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Now close root and GET again — should serve from proxy's cache
	rootSrv.Close()

	resp = doGet(t, proxySrv, "/"+ref)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy GET from cache expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestProxyETagEnrichment(t *testing.T) {
	proxySrv, rootSrv, cleanup := testRootAndProxy(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	var env Envelope
	json.Unmarshal(data, &env)
	var item Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	// PUT through proxy (caches locally)
	resp := doPut(t, proxySrv, ref, data)
	resp.Body.Close()

	// Track what etag root receives
	var receivedETag string
	origHandler := rootSrv.Config.Handler
	rootSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedETag = r.Header.Get("If-None-Match")
		origHandler.ServeHTTP(w, r)
	})

	// GET through proxy without client ETag — proxy should inject one
	resp = doGet(t, proxySrv, "/"+ref)
	resp.Body.Close()

	if receivedETag == "" {
		t.Error("proxy should have injected If-None-Match to upstream")
	}
}

func TestProxyPutUpstreamDown(t *testing.T) {
	proxySrv, rootSrv, cleanup := testRootAndProxy(t)
	defer cleanup()

	// Close root to simulate upstream failure
	rootSrv.Close()

	data := loadTestFixture(t, "root.json")
	var env Envelope
	json.Unmarshal(data, &env)
	var item Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	// PUT through proxy — should store locally with 202
	resp := doPut(t, proxySrv, ref, data)
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT with upstream down expected 202, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result map[string]string
	json.Unmarshal(body, &result)
	if result["status"] != "pending_sync" {
		t.Errorf("expected status=pending_sync, got %s", result["status"])
	}

	// Object should be readable from proxy's local cache
	resp = doGet(t, proxySrv, "/"+ref)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET after offline PUT expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestProxyPutSyncPendingCreated(t *testing.T) {
	proxyDir := t.TempDir()
	proxyStore, _ := NewStore(proxyDir, true)
	proxyIndex := NewIndex()
	proxyLimiter := NewRateLimiter(10000, 1000000)
	defer proxyLimiter.Stop()

	// Point upstream at a closed server
	upstream := NewUpstream("http://127.0.0.1:1") // will fail immediately
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := NewSyncPending(pendingDir, upstream, proxyStore, proxyIndex)

	proxy := NewProxy(proxyStore, proxyIndex, proxyLimiter, "", upstream, pending)
	proxySrv := httptest.NewServer(proxy.Router())
	defer proxySrv.Close()

	data := loadTestFixture(t, "root.json")
	var env Envelope
	json.Unmarshal(data, &env)
	var item Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	resp := doPut(t, proxySrv, ref, data)
	resp.Body.Close()

	// Check sync_pending has the file
	entries, _ := os.ReadDir(pendingDir)
	found := false
	for _, e := range entries {
		if e.Name() == ref+".json" {
			found = true
		}
	}
	if !found {
		t.Error("expected object in sync_pending/ after upstream failure")
	}
}

func TestProxyGetNotFound(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	resp := doGet(t, proxySrv, "/nonexistent.00000000-0000-0000-0000-000000000000")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestProxyRevisionConflict(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	var env Envelope
	json.Unmarshal(data, &env)
	var item Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	// PUT once
	resp := doPut(t, proxySrv, ref, data)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first PUT expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// PUT same revision again — should get 409
	resp = doPut(t, proxySrv, ref, data)
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("second PUT expected 409, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestProxyForwardsSearch(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	// PUT some fixtures through proxy
	for _, f := range []string{"root.json", "identity.json", "core_types.json"} {
		data := loadTestFixture(t, f)
		var env Envelope
		json.Unmarshal(data, &env)
		var item Item
		json.Unmarshal(env.Item, &item)
		resp := doPut(t, proxySrv, item.Ref(), data)
		resp.Body.Close()
	}

	resp := doGet(t, proxySrv, "/search")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search expected 200, got %d", resp.StatusCode)
	}
	var list ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	if len(list.Items) < 3 {
		t.Errorf("expected at least 3 items in search, got %d", len(list.Items))
	}
}

func TestProxy304ToClient(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	var env Envelope
	json.Unmarshal(data, &env)
	var item Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	// PUT
	resp := doPut(t, proxySrv, ref, data)
	resp.Body.Close()

	// GET to get ETag
	resp = doGet(t, proxySrv, "/"+ref)
	etag := resp.Header.Get("ETag")
	resp.Body.Close()

	if etag == "" {
		t.Fatal("expected ETag")
	}

	// GET with matching ETag — should get 304
	req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/"+ref, nil)
	req.Header.Set("If-None-Match", etag)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("expected 304, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestProxyClientETagNoCacheMustFetch verifies that when a client sends an
// If-None-Match header but the proxy has no local cache, the proxy does NOT
// forward the client's ETag upstream. Otherwise upstream returns 304 but
// the proxy has nothing to serve → spurious 404.
func TestProxyClientETagNoCacheMustFetch(t *testing.T) {
	proxySrv, rootSrv, cleanup := testRootAndProxy(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	var env Envelope
	json.Unmarshal(data, &env)
	var item Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	// PUT directly to root (proxy has no local copy)
	resp := doPut(t, rootSrv, ref, data)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("root PUT expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Client sends If-None-Match from a previous direct visit to root
	// Proxy has empty cache — must fetch full object, not forward the ETag
	req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/"+ref, nil)
	req.Header.Set("If-None-Match", `"`+strconv.Itoa(item.Revision)+`"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Must get 200 (fetched from upstream), NOT 404 (cache miss after 304)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 (proxy must fetch from upstream), got %d", resp.StatusCode)
	}
}

// TestProxyGet502FallsBackToCache verifies that when upstream returns 502
// (server down behind a reverse proxy), the proxy falls back to local cache.
func TestProxyGet502FallsBackToCache(t *testing.T) {
	// Set up a fake upstream that returns 502
	badUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer badUpstream.Close()

	proxyDir := t.TempDir()
	proxyStore, _ := NewStore(proxyDir, true)
	proxyIndex := NewIndex()
	proxyLimiter := NewRateLimiter(10000, 1000000)
	defer proxyLimiter.Stop()

	upstream := NewUpstream(badUpstream.URL)
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := NewSyncPending(pendingDir, upstream, proxyStore, proxyIndex)

	proxy := NewProxy(proxyStore, proxyIndex, proxyLimiter, "", upstream, pending)
	proxySrv := httptest.NewServer(proxy.Router())
	defer proxySrv.Close()

	// Pre-populate local cache
	data := loadTestFixture(t, "root.json")
	var env Envelope
	json.Unmarshal(data, &env)
	var item Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()
	ts, _ := item.Timestamp()
	proxyStore.Write(ref, data, ts)
	proxyIndex.Update(ref, &item, ts)

	// GET through proxy — upstream returns 502, should fall back to cache
	resp := doGet(t, proxySrv, "/"+ref)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 from cache fallback, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

// TestProxyInbound502FallsBackToLocal verifies that when upstream returns 502
// for an inbound query, the proxy falls back to the local index.
func TestProxyInbound502FallsBackToLocal(t *testing.T) {
	badUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer badUpstream.Close()

	proxyDir := t.TempDir()
	proxyStore, _ := NewStore(proxyDir, true)
	proxyIndex := NewIndex()
	proxyLimiter := NewRateLimiter(10000, 1000000)
	defer proxyLimiter.Stop()

	upstream := NewUpstream(badUpstream.URL)
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := NewSyncPending(pendingDir, upstream, proxyStore, proxyIndex)

	proxy := NewProxy(proxyStore, proxyIndex, proxyLimiter, "", upstream, pending)
	proxySrv := httptest.NewServer(proxy.Router())
	defer proxySrv.Close()

	ref := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.00000000-0000-0000-0000-000000000000"
	resp := doGet(t, proxySrv, "/"+ref+"/inbound")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 from local fallback, got %d: %s", resp.StatusCode, body)
	}
	var list ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	// Should return an empty list, not a 502
	if list.Items == nil {
		t.Error("expected items array (even if empty), got nil")
	}
}
