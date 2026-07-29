package freenet

import (
	"encoding/json"
	"errors"
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
	if got := q.Depth(); got != 5 {
		t.Fatalf("Depth = %d, want 5 — an unreadable stray file must not break counting", got)
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

// The lock that fixes the revision-loss race must not put a full queue scan in
// the write path's way: Claim decoding every queued envelope while holding the
// lock Put needs would let a backlog delay client writes.
func TestQueueClaimDoesNotScanWholeQueue(t *testing.T) {
	q := testQueue(t)

	// 400 jobs of ~20 KiB each: decoding them all per claim would be ~8 MiB of
	// JSON per cycle, and 200 cycles would take many seconds.
	big := make([]byte, 20<<10)
	for i := range big {
		big[i] = 'x'
	}
	const jobs = 400
	for i := 0; i < jobs; i++ {
		j := job(refWithSuffix(i), 1)
		j.Envelope = json.RawMessage(`{"blob":"` + string(big) + `"}`)
		if err := q.Put(j); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	for i := 0; i < 200; i++ {
		claimed, err := q.Claim(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if claimed == nil {
			t.Fatalf("Claim %d returned nil with %d jobs queued", i, q.Depth())
		}
		if err := q.Requeue(claimed); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("200 claim/requeue cycles over %d queued jobs took %v — Claim is scanning the whole queue", jobs, elapsed)
	}
}

// refWithSuffix builds distinct valid refs for bulk tests.
func refWithSuffix(i int) string {
	hex := "0123456789abcdef"
	suffix := string([]byte{hex[(i>>8)&0xf], hex[(i>>4)&0xf], hex[i&0xf]})
	return refA[:len(refA)-3] + suffix
}

// A job that could not be moved into failed/ stays in inflight/ and must
// remain countable, not silently disappear.
func TestQueueInflightDepthExposesStrandedJobs(t *testing.T) {
	q := testQueue(t)
	if err := q.Put(job(refA, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim(time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := q.InflightDepth(); got != 1 {
		t.Fatalf("InflightDepth = %d, want 1", got)
	}
}

// A failed claim must leave the job claimable. Dropping the index entry before
// the rename succeeded would hide a job that is still sitting on disk.
func TestQueueFailedClaimKeepsJobVisible(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — directory permissions would not be enforced")
	}
	q := testQueue(t)
	if err := q.Put(job(refA, 1)); err != nil {
		t.Fatal(err)
	}

	// Make the rename into inflight/ fail.
	if err := os.Chmod(q.inflightDir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(q.inflightDir, 0o755)

	if _, err := q.Claim(time.Now()); err == nil {
		t.Fatal("Claim succeeded, want the rename failure surfaced")
	}
	if got := q.Depth(); got != 1 {
		t.Fatalf("Depth = %d, want 1 — a failed claim must not lose track of the job", got)
	}

	// Once the obstruction clears, the job is claimable again.
	os.Chmod(q.inflightDir, 0o755)
	claimed, err := q.Claim(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.Revision != 1 {
		t.Fatalf("claimed %v, want the job back", claimed)
	}
}

// Corruption present at startup must be parked and countable, not invisible.
func TestQueueParksUnreadablePendingJobsAtStartup(t *testing.T) {
	dir := t.TempDir()
	q1, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := q1.Put(job(refA, 1)); err != nil {
		t.Fatal(err)
	}
	// Corrupt it, as a torn write or bad disk would.
	if err := os.WriteFile(q1.pendingPath(refA), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	q2, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := q2.Depth(); got != 0 {
		t.Fatalf("Depth = %d, want 0", got)
	}
	if got := q2.FailedDepth(); got != 1 {
		t.Fatalf("FailedDepth = %d, want 1 — a corrupt job must be parked, not silently ignored", got)
	}
	if n := countJSON(t, q2.dir); n != 0 {
		t.Fatalf("%d files left in pending, want 0", n)
	}
}

// Crash between Fail's write and its cleanup: recovery must not resurrect a job
// that already has a terminal record.
func TestQueueDoesNotRecoverJobsAlreadyRecordedAsFailed(t *testing.T) {
	dir := t.TempDir()
	q1, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := q1.Put(job(refA, 3)); err != nil {
		t.Fatal(err)
	}
	claimed, _ := q1.Claim(time.Now())
	claimed.Attempts = 4
	claimed.LastError = "gave up"

	// Write the terminal record but leave the inflight copy behind, exactly as
	// a crash between the two steps would.
	if err := q1.write(q1.failedPath(refA), claimed); err != nil {
		t.Fatal(err)
	}

	q2, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := q2.Depth(); got != 0 {
		t.Fatalf("Depth = %d, want 0 — an exhausted job must not be republished past its budget", got)
	}
	if got := q2.InflightDepth(); got != 0 {
		t.Fatalf("InflightDepth = %d, want 0 — the stale inflight copy should be cleared", got)
	}
	if got := q2.FailedDepth(); got != 1 {
		t.Fatalf("FailedDepth = %d, want 1", got)
	}
}

// failed_queued must match what is actually on disk: two failures of the same
// ref overwrite one file.
func TestQueueFailedCountMatchesDisk(t *testing.T) {
	q := testQueue(t)

	for _, rev := range []int{1, 2} {
		if err := q.Put(job(refA, rev)); err != nil {
			t.Fatal(err)
		}
		claimed, err := q.Claim(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Fail(claimed); err != nil {
			t.Fatal(err)
		}
	}

	onDisk := countJobFiles(q.failedDir)
	if onDisk != 1 {
		t.Fatalf("%d files in failed/, want 1 (same ref overwrites)", onDisk)
	}
	if got := q.FailedDepth(); got != onDisk {
		t.Fatalf("FailedDepth = %d, want %d to match disk", got, onDisk)
	}
}

// The rename is what makes a job live; the directory sync only makes it
// survive a power loss. If the sync fails the job is still queued, so the
// in-memory index must agree with the filesystem — reporting it as dropped
// would abandon a job that is sitting right there.
func TestQueueSyncFailureStillEnqueues(t *testing.T) {
	q := testQueue(t)

	original := syncDirFn
	syncDirFn = func(string) error { return errors.New("simulated fsync failure") }
	defer func() { syncDirFn = original }()

	if err := q.Put(job(refA, 1)); err != nil {
		t.Fatalf("Put = %v, want success — the file was installed by the rename", err)
	}
	if got := q.Depth(); got != 1 {
		t.Fatalf("Depth = %d, want 1 — a durability warning must not orphan the job", got)
	}
	if n := countJSON(t, q.dir); n != 1 {
		t.Fatalf("%d files in pending, want 1", n)
	}
	claimed, err := q.Claim(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.Revision != 1 {
		t.Fatalf("claimed %v, want the job to be processable", claimed)
	}
}

// Fail must stay consistent too: a sync failure cannot leave the terminal
// record uncounted or the inflight copy behind.
func TestQueueSyncFailureStillRecordsFailure(t *testing.T) {
	q := testQueue(t)
	if err := q.Put(job(refA, 1)); err != nil {
		t.Fatal(err)
	}
	claimed, _ := q.Claim(time.Now())

	original := syncDirFn
	syncDirFn = func(string) error { return errors.New("simulated fsync failure") }
	defer func() { syncDirFn = original }()

	if err := q.Fail(claimed); err != nil {
		t.Fatalf("Fail = %v, want success", err)
	}
	if got := q.FailedDepth(); got != 1 {
		t.Fatalf("FailedDepth = %d, want 1", got)
	}
	if got := q.InflightDepth(); got != 0 {
		t.Fatalf("InflightDepth = %d, want 0 — the inflight copy must still be cleared", got)
	}
}

// hasTerminalRecord must suppress recovery only for equal-or-newer records.
func TestQueueRecoversWhenTerminalRecordIsOlder(t *testing.T) {
	dir := t.TempDir()
	q1, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	// An old failure for rev 1 is on record...
	old := job(refA, 1)
	if err := q1.write(q1.failedPath(refA), old); err != nil {
		t.Fatal(err)
	}
	// ...while rev 4 was in flight when the hub died.
	if err := q1.write(q1.inflightPath(refA), job(refA, 4)); err != nil {
		t.Fatal(err)
	}

	q2, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := q2.Depth(); got != 1 {
		t.Fatalf("Depth = %d, want 1 — a newer in-flight revision must still be recovered", got)
	}
	claimed, _ := q2.Claim(time.Now())
	if claimed == nil || claimed.Revision != 4 {
		t.Fatalf("claimed %v, want rev 4", claimed)
	}
}

func TestQueueDoesNotRecoverWhenTerminalRecordIsNewer(t *testing.T) {
	dir := t.TempDir()
	q1, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := q1.write(q1.failedPath(refA), job(refA, 9)); err != nil {
		t.Fatal(err)
	}
	if err := q1.write(q1.inflightPath(refA), job(refA, 4)); err != nil {
		t.Fatal(err)
	}

	q2, err := newQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := q2.Depth(); got != 0 {
		t.Fatalf("Depth = %d, want 0 — a newer terminal record supersedes the stale inflight copy", got)
	}
}
