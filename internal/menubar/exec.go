package menubar

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// maxCapture bounds each captured stream. The menu never shows raw CLI
// output; the bound only protects memory and log volume. Exceeding it is a
// hard error: a truncated JSON object could otherwise parse as valid data.
const maxCapture = 1 << 20 // 1 MiB

// waitDelay bounds how long Wait waits for the output pipes to close after
// the process exits or is killed. A descendant that inherits stdout/stderr
// cannot keep the CLI call (and its context) alive past this deadline.
const waitDelay = 5 * time.Second

// errOutputTooLarge marks a bounded capture overflow.
var errOutputTooLarge = errors.New("sparky CLI output exceeded the capture limit")

// errWaitDelay marks a process whose output pipes stayed open past WaitDelay.
var errWaitDelay = errors.New("sparky CLI left its output open")

// runResult is the finished output of one CLI invocation.
type runResult struct {
	Stdout []byte
	Stderr []byte
	Code   int
}

// runner executes one CLI invocation with an argument array. It never goes
// through a shell. Tests inject a fake runner.
type runner func(ctx context.Context, binary string, args []string) (runResult, error)

// execRunner is the production runner: exec.CommandContext with separate
// bounded stdout/stderr captures, an explicit pipe-drain deadline, and a
// hard failure on overflow. The command is never given stdin, so a hung CLI
// cannot block on it.
func execRunner(ctx context.Context, binary string, args []string) (runResult, error) {
	return execRunnerEnv(ctx, binary, args, nil)
}

// execRunnerEnv is execRunner with an optional extra environment. It exists
// so tests can drive the real runner against the test binary as a helper
// CLI; production always passes nil and inherits the app environment.
func execRunnerEnv(ctx context.Context, binary string, args []string, env []string) (runResult, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr boundedBuffer
	stdout.limit = maxCapture
	stderr.limit = maxCapture
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// After the context cancels (or the process exits), wait at most
	// waitDelay for inherited pipes to close before forcibly reaping. This
	// keeps a straggling grandchild from pinning the goroutine.
	cmd.WaitDelay = waitDelay
	err := cmd.Run()
	code := 0
	switch {
	case err == nil:
		code = 0
	case errors.Is(err, exec.ErrWaitDelay):
		// The process finished (or was killed) but a descendant kept the
		// pipes open. Treat it as a bounded failure.
		err = errWaitDelay
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// A non-zero exit is a normal CLI result, not a run error; the
			// caller inspects Code (and the JSON error envelope).
			code = exitErr.ExitCode()
			err = nil
		} else {
			// The process could not be started (missing binary,
			// permission) or another wait failure occurred.
			return runResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Code: -1}, err
		}
	}
	if stdout.overflow || stderr.overflow {
		return runResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Code: code}, errOutputTooLarge
	}
	if ctx.Err() != nil {
		return runResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Code: code}, ctx.Err()
	}
	return runResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Code: code}, err
}

// boundedBuffer keeps only its first limit bytes. Once more data arrives it
// stops the copy and records overflow so the caller can reject the output
// instead of parsing a valid prefix of a truncated document.
type boundedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); len(p) > remaining {
		if remaining > 0 {
			_, _ = b.buf.Write(p[:remaining])
		}
		b.overflow = true
		return remaining, errOutputTooLarge
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }

// friendlyError turns one failed CLI invocation into a short, safe message.
// It prefers the CLI's own JSON error envelope ({"error": ..., "code": ...})
// and never dumps raw stderr, which may contain secrets or host details.
func friendlyError(result runResult, runErr error, action string) error {
	if errors.Is(runErr, errOutputTooLarge) {
		return errors.New(action + " returned too much data. Update Sparky on this Mac and the Sparks.")
	}
	if errors.Is(runErr, errWaitDelay) {
		return errors.New(action + " left the Sparky CLI output open and timed out.")
	}
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		// The caller canceled (for example a load that outlived the menu) or
		// the bounded timeout elapsed. Neither is a product failure.
		if errors.Is(runErr, context.DeadlineExceeded) {
			return errors.New(action + " timed out. Sparky CLI did not answer in time.")
		}
		return errors.New(action + " was canceled.")
	}
	if runErr != nil && result.Code == -1 {
		return errors.New("Could not start the Sparky CLI. Check the bundled CLI path.")
	}
	for _, stream := range [][]byte{result.Stdout, result.Stderr} {
		var envelope struct {
			Error string `json:"error"`
		}
		if jsonUnmarshalOne(stream, &envelope) == nil && strings.TrimSpace(envelope.Error) != "" {
			return errors.New(sanitizeMessage(envelope.Error))
		}
	}
	if result.Code == 0 {
		return errors.New(action + " returned invalid data. Update Sparky on this Mac and the Sparks.")
	}
	switch result.Code {
	case 2:
		return errors.New(action + " was rejected by the Sparky CLI (usage error).")
	case 130, 143:
		return errors.New(action + " was interrupted on the Sparks. Remote work may continue.")
	}
	return errors.New(action + " failed. Run sparky status in a terminal for details.")
}

// sanitizeMessage trims a CLI-provided error to one short line and removes
// control characters so a menu item stays readable.
func sanitizeMessage(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	s = strings.TrimSpace(b.String())
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
