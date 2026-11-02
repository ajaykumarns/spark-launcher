package sparky

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess is never a real test: it is re-invoked as a genuine
// subprocess of runCommand via os.Args[0]. The mode is selected by the
// SPARKY_TEST_HELPER environment variable, forwarded through /usr/bin/env
// so runCommand's exact argv path stays untouched.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("SPARKY_TEST_HELPER")
	if mode == "" {
		t.Skip("helper process only")
	}
	switch mode {
	case "echo":
		os.Stdout.WriteString("out-data\n")
		os.Stderr.WriteString("err-data\n")
		os.Exit(0)
	case "exit3":
		os.Exit(3)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "stdin":
		b := make([]byte, 1<<16)
		n, _ := os.Stdin.Read(b)
		os.Stdout.Write(b[:n])
		os.Exit(0)
	case "big":
		// Keep writing well past the stdout limit; the parent must kill
		// this process instead of buffering forever.
		chunk := make([]byte, 64<<10)
		for {
			os.Stdout.Write(chunk)
			time.Sleep(5 * time.Millisecond)
		}
	case "bigdone":
		// Write past the limit and exit; the parent must still report an
		// error, not success with silently truncated data.
		chunk := make([]byte, 64<<10)
		for i := 0; i < 200; i++ {
			os.Stdout.Write(chunk)
		}
		os.Exit(0)
	case "keeppipe":
		// Exit while a grandchild keeps the inherited stdout pipe open.
		grandchild := exec.Command("sh", "-c", "sleep 5")
		grandchild.Stdout = os.Stdout
		grandchild.Stderr = os.Stderr
		_ = grandchild.Start()
		os.Exit(0)
	}
	os.Exit(0)
}

// helperArgs builds a real argv running the test binary as a helper with
// the given mode; /usr/bin/env supplies the mode environment variable.
func helperArgs(mode string, extraEnv ...string) []string {
	args := []string{"/usr/bin/env", "SPARKY_TEST_HELPER=" + mode}
	args = append(args, extraEnv...)
	return append(args, os.Args[0], "-test.run=^TestHelperProcess$")
}

func TestRunCommandNormalOutputAndExitCode(t *testing.T) {
	r, err := runCommand(context.Background(), helperArgs("echo"), nil)
	if err != nil || r.Code != 0 {
		t.Fatalf("echo: code %d err %v out %q err %q", r.Code, err, r.Out, r.Err)
	}
	if r.Out != "out-data\n" || r.Err != "err-data\n" {
		t.Fatalf("exact capture broken: out %q err %q", r.Out, r.Err)
	}
	r, err = runCommand(context.Background(), helperArgs("exit3"), nil)
	if err != nil {
		t.Fatalf("exit3 returned error: %v", err)
	}
	if r.Code != 3 {
		t.Fatalf("child exit code not preserved: %d", r.Code)
	}
}

func TestRunCommandStdinExactBytes(t *testing.T) {
	payload := []byte("exact stdin bytes \x00\x01\x02 tail")
	r, err := runCommand(context.Background(), helperArgs("stdin"), payload)
	if err != nil || r.Code != 0 {
		t.Fatalf("stdin helper: code %d err %v", r.Code, err)
	}
	if r.Out != "exact stdin bytes \x00\x01\x02 tail" {
		t.Fatalf("stdin bytes not passed exactly: %q", r.Out)
	}
}

func TestRunCommandOverflowKillsAndErrors(t *testing.T) {
	oldLimit := runCommandStdoutLimit
	oldDelay := runCommandWaitDelay
	runCommandStdoutLimit = 1 << 20
	runCommandWaitDelay = 2 * time.Second
	t.Cleanup(func() { runCommandStdoutLimit = oldLimit; runCommandWaitDelay = oldDelay })

	start := time.Now()
	r, err := runCommand(context.Background(), helperArgs("big"), nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("oversized output must error, got code %d out-len %d", r.Code, len(r.Out))
	}
	if !strings.Contains(err.Error(), "capture limit") {
		t.Errorf("unsafe or wrong overflow error: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("overflow cancel was not prompt: %v", elapsed)
	}
	if strings.Contains(err.Error(), "big") || strings.Contains(r.Err, "argv") {
		t.Errorf("error must not include argv or payload contents: %v / %q", err, r.Err)
	}

	// Even a clean child exit after overflow must not be success.
	r, err = runCommand(context.Background(), helperArgs("bigdone"), nil)
	if err == nil {
		t.Errorf("overflow after child exit must not be success: code %d", r.Code)
	}
}

func TestRunCommandContextCancellation(t *testing.T) {
	oldDelay := runCommandWaitDelay
	runCommandWaitDelay = 2 * time.Second
	t.Cleanup(func() { runCommandWaitDelay = oldDelay })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := runCommand(ctx, helperArgs("sleep"), nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("canceled command must return an error, never Code0/nil")
	}
	if elapsed > 5*time.Second {
		t.Errorf("cancellation not prompt: %v", elapsed)
	}
}

func TestRunCommandInheritedOpenPipeBounded(t *testing.T) {
	oldDelay := runCommandWaitDelay
	runCommandWaitDelay = 500 * time.Millisecond
	t.Cleanup(func() { runCommandWaitDelay = oldDelay })

	start := time.Now()
	r, err := runCommand(context.Background(), helperArgs("keeppipe"), nil)
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("open inherited pipe hung Wait: %v", elapsed)
	}
	// The own child was reaped (it exited 0) but the pipe was force-closed
	// by WaitDelay; the run must be bounded and reported, not silent.
	if err == nil && r.Code != 0 {
		t.Fatalf("bounded completion lost code: %+v %v", r, err)
	}
	if err != nil && !strings.Contains(err.Error(), "pipe") {
		t.Errorf("unexpected bounded-completion error: %v", err)
	}
	_ = r
}

func TestRunCommandNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		if _, err := runCommand(context.Background(), helperArgs("echo"), nil); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	// Overflowing and canceled runs must also be fully reaped.
	oldLimit := runCommandStdoutLimit
	runCommandStdoutLimit = 1 << 10
	if _, err := runCommand(context.Background(), helperArgs("big"), nil); err == nil {
		t.Fatal("expected overflow error")
	}
	runCommandStdoutLimit = oldLimit
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, _ = runCommand(ctx, helperArgs("sleep"), nil)
	time.Sleep(500 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("goroutine leak: before %d after %d", before, after)
	}
}
