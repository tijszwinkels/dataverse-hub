package freenet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ErrAborted marks a publish that the hub itself cut short (shutdown), as
// opposed to one the publisher failed. The worker requeues an aborted job
// without spending a retry attempt on it.
var ErrAborted = errors.New("publish aborted")

// maxOutputBytes caps how much publisher output is kept for logs and the
// status endpoint. The real publisher prints a per-target poke report, which
// is small; a runaway script must not be able to balloon hub memory.
const maxOutputBytes = 8 << 10

// Publisher hands a signed envelope to whatever actually writes it to Freenet.
type Publisher interface {
	Publish(ctx context.Context, j *Job) (output string, err error)
}

// execPublisher runs an external publish command — in production
// scripts/publish-v2.sh, which does snapshot PUT → GET-back confirm →
// head PUT → poke each relation target.
//
// The command is executed directly, never through a shell: the only
// caller-controlled value is the envelope, and it is passed as a file rather
// than interpolated into a command line, so nothing in a signed object can
// influence how the command is parsed.
type execPublisher struct {
	cmd     string
	timeout time.Duration
	tmpDir  string
}

func newExecPublisher(cmd string, timeout time.Duration, tmpDir string) *execPublisher {
	return &execPublisher{cmd: cmd, timeout: timeout, tmpDir: tmpDir}
}

// Publish writes the envelope to a temp file and runs the publish command
// against it, returning the combined stdout/stderr either way.
func (p *execPublisher) Publish(ctx context.Context, j *Job) (string, error) {
	path, err := p.writeEnvelope(j)
	if err != nil {
		return "", err
	}
	defer os.Remove(path)

	runCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, p.cmd, path)
	cmd.Env = os.Environ() // the publisher needs PATH/HOME and its Freenet config
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// Kill the whole process group on cancellation: the real publisher shells
	// out to node/fdev, and killing only the script would leave those children
	// running against Freenet long after we gave up on them.
	configureProcessGroup(cmd)
	// Backstop in case a child outlives the signal and holds the pipes open.
	cmd.WaitDelay = 5 * time.Second

	runErr := cmd.Run()
	output := truncateOutput(out.String())

	switch {
	case ctx.Err() != nil:
		// The hub is shutting down — not the publisher's fault.
		return output, fmt.Errorf("%w: %v", ErrAborted, ctx.Err())
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return output, fmt.Errorf("publish command timed out after %s", p.timeout)
	case runErr != nil:
		return output, fmt.Errorf("publish command failed: %w", runErr)
	}
	return output, nil
}

// writeEnvelope materializes the job's envelope where the publish command can
// read it. Mode 0600: a signed public object is not a secret, but there is no
// reason to widen it either.
func (p *execPublisher) writeEnvelope(j *Job) (string, error) {
	if err := os.MkdirAll(p.tmpDir, 0o755); err != nil {
		return "", fmt.Errorf("freenet publisher tmpdir: %w", err)
	}
	f, err := os.CreateTemp(p.tmpDir, "envelope-*.json")
	if err != nil {
		return "", fmt.Errorf("freenet publisher temp file: %w", err)
	}
	name := f.Name()
	if _, err := f.Write(j.Envelope); err != nil {
		f.Close()
		os.Remove(name)
		return "", fmt.Errorf("freenet publisher write envelope: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("freenet publisher close envelope: %w", err)
	}
	return name, nil
}

// validateCommand checks at startup that publish_cmd is an absolute path to an
// executable file, so a typo in hub.toml is a loud boot failure rather than a
// mirror that fails every single job in the background.
func validateCommand(cmd string) error {
	if cmd == "" {
		return errors.New("publish_cmd is empty")
	}
	if !filepath.IsAbs(cmd) {
		return fmt.Errorf("publish_cmd %q must be an absolute path", cmd)
	}
	info, err := os.Stat(cmd)
	if err != nil {
		return fmt.Errorf("publish_cmd %q: %w", cmd, err)
	}
	if info.IsDir() {
		return fmt.Errorf("publish_cmd %q is a directory", cmd)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("publish_cmd %q is not executable", cmd)
	}
	return nil
}

func truncateOutput(s string) string {
	if len(s) <= maxOutputBytes {
		return s
	}
	return s[:maxOutputBytes] + "\n… (output truncated)"
}
