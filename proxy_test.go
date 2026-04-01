package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/upstream"
)

// testRootAndProxy creates a root hub server and a proxy pointing at it.
// Returns (proxyServer, rootServer, cleanup).
func testRootAndProxy(t *testing.T) (*httptest.Server, *httptest.Server, func()) {
	t.Helper()

	// Root hub
	rootDir := t.TempDir()
	rootStore, _ := storage.NewStore(rootDir, true)
	rootShared := realm.NewSharedRealms()
	rootIndex := storage.NewIndex(rootShared)
	rootLimiter := auth.NewRateLimiter(10000, 1000000)
	rootAuth := auth.NewAuthStore(168 * time.Hour)
	rootHub := serving.NewHub(rootStore, rootIndex, rootLimiter, rootAuth, "", rootShared)
	rootSrv := httptest.NewServer(rootHub.Router())

	// Proxy
	proxyDir := t.TempDir()
	proxyStore, _ := storage.NewStore(proxyDir, true)
	proxyShared := realm.NewSharedRealms()
	proxyIndex := storage.NewIndex(proxyShared)
	proxyLimiter := auth.NewRateLimiter(10000, 1000000)
	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	up := upstream.NewClient(rootSrv.URL)
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := upstream.NewSyncPending(pendingDir, up, proxyStore, proxyIndex)

	proxy := serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", up, pending, proxyShared)
	proxySrv := httptest.NewServer(proxy.Router())

	return proxySrv, rootSrv, func() {
		proxySrv.Close()
		rootSrv.Close()
		rootLimiter.Stop()
		proxyLimiter.Stop()
		rootAuth.Stop()
		proxyAuth.Stop()
	}
}

func TestProxyPutAndGet(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
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

	var gotEnv object.Envelope
	json.Unmarshal(body, &gotEnv)
	var gotItem object.Item
	json.Unmarshal(gotEnv.Item, &gotItem)
	if gotItem.Ref() != ref {
		t.Errorf("expected ref %s, got %s", ref, gotItem.Ref())
	}
}

func TestProxyGetCachesLocally(t *testing.T) {
	proxySrv, rootSrv, cleanup := testRootAndProxy(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
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
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
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
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
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
	proxyStore, _ := storage.NewStore(proxyDir, true)
	proxyShared := realm.NewSharedRealms()
	proxyIndex := storage.NewIndex(proxyShared)
	proxyLimiter := auth.NewRateLimiter(10000, 1000000)
	defer proxyLimiter.Stop()

	// Point upstream at a closed server
	up := upstream.NewClient("http://127.0.0.1:1") // will fail immediately
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := upstream.NewSyncPending(pendingDir, up, proxyStore, proxyIndex)

	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	defer proxyAuth.Stop()
	proxy := serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", up, pending, proxyShared)
	proxySrv := httptest.NewServer(proxy.Router())
	defer proxySrv.Close()

	data := loadTestFixture(t, "root.json")
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
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
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
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
		var env object.Envelope
		json.Unmarshal(data, &env)
		var item object.Item
		json.Unmarshal(env.Item, &item)
		resp := doPut(t, proxySrv, item.Ref(), data)
		resp.Body.Close()
	}

	resp := doGet(t, proxySrv, "/search")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search expected 200, got %d", resp.StatusCode)
	}
	var list object.ListResponse
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
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
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
// forward the client's ETag upstream. The proxy fetches the full object,
// caches it, then correctly returns 304 (client already has this revision).
func TestProxyClientETagNoCacheMustFetch(t *testing.T) {
	proxySrv, rootSrv, cleanup := testRootAndProxy(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	// PUT directly to root (proxy has no local copy)
	resp := doPut(t, rootSrv, ref, data)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("root PUT expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Client sends If-None-Match from a previous direct visit to root
	// Proxy has empty cache — must fetch full object, not forward client's ETag
	req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/"+ref, nil)
	req.Header.Set("If-None-Match", `"`+strconv.Itoa(item.Revision)+`"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Proxy fetches from upstream (200), caches, then sees client already
	// has this revision → 304. NOT 404 (which would happen if proxy forwarded
	// client's ETag and had nothing to serve after upstream's 304).
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotModified {
		t.Errorf("expected 200 or 304, got %d", resp.StatusCode)
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
	proxyStore, _ := storage.NewStore(proxyDir, true)
	proxyShared := realm.NewSharedRealms()
	proxyIndex := storage.NewIndex(proxyShared)
	proxyLimiter := auth.NewRateLimiter(10000, 1000000)
	defer proxyLimiter.Stop()

	up := upstream.NewClient(badUpstream.URL)
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := upstream.NewSyncPending(pendingDir, up, proxyStore, proxyIndex)

	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	defer proxyAuth.Stop()
	proxy := serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", up, pending, proxyShared)
	proxySrv := httptest.NewServer(proxy.Router())
	defer proxySrv.Close()

	// Pre-populate local cache
	data := loadTestFixture(t, "root.json")
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
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
	proxyStore, _ := storage.NewStore(proxyDir, true)
	proxyShared := realm.NewSharedRealms()
	proxyIndex := storage.NewIndex(proxyShared)
	proxyLimiter := auth.NewRateLimiter(10000, 1000000)
	defer proxyLimiter.Stop()

	up := upstream.NewClient(badUpstream.URL)
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := upstream.NewSyncPending(pendingDir, up, proxyStore, proxyIndex)

	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	defer proxyAuth.Stop()
	proxy := serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", up, pending, proxyShared)
	proxySrv := httptest.NewServer(proxy.Router())
	defer proxySrv.Close()

	ref := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.00000000-0000-0000-0000-000000000000"
	resp := doGet(t, proxySrv, "/"+ref+"/inbound")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 from local fallback, got %d: %s", resp.StatusCode, body)
	}
	var list object.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	// Should return an empty list, not a 502
	if list.Items == nil {
		t.Error("expected items array (even if empty), got nil")
	}
}

// TestProxyGet404FallsBackToLocalAndPushes verifies that when upstream returns
// 404 but the proxy has the object locally, it serves from cache AND pushes
// the object to upstream.
func TestProxyGet404FallsBackToLocalAndPushes(t *testing.T) {
	// Track what upstream receives
	var putReceived bool
	var putBody []byte
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putReceived = true
			putBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			w.Write(putBody)
			return
		}
		// GET returns 404 — upstream doesn't have the object
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(object.APIError{Error: "not found", Code: "NOT_FOUND"})
	}))
	defer fakeUpstream.Close()

	proxyDir := t.TempDir()
	proxyStore, _ := storage.NewStore(proxyDir, true)
	proxyShared := realm.NewSharedRealms()
	proxyIndex := storage.NewIndex(proxyShared)
	proxyLimiter := auth.NewRateLimiter(10000, 1000000)
	defer proxyLimiter.Stop()

	up := upstream.NewClient(fakeUpstream.URL)
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := upstream.NewSyncPending(pendingDir, up, proxyStore, proxyIndex)

	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	defer proxyAuth.Stop()
	proxy := serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", up, pending, proxyShared)
	proxySrv := httptest.NewServer(proxy.Router())
	defer proxySrv.Close()

	// Pre-populate proxy's local cache
	data := loadTestFixture(t, "root.json")
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()
	ts, _ := item.Timestamp()
	proxyStore.Write(ref, data, ts)
	proxyIndex.Update(ref, &item, ts)

	// GET through proxy — upstream returns 404, proxy should serve from local
	resp := doGet(t, proxySrv, "/"+ref)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 from local fallback on upstream 404, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Give the fire-and-forget goroutine a moment to push
	for i := 0; i < 50 && !putReceived; i++ {
		sleepMs(t, 10)
	}

	if !putReceived {
		t.Error("expected proxy to push object to upstream after 404")
	}
	if len(putBody) == 0 {
		t.Error("expected non-empty PUT body")
	}
}

// TestProxyGet404NotFoundBothSides verifies that when both upstream and
// local return nothing, we still get a 404.
func TestProxyGet404NotFoundBothSides(t *testing.T) {
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(object.APIError{Error: "not found", Code: "NOT_FOUND"})
	}))
	defer fakeUpstream.Close()

	proxyDir := t.TempDir()
	proxyStore, _ := storage.NewStore(proxyDir, true)
	proxyShared := realm.NewSharedRealms()
	proxyIndex := storage.NewIndex(proxyShared)
	proxyLimiter := auth.NewRateLimiter(10000, 1000000)
	defer proxyLimiter.Stop()

	up := upstream.NewClient(fakeUpstream.URL)
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := upstream.NewSyncPending(pendingDir, up, proxyStore, proxyIndex)

	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	defer proxyAuth.Stop()
	proxy := serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", up, pending, proxyShared)
	proxySrv := httptest.NewServer(proxy.Router())
	defer proxySrv.Close()

	resp := doGet(t, proxySrv, "/nonexistent.00000000-0000-0000-0000-000000000000")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestProxyCacheLocallySkipsOlderRevision verifies that cacheLocally does NOT
// overwrite a newer local revision with an older upstream revision.
func TestProxyCacheLocallySkipsOlderRevision(t *testing.T) {
	proxyDir := t.TempDir()
	proxyStore, _ := storage.NewStore(proxyDir, true)
	proxyShared := realm.NewSharedRealms()
	proxyIndex := storage.NewIndex(proxyShared)
	proxyLimiter := auth.NewRateLimiter(10000, 1000000)
	defer proxyLimiter.Stop()

	// Use a fake upstream that tracks pushes
	var pushReceived atomic.Bool
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			pushReceived.Store(true)
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			w.Write(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fakeUpstream.Close()

	up := upstream.NewClient(fakeUpstream.URL)
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := upstream.NewSyncPending(pendingDir, up, proxyStore, proxyIndex)

	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	defer proxyAuth.Stop()
	proxy := serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", up, pending, proxyShared)

	// Load fixture and store as rev 28 (the fixture's actual revision)
	data := loadTestFixture(t, "root.json")
	var env object.Envelope
	json.Unmarshal(data, &env)
	var item object.Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()
	ts, _ := item.Timestamp()

	proxyStore.Write(ref, data, ts)
	proxyIndex.Update(ref, &item, ts)

	localRev := item.Revision
	if localRev == 0 {
		t.Fatal("fixture should have revision > 0")
	}

	// Forge an older revision by patching the JSON (just decrement revision)
	// We can't re-sign, but cacheLocally parses before checking signature
	olderData := forgeRevision(t, data, localRev-1)

	proxy.CacheLocally(ref, olderData)

	// Verify local still has the original revision
	meta, found := proxyIndex.GetMeta(ref)
	if !found {
		t.Fatal("object should still be in index")
	}
	if meta.Revision != localRev {
		t.Errorf("expected local revision %d to be preserved, got %d", localRev, meta.Revision)
	}

	// Verify it tried to push local to upstream
	for i := 0; i < 50 && !pushReceived.Load(); i++ {
		sleepMs(t, 10)
	}
	if !pushReceived.Load() {
		t.Error("expected proxy to push newer local version to upstream")
	}
}

// TestProxyListCachesItemsInBackground verifies that items returned from a
// list/inbound/search upstream response are cached locally in the background.
func TestProxyListCachesItemsInBackground(t *testing.T) {
	proxySrv, rootSrv, cleanup := testRootAndProxy(t)
	defer cleanup()

	// PUT fixtures directly to root (proxy has no local copies)
	fixtures := []string{"root.json", "identity.json", "core_types.json"}
	refs := make([]string, len(fixtures))
	for i, f := range fixtures {
		data := loadTestFixture(t, f)
		var env object.Envelope
		json.Unmarshal(data, &env)
		var item object.Item
		json.Unmarshal(env.Item, &item)
		refs[i] = item.Ref()
		resp := doPut(t, rootSrv, item.Ref(), data)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("root PUT %s expected 201, got %d", f, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Query search through proxy — triggers background caching
	resp := doGet(t, proxySrv, "/search")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search expected 200, got %d", resp.StatusCode)
	}
	var list object.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	if len(list.Items) < 3 {
		t.Fatalf("expected at least 3 items in search, got %d", len(list.Items))
	}

	// Wait for background goroutine to finish (3 items × 200ms + fetch time)
	// Poll by checking if the last ref is accessible after closing root.
	time.Sleep(2 * time.Second)

	// Close root hub — from now on, proxy can only serve from local cache
	rootSrv.Close()

	// Verify all items are now cached locally
	for i, ref := range refs {
		resp = doGet(t, proxySrv, "/"+ref)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("fixture %s (ref %s): expected 200 from cache, got %d", fixtures[i], ref, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// --- upstream_push = "all" tests ---

// TestProxyPutPrivateObjectForwardedWhenUpstreamPushAll verifies that when
// UpstreamPush is "all", private objects (identity-realm) are forwarded to
// upstream instead of stored locally only.
func TestProxyPutPrivateObjectForwardedWhenUpstreamPushAll(t *testing.T) {
	// Track what upstream receives
	var putReceived atomic.Bool
	var putRef string
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putReceived.Store(true)
			putRef = r.URL.Path[1:] // strip leading /
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			w.Write(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(object.APIError{Error: "not found", Code: "NOT_FOUND"})
	}))
	defer fakeUpstream.Close()

	proxyDir := t.TempDir()
	proxyStore, _ := storage.NewStore(proxyDir, true)
	proxyShared := realm.NewSharedRealms()
	proxyIndex := storage.NewIndex(proxyShared)
	proxyLimiter := auth.NewRateLimiter(10000, 1000000)
	defer proxyLimiter.Stop()

	up := upstream.NewClient(fakeUpstream.URL)
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := upstream.NewSyncPending(pendingDir, up, proxyStore, proxyIndex)

	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	defer proxyAuth.Stop()
	proxy := serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", up, pending, proxyShared)
	proxy.UpstreamPush = "all" // <-- the new flag
	proxySrv := httptest.NewServer(proxy.Router())
	defer proxySrv.Close()

	// Create a private object (identity-realm only)
	priv, pubkey := testKeypair(t)
	id := "aaaa1111-1111-4111-8111-111111111111"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")

	resp := doPut(t, proxySrv, ref, data)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Should get 201 (forwarded to upstream, not 202 pending)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT private with upstream_push=all: expected 201, got %d: %s", resp.StatusCode, body)
	}

	// Upstream should have received the PUT
	if !putReceived.Load() {
		t.Fatal("expected private object to be forwarded to upstream when upstream_push=all")
	}
	if putRef != ref {
		t.Errorf("upstream received ref %q, want %q", putRef, ref)
	}
}

// TestProxyPutPrivateObjectLocalOnlyByDefault verifies that with default config,
// private objects are NOT forwarded to upstream (existing behavior preserved).
func TestProxyPutPrivateObjectLocalOnlyByDefault(t *testing.T) {
	var putReceived atomic.Bool
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putReceived.Store(true)
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			w.Write(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(object.APIError{Error: "not found", Code: "NOT_FOUND"})
	}))
	defer fakeUpstream.Close()

	proxyDir := t.TempDir()
	proxyStore, _ := storage.NewStore(proxyDir, true)
	proxyShared := realm.NewSharedRealms()
	proxyIndex := storage.NewIndex(proxyShared)
	proxyLimiter := auth.NewRateLimiter(10000, 1000000)
	defer proxyLimiter.Stop()

	up := upstream.NewClient(fakeUpstream.URL)
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := upstream.NewSyncPending(pendingDir, up, proxyStore, proxyIndex)

	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	defer proxyAuth.Stop()
	// UpstreamPush defaults to "" which means "public" only
	proxy := serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", up, pending, proxyShared)
	proxySrv := httptest.NewServer(proxy.Router())
	defer proxySrv.Close()

	priv, pubkey := testKeypair(t)
	id := "bbbb2222-2222-4222-8222-222222222222"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")

	resp := doPut(t, proxySrv, ref, data)
	resp.Body.Close()

	// Should get 201 (stored locally)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT private with default config: expected 201, got %d", resp.StatusCode)
	}

	// Upstream should NOT have received the PUT
	time.Sleep(50 * time.Millisecond)
	if putReceived.Load() {
		t.Fatal("private object should NOT be forwarded to upstream with default upstream_push")
	}
}

// TestProxyPutUpstreamPushAllPendingSyncOnFailure verifies that when
// upstream_push=all and upstream is down, private objects go to sync_pending.
func TestProxyPutUpstreamPushAllPendingSyncOnFailure(t *testing.T) {
	proxyDir := t.TempDir()
	proxyStore, _ := storage.NewStore(proxyDir, true)
	proxyShared := realm.NewSharedRealms()
	proxyIndex := storage.NewIndex(proxyShared)
	proxyLimiter := auth.NewRateLimiter(10000, 1000000)
	defer proxyLimiter.Stop()

	up := upstream.NewClient("http://127.0.0.1:1") // will fail immediately
	pendingDir := filepath.Join(proxyDir, "sync_pending")
	pending := upstream.NewSyncPending(pendingDir, up, proxyStore, proxyIndex)

	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	defer proxyAuth.Stop()
	proxy := serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", up, pending, proxyShared)
	proxy.UpstreamPush = "all"
	proxySrv := httptest.NewServer(proxy.Router())
	defer proxySrv.Close()

	priv, pubkey := testKeypair(t)
	id := "cccc3333-3333-4333-8333-333333333333"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{pubkey}, "NOTE")

	resp := doPut(t, proxySrv, ref, data)
	resp.Body.Close()

	// Should get 202 (stored locally, sync pending)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("PUT private with upstream down: expected 202, got %d", resp.StatusCode)
	}

	// Check sync_pending has the file
	entries, _ := os.ReadDir(pendingDir)
	found := false
	for _, e := range entries {
		if e.Name() == ref+".json" {
			found = true
		}
	}
	if !found {
		t.Error("expected private object in sync_pending/ when upstream_push=all and upstream down")
	}
}

// forgeRevision patches the revision in a JSON envelope (for testing only).
func forgeRevision(t *testing.T, data []byte, rev int) []byte {
	t.Helper()
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	var itemRaw map[string]json.RawMessage
	json.Unmarshal(raw["item"], &itemRaw)
	itemRaw["revision"], _ = json.Marshal(rev)
	raw["item"], _ = json.Marshal(itemRaw)
	result, _ := json.Marshal(raw)
	return result
}

// sleepMs is a test helper for short waits.
func sleepMs(t *testing.T, ms int) {
	t.Helper()
	<-time.After(time.Duration(ms) * time.Millisecond)
}
