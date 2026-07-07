package serving

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/events"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
)

const (
	defaultEventsMaxSubscribers = 256
	sseSubscriberBuffer         = 256
	ssePingInterval             = 25 * time.Second
	sseAuthRecheckInterval      = 60 * time.Second
	maxLongPollWait             = 60 * time.Second
	replayBatchSize             = 500
)

// emitPut records a change event for a stored object. Safe on a nil log.
func emitPut(elog *events.Log, item *object.Item, realms object.InField) {
	elog.Record(events.Event{
		Op:       "put",
		Ref:      item.Ref(),
		Revision: item.Revision,
		Type:     item.Type,
		Pubkey:   item.Pubkey,
		Realms:   []string(realms),
	})
}

// eventFilter is the per-subscriber delivery gate: the realm-auth check
// (identical to GET /{ref}, same merged graph+TOML resolver) AND-ed with
// optional query filters. The resolver's internal state is live (graph
// SHARED_REALM ingest mutates it in place), so membership changes apply to
// in-flight streams.
type eventFilter struct {
	authPK   string
	resolver realm.RealmResolver
	typ      string
	by       string
	realm    string
}

func filterFromRequest(r *http.Request, resolver realm.RealmResolver) eventFilter {
	q := r.URL.Query()
	return eventFilter{
		authPK:   auth.AuthPubkey(r),
		resolver: resolver,
		typ:      q.Get("type"),
		by:       q.Get("by"),
		realm:    q.Get("realm"),
	}
}

func (f eventFilter) match(ev events.Event) bool {
	if !realm.CanRead(ev.Realms, f.authPK, f.resolver) {
		return false
	}
	if f.typ != "" && ev.Type != f.typ {
		return false
	}
	if f.by != "" && ev.Pubkey != f.by {
		return false
	}
	if f.realm != "" {
		found := false
		for _, r := range ev.Realms {
			if r == f.realm {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// eventsPage is the JSON envelope for GET /events.
type eventsPage struct {
	Events     []events.Event `json:"events"`
	NextCursor string         `json:"next_cursor"`
	Live       bool           `json:"live"`
	Reset      bool           `json:"reset,omitempty"`
}

// serveEvents handles GET /events for both root and proxy modes.
// A nil log means events are disabled: 404, indistinguishable from an old
// hub, which is exactly the feature-detection contract.
func serveEvents(w http.ResponseWriter, r *http.Request, elog *events.Log, resolver realm.RealmResolver, astore *auth.AuthStore, subs *atomic.Int64, maxSubs int) {
	if elog == nil {
		writeError(w, http.StatusNotFound, "events disabled", "NOT_FOUND")
		return
	}
	filter := filterFromRequest(r, resolver)
	if acceptsEventStream(r) {
		serveEventsSSE(w, r, elog, filter, astore, subs, maxSubs)
		return
	}
	serveEventsJSON(w, r, elog, filter)
}

func acceptsEventStream(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// readFiltered replays events after `since`, applying the delivery filter.
// Returns the matching events (≤ limit), the cursor of the last *raw* event
// examined (so callers advance past filtered events), whether the read caught
// up with the journal head, and whether the cursor was reset.
func readFiltered(elog *events.Log, since string, limit int, filter eventFilter) (matched []events.Event, lastRaw string, live, reset bool, err error) {
	lastRaw = since
	for len(matched) < limit {
		raw, rst, rerr := elog.ReadSince(lastRaw, replayBatchSize)
		if rerr != nil {
			return nil, "", false, false, rerr
		}
		if rst {
			return nil, "", false, true, nil
		}
		for _, ev := range raw {
			lastRaw = ev.Cursor
			if filter.match(ev) {
				matched = append(matched, ev)
				if len(matched) >= limit {
					break
				}
			}
		}
		if len(raw) < replayBatchSize {
			// Journal exhausted. Live only if we consumed everything (didn't
			// stop mid-batch because limit was reached).
			live = len(matched) < limit || lastRaw == raw[len(raw)-1].Cursor
			break
		}
	}
	return matched, lastRaw, live, false, nil
}

func serveEventsJSON(w http.ResponseWriter, r *http.Request, elog *events.Log, filter eventFilter) {
	q := r.URL.Query()
	since := q.Get("since")
	limit := parseLimit(q.Get("limit"), 200, 1000)

	// Bootstrap: no cursor → hand out the head so the caller can do its full
	// sync first and then follow from before that sync started.
	if since == "" {
		writeEventsPage(w, eventsPage{Events: []events.Event{}, NextCursor: elog.Head(), Live: true})
		return
	}

	var wait time.Duration
	if ws := q.Get("wait"); ws != "" {
		if d, err := time.ParseDuration(ws); err == nil && d > 0 {
			wait = min(d, maxLongPollWait)
		}
	}

	// Subscribe before reading so nothing slips between replay and long-poll.
	var sub *events.Subscription
	if wait > 0 {
		sub = elog.Subscribe(sseSubscriberBuffer)
		if sub != nil {
			defer sub.Close()
		}
	}

	matched, lastRaw, live, reset, err := readFiltered(elog, since, limit, filter)
	if err != nil {
		log.Printf("ERROR: GET /events: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error", "INTERNAL")
		return
	}
	if reset {
		writeEventsPage(w, eventsPage{Events: []events.Event{}, NextCursor: elog.Head(), Live: true, Reset: true})
		return
	}
	if len(matched) > 0 || wait == 0 || sub == nil {
		writeEventsPage(w, eventsPage{Events: matched, NextCursor: lastRaw, Live: live})
		return
	}

	// Long-poll: wait for the first matching event, advancing past filtered ones.
	lastSeq := events.CursorSeq(lastRaw)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok { // dropped as slow — respond with what we know
				writeEventsPage(w, eventsPage{Events: []events.Event{}, NextCursor: lastRaw, Live: true})
				return
			}
			if events.CursorSeq(ev.Cursor) <= lastSeq {
				continue // already covered by the replay read
			}
			lastRaw = ev.Cursor
			if filter.match(ev) {
				writeEventsPage(w, eventsPage{Events: []events.Event{ev}, NextCursor: ev.Cursor, Live: true})
				return
			}
		case <-timer.C:
			writeEventsPage(w, eventsPage{Events: []events.Event{}, NextCursor: lastRaw, Live: true})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func writeEventsPage(w http.ResponseWriter, page eventsPage) {
	if page.Events == nil {
		page.Events = []events.Event{}
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(page)
}

func serveEventsSSE(w http.ResponseWriter, r *http.Request, elog *events.Log, filter eventFilter, astore *auth.AuthStore, subs *atomic.Int64, maxSubs int) {
	if maxSubs <= 0 {
		maxSubs = defaultEventsMaxSubscribers
	}
	if subs.Add(1) > int64(maxSubs) {
		subs.Add(-1)
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "too many event subscribers", "OVERLOADED")
		return
	}
	defer subs.Add(-1)

	// The server's WriteTimeout would sever every stream (main.go sets 30s);
	// lift it for this connection only. Errors are non-fatal (e.g. h2).
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		log.Printf("WARN: GET /events: clear write deadline: %v", err)
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := sseWrite(w, rc, "retry: 3000\n\n"); err != nil {
		return
	}

	// Subscribe before replay so no event slips between the two.
	sub := elog.Subscribe(sseSubscriberBuffer)
	if sub == nil {
		return // log shutting down
	}
	defer sub.Close()

	since := r.URL.Query().Get("since")
	if since == "" {
		since = r.Header.Get("Last-Event-ID")
	}

	var lastSeq uint64
	if since == "" {
		// Live-only from now.
		lastSeq = events.CursorSeq(elog.Head())
	} else {
		// Replay, batch by batch. On reset, tell the client to revalidate and
		// continue live from the head — its next connect carries a valid cursor.
		cursor := since
		for {
			matched, lastRaw, live, reset, err := readFiltered(elog, cursor, replayBatchSize, filter)
			if err != nil {
				log.Printf("ERROR: GET /events (sse replay): %v", err)
				return
			}
			if reset {
				head := elog.Head()
				if err := sseWriteFrame(w, rc, "", "reset", fmt.Sprintf(`{"cursor":%q}`, head)); err != nil {
					return
				}
				lastSeq = events.CursorSeq(head)
				break
			}
			for _, ev := range matched {
				data, merr := json.Marshal(ev)
				if merr != nil {
					continue
				}
				if err := sseWriteFrame(w, rc, ev.Cursor, "", string(data)); err != nil {
					return
				}
			}
			cursor = lastRaw
			lastSeq = events.CursorSeq(lastRaw)
			if live {
				break
			}
		}
	}

	token := auth.RequestToken(r)
	ping := time.NewTicker(ssePingInterval)
	defer ping.Stop()
	authCheck := time.NewTicker(sseAuthRecheckInterval)
	defer authCheck.Stop()

	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				// Dropped as a slow consumer (or log closed). The client
				// reconnects with Last-Event-ID and replays what it missed.
				return
			}
			if events.CursorSeq(ev.Cursor) <= lastSeq {
				continue // overlap with replay
			}
			lastSeq = events.CursorSeq(ev.Cursor)
			if !filter.match(ev) {
				continue
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if err := sseWriteFrame(w, rc, ev.Cursor, "", string(data)); err != nil {
				return
			}
		case <-ping.C:
			if err := sseWrite(w, rc, ": ping\n\n"); err != nil {
				return
			}
		case <-authCheck.C:
			// Tokens are in-memory and expire; a subscriber whose token died
			// must re-auth rather than keep receiving realm-filtered events
			// under stale credentials.
			if token != "" && astore != nil {
				if _, ok := astore.ValidateToken(token); !ok {
					sseWriteFrame(w, rc, "", "auth", `{"status":"expired"}`)
					return
				}
			}
		case <-r.Context().Done():
			return
		}
	}
}

func sseWrite(w http.ResponseWriter, rc *http.ResponseController, s string) error {
	if _, err := fmt.Fprint(w, s); err != nil {
		return err
	}
	return rc.Flush()
}

// sseWriteFrame writes one SSE frame; id and event are omitted when empty.
func sseWriteFrame(w http.ResponseWriter, rc *http.ResponseController, id, event, data string) error {
	var b strings.Builder
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	if event != "" {
		b.WriteString("event: " + event + "\n")
	}
	b.WriteString("data: " + data + "\n\n")
	return sseWrite(w, rc, b.String())
}
