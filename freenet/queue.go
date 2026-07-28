package freenet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// queue is a durable, directory-backed job queue with at-most-one pending job
// per ref.
//
// Layout under the configured queue dir:
//
//	<dir>/<ref>.json           pending — waiting to be published
//	<dir>/inflight/<ref>.json  claimed by the worker, publish in progress
//	<dir>/failed/<ref>.json    gave up after the retry budget (kept for operators)
//
// Naming the pending file after the ref gives dedupe/supersede for free: a
// newer revision atomically replaces the queued older one. A claim is a rename
// into inflight/, so a revision arriving mid-publish lands in a fresh pending
// file instead of being swallowed when the in-flight job completes. Stranded
// inflight/ files (hub killed mid-publish) are recovered to pending on open.
type queue struct {
	dir         string
	inflightDir string
	failedDir   string
}

// newQueue opens (creating if needed) the queue directories and recovers any
// job stranded in inflight/ by an unclean shutdown.
func newQueue(dir string) (*queue, error) {
	q := &queue{
		dir:         dir,
		inflightDir: filepath.Join(dir, "inflight"),
		failedDir:   filepath.Join(dir, "failed"),
	}
	for _, d := range []string{q.dir, q.inflightDir, q.failedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("freenet queue mkdir %s: %w", d, err)
		}
	}
	if err := q.recoverInflight(); err != nil {
		return nil, err
	}
	return q, nil
}

// Put enqueues a job, superseding a queued job for the same ref when the new
// revision is strictly newer. An equal-or-older revision is dropped: it is
// either a duplicate or a stale write that the queued job already covers.
func (q *queue) Put(j *Job) error {
	if !object.IsValidRef(j.Ref) {
		return fmt.Errorf("freenet queue: refusing job with invalid ref %q", j.Ref)
	}
	if existing, err := q.read(q.pendingPath(j.Ref)); err == nil && existing.Revision >= j.Revision {
		return nil
	}
	return q.write(q.pendingPath(j.Ref), j)
}

// Claim takes the oldest runnable pending job and moves it to inflight/.
// Returns (nil, nil) when nothing is runnable — either the queue is empty or
// every pending job is still waiting out its retry backoff.
func (q *queue) Claim(now time.Time) (*Job, error) {
	jobs, err := q.pending()
	if err != nil {
		return nil, err
	}

	var best *Job
	for _, j := range jobs {
		if j.NextAttemptAt.After(now) {
			continue
		}
		if best == nil || j.EnqueuedAt.Before(best.EnqueuedAt) ||
			(j.EnqueuedAt.Equal(best.EnqueuedAt) && j.Ref < best.Ref) {
			best = j
		}
	}
	if best == nil {
		return nil, nil
	}

	if err := os.Rename(q.pendingPath(best.Ref), q.inflightPath(best.Ref)); err != nil {
		return nil, fmt.Errorf("freenet queue claim %s: %w", best.Ref, err)
	}
	return best, nil
}

// Done drops a successfully published job.
func (q *queue) Done(j *Job) error {
	if err := os.Remove(q.inflightPath(j.Ref)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("freenet queue done %s: %w", j.Ref, err)
	}
	return nil
}

// Requeue returns an in-flight job to pending for another attempt. A newer
// revision enqueued while this job was in flight wins — Put's supersede rule
// keeps it and this attempt is simply dropped.
func (q *queue) Requeue(j *Job) error {
	if err := q.Put(j); err != nil {
		return err
	}
	return q.Done(j)
}

// Fail records a job that exhausted its retries under failed/ so it stays
// visible to operators instead of vanishing.
func (q *queue) Fail(j *Job) error {
	if err := q.write(q.failedPath(j.Ref), j); err != nil {
		return err
	}
	return q.Done(j)
}

// Depth counts pending jobs, including those waiting out a retry backoff.
func (q *queue) Depth() int {
	jobs, err := q.pending()
	if err != nil {
		return 0
	}
	return len(jobs)
}

// pending decodes every readable pending job. Unreadable or malformed files
// are skipped rather than failing the scan, so one bad file cannot wedge the
// queue.
func (q *queue) pending() ([]*Job, error) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("freenet queue list: %w", err)
	}

	var jobs []*Job
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		j, err := q.read(filepath.Join(q.dir, name))
		if err != nil {
			continue
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// recoverInflight moves jobs stranded by an unclean shutdown back to pending.
func (q *queue) recoverInflight() error {
	entries, err := os.ReadDir(q.inflightDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("freenet queue recover: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		j, err := q.read(filepath.Join(q.inflightDir, name))
		if err != nil {
			continue
		}
		// Put drops it if a newer revision is already pending; either way the
		// inflight copy goes away.
		if err := q.Put(j); err != nil {
			continue
		}
		os.Remove(filepath.Join(q.inflightDir, name))
	}
	return nil
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
	return nil
}
