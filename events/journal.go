// Package events provides the hub's append-only change journal and its
// fan-out to live subscribers. Cursors are "<epoch>:<seq>": seq is a per-hub
// monotonic counter assigned at journal-append time; epoch identifies journal
// continuity — it changes whenever the journal cannot prove it hasn't lost
// events (first boot, deleted/corrupt journal), which tells resuming
// subscribers to do a full revalidation instead of silently missing changes.
//
// The journal is disposable by design: it is never consulted to serve
// objects, only to replay change notifications. Losing it costs subscribers
// one revalidation sweep, nothing more.
package events

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one change notification. Skinny: no object body — consumers fetch
// the ref via GET (conditional, realm-checked). Delivery is at-least-once;
// (Ref, Revision) is the idempotency key.
type Event struct {
	Cursor     string    `json:"cursor"`
	Op         string    `json:"op"` // "put"; "delete" reserved for tombstones
	Ref        string    `json:"ref"`
	Revision   int       `json:"revision"`
	Type       string    `json:"type,omitempty"`
	Pubkey     string    `json:"pubkey,omitempty"`
	Realms     []string  `json:"realms,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
}

// Options configures a Log. Zero values select defaults.
type Options struct {
	Retention        time.Duration // replay window; segments older than this are pruned (default 168h)
	SegmentMaxEvents int           // events per journal segment before rotation (default 10000)
	RingSize         int           // in-memory tail cache for cheap replays (default 4096)
}

const (
	defaultRetention        = 168 * time.Hour
	defaultSegmentMaxEvents = 10000
	defaultRingSize         = 4096
)

// CursorSeq extracts the sequence number from a cursor, 0 if unparseable.
func CursorSeq(cursor string) uint64 {
	_, seq, ok := parseCursor(cursor)
	if !ok {
		return 0
	}
	return seq
}

func parseCursor(cursor string) (epoch string, seq uint64, ok bool) {
	i := strings.LastIndexByte(cursor, ':')
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(cursor[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return cursor[:i], n, true
}

type segmentInfo struct {
	path     string
	firstSeq uint64 // seq of first event; for an empty tail segment, the next seq to be written
}

// Subscription is a live event feed. C is closed when the subscriber falls
// behind (buffer overflow) or the log shuts down — reconnect and replay via
// ReadSince from your last cursor.
type Subscription struct {
	C  <-chan Event
	ch chan Event
	id int
	l  *Log
}

// Close unregisters the subscription. Safe to call multiple times.
func (s *Subscription) Close() {
	if s == nil {
		return
	}
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	s.l.dropLocked(s.id)
}

// Log is the journal plus its subscriber fan-out. A nil *Log is valid and
// inert (events disabled): Record/Head/Subscribe/Close no-op, ReadSince
// signals reset.
type Log struct {
	mu  sync.Mutex
	dir string
	opt Options

	epoch    string
	seq      uint64
	seg      *os.File
	segIndex int
	segCount int
	segments []segmentInfo

	ring      []Event
	ringStart uint64 // seq of ring[0]

	subs      map[int]*Subscription
	nextSubID int
	closed    bool
}

type stateFile struct {
	Epoch string `json:"epoch"`
	Seq   uint64 `json:"seq"`
}

// Open opens (or initializes) the journal in dir.
func Open(dir string, opt Options) (*Log, error) {
	if opt.Retention <= 0 {
		opt.Retention = defaultRetention
	}
	if opt.SegmentMaxEvents <= 0 {
		opt.SegmentMaxEvents = defaultSegmentMaxEvents
	}
	if opt.RingSize <= 0 {
		opt.RingSize = defaultRingSize
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("events: create dir: %w", err)
	}

	l := &Log{dir: dir, opt: opt, subs: make(map[int]*Subscription)}

	state, stateOK := readState(filepath.Join(dir, "state.json"))
	segs, lastSeq, segsOK := scanSegments(dir, state.Epoch)

	if stateOK && segsOK && len(segs) > 0 {
		// Continuity proven: resume epoch and sequence.
		l.epoch = state.Epoch
		l.seq = lastSeq
		if state.Seq > l.seq {
			l.seq = state.Seq
		}
		l.segments = segs
		tail := segs[len(segs)-1]
		l.segIndex = segmentIndex(tail.path)
		f, count, err := openSegmentAppend(tail.path)
		if err != nil {
			return nil, err
		}
		l.seg = f
		l.segCount = count
	} else {
		// First boot, or continuity unprovable (missing/corrupt state,
		// missing/foreign segments): new epoch, stale files set aside.
		if err := l.startFresh(); err != nil {
			return nil, err
		}
	}
	l.ringStart = l.seq + 1 // ring starts empty
	return l, nil
}

// startFresh mints a new epoch and an empty first segment, renaming any
// leftover journal files out of the way. Caller context: Open only.
func (l *Log) startFresh() error {
	leftovers, _ := filepath.Glob(filepath.Join(l.dir, "journal-*.jsonl"))
	for _, p := range leftovers {
		if err := os.Rename(p, p+".stale"); err != nil {
			log.Printf("WARN: events: set aside stale segment %s: %v", p, err)
		}
	}

	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("events: epoch: %w", err)
	}
	l.epoch = hex.EncodeToString(b)
	l.seq = 0
	l.segIndex = 1
	path := l.segmentPath(l.segIndex)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("events: create segment: %w", err)
	}
	l.seg = f
	l.segCount = 0
	l.segments = []segmentInfo{{path: path, firstSeq: 1}}
	if err := l.writeState(); err != nil {
		return err
	}
	log.Printf("events: journal initialized (epoch %s) in %s", l.epoch, l.dir)
	return nil
}

func (l *Log) segmentPath(index int) string {
	return filepath.Join(l.dir, fmt.Sprintf("journal-%06d.jsonl", index))
}

func segmentIndex(path string) int {
	base := filepath.Base(path)
	base = strings.TrimPrefix(base, "journal-")
	base = strings.TrimSuffix(base, ".jsonl")
	n, _ := strconv.Atoi(base)
	return n
}

// readState loads state.json; ok=false on missing/corrupt.
func readState(path string) (stateFile, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return stateFile{}, false
	}
	var s stateFile
	if json.Unmarshal(data, &s) != nil || s.Epoch == "" {
		return stateFile{}, false
	}
	return s, true
}

// scanSegments enumerates journal segments, verifying every first line
// belongs to epoch. Returns segments (sorted), the last seq found in the tail
// segment, and ok=false when any segment is unreadable or from another epoch.
func scanSegments(dir, epoch string) ([]segmentInfo, uint64, bool) {
	paths, err := filepath.Glob(filepath.Join(dir, "journal-*.jsonl"))
	if err != nil || len(paths) == 0 {
		return nil, 0, err == nil // no segments is "ok" only in the sense of empty
	}
	sort.Strings(paths)

	var segs []segmentInfo
	var lastSeq uint64
	for i, p := range paths {
		first, last, count, err := scanSegmentFile(p)
		if err != nil {
			log.Printf("WARN: events: unreadable segment %s: %v", p, err)
			return nil, 0, false
		}
		if count == 0 {
			// Empty segment: only the tail may be empty (crash after rotation).
			if i != len(paths)-1 {
				return nil, 0, false
			}
			segs = append(segs, segmentInfo{path: p, firstSeq: lastSeq + 1})
			continue
		}
		fe, fs, ok := parseCursor(first)
		if !ok || fe != epoch {
			return nil, 0, false
		}
		_, ls, ok := parseCursor(last)
		if !ok {
			return nil, 0, false
		}
		segs = append(segs, segmentInfo{path: p, firstSeq: fs})
		lastSeq = ls
	}
	return segs, lastSeq, true
}

// scanSegmentFile returns the first and last cursor and the line count.
func scanSegmentFile(path string) (first, last string, count int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			return "", "", 0, fmt.Errorf("corrupt line %d", count+1)
		}
		if count == 0 {
			first = ev.Cursor
		}
		last = ev.Cursor
		count++
	}
	return first, last, count, sc.Err()
}

func openSegmentAppend(path string) (*os.File, int, error) {
	_, _, count, err := scanSegmentFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("events: open segment: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, 0, fmt.Errorf("events: open segment: %w", err)
	}
	return f, count, nil
}

// writeState persists {epoch, seq} atomically. Called on init, rotation, and
// Close; between those, seq is recovered by scanning the tail segment.
func (l *Log) writeState() error {
	data, _ := json.Marshal(stateFile{Epoch: l.epoch, Seq: l.seq})
	tmp, err := os.CreateTemp(l.dir, ".state-*")
	if err != nil {
		return fmt.Errorf("events: state temp: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("events: state write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("events: state sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("events: state close: %w", err)
	}
	if err := os.Rename(name, filepath.Join(l.dir, "state.json")); err != nil {
		return fmt.Errorf("events: state rename: %w", err)
	}
	return nil
}

// Epoch returns the journal's continuity epoch ("" on nil).
func (l *Log) Epoch() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.epoch
}

// Head returns the cursor of the newest event ("" on nil log; "<epoch>:0"
// before the first event — a valid since= for "everything from now").
func (l *Log) Head() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprintf("%s:%012d", l.epoch, l.seq)
}

// Record assigns the next cursor and receipt time, appends the event durably,
// and fans it out to live subscribers. Errors are logged, never fatal: the
// object write already succeeded and events are at-least-once with
// revalidation, so a lost notification is recoverable by design.
func (l *Log) Record(ev Event) Event {
	if l == nil {
		return ev
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ev
	}

	l.seq++
	ev.Cursor = fmt.Sprintf("%s:%012d", l.epoch, l.seq)
	ev.ReceivedAt = time.Now().UTC()

	line, err := json.Marshal(ev)
	if err != nil {
		log.Printf("ERROR: events: marshal %s: %v", ev.Ref, err)
		return ev
	}
	if _, err := l.seg.Write(append(line, '\n')); err != nil {
		log.Printf("ERROR: events: append %s: %v", ev.Ref, err)
		return ev
	}
	if err := l.seg.Sync(); err != nil {
		log.Printf("WARN: events: fsync: %v", err)
	}
	l.segCount++

	// Ring cache of the tail.
	if len(l.ring) >= l.opt.RingSize {
		copy(l.ring, l.ring[1:])
		l.ring[len(l.ring)-1] = ev
		l.ringStart++
	} else {
		l.ring = append(l.ring, ev)
	}

	if l.segCount >= l.opt.SegmentMaxEvents {
		l.rotateLocked()
	}

	// Fan out; a full buffer means the subscriber lost the race with its own
	// consumption — close it, it resumes via ReadSince (at-least-once).
	for id, sub := range l.subs {
		select {
		case sub.ch <- ev:
		default:
			log.Printf("WARN: events: dropping slow subscriber %d", id)
			l.dropLocked(id)
		}
	}
	return ev
}

// rotateLocked closes the current segment, persists state, opens the next
// segment, and prunes segments older than the retention window.
func (l *Log) rotateLocked() {
	if err := l.seg.Close(); err != nil {
		log.Printf("WARN: events: close segment: %v", err)
	}
	if err := l.writeState(); err != nil {
		log.Printf("WARN: events: %v", err)
	}
	l.segIndex++
	path := l.segmentPath(l.segIndex)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("ERROR: events: create segment %s: %v — journal frozen", path, err)
		l.closed = true
		return
	}
	l.seg = f
	l.segCount = 0
	l.segments = append(l.segments, segmentInfo{path: path, firstSeq: l.seq + 1})

	// Prune: only non-tail segments, by age of their last write.
	cutoff := time.Now().Add(-l.opt.Retention)
	kept := l.segments[:0]
	for i, s := range l.segments {
		if i < len(l.segments)-1 {
			if fi, err := os.Stat(s.path); err == nil && fi.ModTime().Before(cutoff) {
				if err := os.Remove(s.path); err != nil {
					log.Printf("WARN: events: prune %s: %v", s.path, err)
					kept = append(kept, s)
				} else {
					log.Printf("events: pruned segment %s (replay window %v)", filepath.Base(s.path), l.opt.Retention)
				}
				continue
			}
		}
		kept = append(kept, s)
	}
	l.segments = kept
}

// ReadSince returns up to limit events with sequence greater than cursor's.
// reset=true means the cursor cannot be honored (wrong epoch, unparseable,
// future, or pruned past): the caller must revalidate and resume from Head().
func (l *Log) ReadSince(cursor string, limit int) (evs []Event, reset bool, err error) {
	if l == nil {
		return nil, true, nil
	}
	if limit <= 0 {
		limit = 1000
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	epoch, since, ok := parseCursor(cursor)
	if !ok || epoch != l.epoch || since > l.seq {
		return nil, true, nil
	}
	if since == l.seq {
		return nil, false, nil
	}
	oldest := uint64(1)
	if len(l.segments) > 0 {
		oldest = l.segments[0].firstSeq
	}
	if since+1 < oldest {
		return nil, true, nil // fell out of the replay window
	}

	// Fast path: the ring covers the whole range.
	if since+1 >= l.ringStart {
		start := int(since + 1 - l.ringStart)
		end := len(l.ring)
		if end-start > limit {
			end = start + limit
		}
		return append([]Event(nil), l.ring[start:end]...), false, nil
	}

	// Cold path: scan segments (they always contain the full retained range).
	for _, s := range l.segments {
		if len(evs) >= limit {
			break
		}
		// Skip segments that end before our range begins.
		if s.firstSeq > l.seq {
			continue
		}
		f, ferr := os.Open(s.path)
		if ferr != nil {
			return nil, false, fmt.Errorf("events: read segment: %w", ferr)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() && len(evs) < limit {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var ev Event
			if json.Unmarshal(line, &ev) != nil {
				continue
			}
			if CursorSeq(ev.Cursor) > since {
				evs = append(evs, ev)
			}
		}
		f.Close()
		if serr := sc.Err(); serr != nil {
			return nil, false, fmt.Errorf("events: scan segment: %w", serr)
		}
	}
	return evs, false, nil
}

// Subscribe registers a live feed with the given buffer (default 256).
// Returns nil on a nil log.
func (l *Log) Subscribe(buf int) *Subscription {
	if l == nil {
		return nil
	}
	if buf <= 0 {
		buf = 256
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.nextSubID++
	ch := make(chan Event, buf)
	sub := &Subscription{C: ch, ch: ch, id: l.nextSubID, l: l}
	l.subs[sub.id] = sub
	return sub
}

// dropLocked unregisters and closes a subscription. Caller holds l.mu.
func (l *Log) dropLocked(id int) {
	if sub, ok := l.subs[id]; ok {
		delete(l.subs, id)
		close(sub.ch)
	}
}

// Close persists state and shuts down all subscriptions.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	for id := range l.subs {
		l.dropLocked(id)
	}
	if err := l.writeState(); err != nil {
		log.Printf("WARN: events: %v", err)
	}
	return l.seg.Close()
}
