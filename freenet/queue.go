package freenet

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tijszwinkels/dataverse-hub/object"
)

// Job is one pending mirror publish: the signed envelope plus the bookkeeping
// needed to retry it across restarts.
type Job struct {
	Ref           string          `json:"ref"`
	Revision      int             `json:"revision"`
	EnqueuedAt    time.Time       `json:"enqueued_at"`
	NextAttemptAt time.Time       `json:"next_attempt_at,omitempty"`
	Attempts      int             `json:"attempts"`
	LastError     string          `json:"last_error,omitempty"`
	Envelope      json.RawMessage `json:"envelope"`
}

// pendingEntry is the in-memory summary of one pending job — everything Claim
// needs to choose the next job, without reading the job's envelope.
type pendingEntry struct {
	revision      int
	enqueuedAt    time.Time
	nextAttemptAt time.Time
}

// queue is a durable, directory-backed job queue with at-most-one pending job
// per ref.
//
// Layout under the configured queue dir:
//
//	<dir>/<ref>.json           pending — waiting to be published
//	<dir>/inflight/<ref>.json  claimed by the worker, publish in progress
//	<dir>/failed/<ref>.json    gave up after the retry budget (kept for operators)
//	<dir>/tmp/                 envelopes staged for the publish command
//
// Naming the pending file after the ref gives dedupe/supersede for free: a
// newer revision atomically replaces the queued older one. A claim is a rename
// into inflight/, so a revision arriving mid-publish lands in a fresh pending
// file instead of being swallowed when the in-flight job completes. Stranded
// inflight/ files (hub killed mid-publish) are recovered to pending on open.
type queue struct {
	// mu serializes every queue operation. The HTTP write path calls Put while
	// the worker calls Claim/Done/Requeue/Fail, and the read-then-act sequences
	// inside them (Put's supersede check, Claim's choose-then-rename) are not
	// atomic on their own: without this lock a Put landing between Claim's
	// choice and its rename would be renamed into inflight and then deleted by
	// Done, losing that revision for good.
	//
	// Everything done under this lock is O(1) in disk I/O. `pending` is an
	// in-memory index precisely so Claim does not have to read the whole queue
	// while holding the lock that client writes need — a backlog must never
	// become write-path latency.
	mu          sync.Mutex
	pending     map[string]pendingEntry
	failedCount int

	dir         string
	inflightDir string
	failedDir   string
}

// newQueue opens (creating if needed) the queue directories, indexes the
// pending jobs, and recovers any job stranded in inflight/ by an unclean
// shutdown.
func newQueue(dir string) (*queue, error) {
	q := &queue{
		pending:     make(map[string]pendingEntry),
		dir:         dir,
		inflightDir: filepath.Join(dir, "inflight"),
		failedDir:   filepath.Join(dir, "failed"),
	}
	for _, d := range []string{q.dir, q.inflightDir, q.failedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("freenet queue mkdir %s: %w", d, err)
		}
	}
	if err := q.loadPending(); err != nil {
		return nil, err
	}
	q.failedCount = countJobFiles(q.failedDir)
	q.recoverInflight()
	return q, nil
}

// Put enqueues a job, superseding a queued job for the same ref when the new
// revision is strictly newer. An equal-or-older revision is dropped: it is
// either a duplicate or a stale write that the queued job already covers.
func (q *queue) Put(j *Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.put(j)
}

// put is Put without the lock. Callers must hold q.mu.
func (q *queue) put(j *Job) error {
	if !object.IsValidRef(j.Ref) {
		return fmt.Errorf("freenet queue: refusing job with invalid ref %q", j.Ref)
	}
	if existing, ok := q.pending[j.Ref]; ok && existing.revision >= j.Revision {
		return nil
	}
	if err := q.write(q.pendingPath(j.Ref), j); err != nil {
		return err
	}
	q.pending[j.Ref] = pendingEntry{
		revision:      j.Revision,
		enqueuedAt:    j.EnqueuedAt,
		nextAttemptAt: j.NextAttemptAt,
	}
	return nil
}

// Claim takes the oldest runnable pending job and moves it to inflight/.
// Returns (nil, nil) when nothing is runnable — either the queue is empty or
// every pending job is still waiting out its retry backoff.
//
// Candidate selection is pure in-memory work; only the chosen job is touched
// on disk.
func (q *queue) Claim(now time.Time) (*Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for {
		ref, ok := q.nextRunnable(now)
		if !ok {
			return nil, nil
		}
		// Rename first: dropping the index entry before the rename succeeded
		// would hide a job that is still sitting in the pending directory.
		if err := os.Rename(q.pendingPath(ref), q.inflightPath(ref)); err != nil {
			return nil, fmt.Errorf("freenet queue claim %s: %w", ref, err)
		}
		delete(q.pending, ref)
		j, err := q.read(q.inflightPath(ref))
		if err != nil {
			// The file vanished or was corrupted underneath us. Park it rather
			// than wedging the queue, and move on to the next candidate.
			log.Printf("[freenet] ERROR: unreadable job %s, parking in failed/: %v", ref, err)
			isNew := !fileExists(q.failedPath(ref))
			if renameErr := os.Rename(q.inflightPath(ref), q.failedPath(ref)); renameErr != nil {
				log.Printf("[freenet] ERROR: could not park %s: %v", ref, renameErr)
			} else if isNew {
				q.failedCount++
			}
			continue
		}
		return j, nil
	}
}

// nextRunnable picks the oldest enqueued job whose backoff has elapsed.
// Caller must hold q.mu.
func (q *queue) nextRunnable(now time.Time) (string, bool) {
	var bestRef string
	var best pendingEntry
	for ref, e := range q.pending {
		if e.nextAttemptAt.After(now) {
			continue
		}
		if bestRef == "" || e.enqueuedAt.Before(best.enqueuedAt) ||
			(e.enqueuedAt.Equal(best.enqueuedAt) && ref < bestRef) {
			bestRef, best = ref, e
		}
	}
	return bestRef, bestRef != ""
}

// Done drops a successfully published job.
func (q *queue) Done(j *Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.done(j)
}

// done is Done without the lock. Callers must hold q.mu.
func (q *queue) done(j *Job) error {
	if err := os.Remove(q.inflightPath(j.Ref)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("freenet queue done %s: %w", j.Ref, err)
	}
	return nil
}

// Requeue returns an in-flight job to pending for another attempt. A newer
// revision enqueued while this job was in flight wins — put's supersede rule
// keeps it and this attempt is simply dropped.
func (q *queue) Requeue(j *Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if err := q.put(j); err != nil {
		return err
	}
	return q.done(j)
}

// Fail records a job that exhausted its retries under failed/ so it stays
// visible to operators instead of vanishing.
//
// If the failed/ write fails there is nowhere durable to put it, so the job
// deliberately stays in inflight/ — InflightDepth keeps it countable and the
// next start recovers it to pending.
func (q *queue) Fail(j *Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	// failedCount tracks files, not events: a second failure of the same ref
	// overwrites one file and must not double-count.
	isNew := !fileExists(q.failedPath(j.Ref))
	if err := q.write(q.failedPath(j.Ref), j); err != nil {
		return err
	}
	if isNew {
		q.failedCount++
	}
	return q.done(j)
}

// Depth counts pending jobs, including those waiting out a retry backoff.
func (q *queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// FailedDepth counts jobs that exhausted their retries and are waiting for an
// operator. Seeded from disk at open, so it survives a restart.
func (q *queue) FailedDepth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.failedCount
}

// InflightDepth counts job files under inflight/. During a publish this is 1.
// A value that stays above the worker's actual in-flight count means a job was
// stranded — recovery could not move it — and needs an operator.
func (q *queue) InflightDepth() int {
	return countJobFiles(q.inflightDir)
}

// countJobFiles counts job files in dir without opening them.
func countJobFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if isJobFile(e) {
			n++
		}
	}
	return n
}

func isJobFile(e os.DirEntry) bool {
	name := e.Name()
	return !e.IsDir() && !strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".json")
}

// loadPending indexes the pending directory at startup. Unreadable or
// malformed files are logged and skipped rather than failing the open, so one
// bad file cannot stop the hub from booting.
func (q *queue) loadPending() error {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("freenet queue list: %w", err)
	}

	for _, e := range entries {
		if !isJobFile(e) {
			continue
		}
		path := filepath.Join(q.dir, e.Name())
		j, err := q.read(path)
		if err != nil {
			// Corruption that predates this process would otherwise be
			// invisible forever: not indexed, so never claimed, never counted.
			// Park it where an operator will find it.
			log.Printf("[freenet] ERROR: unreadable queue file %s, parking in failed/: %v", path, err)
			q.park(path, e.Name())
			continue
		}
		q.pending[j.Ref] = pendingEntry{
			revision:      j.Revision,
			enqueuedAt:    j.EnqueuedAt,
			nextAttemptAt: j.NextAttemptAt,
		}
	}
	return nil
}

// recoverInflight moves jobs stranded by an unclean shutdown back to pending.
// A job that cannot be recovered stays in inflight/, where InflightDepth keeps
// it visible; the failure is logged rather than swallowed.
func (q *queue) recoverInflight() {
	entries, err := os.ReadDir(q.inflightDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[freenet] ERROR: cannot read inflight dir %s: %v", q.inflightDir, err)
		}
		return
	}

	stranded := 0
	for _, e := range entries {
		if !isJobFile(e) {
			continue
		}
		path := filepath.Join(q.inflightDir, e.Name())
		j, err := q.read(path)
		if err != nil {
			log.Printf("[freenet] ERROR: stranded inflight job %s is unreadable: %v", path, err)
			stranded++
			continue
		}
		// A terminal record for this revision means we crashed between Fail's
		// write and its cleanup. Requeueing would publish a job past its retry
		// budget while it is simultaneously recorded as failed.
		if q.hasTerminalRecord(j) {
			log.Printf("[freenet] %s rev %d is already recorded as failed, discarding the stale inflight copy", j.Ref, j.Revision)
			if err := os.Remove(path); err != nil {
				log.Printf("[freenet] WARN: could not remove stale inflight copy %s: %v", path, err)
			}
			continue
		}
		// put drops it if a newer revision is already pending; either way the
		// inflight copy goes away.
		if err := q.put(j); err != nil {
			log.Printf("[freenet] ERROR: cannot recover inflight job %s, leaving it in place: %v", path, err)
			stranded++
			continue
		}
		if err := os.Remove(path); err != nil {
			log.Printf("[freenet] WARN: recovered %s but could not remove the inflight copy: %v", j.Ref, err)
		}
		log.Printf("[freenet] recovered interrupted job %s rev %d", j.Ref, j.Revision)
	}
	if stranded > 0 {
		log.Printf("[freenet] ERROR: %d job(s) stranded in %s and could not be recovered — they will NOT be published until this is resolved",
			stranded, q.inflightDir)
	}
}

// park moves an unreadable queue file into failed/ so it stays countable.
func (q *queue) park(path, name string) {
	dst := filepath.Join(q.failedDir, name)
	isNew := !fileExists(dst)
	if err := os.Rename(path, dst); err != nil {
		log.Printf("[freenet] ERROR: could not park %s: %v", path, err)
		return
	}
	if isNew {
		q.failedCount++
	}
}

// hasTerminalRecord reports whether failed/ already holds this revision (or a
// newer one) for the job's ref.
func (q *queue) hasTerminalRecord(j *Job) bool {
	recorded, err := q.read(q.failedPath(j.Ref))
	return err == nil && recorded.Revision >= j.Revision
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (q *queue) pendingPath(ref string) string  { return filepath.Join(q.dir, ref+".json") }
func (q *queue) inflightPath(ref string) string { return filepath.Join(q.inflightDir, ref+".json") }
func (q *queue) failedPath(ref string) string   { return filepath.Join(q.failedDir, ref+".json") }

func (q *queue) read(path string) (*Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, err
	}
	if !object.IsValidRef(j.Ref) {
		return nil, fmt.Errorf("freenet queue: job file %s holds invalid ref %q", path, j.Ref)
	}
	return &j, nil
}

// write serializes a job to path via a temp file + rename, so a crash mid-write
// leaves either the old file or the new one, never a truncated job.
func (q *queue) write(path string, j *Job) error {
	data, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("freenet queue marshal %s: %w", j.Ref, err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("freenet queue temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("freenet queue write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("freenet queue sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("freenet queue close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("freenet queue rename: %w", err)
	}

	// The rename has committed the file: it is live, and every caller's
	// in-memory bookkeeping must now match the filesystem. Syncing the
	// directory is what additionally makes it survive a host power-loss
	// (fsync(2)), so a failure here degrades durability without invalidating
	// the write — returning an error would make callers record a job as
	// dropped while it is in fact sitting in the queue, which is strictly
	// worse. Log it loudly and report success.
	if err := syncDirFn(dir); err != nil {
		log.Printf("[freenet] ERROR: %v — the record for %s is written to %s and is live, but may not survive a host power loss", err, j.Ref, dir)
	}
	return nil
}

// syncDirFn is indirected so tests can exercise the durability-failure path.
var syncDirFn = syncDir

// syncDir flushes a directory's metadata so renames within it are durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("freenet queue open dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("freenet queue sync dir %s: %w", dir, err)
	}
	return nil
}
