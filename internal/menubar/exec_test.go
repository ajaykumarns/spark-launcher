package menubar

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary act as a real fake CLI child. Tests spawn the
// test executable with MENUBAR_HELPER set; the child runs helperMain instead
// of the test suite. This avoids depending on a shell or any external script
// and exercises the real execRunner path.
func TestMain(m *testing.M) {
	if mode := os.Getenv("MENUBAR_HELPER"); mode != "" {
		helperMain(mode)
		return
	}
	os.Exit(m.Run())
}

func helperMain(mode string) {
	switch mode {
	case "echo-args":
		// Print each argv element as a JSON array so tests can prove the
		// argument array survives intact (including spaces and metachars).
		args := os.Args[1:]
		enc := "["
		for i, a := range args {
			if i > 0 {
				enc += ","
			}
			enc += `"` + strings.ReplaceAll(strings.ReplaceAll(a, `\`, `\\`), `"`, `\"`) + `"`
		}
		enc += "]"
		os.Stdout.WriteString(enc)
	case "exit-code":
		os.Exit(7)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Stdout.WriteString("too late")
	case "inherit":
		// A child that inherits our stdout/stderr and outlives us. The
		// parent exits immediately, but the pipe stays open in the child.
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), "MENUBAR_HELPER=inherit-child")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			os.Stderr.WriteString("start child: " + err.Error())
			os.Exit(3)
		}
		os.Stdout.WriteString("parent done")
	case "inherit-child":
		time.Sleep(30 * time.Second)
	case "overflow":
		// Write more than maxCapture to stdout.
		chunk := strings.Repeat("x", 64*1024)
		for i := 0; i < 20; i++ {
			os.Stdout.WriteString(chunk)
		}
	default:
		os.Stderr.WriteString("unknown helper mode")
		os.Exit(2)
	}
}

// helperBinary returns the current test executable: a real CLI stand-in.
func helperBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

func helperEnv(mode string) []string {
	return append(os.Environ(), "MENUBAR_HELPER="+mode)
}

func TestExecRunnerArgumentArrayWithSpecialCharacters(t *testing.T) {
	// execRunner has no environment seam, so run the helper through a tiny
	// wrapper runner that injects MENUBAR_HELPER via exec.Command instead.
	exe := helperBinary(t)
	special := []string{"spark/glm", "a b", "semi;quote'\"`$()&|", "üñï"}

	out, err := runHelper(t, exe, "echo-args", special)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantTail := `,"a b","semi;quote'\"` + "`" + `$()&|","üñï"]`
	if !strings.HasPrefix(string(out.Stdout), `["spark/glm"`) || !strings.HasSuffix(string(out.Stdout), wantTail) {
		t.Fatalf("argv not preserved: %s", out.Stdout)
	}
}

func TestExecRunnerExitCode(t *testing.T) {
	out, err := runHelper(t, helperBinary(t), "exit-code", nil)
	if err != nil {
		t.Fatalf("unexpected run error: %v", err)
	}
	if out.Code != 7 {
		t.Fatalf("code = %d, want 7", out.Code)
	}
}

func TestExecRunnerContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runHelperCtx(t, ctx, helperBinary(t), "sleep", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
}

func TestExecRunnerInheritedPipesAreReaped(t *testing.T) {
	// The parent exits at once but a grandchild holds stdout open. Without
	// WaitDelay the call would block for 30s; the bounded policy returns
	// quickly with a clear ErrWaitDelay-style failure.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	out, err := runHelperCtx(t, ctx, helperBinary(t), "inherit", nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected a bounded failure, got output %q", out.Stdout)
	}
	if !errors.Is(err, errWaitDelay) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want errWaitDelay or deadline", err)
	}
	if elapsed > waitDelay+10*time.Second {
		t.Fatalf("inherited pipe pinned the call for %v", elapsed)
	}
	if !strings.Contains(string(out.Stdout), "parent done") {
		t.Fatalf("parent output lost: %q", out.Stdout)
	}
}

func TestExecRunnerRejectsOverflow(t *testing.T) {
	out, err := runHelper(t, helperBinary(t), "overflow", nil)
	if !errors.Is(err, errOutputTooLarge) {
		t.Fatalf("err = %v, want errOutputTooLarge", err)
	}
	if len(out.Stdout) > maxCapture {
		t.Fatalf("captured %d bytes, want <= %d", len(out.Stdout), maxCapture)
	}
	// friendlyError must surface a short message, not the data.
	if msg := friendlyError(out, err, "Status refresh").Error(); !strings.Contains(msg, "too much data") {
		t.Fatalf("message = %q", msg)
	}
}

func TestBoundedBufferReportsOverflow(t *testing.T) {
	var b boundedBuffer
	b.limit = 4
	if n, err := b.Write([]byte("abcd")); err != nil || n != 4 {
		t.Fatalf("fill: n=%d err=%v", n, err)
	}
	n, err := b.Write([]byte("efgh"))
	if !errors.Is(err, errOutputTooLarge) {
		t.Fatalf("overflow err = %v", err)
	}
	if string(b.Bytes()) != "abcd" {
		t.Fatalf("buf = %q", b.Bytes())
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0 remaining space", n)
	}
}

// runHelper drives the real execRunnerEnv against the test binary as a
// helper CLI, so the tests exercise the production capture, WaitDelay and
// overflow policy rather than a copy of it.
func runHelper(t *testing.T, exe, mode string, args []string) (runResult, error) {
	t.Helper()
	return runHelperCtx(t, context.Background(), exe, mode, args)
}

func runHelperCtx(t *testing.T, ctx context.Context, exe, mode string, args []string) (runResult, error) {
	t.Helper()
	return execRunnerEnv(ctx, exe, args, helperEnv(mode))
}
