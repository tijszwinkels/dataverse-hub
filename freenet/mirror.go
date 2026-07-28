// Package freenet implements the hub's optional write-through mirror: every
// public dataverse001 object the hub accepts is asynchronously republished to
// Freenet through an external publish command.
//
// The mirror is write-only by design. Nothing here is ever consulted to serve
// a read: a Freenet not-found can stall for minutes, which is fine for a
// background republish and unacceptable on a serving path.
package freenet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
)

const (
	// maxRecentEvents bounds the status endpoint's job history.
	maxRecentEvents = 20
	// defaultBackoffBase is the first retry delay; it doubles per attempt.
	defaultBackoffBase = 30 * time.Second
	// maxBackoff caps the exponential growth.
	maxBackoff = 10 * time.Minute
	// defaultPollInterval is how often the idle worker re-checks the queue.
	// Enqueues wake it immediately; this only bounds how late a job whose
	// retry backoff has expired starts.
	defaultPollInterval = 5 * time.Second
)

// Options configures a Mirror.
type Options struct {
	QueueDir   string        // directory holding pending/inflight/failed jobs
	PublishCmd string        // absolute path to the publish command
	Timeout    time.Duration // per-publish wall-clock budget
	Retries    int           // retry attempts after the initial one
}

// Event is one job transition, kept for the status endpoint.
type Event struct {
	Ref        string    `json:"ref"`
	Revision   int       `json:"revision"`
	Status     string    `json:"status"` // "succeeded", "retrying", or "failed"
	Attempts   int       `json:"attempts"`
	DurationMS int64     `json:"duration_ms"`
	At         time.Time `json:"at"`
	Error      string    `json:"error,omitempty"`
}

// Status is the snapshot served by GET /freenet/status.
type Status struct {
	Enabled    bool    `json:"enabled"`
	QueueDepth int     `json:"queue_depth"`
	InFlight   int     `json:"in_flight"`
	Succeeded  uint64  `json:"succeeded"`
	Failed     uint64  `json:"failed"`
	LastError  string  `json:"last_error"`
	Recent     []Event `json:"recent"`
}

// Mirror asynchronously republishes accepted public objects to Freenet.
//
// A nil *Mirror is a fully functional disabled mirror: every method is
// nil-safe, so call sites stay one-liners and "freenet disabled" needs no
// branch anywhere in the serving path.
type Mirror struct {
	q       *queue
	pub     Publisher
	retries int

	backoffBase  time.Duration
	pollInterval time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	notify chan struct{}
	wg     sync.WaitGroup
	once   sync.Once

	mu        sync.Mutex
	inFlight  *Job
	succeeded uint64
	failed    uint64
	lastError string
	recent    []Event
}

// New validates the configuration and opens the on-disk queue, recovering any
// jobs left behind by a previous run.
func New(opts Options) (*Mirror, error) {
	if err := validateCommand(opts.PublishCmd); err != nil {
		return nil, err
	}
	if opts.QueueDir == "" {
		return nil, errors.New("freenet queue_dir is empty")
	}
	if opts.Timeout <= 0 {
		return nil, fmt.Errorf("freenet timeout must be positive, got %s", opts.Timeout)
	}
	if opts.Retries < 0 {
		return nil, fmt.Errorf("freenet retries must not be negative, got %d", opts.Retries)
	}

	q, err := newQueue(opts.QueueDir)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Mirror{
		q:            q,
		pub:          newExecPublisher(opts.PublishCmd, opts.Timeout, tmpDirFor(opts.QueueDir)),
		retries:      opts.Retries,
		backoffBase:  defaultBackoffBase,
		pollInterval: defaultPollInterval,
		ctx:          ctx,
		cancel:       cancel,
		notify:       make(chan struct{}, 1),
	}, nil
}

// Start launches the single publish worker. Publishes are serialized: a
// Freenet publish is heavy, and one object at a time keeps the load
// predictable and the ordering obvious.
func (m *Mirror) Start() {
	if m == nil {
		return
	}
	m.wg.Add(1)
	go m.run()
	log.Printf("[freenet] mirror started (queue: %s, %d pending)", m.q.dir, m.q.Depth())
}

// Stop cancels any in-flight publish and waits for the worker to finish. The
// interrupted job is returned to the queue without burning a retry attempt, so
// it resumes on the next boot.
func (m *Mirror) Stop() {
	if m == nil {
		return
	}
	m.once.Do(func() {
		m.cancel()
		m.wg.Wait()
		log.Printf("[freenet] mirror stopped (%d pending)", m.q.Depth())
	})
}

// Publish enqueues an accepted object for mirroring and returns immediately.
//
// Only globally-public dataverse001 objects are mirrored. That check lives
// here rather than at the call sites so there is exactly one gate between the
// hub and a public network: a mis-wired or newly added call site cannot leak
// an identity-realm or shared-realm object. "server-public" is deliberately
// excluded — it means hub-local, and realm.IsGlobalObject is the same
// predicate that already decides what gets pushed upstream.
//
// The enqueue itself is synchronous: it is one small temp-file+rename, the
// same cost the hub already pays in storage.Store.Write on this code path, and
// doing it inline is what makes "a pending mirror survives a restart" true. It
// never fails the client's write — an enqueue error is logged and dropped.
func (m *Mirror) Publish(ref string, revision int, realms object.InField, envelope []byte) {
	if m == nil {
		return
	}
	if !realm.IsGlobalObject(realms) {
		return
	}
	if !object.IsValidRef(ref) {
		log.Printf("[freenet] WARN: refusing to mirror invalid ref %q", ref)
		return
	}

	// Copy the envelope: the caller's buffer is not ours to hold on to.
	buf := make([]byte, len(envelope))
	copy(buf, envelope)

	j := &Job{
		Ref:        ref,
		Revision:   revision,
		EnqueuedAt: time.Now(),
		Envelope:   json.RawMessage(buf),
	}
	if err := m.q.Put(j); err != nil {
		log.Printf("[freenet] ERROR: enqueue %s rev %d: %v", ref, revision, err)
		return
	}
	log.Printf("[freenet] queued %s rev %d (depth: %d)", ref, revision, m.q.Depth())
	m.wake()
}

// Status reports the mirror's state. Safe on a nil (disabled) mirror.
func (m *Mirror) Status() Status {
	if m == nil {
		return Status{Recent: []Event{}}
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	inFlight := 0
	if m.inFlight != nil {
		inFlight = 1
	}
	recent := make([]Event, len(m.recent))
	copy(recent, m.recent)

	return Status{
		Enabled:    true,
		QueueDepth: m.q.Depth(),
		InFlight:   inFlight,
		Succeeded:  m.succeeded,
		Failed:     m.failed,
		LastError:  m.lastError,
		Recent:     recent,
	}
}

// run is the worker loop: drain everything runnable, then sleep until an
// enqueue wakes us or the poll interval expires (which is what picks up jobs
// whose retry backoff has elapsed).
func (m *Mirror) run() {
	defer m.wg.Done()

	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	for {
		m.drain()
		select {
		case <-m.ctx.Done():
			return
		case <-m.notify:
		case <-ticker.C:
		}
	}
}

func (m *Mirror) drain() {
	for m.ctx.Err() == nil {
		j, err := m.q.Claim(time.Now())
		if err != nil {
			log.Printf("[freenet] ERROR: claim job: %v", err)
			return
		}
		if j == nil {
			return
		}
		m.publish(j)
	}
}

// publish runs one job and applies the outcome to the queue.
func (m *Mirror) publish(j *Job) {
	m.setInFlight(j)
	start := time.Now()
	output, err := m.pub.Publish(m.ctx, j)
	elapsed := time.Since(start)
	m.setInFlight(nil)

	switch {
	case err == nil:
		log.Printf("[freenet] published %s rev %d in %s (attempt %d)",
			j.Ref, j.Revision, elapsed.Round(time.Millisecond), j.Attempts+1)
		if err := m.q.Done(j); err != nil {
			log.Printf("[freenet] WARN: dequeue %s: %v", j.Ref, err)
		}
		m.succeed(j, elapsed)

	case errors.Is(err, ErrAborted):
		// Shutdown, not a publish failure: requeue without spending an attempt.
		log.Printf("[freenet] publish of %s rev %d interrupted by shutdown, requeued", j.Ref, j.Revision)
		if err := m.q.Requeue(j); err != nil {
			log.Printf("[freenet] WARN: requeue %s: %v", j.Ref, err)
		}

	default:
		j.Attempts++
		j.LastError = describeFailure(err, output)
		if j.Attempts > m.retries {
			log.Printf("[freenet] ERROR: giving up on %s rev %d after %d attempt(s): %v\n%s",
				j.Ref, j.Revision, j.Attempts, err, output)
			if err := m.q.Fail(j); err != nil {
				log.Printf("[freenet] WARN: record failed job %s: %v", j.Ref, err)
			}
			m.fail(j, elapsed)
			return
		}
		backoff := m.backoffFor(j.Attempts)
		j.NextAttemptAt = time.Now().Add(backoff)
		log.Printf("[freenet] WARN: publish %s rev %d failed (attempt %d/%d), retrying in %s: %v\n%s",
			j.Ref, j.Revision, j.Attempts, m.retries+1, backoff, err, output)
		if err := m.q.Requeue(j); err != nil {
			log.Printf("[freenet] WARN: requeue %s: %v", j.Ref, err)
		}
		m.retry(j, elapsed)
	}
}

// backoffFor returns the delay before attempt n+1: base, 2x base, 4x base …
// capped at maxBackoff.
func (m *Mirror) backoffFor(attempts int) time.Duration {
	d := m.backoffBase
	for i := 1; i < attempts && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

func (m *Mirror) wake() {
	select {
	case m.notify <- struct{}{}:
	default: // already signalled; the worker will see the queue either way
	}
}

func (m *Mirror) setInFlight(j *Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight = j
}

func (m *Mirror) succeed(j *Job, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.succeeded++
	m.appendEvent(Event{
		Ref: j.Ref, Revision: j.Revision, Status: "succeeded",
		Attempts: j.Attempts + 1, DurationMS: d.Milliseconds(), At: time.Now(),
	})
}

func (m *Mirror) retry(j *Job, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastError = j.LastError
	m.appendEvent(Event{
		Ref: j.Ref, Revision: j.Revision, Status: "retrying",
		Attempts: j.Attempts, DurationMS: d.Milliseconds(), At: time.Now(), Error: j.LastError,
	})
}

func (m *Mirror) fail(j *Job, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed++
	m.lastError = j.LastError
	m.appendEvent(Event{
		Ref: j.Ref, Revision: j.Revision, Status: "failed",
		Attempts: j.Attempts, DurationMS: d.Milliseconds(), At: time.Now(), Error: j.LastError,
	})
}

// record appends an event. Callers hold no lock.
func (m *Mirror) record(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendEvent(e)
}

// appendEvent prepends to the newest-first ring. Caller must hold m.mu.
func (m *Mirror) appendEvent(e Event) {
	m.recent = append([]Event{e}, m.recent...)
	if len(m.recent) > maxRecentEvents {
		m.recent = m.recent[:maxRecentEvents]
	}
}

// describeFailure combines the error with the tail of the publisher's output,
// which is where publish-v2.sh puts its per-target poke report.
func describeFailure(err error, output string) string {
	if output == "" {
		return err.Error()
	}
	return err.Error() + ": " + lastLines(output, 5)
}

// lastLines flattens the final n non-empty lines of s onto one line, so a
// multi-line publisher report stays readable in a single log record.
func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// tmpDirFor is where envelopes are staged for the publish command — inside the
// queue dir so it shares a filesystem and an operator has one place to look.
func tmpDirFor(queueDir string) string {
	return filepath.Join(queueDir, "tmp")
}
