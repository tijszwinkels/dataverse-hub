package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/events"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
)

// testHubWithEvents builds a root-mode hub with an events journal attached.
func testHubWithEvents(t *testing.T) (*httptest.Server, *events.Log, func()) {
	t.Helper()
	dir := t.TempDir()
	store, _ := storage.NewStore(dir, true)
	shared := realm.NewSharedRealms()
	index := storage.NewIndex(shared)
	limiter := auth.NewRateLimiter(100000, 10000000)
	authStore := auth.NewAuthStore(168 * time.Hour)
	elog, err := events.Open(dir+"/events", events.Options{})
	if err != nil {
		t.Fatal(err)
	}
	hub := serving.NewHub(store, index, limiter, authStore, "", shared)
	hub.Events = elog
	srv := httptest.NewServer(hub.Router())
	return srv, elog, func() {
		// Close the journal first: it closes all subscriptions, which makes
		// SSE handlers return, which lets srv.Close() finish. Production
		// shutdown must use the same order.
		elog.Close()
		srv.Close()
		limiter.Stop()
		authStore.Stop()
	}
}

// eventsPage is the JSON envelope of GET /events.
type eventsPage struct {
	Events     []events.Event `json:"events"`
	NextCursor string         `json:"next_cursor"`
	Live       bool           `json:"live"`
	Reset      bool           `json:"reset,omitempty"`
}

func getEventsPage(t *testing.T, url, token string) eventsPage {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, body)
	}
	var page eventsPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode events page: %v", err)
	}
	return page
}

func putSignedObject(t *testing.T, srvURL string, data []byte, ref string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, srvURL+"/"+ref, strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT %s: %d %s", ref, resp.StatusCode, body)
	}
}

// signedNote builds a signed NOTE object in the given realms. Returns (ref, envelope).
func signedNote(t *testing.T, id string, realms []string) (string, []byte, string) {
	t.Helper()
	priv, pubkey := testKeypair(t)
	item := map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-07-08T10:00:00Z",
		"in":         realms,
		"type":       "NOTE",
		"content":    map[string]any{"text": "hello"},
	}
	// Identity-realm objects must be owned by their signer.
	for i, r := range realms {
		if r == "SELF" {
			realms[i] = pubkey
		}
	}
	item["in"] = realms
	return pubkey + "." + id, buildSignedObject(t, priv, item), pubkey
}

func TestEventsDisabledIs404(t *testing.T) {
	dir := t.TempDir()
	store, _ := storage.NewStore(dir, true)
	shared := realm.NewSharedRealms()
	index := storage.NewIndex(shared)
	limiter := auth.NewRateLimiter(100000, 10000000)
	defer limiter.Stop()
	authStore := auth.NewAuthStore(168 * time.Hour)
	defer authStore.Stop()
	hub := serving.NewHub(store, index, limiter, authStore, "", shared) // no Events set
	srv := httptest.NewServer(hub.Router())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("events disabled: got %d, want 404", resp.StatusCode)
	}
}

func TestEventsJSONBootstrapAndReplay(t *testing.T) {
	srv, _, cleanup := testHubWithEvents(t)
	defer cleanup()

	// Bootstrap: no since → no events, head cursor, live.
	page := getEventsPage(t, srv.URL+"/events", "")
	if len(page.Events) != 0 || page.NextCursor == "" || !page.Live {
		t.Fatalf("bootstrap page = %+v", page)
	}
	head := page.NextCursor

	ref, env, pubkey := signedNote(t, "0f0e0d0c-0b0a-4988-8776-655443322110", []string{"dataverse001"})
	putSignedObject(t, srv.URL, env, ref)

	page = getEventsPage(t, srv.URL+"/events?since="+head, "")
	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(page.Events), page)
	}
	ev := page.Events[0]
	if ev.Ref != ref || ev.Op != "put" || ev.Type != "NOTE" || ev.Pubkey != pubkey || ev.Revision != 0 {
		t.Errorf("event = %+v", ev)
	}
	if page.NextCursor != ev.Cursor || !page.Live {
		t.Errorf("next_cursor=%q live=%v, want cursor of last event and live", page.NextCursor, page.Live)
	}

	// Resume from the event's cursor: caught up.
	page = getEventsPage(t, srv.URL+"/events?since="+ev.Cursor, "")
	if len(page.Events) != 0 || !page.Live {
		t.Errorf("caught-up page = %+v", page)
	}
}

func TestEventsResetOnUnknownCursor(t *testing.T) {
	srv, _, cleanup := testHubWithEvents(t)
	defer cleanup()

	page := getEventsPage(t, srv.URL+"/events?since=deadbeef:000000000042", "")
	if !page.Reset {
		t.Fatalf("want reset=true, got %+v", page)
	}
	if page.NextCursor == "" {
		t.Fatalf("reset page must include head cursor to resume from")
	}
}

func TestEventsRealmFilteringPerSubscriber(t *testing.T) {
	srv, _, cleanup := testHubWithEvents(t)
	defer cleanup()

	head := getEventsPage(t, srv.URL+"/events", "").NextCursor

	// One public object, one identity-realm (private) object.
	pubRef, pubEnv, _ := signedNote(t, "aaaaaaaa-1111-4222-8333-444444444444", []string{"dataverse001"})
	putSignedObject(t, srv.URL, pubEnv, pubRef)

	privPriv, privPubkey := testKeypair(t)
	privItem := map[string]any{
		"id":         "bbbbbbbb-1111-4222-8333-444444444444",
		"pubkey":     privPubkey,
		"created_at": "2026-07-08T10:00:00Z",
		"in":         []string{privPubkey},
		"type":       "NOTE",
		"content":    map[string]any{"text": "secret"},
	}
	privEnv := buildSignedObject(t, privPriv, privItem)
	privRef := privPubkey + "." + "bbbbbbbb-1111-4222-8333-444444444444"
	putSignedObject(t, srv.URL, privEnv, privRef)

	// Anonymous subscriber: public event only, private ref never appears.
	page := getEventsPage(t, srv.URL+"/events?since="+head, "")
	if len(page.Events) != 1 || page.Events[0].Ref != pubRef {
		t.Fatalf("anon events = %+v, want only %s", page.Events, pubRef)
	}
	// next_cursor must still advance past the (hidden) private event so the
	// subscriber does not re-scan it forever.
	if page.NextCursor == page.Events[0].Cursor {
		t.Errorf("next_cursor should advance past filtered events")
	}

	// The owner sees both.
	token := authenticateAs(t, srv, privPriv, privPubkey)
	page = getEventsPage(t, srv.URL+"/events?since="+head, token)
	if len(page.Events) != 2 {
		t.Fatalf("owner events = %+v, want 2", page.Events)
	}

	// A different authenticated user sees only the public event.
	otherPriv, otherPubkey := testKeypair(t)
	otherToken := authenticateAs(t, srv, otherPriv, otherPubkey)
	page = getEventsPage(t, srv.URL+"/events?since="+head, otherToken)
	if len(page.Events) != 1 || page.Events[0].Ref != pubRef {
		t.Fatalf("other-user events = %+v, want only %s", page.Events, pubRef)
	}
}

func TestEventsTypeFilter(t *testing.T) {
	srv, _, cleanup := testHubWithEvents(t)
	defer cleanup()
	head := getEventsPage(t, srv.URL+"/events", "").NextCursor

	noteRef, noteEnv, _ := signedNote(t, "cccccccc-1111-4222-8333-444444444444", []string{"dataverse001"})
	putSignedObject(t, srv.URL, noteEnv, noteRef)

	priv, pubkey := testKeypair(t)
	taskItem := map[string]any{
		"id":         "dddddddd-1111-4222-8333-444444444444",
		"pubkey":     pubkey,
		"created_at": "2026-07-08T10:00:00Z",
		"in":         []string{"dataverse001"},
		"type":       "TASK",
		"content":    map[string]any{"title": "t"},
	}
	taskRef := pubkey + ".dddddddd-1111-4222-8333-444444444444"
	putSignedObject(t, srv.URL, buildSignedObject(t, priv, taskItem), taskRef)

	page := getEventsPage(t, srv.URL+"/events?since="+head+"&type=TASK", "")
	if len(page.Events) != 1 || page.Events[0].Ref != taskRef {
		t.Fatalf("type-filtered events = %+v, want only %s", page.Events, taskRef)
	}
}

func TestEventsLongPollWait(t *testing.T) {
	srv, _, cleanup := testHubWithEvents(t)
	defer cleanup()
	head := getEventsPage(t, srv.URL+"/events", "").NextCursor

	ref, env, _ := signedNote(t, "eeeeeeee-1111-4222-8333-444444444444", []string{"dataverse001"})
	go func() {
		time.Sleep(150 * time.Millisecond)
		putSignedObject(t, srv.URL, env, ref)
	}()

	start := time.Now()
	page := getEventsPage(t, srv.URL+"/events?since="+head+"&wait=5s", "")
	if len(page.Events) != 1 || page.Events[0].Ref != ref {
		t.Fatalf("long-poll events = %+v, want [%s]", page.Events, ref)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("long-poll took %v, should return promptly after the PUT", time.Since(start))
	}
}

// sseFrame is one parsed server-sent event.
type sseFrame struct {
	id    string
	event string
	data  string
}

// readSSE reads frames from an SSE body into a channel until the body closes.
func readSSE(body io.Reader) <-chan sseFrame {
	out := make(chan sseFrame, 16)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(body)
		var f sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if f.data != "" || f.event != "" {
					out <- f
				}
				f = sseFrame{}
			case strings.HasPrefix(line, "id: "):
				f.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				f.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				f.data = line[6:]
			case strings.HasPrefix(line, ":"):
				// comment/ping — ignore
			}
		}
	}()
	return out
}

// nextDataFrame waits for the next non-control frame.
func nextDataFrame(t *testing.T, frames <-chan sseFrame, timeout time.Duration) sseFrame {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("SSE stream closed unexpectedly")
			}
			if f.event == "" && f.data != "" {
				return f
			}
		case <-deadline:
			t.Fatal("timed out waiting for SSE event")
		}
	}
}

func openSSE(t *testing.T, url string) (*http.Response, <-chan sseFrame) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("SSE connect: %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE content-type = %q", ct)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp, readSSE(resp.Body)
}

func TestEventsSSELiveStream(t *testing.T) {
	srv, elog, cleanup := testHubWithEvents(t)
	defer cleanup()

	_, frames := openSSE(t, srv.URL+"/events")

	// Give the handler a moment to subscribe before writing.
	time.Sleep(100 * time.Millisecond)
	ref, env, _ := signedNote(t, "f0f0f0f0-1111-4222-8333-444444444444", []string{"dataverse001"})
	putSignedObject(t, srv.URL, env, ref)

	f := nextDataFrame(t, frames, 5*time.Second)
	var ev events.Event
	if err := json.Unmarshal([]byte(f.data), &ev); err != nil {
		t.Fatalf("bad SSE data %q: %v", f.data, err)
	}
	if ev.Ref != ref || f.id != ev.Cursor {
		t.Errorf("SSE event = %+v (id %q)", ev, f.id)
	}
	if elog.Head() != ev.Cursor {
		t.Errorf("delivered cursor %q != journal head %q", ev.Cursor, elog.Head())
	}
}

func TestEventsSSEReplayThenLive(t *testing.T) {
	srv, _, cleanup := testHubWithEvents(t)
	defer cleanup()
	head := getEventsPage(t, srv.URL+"/events", "").NextCursor

	// One event before connecting…
	ref1, env1, _ := signedNote(t, "a1a1a1a1-1111-4222-8333-444444444444", []string{"dataverse001"})
	putSignedObject(t, srv.URL, env1, ref1)

	_, frames := openSSE(t, srv.URL+"/events?since="+head)

	// …must be replayed…
	f := nextDataFrame(t, frames, 5*time.Second)
	var ev events.Event
	json.Unmarshal([]byte(f.data), &ev)
	if ev.Ref != ref1 {
		t.Fatalf("replayed event = %+v, want %s", ev, ref1)
	}

	// …followed by live events.
	ref2, env2, _ := signedNote(t, "a2a2a2a2-1111-4222-8333-444444444444", []string{"dataverse001"})
	putSignedObject(t, srv.URL, env2, ref2)
	f = nextDataFrame(t, frames, 5*time.Second)
	json.Unmarshal([]byte(f.data), &ev)
	if ev.Ref != ref2 {
		t.Fatalf("live event = %+v, want %s", ev, ref2)
	}
}

func TestEventsSSEResetFrame(t *testing.T) {
	srv, _, cleanup := testHubWithEvents(t)
	defer cleanup()

	_, frames := openSSE(t, srv.URL+"/events?since=deadbeef:000000000099")

	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("stream closed before reset frame")
			}
			if f.event == "reset" {
				var d struct {
					Cursor string `json:"cursor"`
				}
				if err := json.Unmarshal([]byte(f.data), &d); err != nil || d.Cursor == "" {
					t.Fatalf("reset frame data %q", f.data)
				}
				return
			}
		case <-deadline:
			t.Fatal("no reset frame")
		}
	}
}

func TestEventsSSESubscriberCap(t *testing.T) {
	// The cap is a Hub field; build a dedicated hub with cap 1.
	dir := t.TempDir()
	store, _ := storage.NewStore(dir, true)
	shared := realm.NewSharedRealms()
	index := storage.NewIndex(shared)
	limiter := auth.NewRateLimiter(100000, 10000000)
	defer limiter.Stop()
	authStore := auth.NewAuthStore(168 * time.Hour)
	defer authStore.Stop()
	elog, _ := events.Open(dir+"/events", events.Options{})
	hub := serving.NewHub(store, index, limiter, authStore, "", shared)
	hub.Events = elog
	hub.EventsMaxSubscribers = 1
	capped := httptest.NewServer(hub.Router())
	defer func() { // journal first, so the held SSE stream ends (see testHubWithEvents)
		elog.Close()
		capped.Close()
	}()

	openSSE(t, capped.URL+"/events") // occupies the only slot

	req, _ := http.NewRequest(http.MethodGet, capped.URL+"/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-cap SSE connect: got %d, want 503", resp.StatusCode)
	}
}
