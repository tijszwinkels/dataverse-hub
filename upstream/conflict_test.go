package upstream

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/storage"
)

func conflictCandidates(t *testing.T) (string, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk := base64.RawURLEncoding.EncodeToString(elliptic.MarshalCompressed(key.Curve, key.X, key.Y))
	ref := pk + ".00000000-0000-4000-8000-000000000001"
	makeObject := func(text string) []byte {
		item, _ := json.Marshal(map[string]any{"pubkey": pk, "id": "00000000-0000-4000-8000-000000000001", "ref": ref,
			"in": []string{"dataverse001"}, "created_at": "2026-09-05T00:00:00Z", "revision": 1, "content": map[string]string{"text": text}})
		canonical, err := object.CanonicalJSON(item)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(canonical)
		sig, err := ecdsa.SignASN1(rand.Reader, key, hash[:])
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(object.Envelope{Is: "instructionGraph001", Item: item, Signature: base64.StdEncoding.EncodeToString(sig)})
		return data
	}
	return ref, makeObject("local edit"), makeObject("upstream edit")
}

func TestPendingConflictPreservesEditWithoutReplacingLocal(t *testing.T) {
	ref, local, remote := conflictCandidates(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(remote)
	}))
	defer srv.Close()
	dir := t.TempDir()
	store, err := storage.NewStore(dir, false) // preservation must not depend on ordinary backups
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ref, local, time.Now()); err != nil {
		t.Fatal(err)
	}
	sp := NewSyncPending(filepath.Join(dir, "sync_pending"), NewClient(srv.URL), store, nil)
	if err := sp.Add(ref, local); err != nil {
		t.Fatal(err)
	}
	if !sp.pushOne(ref) {
		t.Fatal("conflict should leave the retry queue after preservation")
	}
	got, _ := store.Read(ref)
	if !bytes.Equal(got, local) {
		t.Fatal("conflict silently replaced the local edit")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "sync_conflicts", "*.json"))
	if len(files) != 1 {
		t.Fatalf("expected one preserved pending edit, got %d", len(files))
	}
	saved, _ := os.ReadFile(files[0])
	if !bytes.Equal(saved, local) {
		t.Fatal("archive does not contain rejected edit")
	}
	refs, _ := sp.List()
	if len(refs) != 0 {
		t.Fatal("resolved retry should leave queue")
	}
}

func TestPendingConflictArchiveFailureKeepsQueue(t *testing.T) {
	ref, local, remote := conflictCandidates(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.Write(remote)
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sync_conflicts"), []byte("block archive directory"), 0600); err != nil {
		t.Fatal(err)
	}
	sp := NewSyncPending(filepath.Join(dir, "sync_pending"), NewClient(srv.URL), nil, nil)
	if err := sp.Add(ref, local); err != nil {
		t.Fatal(err)
	}
	if sp.pushOne(ref) {
		t.Fatal("archive failure should stop retries")
	}
	got, _ := os.ReadFile(filepath.Join(sp.dir, ref+".json"))
	if !bytes.Equal(got, local) {
		t.Fatal("archive failure discarded pending edit")
	}
}

func TestPendingReplyDoesNotRemoveAConcurrentEdit(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ref, original, replacement := conflictCandidates(t)
			var sp *SyncPending
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					if err := sp.Add(ref, replacement); err != nil {
						t.Error(err)
					}
					w.WriteHeader(status)
					return
				}
				w.Write(replacement)
			}))
			defer srv.Close()
			sp = NewSyncPending(filepath.Join(t.TempDir(), "sync_pending"), NewClient(srv.URL), nil, nil)
			if err := sp.Add(ref, original); err != nil {
				t.Fatal(err)
			}
			sp.pushOne(ref)
			got, _ := os.ReadFile(filepath.Join(sp.dir, ref+".json"))
			if !bytes.Equal(got, replacement) {
				t.Fatal("reply for old upload removed a newer pending edit")
			}
		})
	}
}

func TestPendingIdenticalReplayIsNotAConflict(t *testing.T) {
	ref, local, _ := conflictCandidates(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.Write(local)
	}))
	defer srv.Close()
	dir := t.TempDir()
	sp := NewSyncPending(filepath.Join(dir, "sync_pending"), NewClient(srv.URL), nil, nil)
	if err := sp.Add(ref, local); err != nil {
		t.Fatal(err)
	}
	if !sp.pushOne(ref) {
		t.Fatal("identical replay should be acknowledged")
	}
	refs, _ := sp.List()
	if len(refs) != 0 {
		t.Fatal("identical replay left in queue")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "sync_conflicts", "*.json"))
	if len(files) != 0 {
		t.Fatal("identical replay should not create a conflict")
	}
}
