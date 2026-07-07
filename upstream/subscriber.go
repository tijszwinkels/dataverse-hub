package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tijszwinkels/dataverse-hub/events"
)

const (
	subscriberBackoffMin    = time.Second
	subscriberBackoffMax    = 60 * time.Second
	subscriberRetryNoEvents = 10 * time.Minute // upstream is an old hub without /events
	subscriberHealthyAfter  = 30 * time.Second // a stream this long resets the backoff
)

var errEventsUnsupported = errors.New("upstream has no /events endpoint")

// SubscriberCallbacks connect the transport loop to the proxy's apply logic.
// OnEvent is called synchronously per change event; the upstream cursor is
// persisted only after it returns, so a crash mid-apply redelivers the event
// (at-least-once, idempotent by ref+revision). OnReset runs the cache
// revalidation sweep when the upstream cannot honor our cursor.
type SubscriberCallbacks struct {
	OnEvent func(ev events.Event)
	OnReset func()
}

// Subscriber maintains one SSE subscription to the upstream hub's /events
// feed, with durable cursor resume, reconnect backoff, and reset handling.
// Lifecycle mirrors SyncPending: Start once, Stop to terminate.
type Subscriber struct {
	baseURL    string
	cursorPath string
	upstream   *Client // availability gate + active probe while down (may be nil)
	cb         SubscriberCallbacks
	client     *http.Client

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewSubscriber creates a subscriber for baseURL's /events feed, persisting
// its resume cursor at cursorPath. upstream (optional) gates connection
// attempts on availability; while down, the subscriber probes it actively —
// waiting for the 30s background health-checker alone would leave the feed
// disconnected long after the upstream is back.
func NewSubscriber(baseURL, cursorPath string, upstream *Client, cb SubscriberCallbacks) *Subscriber {
	ctx, cancel := context.WithCancel(context.Background())
	return &Subscriber{
		baseURL:    strings.TrimRight(baseURL, "/"),
		cursorPath: cursorPath,
		upstream:   upstream,
		cb:         cb,
		// No overall client timeout: the stream is long-lived by design.
		// Dial and response-header timeouts still bound a dead upstream.
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start launches the subscription loop.
func (s *Subscriber) Start() {
	s.wg.Add(1)
	go s.run()
}

// Stop terminates the loop and waits for it to finish. Safe to call once.
func (s *Subscriber) Stop() {
	s.cancel()
	s.wg.Wait()
}

func (s *Subscriber) run() {
	defer s.wg.Done()
	backoff := subscriberBackoffMin
	for {
		if s.ctx.Err() != nil {
			return
		}
		if s.upstream != nil && !s.upstream.Available() {
			s.upstream.HealthCheck() // probe: don't wait for the 30s checker
			if !s.upstream.Available() {
				if !s.sleep(5 * time.Second) {
					return
				}
				continue
			}
		}

		start := time.Now()
		err := s.streamOnce()
		if s.ctx.Err() != nil {
			return
		}

		if errors.Is(err, errEventsUnsupported) {
			log.Printf("[proxy] events: upstream %s has no /events (old hub?) — retrying in %v", s.baseURL, subscriberRetryNoEvents)
			if !s.sleep(subscriberRetryNoEvents) {
				return
			}
			backoff = subscriberBackoffMin
			continue
		}

		if err != nil {
			log.Printf("[proxy] events: upstream stream ended: %v (reconnect in ~%v)", err, backoff)
		} else {
			log.Printf("[proxy] events: upstream closed the stream (reconnect in ~%v)", backoff)
		}
		if time.Since(start) > subscriberHealthyAfter {
			backoff = subscriberBackoffMin // the stream was healthy; don't punish the reconnect
		}
		jitter := time.Duration(rand.Int63n(int64(backoff)/2 + 1))
		if !s.sleep(backoff + jitter) {
			return
		}
		if backoff *= 2; backoff > subscriberBackoffMax {
			backoff = subscriberBackoffMax
		}
	}
}

// streamOnce connects and consumes the stream until it ends. A nil return
// means the server closed cleanly.
func (s *Subscriber) streamOnce() error {
	cursor := s.loadCursor()
	u := s.baseURL + "/events"
	if cursor != "" {
		u += "?since=" + url.QueryEscape(cursor)
	}
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		io.Copy(io.Discard, resp.Body)
		return errEventsUnsupported
	case resp.StatusCode != http.StatusOK:
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("upstream /events returned %d", resp.StatusCode)
	}
	log.Printf("[proxy] events: subscribed to %s/events (cursor %q)", s.baseURL, cursor)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var evType, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data != "" {
				s.dispatch(evType, data)
			}
			evType, data = "", ""
		case strings.HasPrefix(line, "data: "):
			data = line[len("data: "):]
		case strings.HasPrefix(line, "event: "):
			evType = line[len("event: "):]
		case strings.HasPrefix(line, ":"):
			// heartbeat comment
		}
	}
	return sc.Err()
}

func (s *Subscriber) dispatch(evType, data string) {
	switch evType {
	case "": // change event
		var ev events.Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			log.Printf("WARN: [proxy] events: bad event payload: %v", err)
			return
		}
		if s.cb.OnEvent != nil {
			s.cb.OnEvent(ev)
		}
		s.saveCursor(ev.Cursor) // after apply: crash → redeliver, never skip

	case "reset":
		var d struct {
			Cursor string `json:"cursor"`
		}
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			log.Printf("WARN: [proxy] events: bad reset payload: %v", err)
		}
		log.Printf("[proxy] events: upstream reset (journal loss or expired cursor) — revalidating cache")
		if s.cb.OnReset != nil {
			s.cb.OnReset()
		}
		if d.Cursor != "" {
			s.saveCursor(d.Cursor)
		}

	case "auth":
		// The proxy subscribes anonymously today; nothing to refresh.
		log.Printf("WARN: [proxy] events: upstream sent auth frame on anonymous subscription")
	}
}

type cursorFile struct {
	Cursor string `json:"cursor"`
}

func (s *Subscriber) loadCursor() string {
	data, err := os.ReadFile(s.cursorPath)
	if err != nil {
		return ""
	}
	var c cursorFile
	if json.Unmarshal(data, &c) != nil {
		log.Printf("WARN: [proxy] events: corrupt cursor file %s — starting from head", s.cursorPath)
		return ""
	}
	return c.Cursor
}

func (s *Subscriber) saveCursor(cursor string) {
	if cursor == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.cursorPath), 0755); err != nil {
		log.Printf("WARN: [proxy] events: cursor dir: %v", err)
		return
	}
	data, _ := json.Marshal(cursorFile{Cursor: cursor})
	tmp, err := os.CreateTemp(filepath.Dir(s.cursorPath), ".cursor-*")
	if err != nil {
		log.Printf("WARN: [proxy] events: cursor temp: %v", err)
		return
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		log.Printf("WARN: [proxy] events: cursor write: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		log.Printf("WARN: [proxy] events: cursor close: %v", err)
		return
	}
	if err := os.Rename(name, s.cursorPath); err != nil {
		log.Printf("WARN: [proxy] events: cursor rename: %v", err)
	}
}

// sleep waits d or returns false when the subscriber is stopping.
func (s *Subscriber) sleep(d time.Duration) bool {
	select {
	case <-s.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
