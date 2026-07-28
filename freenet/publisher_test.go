package freenet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePublisherPath is the fixture script standing in for publish-v2.sh.
func fakePublisherPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("testdata/fake-publish.sh")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// newTestPublisher wires the fake publisher script to a fresh log file and
// returns the publisher plus a func reading back the recorded invocations.
func newTestPublisher(t *testing.T, timeout time.Duration) (*execPublisher, func() []string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "invocations.log")
	t.Setenv("FAKE_PUBLISH_LOG", logPath)

	tmpDir := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}

	p := newExecPublisher(fakePublisherPath(t), timeout, tmpDir)
	return p, func() []string {
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

func TestPublisherPassesEnvelopeAsFile(t *testing.T) {
	p, invocations := newTestPublisher(t, 10*time.Second)

	j := job(refA, 4)
	j.Envelope = json.RawMessage(`{"is":"instructionGraph001","item":{"id":"x"},"signature":"sig"}`)

	out, err := p.Publish(context.Background(), j)
	if err != nil {
		t.Fatalf("Publish: %v (output: %s)", err, out)
	}

	got := invocations()
	if len(got) != 1 {
		t.Fatalf("%d invocations, want 1", len(got))
	}
	if got[0] != string(j.Envelope) {
		t.Fatalf("publisher received %s, want the exact envelope %s", got[0], j.Envelope)
	}
}

func TestPublisherCapturesOutput(t *testing.T) {
	p, _ := newTestPublisher(t, 10*time.Second)

	out, err := p.Publish(context.Background(), job(refA, 1))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !strings.Contains(out, "fake-publish: received") {
		t.Errorf("stdout not captured, got %q", out)
	}
	if !strings.Contains(out, "fake-publish: stderr line") {
		t.Errorf("stderr not captured, got %q", out)
	}
}

func TestPublisherReportsNonZeroExit(t *testing.T) {
	t.Setenv("FAKE_PUBLISH_EXIT", "3")
	p, _ := newTestPublisher(t, 10*time.Second)

	out, err := p.Publish(context.Background(), job(refA, 1))
	if err == nil {
		t.Fatal("Publish succeeded, want an error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("error %q does not name the exit status", err)
	}
	if !strings.Contains(out, "fake-publish: received") {
		t.Errorf("output should still be captured on failure, got %q", out)
	}
}

func TestPublisherTimesOut(t *testing.T) {
	t.Setenv("FAKE_PUBLISH_SLEEP", "30")
	p, _ := newTestPublisher(t, 300*time.Millisecond)

	start := time.Now()
	_, err := p.Publish(context.Background(), job(refA, 1))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Publish succeeded, want a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q does not mention the timeout", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Publish took %v, want it killed near the 300ms timeout", elapsed)
	}
	if errors.Is(err, ErrAborted) {
		t.Error("timeout must not be reported as a shutdown abort — it is a real failure")
	}
}

func TestPublisherTimeoutKillsWholeProcessGroup(t *testing.T) {
	childMarker := filepath.Join(t.TempDir(), "orphan.txt")
	t.Setenv("FAKE_PUBLISH_SLEEP", "30")
	t.Setenv("FAKE_PUBLISH_CHILD", childMarker)
	p, _ := newTestPublisher(t, 200*time.Millisecond)

	if _, err := p.Publish(context.Background(), job(refA, 1)); err == nil {
		t.Fatal("Publish succeeded, want a timeout error")
	}

	// The real publisher shells out to node/fdev; killing only the script
	// would leave those children hammering Freenet past the timeout.
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(childMarker); err == nil {
		t.Error("a child of the timed-out publisher survived — process group was not killed")
	}
}

func TestPublisherAbortsOnContextCancel(t *testing.T) {
	t.Setenv("FAKE_PUBLISH_SLEEP", "30")
	p, _ := newTestPublisher(t, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := p.Publish(ctx, job(refA, 1))
	if err == nil {
		t.Fatal("Publish succeeded, want an abort error")
	}
	if !errors.Is(err, ErrAborted) {
		t.Errorf("error %v, want it to wrap ErrAborted so the worker can requeue without burning an attempt", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Publish took %v after cancel, want a prompt shutdown", elapsed)
	}
}

func TestPublisherCleansUpEnvelopeTempFile(t *testing.T) {
	p, _ := newTestPublisher(t, 10*time.Second)

	if _, err := p.Publish(context.Background(), job(refA, 1)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(p.tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%d temp files left behind, want 0", len(entries))
	}
}

func TestValidateCommand(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "plain.sh")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		cmd     string
		wantErr bool
	}{
		{"executable fixture", fakePublisherPath(t), false},
		{"missing file", filepath.Join(dir, "nope.sh"), true},
		{"not executable", notExec, true},
		{"a directory", dir, true},
		{"empty", "", true},
		{"relative path", "./publish.sh", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCommand(tc.cmd)
			if tc.wantErr && err == nil {
				t.Errorf("validateCommand(%q) = nil, want an error", tc.cmd)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateCommand(%q) = %v, want nil", tc.cmd, err)
			}
		})
	}
}
