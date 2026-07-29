package freenet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/object"
)

// testMirror builds a Mirror wired to the fake publisher script, with the
// timers wound down so retry/backoff behaviour is testable in milliseconds.
// Returns the mirror and a func reading back the publisher's invocations.
func testMirror(t *testing.T, retries int) (*Mirror, func() []string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "invocations.log")
	t.Setenv("FAKE_PUBLISH_LOG", logPath)

	m, err := New(Options{
		QueueDir:   filepath.Join(dir, "queue"),
		PublishCmd: fakePublisherPath(t),
		Timeout:    10 * time.Second,
		Retries:    retries,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.backoffBase = 5 * time.Millisecond
	m.pollInterval = 5 * time.Millisecond

	return m, func() []string {
		data, err := os.ReadFile(logPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatal(err)
		}
		var lines []string
		for _, l := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		return lines
	}
}

// record appends an event directly. Test-only: production code reaches
// appendEvent through succeed/retry/fail/drop.
func (m *Mirror) record(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendEvent(e)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

var publicRealms = object.InField{"dataverse001"}

func envelopeFor(rev int) []byte {
	return []byte(`{"is":"instructionGraph001","item":{"revision":` + itoa(rev) + `},"signature":"sig"}`)
}

func TestMirrorPublishesPublicObject(t *testing.T) {
	m, invocations := testMirror(t, 3)
	m.Start()
	defer m.Stop()

	m.Publish(refA, 1, publicRealms, envelopeFor(1))

	waitFor(t, "the publish to complete", func() bool { return m.Status().Succeeded == 1 })

	got := invocations()
	if len(got) != 1 {
		t.Fatalf("%d invocations, want 1", len(got))
	}
	if got[0] != string(envelopeFor(1)) {
		t.Errorf("published %s, want the exact envelope", got[0])
	}

	st := m.Status()
	if !st.Enabled {
		t.Error("Status().Enabled = false, want true")
	}
	if st.QueueDepth != 0 || st.InFlight != 0 || st.Failed != 0 {
		t.Errorf("after success: depth=%d inflight=%d failed=%d, want all 0", st.QueueDepth, st.InFlight, st.Failed)
	}
	if len(st.Recent) != 1 || st.Recent[0].Status != "succeeded" || st.Recent[0].Ref != refA {
		t.Errorf("Recent = %+v, want one succeeded event for %s", st.Recent, refA)
	}
}

// The security-critical case: nothing outside the public dataverse001 realm
// may ever reach the publisher.
func TestMirrorOnlyPublishesGlobalPublicObjects(t *testing.T) {
	pubkey := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"
	notMirrored := []struct {
		name   string
		realms object.InField
	}{
		{"identity realm", object.InField{pubkey}},
		{"shared realm", object.InField{pubkey + ".Talksome"}},
		{"server-public (hub-local, not global)", object.InField{"server-public"}},
		{"identity + shared", object.InField{pubkey, pubkey + ".ConverseAI"}},
		{"no realms at all", nil},
		{"empty realm list", object.InField{}},
	}

	for _, tc := range notMirrored {
		t.Run(tc.name, func(t *testing.T) {
			m, invocations := testMirror(t, 3)
			m.Start()
			defer m.Stop()

			m.Publish(refA, 1, tc.realms, envelopeFor(1))

			// Give the worker a real chance to do the wrong thing.
			time.Sleep(150 * time.Millisecond)
			if got := invocations(); len(got) != 0 {
				t.Fatalf("%s leaked to the publisher: %v", tc.name, got)
			}
			if st := m.Status(); st.QueueDepth != 0 || st.Succeeded != 0 {
				t.Fatalf("depth=%d succeeded=%d, want 0 — job must never be enqueued", st.QueueDepth, st.Succeeded)
			}
		})
	}

	// Control: dataverse001 alongside an identity realm is public content and
	// is mirrored, matching how upstream_push="public" already treats it.
	t.Run("dataverse001 plus identity realm is mirrored", func(t *testing.T) {
		m, invocations := testMirror(t, 3)
		m.Start()
		defer m.Stop()

		m.Publish(refA, 1, object.InField{"dataverse001", pubkey}, envelopeFor(1))
		waitFor(t, "the publish to complete", func() bool { return len(invocations()) == 1 })
	})
}

func TestMirrorPublishDoesNotBlockCaller(t *testing.T) {
	t.Setenv("FAKE_PUBLISH_SLEEP", "30")
	m, _ := testMirror(t, 3)
	m.Start()
	defer m.Stop()

	start := time.Now()
	m.Publish(refA, 1, publicRealms, envelopeFor(1))
	elapsed := time.Since(start)

	// Enqueue is a small local file write; the multi-minute publish is the
	// worker's problem, never the client's.
	if elapsed > 2*time.Second {
		t.Fatalf("Publish blocked for %v — the client write must never wait on Freenet", elapsed)
	}
}

func TestMirrorSkipsInvalidRef(t *testing.T) {
	m, invocations := testMirror(t, 3)
	m.Start()
	defer m.Stop()

	m.Publish("../../etc/passwd", 1, publicRealms, envelopeFor(1))

	time.Sleep(150 * time.Millisecond)
	if got := invocations(); len(got) != 0 {
		t.Fatalf("invalid ref reached the publisher: %v", got)
	}
}

func TestMirrorRetriesUntilSuccess(t *testing.T) {
	t.Setenv("FAKE_PUBLISH_FAIL_TIMES", "2")
	m, invocations := testMirror(t, 3)
	m.Start()
	defer m.Stop()

	m.Publish(refA, 1, publicRealms, envelopeFor(1))

	waitFor(t, "the retried publish to succeed", func() bool { return m.Status().Succeeded == 1 })
	if got := invocations(); len(got) != 3 {
		t.Fatalf("%d invocations, want 3 (2 failures then success)", len(got))
	}
	if st := m.Status(); st.Failed != 0 {
		t.Errorf("Failed = %d, want 0 — the job eventually succeeded", st.Failed)
	}
}

func TestMirrorGivesUpAfterRetryBudget(t *testing.T) {
	t.Setenv("FAKE_PUBLISH_EXIT", "3")
	m, invocations := testMirror(t, 2) // 1 initial attempt + 2 retries
	m.Start()
	defer m.Stop()

	m.Publish(refA, 9, publicRealms, envelopeFor(9))

	waitFor(t, "the mirror to give up", func() bool { return m.Status().Failed == 1 })

	if got := invocations(); len(got) != 3 {
		t.Fatalf("%d invocations, want 3 (initial + 2 retries)", len(got))
	}

	st := m.Status()
	if st.QueueDepth != 0 {
		t.Errorf("QueueDepth = %d, want 0 — the job left the pending queue", st.QueueDepth)
	}
	if st.LastError == "" {
		t.Error("LastError is empty, want the publisher's failure recorded")
	}
	if len(st.Recent) == 0 || st.Recent[0].Status != "failed" {
		t.Errorf("Recent[0] = %+v, want a failed event", st.Recent)
	}

	// A dropped mirror must stay visible to an operator, not vanish.
	failedFile := filepath.Join(m.q.failedDir, refA+".json")
	data, err := os.ReadFile(failedFile)
	if err != nil {
		t.Fatalf("failed job not recorded on disk: %v", err)
	}
	var recorded Job
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.Revision != 9 || recorded.Attempts != 3 || recorded.LastError == "" {
		t.Errorf("recorded job = %+v, want rev 9, 3 attempts, and an error", recorded)
	}
}

func TestMirrorSupersedesQueuedRevision(t *testing.T) {
	m, _ := testMirror(t, 3) // deliberately not started: inspect the queue directly
	defer m.Stop()

	m.Publish(refA, 1, publicRealms, envelopeFor(1))
	m.Publish(refA, 2, publicRealms, envelopeFor(2))
	m.Publish(refA, 3, publicRealms, envelopeFor(3))

	if got := m.Status().QueueDepth; got != 1 {
		t.Fatalf("QueueDepth = %d, want 1 — a burst of writes to one object collapses", got)
	}
	claimed, err := m.q.Claim(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Revision != 3 {
		t.Fatalf("queued revision %d, want 3 (newest supersedes)", claimed.Revision)
	}
}

func TestMirrorResumesQueueAfterRestart(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	logPath := filepath.Join(dir, "invocations.log")
	t.Setenv("FAKE_PUBLISH_LOG", logPath)

	opts := Options{
		QueueDir:   queueDir,
		PublishCmd: fakePublisherPath(t),
		Timeout:    10 * time.Second,
		Retries:    3,
	}

	// First hub: enqueue, then "crash" before the worker ever runs.
	first, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	first.Publish(refA, 1, publicRealms, envelopeFor(1))
	first.Publish(refB, 2, publicRealms, envelopeFor(2))
	if got := first.Status().QueueDepth; got != 2 {
		t.Fatalf("QueueDepth = %d, want 2", got)
	}

	// Second hub over the same queue dir picks the work up.
	second, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	second.backoffBase = 5 * time.Millisecond
	second.pollInterval = 5 * time.Millisecond
	if got := second.Status().QueueDepth; got != 2 {
		t.Fatalf("QueueDepth after restart = %d, want 2 — pending mirrors must survive", got)
	}

	second.Start()
	defer second.Stop()
	waitFor(t, "both queued jobs to publish", func() bool { return second.Status().Succeeded == 2 })

	data, _ := os.ReadFile(logPath)
	if n := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; n != 2 {
		t.Fatalf("%d invocations, want 2", n)
	}
}

func TestMirrorStopRequeuesInFlightJob(t *testing.T) {
	t.Setenv("FAKE_PUBLISH_SLEEP", "30")
	m, _ := testMirror(t, 3)
	m.Start()

	m.Publish(refA, 1, publicRealms, envelopeFor(1))
	waitFor(t, "the publish to start", func() bool { return m.Status().InFlight == 1 })

	start := time.Now()
	m.Stop()
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("Stop took %v, want a prompt shutdown that kills the publisher", elapsed)
	}

	// The interrupted job is still pending, and shutdown did not count as a
	// failed attempt against its retry budget.
	if got := m.Status().QueueDepth; got != 1 {
		t.Fatalf("QueueDepth after Stop = %d, want 1 — the interrupted job must survive", got)
	}
	claimed, err := m.q.Claim(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0 — a shutdown is not a publish failure", claimed.Attempts)
	}
}

func TestMirrorStopIsIdempotent(t *testing.T) {
	m, _ := testMirror(t, 3)
	m.Start()
	m.Stop()
	m.Stop() // must not panic on a double close
}

func TestNilMirrorIsDisabledAndSafe(t *testing.T) {
	var m *Mirror

	// Every call site is a plain one-liner; a disabled mirror must no-op.
	m.Publish(refA, 1, publicRealms, envelopeFor(1))
	m.Start()
	m.Stop()

	st := m.Status()
	if st.Enabled {
		t.Error("Status().Enabled = true for a nil mirror, want false")
	}
	if st.Recent == nil {
		t.Error("Recent is nil, want an empty slice so it serializes as []")
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"enabled":false`) || !strings.Contains(string(data), `"recent":[]`) {
		t.Errorf("disabled status JSON = %s", data)
	}
}

func TestNewRejectsUnusableConfig(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		opts Options
	}{
		{"missing publish_cmd", Options{QueueDir: dir, PublishCmd: "", Timeout: time.Second, Retries: 1}},
		{"publish_cmd does not exist", Options{QueueDir: dir, PublishCmd: filepath.Join(dir, "nope.sh"), Timeout: time.Second, Retries: 1}},
		{"empty queue_dir", Options{QueueDir: "", PublishCmd: fakePublisherPath(t), Timeout: time.Second, Retries: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); err == nil {
				t.Errorf("New(%+v) succeeded, want an error", tc.opts)
			}
		})
	}
}

func TestMirrorRecentIsCapped(t *testing.T) {
	m, _ := testMirror(t, 3)
	for i := 0; i < maxRecentEvents+10; i++ {
		m.record(Event{Ref: refA, Status: "succeeded"})
	}
	if got := len(m.Status().Recent); got != maxRecentEvents {
		t.Fatalf("len(Recent) = %d, want it capped at %d", got, maxRecentEvents)
	}
}

// An enqueue that cannot reach disk must not vanish: the write still succeeds
// for the client, but the mirror has to say it dropped one.
func TestMirrorReportsDroppedEnqueue(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — directory permissions would not be enforced")
	}
	m, invocations := testMirror(t, 3)
	m.Start()
	defer m.Stop()

	// Simulate the queue filesystem going read-only under a still-writable store.
	if err := os.Chmod(m.q.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(m.q.dir, 0o755)

	m.Publish(refA, 1, publicRealms, envelopeFor(1))

	st := m.Status()
	if st.Dropped != 1 {
		t.Fatalf("Dropped = %d, want 1 — a lost mirror job must be visible", st.Dropped)
	}
	if st.LastError == "" {
		t.Error("LastError is empty, want the enqueue failure recorded")
	}
	if len(st.Recent) == 0 || st.Recent[0].Status != "dropped" {
		t.Errorf("Recent[0] = %+v, want a dropped event", st.Recent)
	}
	time.Sleep(100 * time.Millisecond)
	if got := invocations(); len(got) != 0 {
		t.Fatalf("nothing should have been published: %v", got)
	}
}

// A failed job stays countable across a restart, so monitoring does not see
// the failure disappear when the hub is bounced.
func TestMirrorFailedQueueSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	t.Setenv("FAKE_PUBLISH_LOG", filepath.Join(dir, "invocations.log"))
	t.Setenv("FAKE_PUBLISH_EXIT", "3")

	opts := Options{
		QueueDir:   queueDir,
		PublishCmd: fakePublisherPath(t),
		Timeout:    10 * time.Second,
		Retries:    0,
	}

	first, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	first.backoffBase = 5 * time.Millisecond
	first.pollInterval = 5 * time.Millisecond
	first.Start()
	first.Publish(refA, 1, publicRealms, envelopeFor(1))
	waitFor(t, "the mirror to give up", func() bool { return first.Status().Failed == 1 })
	if got := first.Status().FailedQueued; got != 1 {
		t.Fatalf("FailedQueued = %d, want 1", got)
	}
	first.Stop()

	second, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Stop()
	st := second.Status()
	if st.FailedQueued != 1 {
		t.Fatalf("FailedQueued after restart = %d, want 1 — failures must not vanish on restart", st.FailedQueued)
	}
}

// The status endpoint is reachable by anyone who can complete the public
// challenge flow, so the publisher's raw output must not be echoed there.
// The full detail still goes to the log and the failed/ job file.
func TestMirrorStatusDoesNotLeakPublisherOutput(t *testing.T) {
	t.Setenv("FAKE_PUBLISH_EXIT", "3")
	m, _ := testMirror(t, 0)
	m.Start()
	defer m.Stop()

	m.Publish(refA, 1, publicRealms, envelopeFor(1))
	waitFor(t, "the mirror to give up", func() bool { return m.Status().Failed == 1 })

	st := m.Status()
	if strings.Contains(st.LastError, "fake-publish") {
		t.Errorf("last_error leaks publisher output: %q", st.LastError)
	}
	if len(st.Recent) == 0 || strings.Contains(st.Recent[0].Error, "fake-publish") {
		t.Errorf("recent[0].error leaks publisher output: %+v", st.Recent)
	}
	if st.LastError == "" {
		t.Error("last_error is empty, want the error summary retained")
	}

	// The operator-only record on disk keeps the full diagnosis.
	data, err := os.ReadFile(filepath.Join(m.q.failedDir, refA+".json"))
	if err != nil {
		t.Fatalf("read failed job: %v", err)
	}
	var recorded Job
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorded.LastError, "fake-publish") {
		t.Errorf("failed job file should keep the publisher output, got %q", recorded.LastError)
	}
}

// Drops must not be evicted from the status surface by ordinary job events —
// they are the only trace of a mirror that was lost entirely.
func TestMirrorDroppedRefsAreNotEvictedByOtherEvents(t *testing.T) {
	m, _ := testMirror(t, 3)

	m.drop(&Job{Ref: refA, Revision: 1}, errors.New("disk full"))
	for i := 0; i < maxRecentEvents*2; i++ {
		m.record(Event{Ref: refB, Status: "succeeded"})
	}

	st := m.Status()
	if st.Dropped != 1 {
		t.Fatalf("Dropped = %d, want 1", st.Dropped)
	}
	if len(st.DroppedRefs) != 1 || st.DroppedRefs[0].Ref != refA {
		t.Fatalf("DroppedRefs = %+v, want the dropped ref to survive a busy event ring", st.DroppedRefs)
	}
}

// Status is readable by anyone who can complete the public challenge flow, so
// it must not carry filesystem paths or other internal detail.
func TestStatusSummaryIsSanitized(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"shutdown", fmt.Errorf("%w: context canceled", ErrAborted), "publish aborted by shutdown"},
		{"timeout", fmt.Errorf("%w after 15m0s", ErrTimeout), "publish timed out"},
		{"internal error with a path", fmt.Errorf("freenet publisher temp file: open /srv/hub/queue/tmp/x: permission denied"), "publish failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := statusSummary(tc.err)
			if got != tc.want {
				t.Errorf("statusSummary = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "/") {
				t.Errorf("statusSummary %q contains a path", got)
			}
		})
	}

	// A non-zero exit is safe and useful to report precisely.
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	exitErr := cmd.Run()
	if got := statusSummary(fmt.Errorf("publish command failed: %w", exitErr)); got != "publish command exited 7" {
		t.Errorf("statusSummary = %q, want the exit code named", got)
	}
}

// End-to-end: a publisher failure whose error mentions a path must not put that
// path into the status payload.
func TestMirrorStatusErrorHasNoFilesystemPaths(t *testing.T) {
	t.Setenv("FAKE_PUBLISH_EXIT", "3")
	m, _ := testMirror(t, 0)
	m.Start()
	defer m.Stop()

	m.Publish(refA, 1, publicRealms, envelopeFor(1))
	waitFor(t, "the mirror to give up", func() bool { return m.Status().Failed == 1 })

	st := m.Status()
	if strings.Contains(st.LastError, "/") {
		t.Errorf("last_error %q contains a filesystem path", st.LastError)
	}
	if st.LastError != "publish command exited 3" {
		t.Errorf("last_error = %q, want the sanitized category", st.LastError)
	}
}
