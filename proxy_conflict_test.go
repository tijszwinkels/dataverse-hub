package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/upstream"
)

func conflictingProxy(t *testing.T, private, identical bool) (*httptest.Server, *storage.Store, string, string, []byte, []byte) {
	t.Helper()
	key, pk := testKeypair(t)
	id := "00000000-0000-4000-8000-000000000001"
	ref := pk + "." + id
	localRealms := []string{"dataverse001"}
	if private {
		localRealms = []string{pk}
	}
	item := map[string]any{"pubkey": pk, "id": id, "ref": ref, "in": localRealms, "revision": 1,
		"created_at": "2026-09-05T00:00:00Z", "type": "NOTE", "content": map[string]string{"text": "local"}}
	local := buildSignedObject(t, key, item)
	if !identical {
		item["content"] = map[string]string{"text": "upstream"}
		item["in"] = []string{"dataverse001"}
	}
	remote := buildSignedObject(t, key, item) // a new signature alone is not a conflict
	root := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"1"` {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/search") || strings.HasSuffix(r.URL.Path, "/inbound") {
			json.NewEncoder(w).Encode(object.ListResponse{Items: []json.RawMessage{remote}})
			return
		}
		w.Write(remote)
	}))
	t.Cleanup(root.Close)
	dir := t.TempDir()
	store, err := storage.NewStore(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ref, local, time.Now()); err != nil {
		t.Fatal(err)
	}
	srv := conflictTestProxy(t, root.URL, store, dir)
	return srv, store, dir, ref, local, remote
}

func conflictTestProxy(t *testing.T, upstreamURL string, store *storage.Store, dir string) *httptest.Server {
	t.Helper()
	shared := realm.NewSharedRealms()
	index := storage.NewIndex(shared)
	if _, _, err := index.Rebuild(store); err != nil {
		t.Fatal(err)
	}
	limiter := auth.NewRateLimiter(10000, 100000)
	authStore := auth.NewAuthStore(time.Hour)
	t.Cleanup(limiter.Stop)
	t.Cleanup(authStore.Stop)
	up := upstream.NewClient(upstreamURL)
	pending := upstream.NewSyncPending(filepath.Join(dir, "sync_pending"), up, store, index)
	proxy := serving.NewProxy(store, index, limiter, authStore, "", up, pending, shared)
	srv := httptest.NewServer(proxy.Router())
	t.Cleanup(srv.Close)
	return srv
}

func TestProxyPropagatesConflictFromAnotherProxy(t *testing.T) {
	first, _, _, ref, _, _ := conflictingProxy(t, false, false)
	dir := t.TempDir()
	store, err := storage.NewStore(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	second := conflictTestProxy(t, first.URL, store, dir)
	for _, path := range []string{"/" + ref, "/search", "/" + ref + "/inbound"} {
		resp := doGet(t, second, path)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("REVISION_CONFLICT")) {
			t.Errorf("%s: conflict concealed as %d: %s", path, resp.StatusCode, body)
		}
	}
}

func TestProxyAcceptedUploadDoesNotOverwriteLocalConflict(t *testing.T) {
	srv, store, dir, ref, local, remote := conflictingProxy(t, false, false)
	resp := doPut(t, srv, ref, remote)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("wanted 409, got %d", resp.StatusCode)
	}
	got, _ := store.Read(ref)
	if !bytes.Equal(got, local) {
		t.Fatal("accepted upload response replaced a conflicting local edit")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "conflicts", "*.json"))
	if len(files) != 1 {
		t.Fatalf("expected preserved upload, got %d archives", len(files))
	}
	saved, _ := os.ReadFile(files[0])
	same, err := object.SameItem(saved, remote)
	if err != nil || !same {
		t.Fatalf("wrong archived upload: %v", err)
	}
}

func TestProxyReportsEqualRevisionConflictsOnReads(t *testing.T) {
	srv, store, dir, ref, local, remote := conflictingProxy(t, false, false)
	for _, path := range []string{"/" + ref, "/" + ref + "/json", "/search", "/" + ref + "/inbound"} {
		resp := doGet(t, srv, path)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s: wanted 409, got %d", path, resp.StatusCode)
		}
		if !bytes.Contains(body, []byte("REVISION_CONFLICT")) {
			t.Errorf("%s: missing conflict code", path)
		}
	}
	got, _ := store.Read(ref)
	if !bytes.Equal(got, local) {
		t.Fatal("local edit replaced while reading")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "conflicts", "*.json"))
	if len(files) != 1 {
		t.Fatalf("expected one preserved upstream candidate, got %d", len(files))
	}
	saved, _ := os.ReadFile(files[0])
	if !bytes.Equal(saved, remote) {
		t.Fatal("preserved wrong candidate")
	}
}

func TestProxyConflictArchiveFailureIsReported(t *testing.T) {
	srv, store, dir, ref, local, _ := conflictingProxy(t, false, false)
	if err := os.WriteFile(filepath.Join(dir, "conflicts"), []byte("block archive"), 0600); err != nil {
		t.Fatal(err)
	}
	resp := doGet(t, srv, "/"+ref)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("wanted 500, got %d", resp.StatusCode)
	}
	got, _ := store.Read(ref)
	if !bytes.Equal(got, local) {
		t.Fatal("archive failure replaced local edit")
	}
}

func TestProxyResigningTheSameItemIsNotAConflict(t *testing.T) {
	srv, _, dir, ref, _, _ := conflictingProxy(t, false, true)
	for _, path := range []string{"/" + ref, "/search"} {
		resp := doGet(t, srv, path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: wanted 200, got %d", path, resp.StatusCode)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "conflicts", "*.json"))
	if len(files) != 0 {
		t.Fatal("same item created a conflict")
	}
}

func TestProxyConflictDoesNotRevealAnInaccessibleLocalCopy(t *testing.T) {
	srv, _, _, ref, _, _ := conflictingProxy(t, true, false)
	resp := doGet(t, srv, "/"+ref)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wanted 404 for private local copy, got %d", resp.StatusCode)
	}
	resp = doGet(t, srv, "/search")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search disclosed hidden conflict with %d", resp.StatusCode)
	}
}
