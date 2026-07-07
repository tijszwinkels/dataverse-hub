package events

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestLog(t *testing.T, dir string, o Options) *Log {
	t.Helper()
	l, err := Open(dir, o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func record(l *Log, ref string, rev int) Event {
	return l.Record(Event{Op: "put", Ref: ref, Revision: rev, Type: "NOTE", Pubkey: "Apk", Realms: []string{"dataverse001"}})
}

func TestRecordAssignsMonotonicCursors(t *testing.T) {
	l := openTestLog(t, t.TempDir(), Options{})

	e1 := record(l, "Apk.one", 1)
	e2 := record(l, "Apk.two", 1)

	if e1.Cursor == "" || e2.Cursor == "" {
		t.Fatalf("cursors not assigned: %q %q", e1.Cursor, e2.Cursor)
	}
	if CursorSeq(e1.Cursor) >= CursorSeq(e2.Cursor) {
		t.Errorf("cursors not monotonic: %q then %q", e1.Cursor, e2.Cursor)
	}
	if e1.ReceivedAt.IsZero() {
		t.Errorf("received_at not stamped")
	}
	if l.Head() != e2.Cursor {
		t.Errorf("Head() = %q, want %q", l.Head(), e2.Cursor)
	}
}

func TestReadSinceReturnsOnlyNewerEvents(t *testing.T) {
	l := openTestLog(t, t.TempDir(), Options{})

	e1 := record(l, "Apk.one", 1)
	e2 := record(l, "Apk.two", 1)
	e3 := record(l, "Apk.three", 1)

	evs, reset, err := l.ReadSince(e1.Cursor, 100)
	if err != nil || reset {
		t.Fatalf("ReadSince: evs=%v reset=%v err=%v", evs, reset, err)
	}
	if len(evs) != 2 || evs[0].Cursor != e2.Cursor || evs[1].Cursor != e3.Cursor {
		t.Fatalf("ReadSince after e1 = %+v, want [e2 e3]", evs)
	}

	// Caught-up cursor yields empty, no reset.
	evs, reset, err = l.ReadSince(e3.Cursor, 100)
	if err != nil || reset || len(evs) != 0 {
		t.Fatalf("ReadSince at head: evs=%v reset=%v err=%v", evs, reset, err)
	}

	// Limit respected.
	evs, _, _ = l.ReadSince(e1.Cursor, 1)
	if len(evs) != 1 || evs[0].Cursor != e2.Cursor {
		t.Fatalf("ReadSince limit=1 = %+v, want [e2]", evs)
	}
}

func TestReadSinceSpansSegmentRotation(t *testing.T) {
	l := openTestLog(t, t.TempDir(), Options{SegmentMaxEvents: 3, RingSize: 2})

	var first Event
	for i := 0; i < 10; i++ {
		e := record(l, "Apk.obj", i+1)
		if i == 0 {
			first = e
		}
	}

	// Ring only holds 2 events; reading from the first cursor must hit segments.
	evs, reset, err := l.ReadSince(first.Cursor, 100)
	if err != nil || reset {
		t.Fatalf("ReadSince: reset=%v err=%v", reset, err)
	}
	if len(evs) != 9 {
		t.Fatalf("got %d events, want 9", len(evs))
	}
	for i, ev := range evs {
		if ev.Revision != i+2 {
			t.Errorf("event %d revision = %d, want %d", i, ev.Revision, i+2)
		}
	}
}

func TestResumeAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	l1 := openTestLog(t, dir, Options{})
	e1 := record(l1, "Apk.one", 1)
	epoch := l1.Epoch()
	l1.Close()

	// Clean restart: epoch survives, sequence continues, old cursors replay.
	l2 := openTestLog(t, dir, Options{})
	if l2.Epoch() != epoch {
		t.Fatalf("epoch changed on clean reopen: %q -> %q", epoch, l2.Epoch())
	}
	e2 := record(l2, "Apk.two", 1)
	if CursorSeq(e2.Cursor) <= CursorSeq(e1.Cursor) {
		t.Fatalf("sequence regressed on reopen: %q then %q", e1.Cursor, e2.Cursor)
	}
	evs, reset, err := l2.ReadSince(e1.Cursor, 100)
	if err != nil || reset || len(evs) != 1 || evs[0].Ref != "Apk.two" {
		t.Fatalf("replay across reopen: evs=%+v reset=%v err=%v", evs, reset, err)
	}
}

func TestJournalLossChangesEpoch(t *testing.T) {
	dir := t.TempDir()
	l1 := openTestLog(t, dir, Options{})
	e1 := record(l1, "Apk.one", 1)
	l1.Close()

	// Simulate journal loss.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	l2 := openTestLog(t, dir, Options{})
	if l2.Epoch() == l1.Epoch() {
		t.Fatalf("epoch must change after journal loss")
	}
	_, reset, err := l2.ReadSince(e1.Cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reset {
		t.Fatalf("cursor from lost journal must signal reset")
	}
}

func TestSegmentsMissingWithStateChangesEpoch(t *testing.T) {
	dir := t.TempDir()
	l1 := openTestLog(t, dir, Options{})
	record(l1, "Apk.one", 1)
	l1.Close()

	// state.json survives but segments are gone: continuity is unprovable.
	segs, _ := filepath.Glob(filepath.Join(dir, "journal-*.jsonl"))
	for _, s := range segs {
		os.Remove(s)
	}

	l2 := openTestLog(t, dir, Options{})
	if l2.Epoch() == l1.Epoch() {
		t.Fatalf("epoch must change when segments are missing")
	}
}

func TestUnknownCursorResets(t *testing.T) {
	l := openTestLog(t, t.TempDir(), Options{})
	record(l, "Apk.one", 1)

	for _, cursor := range []string{
		"otherepoch:000000000001", // wrong epoch
		"garbage",                 // unparseable
		l.Epoch() + ":000000999999", // future seq in same epoch
	} {
		_, reset, err := l.ReadSince(cursor, 10)
		if err != nil {
			t.Fatalf("ReadSince(%q): %v", cursor, err)
		}
		if !reset {
			t.Errorf("ReadSince(%q): want reset", cursor)
		}
	}
}

func TestRetentionPrunesOldSegmentsAndResets(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir, Options{SegmentMaxEvents: 2, Retention: 50 * time.Millisecond})

	e1 := record(l, "Apk.a", 1)
	record(l, "Apk.b", 1) // fills segment 1
	record(l, "Apk.c", 1) // rotates into segment 2

	// Age segment 1 beyond retention, then trigger pruning via rotation.
	segs, _ := filepath.Glob(filepath.Join(dir, "journal-*.jsonl"))
	if len(segs) < 2 {
		t.Fatalf("expected >=2 segments, got %v", segs)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(segs[0], old, old)

	record(l, "Apk.d", 1) // fills segment 2, rotation prunes segment 1

	segs, _ = filepath.Glob(filepath.Join(dir, "journal-*.jsonl"))
	for _, s := range segs {
		if filepath.Base(s) == filepath.Base(segs[0]) && s == segs[0] {
			continue
		}
	}
	// Pruned cursor now resets (fell out of the replay window).
	_, reset, err := l.ReadSince(e1.Cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reset {
		t.Errorf("cursor older than retained window must reset")
	}
}

func TestSubscribeReceivesLiveEvents(t *testing.T) {
	l := openTestLog(t, t.TempDir(), Options{})

	sub := l.Subscribe(8)
	defer sub.Close()

	e := record(l, "Apk.live", 3)

	select {
	case got := <-sub.C:
		if got.Cursor != e.Cursor || got.Ref != "Apk.live" {
			t.Fatalf("got %+v, want %+v", got, e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event delivered to subscriber")
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	l := openTestLog(t, t.TempDir(), Options{})

	sub := l.Subscribe(1) // room for exactly one un-consumed event
	record(l, "Apk.one", 1)
	record(l, "Apk.two", 1) // overflows: subscriber must be dropped, channel closed

	deadline := time.After(2 * time.Second)
	var closed bool
	for !closed {
		select {
		case _, ok := <-sub.C:
			if !ok {
				closed = true
			}
		case <-deadline:
			t.Fatal("slow subscriber channel was not closed")
		}
	}
}

func TestNilLogIsSafe(t *testing.T) {
	var l *Log
	l.Record(Event{Op: "put", Ref: "Apk.x", Revision: 1}) // must not panic
	if l.Head() != "" {
		t.Errorf("nil Head() = %q", l.Head())
	}
	if _, reset, err := l.ReadSince("a:1", 10); err != nil || !reset {
		t.Errorf("nil ReadSince should reset")
	}
	if sub := l.Subscribe(1); sub != nil {
		t.Errorf("nil Subscribe should return nil")
	}
	l.Close()
}
