package freenet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const (
	refA = "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.00000000-0000-4000-8000-00000000000a"
	refB = "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.00000000-0000-4000-8000-00000000000b"
)

func testQueue(t *testing.T) *queue {
	t.Helper()
	q, err := newQueue(t.TempDir())
	if err != nil {
		t.Fatalf("newQueue: %v", err)
	}
	return q
}

func job(ref string, rev int) *Job {
	return &Job{
		Ref:        ref,
		Revision:   rev,
		EnqueuedAt: time.Now(),
		Envelope:   json.RawMessage(`{"item":{"revision":` + itoa(rev) + `}}`),
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestQueuePutClaimDone(t *testing.T) {
	q := testQueue(t)

	if err := q.Put(job(refA, 1)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := q.Depth(); got != 1 {
		t.Fatalf("Depth = %d, want 1", got)
	}

	claimed, err := q.Claim(time.Now())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("Claim returned nil, want a job")
	}
	if claimed.Ref != refA || claimed.Revision != 1 {
		t.Fatalf("claimed %s rev %d, want %s rev 1", claimed.Ref, claimed.Revision, refA)
	}

	// A claimed job is no longer pending — a second Claim finds nothing.
	if got := q.Depth(); got != 0 {
		t.Fatalf("Depth after claim = %d, want 0", got)
	}
	again, err := q.Claim(time.Now())
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if again != nil {
		t.Fatalf("second Claim returned %v, want nil", again)
	}

	if err := q.Done(claimed); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if n := countJSON(t, q.inflightDir); n != 0 {
		t.Fatalf("%d files left in inflight after Done, want 0", n)
	}
}

func TestQueueRejectsInvalidRef(t *testing.T) {
	q := testQueue(t)
	for _, bad := range []string{"../escape", "not-a-ref", "", "a/b"} {
		if err := q.Put(job(bad, 1)); err == nil {
			t.Errorf("Put(%q) succeeded, want error", bad)
		}
	}
	if got := q.Depth(); got != 0 {
		t.Fatalf("Depth = %d, want 0 — invalid refs must not reach disk", got)
	}
}

func TestQueueSupersedesOlderRevision(t *testing.T) {
	q := testQueue(t)

	if err := q.Put(job(refA, 1)); err != nil {
		t.Fatal(err)
	}
	if err := q.Put(job(refA, 2)); err != nil {
		t.Fatal(err)
	}

	// Same ref collapses to one pending job, holding the newest revision.
	if got := q.Depth(); got != 1 {
		t.Fatalf("Depth = %d, want 1 (dedupe per ref)", got)
	}
	claimed, _ := q.Claim(time.Now())
	if claimed.Revision != 2 {
		t.Fatalf("claimed revision %d, want 2 (newer supersedes)", claimed.Revision)
	}
}

func TestQueueKeepsNewerRevisionOnOutOfOrderPut(t *testing.T) {
	q := testQueue(t)

	if err := q.Put(job(refA, 5)); err != nil {
		t.Fatal(err)
	}
	// A stale enqueue must not overwrite the newer queued revision.
	if err := q.Put(job(refA, 3)); err != nil {
		t.Fatal(err)
	}

	claimed, _ := q.Claim(time.Now())
	if claimed.Revision != 5 {
		t.Fatalf("claimed revision %d, want 5 (older must not clobber newer)", claimed.Revision)
	}
}

func TestQueueClaimIsFIFO(t *testing.T) {
	q := testQueue(t)

	older := job(refA, 1)
	older.EnqueuedAt = time.Now().Add(-time.Minute)
	newer := job(refB, 1)

	if err := q.Put(newer); err != nil {
		t.Fatal(err)
	}
	if err := q.Put(older); err != nil {
		t.Fatal(err)
	}

	claimed, _ := q.Claim(time.Now())
	if claimed.Ref != refA {
		t.Fatalf("claimed %s, want the older enqueue %s", claimed.Ref, refA)
	}
}

func TestQueueSkipsJobsWaitingOnBackoff(t *testing.T) {
	q := testQueue(t)

	j := job(refA, 1)
	j.NextAttemptAt = time.Now().Add(time.Hour)
	if err := q.Put(j); err != nil {
		t.Fatal(err)
	}

	if claimed, _ := q.Claim(time.Now()); claimed != nil {
		t.Fatalf("claimed %v, want nil — job is still backing off", claimed)
	}
	// Depth counts it: it is pending work, just not runnable yet.
	if got := q.Depth(); got != 1 {
		t.Fatalf("Depth = %d, want 1", got)
	}
	if claimed, _ := q.Claim(time.Now().Add(2 * time.Hour)); claimed == nil {
		t.Fatal("Claim after the backoff window returned nil, want the job")
	}
}

func TestQueueRequeueDoesNotClobberNewerPending(t *testing.T) {
	q := testQueue(t)

	if err := q.Put(job(refA, 1)); err != nil {
		t.Fatal(err)
	}
	claimed, _ := q.Claim(time.Now())

	// While rev 1 is in flight, rev 2 arrives.
	if err := q.Put(job(refA, 2)); err != nil {
		t.Fatal(err)
	}
	// Rev 1 then fails and is requeued — it must not resurrect over rev 2.
	if err := q.Requeue(claimed); err != nil {
		t.Fatalf("Requeue: %v", err)
	}

	if got := q.Depth(); got != 1 {
		t.Fatalf("Depth = %d, want 1", got)
	}
	if n := countJSON(t, q.inflightDir); n != 0 {
		t.Fatalf("%d files left in inflight after Requeue, want 0", n)
	}
	next, _ := q.Claim(time.Now())
	if next.Revision != 2 {
		t.Fatalf("claimed revision %d, want 2", next.Revision)
	}
}

func TestQueueFailMovesToFailedDir(t *testing.T) {
	q := testQueue(t)

	if err := q.Put(job(refA, 1)); err != nil {
		t.Fatal(err)
	}
	claimed, _ := q.Claim(time.Now())
	claimed.LastError = "publisher exited 3"

	if err := q.Fail(claimed); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if n := countJSON(t, q.inflightDir); n != 0 {
		t.Fatalf("%d files left in inflight after Fail, want 0", n)
	}
	if n := countJSON(t, q.failedDir); n != 1 {
		t.Fatalf("%d files in failed/, want 1 — failures must stay visible", n)
	}

	// The recorded failure keeps the diagnosis.
	data, err := os.ReadFile(filepath.Join(q.failedDir, refA+".json"))
	if err != nil {
		t.Fatalf("read failed job: %v", err)
	}
	var recorded Job
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.LastError != "publisher exited 3" {
		t.Fatalf("LastError = %q, want the publisher error", recorded.LastError)
	}
}

func TestQueueRecoversInflightOnRestart(t *testing.T) {
	dir := t.TempDir()

	q1, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := q1.Put(job(refA, 7)); err != nil {
		t.Fatal(err)
	}
	if _, err := q1.Claim(time.Now()); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-publish: the job is stranded in inflight/.
	if n := countJSON(t, q1.inflightDir); n != 1 {
		t.Fatalf("%d files in inflight, want 1", n)
	}

	q2, err := newQueue(dir)
	if err != nil {
		t.Fatalf("reopen queue: %v", err)
	}
	if got := q2.Depth(); got != 1 {
		t.Fatalf("Depth after restart = %d, want 1 — stranded job must be recovered", got)
	}
	claimed, _ := q2.Claim(time.Now())
	if claimed == nil || claimed.Revision != 7 {
		t.Fatalf("claimed %v, want the recovered rev 7 job", claimed)
	}
}

func TestQueueSurvivesRestartWithPendingJobs(t *testing.T) {
	dir := t.TempDir()

	q1, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := q1.Put(job(refA, 1)); err != nil {
		t.Fatal(err)
	}
	if err := q1.Put(job(refB, 1)); err != nil {
		t.Fatal(err)
	}

	q2, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := q2.Depth(); got != 2 {
		t.Fatalf("Depth after restart = %d, want 2", got)
	}
}

func TestQueueIgnoresGarbageFiles(t *testing.T) {
	q := testQueue(t)

	if err := os.WriteFile(filepath.Join(q.dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := q.Put(job(refA, 1)); err != nil {
		t.Fatal(err)
	}

	claimed, err := q.Claim(time.Now())
	if err != nil {
		t.Fatalf("Claim with garbage present: %v", err)
	}
	if claimed == nil || claimed.Ref != refA {
		t.Fatalf("claimed %v, want the valid job — garbage must not block the queue", claimed)
	}
}

func countJSON(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			n++
		}
	}
	return n
}

// Put and Claim run on different goroutines: the HTTP write path enqueues
// while the worker claims. A revision accepted by Put must never be lost.
func TestQueueConcurrentPutAndClaimNeverLosesNewestRevision(t *testing.T) {
	const revisions = 300

	for round := 0; round < 5; round++ {
		q := testQueue(t)

		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			for rev := 1; rev <= revisions; rev++ {
				if err := q.Put(job(refA, rev)); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
			}
		}()

		highest := 0
		drain := func() {
			for {
				j, err := q.Claim(time.Now())
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if j == nil {
					return
				}
				if j.Revision < highest {
					t.Errorf("claimed rev %d after rev %d — revisions went backwards", j.Revision, highest)
				}
				highest = j.Revision
				if err := q.Done(j); err != nil {
					t.Errorf("Done: %v", err)
				}
			}
		}

		for {
			drain()
			select {
			case <-writerDone:
				drain() // final sweep once the writer has stopped
				if highest != revisions {
					t.Fatalf("round %d: highest claimed revision %d, want %d — a queued revision was dropped",
						round, highest, revisions)
				}
				goto nextRound
			default:
			}
		}
	nextRound:
	}
}

// Two write-path goroutines enqueueing different revisions of one object must
// not let the older one win.
func TestQueueConcurrentPutsKeepNewestRevision(t *testing.T) {
	for round := 0; round < 50; round++ {
		q := testQueue(t)

		var wg sync.WaitGroup
		for rev := 1; rev <= 8; rev++ {
			wg.Add(1)
			go func(rev int) {
				defer wg.Done()
				if err := q.Put(job(refA, rev)); err != nil {
					t.Errorf("Put: %v", err)
				}
			}(rev)
		}
		wg.Wait()

		claimed, err := q.Claim(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if claimed == nil || claimed.Revision != 8 {
			t.Fatalf("round %d: queued %v, want revision 8 — an older Put clobbered a newer one", round, claimed)
		}
	}
}

// Depth is called for logging/metrics and must stay cheap: it counts queue
// entries without opening or decoding them.
func TestQueueDepthDoesNotReadJobBodies(t *testing.T) {
	q := testQueue(t)
	for i := 0; i < 5; i++ {
		j := job(refA[:len(refA)-1]+string(rune('a'+i)), 1)
		if err := q.Put(j); err != nil {
			t.Fatal(err)
		}
	}
	if got := q.Depth(); got != 5 {
		t.Fatalf("Depth = %d, want 5", got)
	}

	// An unreadable job file must not make Depth fail or block.
	unreadable := filepath.Join(q.dir, "unreadable.json")
	if err := os.WriteFile(unreadable, []byte("{"), 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(unreadable, 0o644)
	if got := q.Depth(); got < 5 {
		t.Fatalf("Depth = %d, want at least 5 — an unreadable file must not break counting", got)
	}
}

// Jobs that exhausted their retries stay countable after a restart, so the
// status endpoint can keep reporting them.
func TestQueueFailedCountSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	q1, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := q1.Put(job(refA, 1)); err != nil {
		t.Fatal(err)
	}
	claimed, _ := q1.Claim(time.Now())
	claimed.LastError = "boom"
	if err := q1.Fail(claimed); err != nil {
		t.Fatal(err)
	}
	if got := q1.FailedDepth(); got != 1 {
		t.Fatalf("FailedDepth = %d, want 1", got)
	}

	q2, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := q2.FailedDepth(); got != 1 {
		t.Fatalf("FailedDepth after restart = %d, want 1 — failures must stay visible", got)
	}
}
