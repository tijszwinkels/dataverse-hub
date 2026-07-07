package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/events"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/upstream"
)

// eventsChain is a root hub + proxy, both with journals, and an upstream
// events subscriber connecting them.
type eventsChain struct {
	rootSrv  *httptest.Server
	rootHub  *serving.Hub
	rootLog  *events.Log
	rootDir  string
	proxySrv *httptest.Server
	proxy    *serving.Proxy
	proxyLog *events.Log
	proxyDir string
	sub      *upstream.Subscriber
	up       *upstream.Client
}

func newEventsChain(t *testing.T) *eventsChain {
	t.Helper()
	c := &eventsChain{}

	c.rootDir = t.TempDir()
	rootStore, _ := storage.NewStore(c.rootDir, true)
	rootShared := realm.NewSharedRealms()
	rootIndex := storage.NewIndex(rootShared)
	rootLimiter := auth.NewRateLimiter(100000, 10000000)
	rootAuth := auth.NewAuthStore(168 * time.Hour)
	var err error
	c.rootLog, err = events.Open(filepath.Join(c.rootDir, "events"), events.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c.rootHub = serving.NewHub(rootStore, rootIndex, rootLimiter, rootAuth, "", rootShared)
	c.rootHub.Events = c.rootLog
	c.rootSrv = httptest.NewServer(c.rootHub.Router())

	c.proxyDir = t.TempDir()
	proxyStore, _ := storage.NewStore(c.proxyDir, true)
	proxyShared := realm.NewSharedRealms()
	proxyIndex := storage.NewIndex(proxyShared)
	proxyLimiter := auth.NewRateLimiter(100000, 10000000)
	proxyAuth := auth.NewAuthStore(168 * time.Hour)
	c.proxyLog, err = events.Open(filepath.Join(c.proxyDir, "events"), events.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c.up = upstream.NewClient(c.rootSrv.URL)
	pending := upstream.NewSyncPending(filepath.Join(c.proxyDir, "sync_pending"), c.up, proxyStore, proxyIndex)
	c.proxy = serving.NewProxy(proxyStore, proxyIndex, proxyLimiter, proxyAuth, "", c.up, pending, proxyShared)
	c.proxy.Events = c.proxyLog
	c.proxySrv = httptest.NewServer(c.proxy.Router())

	t.Cleanup(func() {
		if c.sub != nil {
			c.sub.Stop()
		}
		c.rootLog.Close()
		c.proxyLog.Close()
		c.rootSrv.Close()
		c.proxySrv.Close()
		rootLimiter.Stop()
		proxyLimiter.Stop()
		rootAuth.Stop()
		proxyAuth.Stop()
	})
	return c
}

// startSubscriber wires the proxy's apply/reset callbacks to the root.
func (c *eventsChain) startSubscriber(t *testing.T) {
	t.Helper()
	c.sub = upstream.NewSubscriber(
		c.rootSrv.URL,
		filepath.Join(c.proxyDir, "events", "upstream-cursor.json"),
		c.up,
		upstream.SubscriberCallbacks{
			OnEvent: c.proxy.ApplyUpstreamEvent,
			OnReset: c.proxy.RevalidateAgainstUpstream,
		},
	)
	c.sub.Start()
}

// waitFor polls until cond returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// proxyEventsSince fetches the proxy's own change feed.
func proxyEventsSince(t *testing.T, c *eventsChain, since string) eventsPage {
	t.Helper()
	url := c.proxySrv.URL + "/events"
	if since != "" {
		url += "?since=" + since
	}
	return getEventsPage(t, url, "")
}

func TestProxyEmitsEventForLocalGlobalPut(t *testing.T) {
	c := newEventsChain(t)
	head := proxyEventsSince(t, c, "").NextCursor

	ref, env, _ := signedNote(t, "11110000-1111-4222-8333-444444444444", []string{"dataverse001"})
	putSignedObject(t, c.proxySrv.URL, env, ref)

	page := proxyEventsSince(t, c, head)
	if len(page.Events) != 1 || page.Events[0].Ref != ref {
		t.Fatalf("proxy events = %+v, want [%s]", page.Events, ref)
	}
	// Proxy cursors belong to the proxy's journal, not the root's.
	if got, rootEpoch := page.Events[0].Cursor, c.rootLog.Epoch(); got[:len(rootEpoch)] == rootEpoch {
		t.Errorf("proxy event cursor %q re-uses the root's epoch — cursors must be per-hop", got)
	}
}

func TestProxyEmitsEventForPrivateLocalPut(t *testing.T) {
	c := newEventsChain(t)
	head := proxyEventsSince(t, c, "").NextCursor

	priv, pubkey := testKeypair(t)
	item := map[string]any{
		"id":         "22220000-1111-4222-8333-444444444444",
		"pubkey":     pubkey,
		"created_at": "2026-07-08T10:00:00Z",
		"in":         []string{pubkey}, // identity realm: stored locally only
		"type":       "NOTE",
		"content":    map[string]any{"text": "private"},
	}
	ref := pubkey + ".22220000-1111-4222-8333-444444444444"
	putSignedObject(t, c.proxySrv.URL, buildSignedObject(t, priv, item), ref)

	// Invisible to anonymous subscribers…
	page := proxyEventsSince(t, c, head)
	if len(page.Events) != 0 {
		t.Fatalf("anon sees private event: %+v", page.Events)
	}
	// …visible to the owner.
	token := authenticateAs(t, c.proxySrv, priv, pubkey)
	page = getEventsPage(t, c.proxySrv.URL+"/events?since="+head, token)
	if len(page.Events) != 1 || page.Events[0].Ref != ref {
		t.Fatalf("owner events = %+v, want [%s]", page.Events, ref)
	}
}

func TestUpstreamEventsFlowDownstream(t *testing.T) {
	c := newEventsChain(t)
	head := proxyEventsSince(t, c, "").NextCursor
	c.startSubscriber(t)

	// Write directly to the ROOT — the proxy must hear about it via its
	// upstream subscription and re-emit into its own journal.
	ref, env, _ := signedNote(t, "33330000-1111-4222-8333-444444444444", []string{"dataverse001"})
	putSignedObject(t, c.rootSrv.URL, env, ref)

	waitFor(t, 5*time.Second, "event to flow downstream", func() bool {
		page := proxyEventsSince(t, c, head)
		for _, ev := range page.Events {
			if ev.Ref == ref {
				return true
			}
		}
		return false
	})

	// Default is cache-on-demand: the pass-through event must NOT have
	// materialized the object in the proxy store.
	if _, err := os.Stat(filepath.Join(c.proxyDir, ref+".json")); !os.IsNotExist(err) {
		t.Errorf("pass-through event should not prefetch the object (err=%v)", err)
	}
}

func TestUpstreamEventsPrefetch(t *testing.T) {
	c := newEventsChain(t)
	c.proxy.EventsPrefetch = true
	c.startSubscriber(t)

	ref, env, _ := signedNote(t, "44440000-1111-4222-8333-444444444444", []string{"dataverse001"})
	putSignedObject(t, c.rootSrv.URL, env, ref)

	waitFor(t, 5*time.Second, "prefetch to cache the object", func() bool {
		_, err := os.Stat(filepath.Join(c.proxyDir, ref+".json"))
		return err == nil
	})
}

func TestEchoSuppression(t *testing.T) {
	c := newEventsChain(t)
	head := proxyEventsSince(t, c, "").NextCursor
	c.startSubscriber(t)

	// PUT through the proxy: forwarded upstream + journaled locally. The
	// root then emits the same (ref, revision) back down the subscription;
	// the proxy must not journal it twice.
	ref, env, _ := signedNote(t, "55550000-1111-4222-8333-444444444444", []string{"dataverse001"})
	putSignedObject(t, c.proxySrv.URL, env, ref)

	// Wait until the echo has certainly arrived (root journals it, the
	// subscriber processes it), then count.
	waitFor(t, 5*time.Second, "root to journal the forwarded PUT", func() bool {
		evs, _, _ := c.rootLog.ReadSince(c.rootLog.Epoch()+":000000000000", 100)
		return len(evs) >= 1
	})
	time.Sleep(300 * time.Millisecond) // give the echo time to (wrongly) double-journal

	page := proxyEventsSince(t, c, head)
	count := 0
	for _, ev := range page.Events {
		if ev.Ref == ref {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("proxy journaled %d events for %s, want exactly 1 (echo must be suppressed)", count, ref)
	}
}

func TestSubscriberResumesFromPersistedCursor(t *testing.T) {
	c := newEventsChain(t)
	head := proxyEventsSince(t, c, "").NextCursor
	c.startSubscriber(t)

	// First event flows live.
	ref1, env1, _ := signedNote(t, "66660000-1111-4222-8333-444444444444", []string{"dataverse001"})
	putSignedObject(t, c.rootSrv.URL, env1, ref1)
	waitFor(t, 5*time.Second, "first event downstream", func() bool {
		page := proxyEventsSince(t, c, head)
		return len(page.Events) >= 1
	})

	// Subscriber goes away (proxy restart); root keeps changing.
	c.sub.Stop()
	ref2, env2, _ := signedNote(t, "77770000-1111-4222-8333-444444444444", []string{"dataverse001"})
	putSignedObject(t, c.rootSrv.URL, env2, ref2)

	// New subscriber resumes from the persisted cursor and must deliver the
	// missed event without a reset sweep.
	c.startSubscriber(t)
	waitFor(t, 5*time.Second, "missed event replayed after resume", func() bool {
		page := proxyEventsSince(t, c, head)
		for _, ev := range page.Events {
			if ev.Ref == ref2 {
				return true
			}
		}
		return false
	})
}

func TestUpstreamJournalLossTriggersRevalidation(t *testing.T) {
	c := newEventsChain(t)
	c.startSubscriber(t)

	// Proxy caches an object at revision 0 by serving a client GET.
	priv, pubkey := testKeypair(t)
	noteItem := map[string]any{
		"id":         "88880000-1111-4222-8333-444444444444",
		"pubkey":     pubkey,
		"created_at": "2026-07-08T10:00:00Z",
		"in":         []string{"dataverse001"},
		"type":       "NOTE",
		"content":    map[string]any{"text": "v0"},
	}
	ref := pubkey + ".88880000-1111-4222-8333-444444444444"
	putSignedObject(t, c.rootSrv.URL, buildSignedObject(t, priv, noteItem), ref)
	req, _ := http.NewRequest(http.MethodGet, c.proxySrv.URL+"/"+ref, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitFor(t, 2*time.Second, "object cached at proxy", func() bool {
		_, err := os.Stat(filepath.Join(c.proxyDir, ref+".json"))
		return err == nil
	})
	proxyHead := proxyEventsSince(t, c, "").NextCursor

	// Upstream loses its journal while the subscriber is disconnected, and
	// the object changes meanwhile.
	c.sub.Stop()
	c.rootLog.Close()
	os.RemoveAll(filepath.Join(c.rootDir, "events"))
	newLog, err := events.Open(filepath.Join(c.rootDir, "events"), events.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c.rootHub.Events = newLog
	t.Cleanup(func() { newLog.Close() })
	c.rootLog = newLog

	// Bump the object to revision 1 (updated_at + revision, resigned).
	noteItem["revision"] = 1
	noteItem["updated_at"] = "2026-07-08T11:00:00Z"
	noteItem["content"] = map[string]any{"text": "v1"}
	putSignedObject(t, c.rootSrv.URL, buildSignedObject(t, priv, noteItem), ref)

	// Reconnect: stale cursor (old epoch) → reset → revalidation sweep must
	// fetch revision 1 and journal it — WITHOUT resetting the proxy's own
	// journal (downstream cursors stay valid).
	c.startSubscriber(t)

	waitFor(t, 10*time.Second, "revalidation to refresh the cached object", func() bool {
		page := proxyEventsSince(t, c, proxyHead)
		for _, ev := range page.Events {
			if ev.Ref == ref && ev.Revision == 1 {
				return true
			}
		}
		return false
	})

	// The pre-reset downstream cursor was honored (no reset flag).
	page := proxyEventsSince(t, c, proxyHead)
	if page.Reset {
		t.Fatalf("proxy journal reset leaked downstream — per-hop cursors must absorb upstream resets")
	}
}
