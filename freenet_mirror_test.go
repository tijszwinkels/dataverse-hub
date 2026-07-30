package main

import (
	"crypto/ecdsa"
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
	"github.com/tijszwinkels/dataverse-hub/freenet"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
)

// mirrorHarness is a hub wired to a Freenet mirror backed by the fake
// publisher script, plus a way to read back what the publisher was asked to
// publish.
type mirrorHarness struct {
	ts          *httptest.Server
	mirror      *freenet.Mirror
	invocations func() []string
}

// newMirrorHub builds a hub whose mirror runs the fake publisher. Passing
// enabled=false wires no mirror at all, which is what the hub does when
// [freenet] is absent or disabled.
func newMirrorHub(t *testing.T, enabled bool) *mirrorHarness {
	t.Helper()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "invocations.log")
	t.Setenv("FAKE_PUBLISH_LOG", logPath)

	store, err := storage.NewStore(filepath.Join(dir, "store"), true)
	if err != nil {
		t.Fatal(err)
	}
	shared := realm.NewSharedRealms()
	index := storage.NewIndex(shared)
	limiter := auth.NewRateLimiter(1000, 100000)
	authStore := auth.NewAuthStore(168 * time.Hour)

	hub := serving.NewHub(store, index, limiter, authStore, "", shared)

	var mirror *freenet.Mirror
	if enabled {
		script, err := filepath.Abs("freenet/testdata/fake-publish.sh")
		if err != nil {
			t.Fatal(err)
		}
		mirror, err = freenet.New(freenet.Options{
			QueueDir:   filepath.Join(dir, "queue"),
			PublishCmd: script,
			Timeout:    10 * time.Second,
			Retries:    0,
		})
		if err != nil {
			t.Fatalf("freenet.New: %v", err)
		}
		mirror.Start()
		hub.Mirror = mirror
	}

	ts := httptest.NewServer(hub.Router())
	t.Cleanup(func() {
		ts.Close()
		mirror.Stop()
		limiter.Stop()
		authStore.Stop()
	})

	return &mirrorHarness{
		ts:     ts,
		mirror: mirror,
		invocations: func() []string {
			data, err := os.ReadFile(logPath)
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				t.Fatal(err)
			}
			var lines []string
			for _, l := range strings.Split(string(data), "\n") {
				if strings.TrimSpace(l) != "" {
					lines = append(lines, l)
				}
			}
			return lines
		},
	}
}

// putObject signs an item into the given realms and PUTs it, returning the ref
// and the response.
func putObject(t *testing.T, ts *httptest.Server, priv *ecdsa.PrivateKey, pubkey, id string, realms []string) (string, *http.Response) {
	t.Helper()
	item := map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-07-28T10:00:00+02:00",
		"in":         realms,
		"type":       "NOTE",
		"content":    map[string]any{"text": "mirror me"},
	}
	body := buildSignedObject(t, priv, item)
	ref := pubkey + "." + id
	return ref, doPut(t, ts, ref, body)
}

func waitForMirror(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestMirrorPublishesAcceptedPublicObject(t *testing.T) {
	h := newMirrorHub(t, true)
	priv, pubkey := testKeypair(t)

	ref, resp := putObject(t, h.ts, priv, pubkey, "11111111-1111-4111-8111-111111111111", []string{"dataverse001"})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT = %d, want 201: %s", resp.StatusCode, body)
	}

	waitForMirror(t, "the object to be mirrored", func() bool { return h.mirror.Status().Succeeded == 1 })

	got := h.invocations()
	if len(got) != 1 {
		t.Fatalf("%d publisher invocations, want 1", len(got))
	}
	// The publisher must receive the stored, canonical envelope — the exact
	// bytes the hub served back.
	var env map[string]any
	if err := json.Unmarshal([]byte(got[0]), &env); err != nil {
		t.Fatalf("publisher received non-JSON: %v", err)
	}
	if env["signature"] == nil || env["item"] == nil {
		t.Errorf("publisher received %s, want a full signed envelope", got[0])
	}
	if st := h.mirror.Status(); len(st.Recent) != 1 || st.Recent[0].Ref != ref {
		t.Errorf("Recent = %+v, want an event for %s", st.Recent, ref)
	}
}

// The leak test at the hub boundary: a private write must never reach the
// publisher, however it was accepted.
func TestMirrorNeverPublishesPrivateObjects(t *testing.T) {
	h := newMirrorHub(t, true)
	priv, pubkey := testKeypair(t)

	cases := []struct {
		name   string
		id     string
		realms []string
	}{
		{"identity realm only", "22222222-2222-4222-8222-222222222222", []string{pubkey}},
		{"server-public", "33333333-3333-4333-8333-333333333333", []string{"server-public"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, resp := putObject(t, h.ts, priv, pubkey, tc.id, tc.realms)
			if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("PUT = %d, want the write to be accepted: %s", resp.StatusCode, body)
			}
		})
	}

	// Give the worker every chance to do the wrong thing.
	time.Sleep(200 * time.Millisecond)
	if got := h.invocations(); len(got) != 0 {
		t.Fatalf("private objects leaked to the publisher: %v", got)
	}
	if st := h.mirror.Status(); st.QueueDepth != 0 || st.Succeeded != 0 {
		t.Fatalf("mirror status = %+v, want nothing queued or published", st)
	}
}

// With the mirror off the hub must behave exactly as it did before this
// feature existed.
func TestMirrorDisabledIsANoOp(t *testing.T) {
	h := newMirrorHub(t, false)
	priv, pubkey := testKeypair(t)

	ref, resp := putObject(t, h.ts, priv, pubkey, "44444444-4444-4444-8444-444444444444", []string{"dataverse001"})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT = %d, want 201: %s", resp.StatusCode, body)
	}

	// The object is stored and served normally.
	get, err := http.Get(h.ts.URL + "/" + ref)
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200", get.StatusCode)
	}

	time.Sleep(150 * time.Millisecond)
	if got := h.invocations(); len(got) != 0 {
		t.Fatalf("disabled mirror still published: %v", got)
	}
}

// A slow or broken publisher must never be visible to the writing client.
func TestMirrorFailureDoesNotAffectClientWrite(t *testing.T) {
	t.Setenv("FAKE_PUBLISH_EXIT", "3")
	h := newMirrorHub(t, true)
	priv, pubkey := testKeypair(t)

	_, resp := putObject(t, h.ts, priv, pubkey, "55555555-5555-4555-8555-555555555555", []string{"dataverse001"})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT = %d, want 201 despite the publisher failing: %s", resp.StatusCode, body)
	}

	waitForMirror(t, "the mirror to record the failure", func() bool { return h.mirror.Status().Failed == 1 })
}

func TestMirrorSlowPublisherDoesNotDelayWrite(t *testing.T) {
	t.Setenv("FAKE_PUBLISH_SLEEP", "30")
	h := newMirrorHub(t, true)
	priv, pubkey := testKeypair(t)

	start := time.Now()
	_, resp := putObject(t, h.ts, priv, pubkey, "66666666-6666-4666-8666-666666666666", []string{"dataverse001"})
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", resp.StatusCode)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("PUT took %v while the publisher slept — the write path must not wait on Freenet", elapsed)
	}
}

func TestFreenetStatusRequiresAuth(t *testing.T) {
	h := newMirrorHub(t, true)

	resp, err := http.Get(h.ts.URL + "/freenet/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /freenet/status = %d, want 401", resp.StatusCode)
	}
}

func TestFreenetStatusReportsJobs(t *testing.T) {
	h := newMirrorHub(t, true)
	priv, pubkey := testKeypair(t)

	ref, resp := putObject(t, h.ts, priv, pubkey, "77777777-7777-4777-8777-777777777777", []string{"dataverse001"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", resp.StatusCode)
	}
	waitForMirror(t, "the object to be mirrored", func() bool { return h.mirror.Status().Succeeded == 1 })

	token := authenticateAs(t, h.ts, priv, pubkey)
	statusResp := doGetWithToken(t, h.ts, "/freenet/status", token)
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(statusResp.Body)
		t.Fatalf("GET /freenet/status = %d, want 200: %s", statusResp.StatusCode, body)
	}
	// Set by the router's jsonContentType middleware, like every other
	// endpoint — asserted here so removing that middleware is a test failure
	// rather than a silently untyped response.
	if ct := statusResp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var st freenet.Status
	if err := json.NewDecoder(statusResp.Body).Decode(&st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !st.Enabled {
		t.Error("enabled = false, want true")
	}
	if st.Succeeded != 1 || st.Failed != 0 || st.QueueDepth != 0 || st.InFlight != 0 {
		t.Errorf("status = %+v, want 1 succeeded and nothing outstanding", st)
	}
	if len(st.Recent) != 1 || st.Recent[0].Ref != ref || st.Recent[0].Status != "succeeded" {
		t.Errorf("recent = %+v, want one succeeded event for %s", st.Recent, ref)
	}
}

// With the mirror off the route must not exist at all: a hub with no
// [freenet] section has exactly the routing table it had before this feature.
func TestFreenetStatusRouteAbsentWhenDisabled(t *testing.T) {
	h := newMirrorHub(t, false)
	priv, pubkey := testKeypair(t)

	token := authenticateAs(t, h.ts, priv, pubkey)
	resp := doGetWithToken(t, h.ts, "/freenet/status", token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /freenet/status = %d, want 404 when the mirror is disabled: %s", resp.StatusCode, body)
	}

	// Unauthenticated too — the endpoint must not even hint that it exists.
	anon, err := http.Get(h.ts.URL + "/freenet/status")
	if err != nil {
		t.Fatal(err)
	}
	defer anon.Body.Close()
	if anon.StatusCode != http.StatusNotFound {
		t.Fatalf("unauthenticated GET /freenet/status = %d, want 404", anon.StatusCode)
	}
}

// The status payload must not echo the publisher's raw output: any keypair can
// complete the challenge flow and read this endpoint.
func TestFreenetStatusOmitsPublisherOutput(t *testing.T) {
	t.Setenv("FAKE_PUBLISH_EXIT", "3")
	h := newMirrorHub(t, true)
	priv, pubkey := testKeypair(t)

	_, resp := putObject(t, h.ts, priv, pubkey, "88888888-8888-4888-8888-888888888888", []string{"dataverse001"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", resp.StatusCode)
	}
	waitForMirror(t, "the mirror to give up", func() bool { return h.mirror.Status().Failed == 1 })

	token := authenticateAs(t, h.ts, priv, pubkey)
	statusResp := doGetWithToken(t, h.ts, "/freenet/status", token)
	defer statusResp.Body.Close()
	body, _ := io.ReadAll(statusResp.Body)

	if strings.Contains(string(body), "fake-publish") {
		t.Errorf("status leaks publisher output to any authenticated caller: %s", body)
	}
	if !strings.Contains(string(body), `"failed":1`) || !strings.Contains(string(body), `"failed_queued":1`) {
		t.Errorf("status = %s, want failed:1 and failed_queued:1", body)
	}
}
