package sparky

// Tests for the public remote CLI stage. Only fake local children and the
// injected seams are used; no real SSH, deployments, or site config.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func remoteTestConfig() clientConfig {
	return clientConfig{
		Gateway: "https://gateway.example",
		Extra: Object{
			"admin_ssh_host":  "spark01",
			"worker_ssh_host": "spark02",
			"remote_root":     "/srv",
			"admin_url":       "https://gateway.example",
		},
	}
}

func remoteUseFakeRunner(fake remoteRunner) func() {
	orig := remoteRun
	remoteRun = fake
	return func() { remoteRun = orig }
}

func TestCliRemoteCommandValidationAndQuoting(t *testing.T) {
	cfg := remoteTestConfig()
	argv, err := cliRemoteCommand(cfg, "load", "glm", true, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if argv[0] != "ssh" || argv[1] != "-o" || argv[2] != "ConnectTimeout=10" || argv[3] != "spark01" {
		t.Fatalf("ssh argv prefix wrong: %v", argv)
	}
	if argv[4] != "~/.local/bin/sparky --json node 'load' '--root' '/srv' '--force' '--model' 'glm'" {
		t.Fatalf("remote command wrong: %q", argv[4])
	}

	// Tokens are POSIX-quoted; the prefix is literal.
	argv, err = cliRemoteCommand(cfg, "load", "glm a'b", false, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if argv[4] != `~/.local/bin/sparky --json node 'load' '--root' '/srv' '--model' 'glm a'"'"'b'` {
		t.Fatalf("quoting wrong: %q", argv[4])
	}

	// Validation happens before any spawn.
	bad := clientConfig{Extra: Object{"admin_ssh_host": "bad host!", "remote_root": "/srv"}}
	if _, err := cliRemoteCommand(bad, "status", "", false, ""); err == nil || !strings.Contains(err.Error(), "SSH host alias") {
		t.Fatalf("bad host: %v", err)
	}
	noRoot := clientConfig{Extra: Object{"admin_ssh_host": "spark01", "remote_root": "srv"}}
	if _, err := cliRemoteCommand(noRoot, "status", "", false, ""); err == nil || !strings.Contains(err.Error(), "absolute remote_root") {
		t.Fatalf("relative root: %v", err)
	}
	if _, err := cliRemoteCommand(cfg, "status", "", true, ""); err == nil || !strings.Contains(err.Error(), "--force is available only for load and unload.") {
		t.Fatalf("force validation: %v", err)
	}
	if _, err := cliRemoteRPC(context.Background(), cfg, "bad op!", nil); err == nil {
		t.Fatal("invalid operation name accepted")
	}
}

func TestCliRemoteJSONSuccessExactPayloadAndTimeouts(t *testing.T) {
	var gotArgv []string
	var gotStdin []byte
	restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		gotArgv, gotStdin = argv, stdin
		return remoteRunResult{stdout: []byte(`{"n":1.50,"ok":true}`), code: 0}, nil
	})
	defer restore()

	cfg := remoteTestConfig()
	object, err := cliRemoteRPC(context.Background(), cfg, "image-profile", Object{"n": json.Number("1.50")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n, _ := object["n"].(json.Number); string(n) != "1.50" {
		t.Fatalf("number spelling lost: %#v", object["n"])
	}
	if string(gotStdin) != `{"n":1.50}` {
		t.Fatalf("payload not exact JSON on stdin: %q", gotStdin)
	}
	// Payload never appears on argv and the operation is shell-quoted.
	remote := gotArgv[len(gotArgv)-1]
	if strings.Contains(remote, "1.50") || !strings.Contains(remote, `'rpc' 'image-profile' '--root' '/srv' '--payload' '-'`) {
		t.Fatalf("remote rpc command wrong: %q", remote)
	}
	// Fakes reject unexpected commands.
	if gotArgv[0] != "ssh" || !strings.HasPrefix(remote, "~/.local/bin/sparky --json node ") {
		t.Fatalf("unexpected command: %v", gotArgv)
	}
	// A token split inside the auth token is completed before use: the
	// earlier chunk alone must never become a wrong shorter link.
	restore3 := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		onStderr([]byte("https://login.tailscale.com/a/Ab"))
		time.Sleep(20 * time.Millisecond)
		onStderr([]byte("C123 then retry"))
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			return remoteRunResult{code: 1}, nil
		}
		return remoteRunResult{code: 1}, nil
	})
	defer restore3()
	_, err = cliRemoteJSON(context.Background(), []string{"ssh", "host"}, nil, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "https://login.tailscale.com/a/AbC123,") {
		t.Fatalf("token split wrong: %v", err)
	}
	if strings.Contains(err.Error(), "/a/Ab,") {
		t.Fatalf("incomplete token became the link: %v", err)
	}
}

// TestCliRemoteJSONTimeoutEnforcement covers the documented deadlines.

func TestCliRemoteJSONEnvelopeAndPrivacy(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		stderr string
		code   int
		want   string
		secret string
	}{
		{"envelope", `{"error":"Model busy."}`, "secret diagnostic", 1, "Model busy.", "secret diagnostic"},
		{"invalid envelope ignored", `{`, "Permission denied, secret", 255, "SSH authentication failed. Run sparky admin ssh spark01 to check your access.", "secret"},
		{"non-255", `{`, "junk", 3, "The device command failed (exit 3). The device did not return an error report. Run sparky doctor to check the installation.", "junk"},
		{"generic 255", `{`, "totally private", 255, "The SSH connection failed (exit 255). Run sparky admin ssh spark01 to see the connection details.", "totally private"},
	}
	for _, tc := range tests {
		restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
			onStderr([]byte(tc.stderr))
			return remoteRunResult{stdout: []byte(tc.stdout), stderr: []byte(tc.stderr), code: tc.code}, nil
		})
		_, err := cliRemoteJSON(context.Background(), []string{"ssh", "host"}, nil, time.Second)
		restore()
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: got %v, want %q", tc.name, err, tc.want)
		}
		if tc.secret != "" && strings.Contains(err.Error(), tc.secret) {
			t.Fatalf("%s: raw diagnostics leaked: %v", tc.name, err)
		}
	}

	// Success paths with non-object, trailing, or null data are invalid.
	for _, stdout := range []string{`[]`, `{} trailing`, `null`, ``, `{"a":1}{"b":2}`} {
		restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
			return remoteRunResult{stdout: []byte(stdout), code: 0}, nil
		})
		_, err := cliRemoteJSON(context.Background(), []string{"ssh", "host"}, nil, time.Second)
		restore()
		if err == nil || err.Error() != "The device returned invalid data. Update sparky on both Sparks." {
			t.Fatalf("stdout %q: got %v, want invalid data", stdout, err)
		}
	}
}

func TestCliRemoteJSONIdentitySplitWrites(t *testing.T) {
	start := time.Now()
	restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		// The URL arrives split across writes while the child runs.
		onStderr([]byte("please visit https://login.tailscale.com/a/"))
		onStderr([]byte("AbC123xyz then retry"))
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			return remoteRunResult{code: 1}, nil
		}
		return remoteRunResult{code: 1}, nil
	})
	defer restore()
	_, err := cliRemoteJSON(context.Background(), []string{"ssh", "host"}, nil, 10*time.Second)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("identity message not prompt: %v", elapsed)
	}
	want := "Tailscale needs an identity check. Open https://login.tailscale.com/a/AbC123xyz, then run the command again."
	if err == nil || err.Error() != want {
		t.Fatalf("identity error: %v", err)
	}
	// A URL split across writes is completed by later stderr and never
	// shows other text.
	restore2 := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		onStderr([]byte("visit https://login.tailscale.com/a/"))
		onStderr([]byte("Xy9 today; see https://evil.example/x"))
		return remoteRunResult{code: 255, stderr: []byte("visit https://login.tailscale.com/a/Xy9 today; see https://evil.example/x")}, nil
	})
	defer restore2()
	_, err = cliRemoteJSON(context.Background(), []string{"ssh", "host"}, nil, time.Second)
	if err == nil || !strings.HasPrefix(err.Error(), "Tailscale needs an identity check. Open https://login.tailscale.com/a/Xy9, then run the command again.") {
		t.Fatalf("partial url: %v", err)
	}
	if strings.Contains(err.Error(), "evil.example") || strings.Contains(err.Error(), "visit") {
		t.Fatalf("raw stderr leaked: %v", err)
	}
}

func TestCliRemoteJSONTimeoutAndReap(t *testing.T) {
	// Real local child, no SSH: killed at the deadline and reaped.
	start := time.Now()
	_, err := cliRemoteJSON(context.Background(), []string{"sh", "-c", "echo waiting; sleep 20"}, nil, 250*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout not prompt: %v", elapsed)
	}
	if !errors.Is(err, errRemoteTimeout) {
		t.Fatalf("want timeout error, got %v", err)
	}
	if err.Error() != "The device command timed out. Remote work may continue on the device." {
		t.Fatalf("public timeout message wrong: %v", err)
	}

	// A canceled parent context is propagated, not reported as a timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cliRemoteJSON(ctx, []string{"sh", "-c", "sleep 20"}, nil, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancel propagated, got %v", err)
	}
}

func TestCliRemoteJSONTimeoutEnforcement(t *testing.T) {
	// Every documented operation deadline reaches the spawned child.
	cfg := remoteTestConfig()
	ops := []struct {
		name string
		call func() (Object, error)
		want time.Duration
	}{
		{"status", func() (Object, error) { return cliRemoteStatus(context.Background(), cfg) }, 90 * time.Second},
		{"load", func() (Object, error) { return cliRemoteResult(context.Background(), cfg, "load", "glm", false, "") }, 7230 * time.Second},
		{"unload", func() (Object, error) { return cliRemoteResult(context.Background(), cfg, "unload", "", true, "") }, 7230 * time.Second},
		{"plan", func() (Object, error) { return cliRemoteResult(context.Background(), cfg, "gateway", "", false, "") }, 120 * time.Second},
		{"rpc image-profile", func() (Object, error) { return cliRemoteRPC(context.Background(), cfg, "image-profile", nil) }, 900 * time.Second},
		{"rpc run", func() (Object, error) { return cliRemoteRPC(context.Background(), cfg, "run", nil) }, 180 * time.Second},
		{"rpc other", func() (Object, error) { return cliRemoteRPC(context.Background(), cfg, "other", nil) }, 120 * time.Second},
		{"ocr install", func() (Object, error) { return cliRemoteOCR(context.Background(), cfg, "install", "", "", false) }, 7230 * time.Second},
		{"ocr status", func() (Object, error) { return cliRemoteOCR(context.Background(), cfg, "status", "", "", false) }, 180 * time.Second},
	}
	for _, op := range ops {
		var left time.Duration
		restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
			deadline, _ := ctx.Deadline()
			left = time.Until(deadline)
			return remoteRunResult{stdout: []byte(`{}`), code: 0}, nil
		})
		if _, err := op.call(); err != nil {
			t.Fatalf("%s: unexpected error: %v", op.name, err)
		}
		restore()
		if left <= 0 || left > op.want || left < op.want-5*time.Second {
			t.Fatalf("%s timeout wrong: %v, want ~%v", op.name, left, op.want)
		}
	}
}

// remoteHelperScript writes an own helper shell script and returns the
// argv to run it. No ssh, services, or site config is involved.
func remoteHelperScript(t *testing.T, body string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"sh", path}
}

func TestRemoteExecRunnerLargeValidOutput(t *testing.T) {
	data := `{"pad":"` + strings.Repeat("a", 4<<20) + `"}`
	jsonPath := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(jsonPath, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	script := remoteHelperScript(t, "cat "+shellQuote(jsonPath)+"\n")
	result, err := remoteExecRunner(context.Background(), script, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.code != 0 {
		t.Fatalf("exit code: %d", result.code)
	}
	object, derr := decode(result.stdout)
	if derr != nil {
		t.Fatalf("large valid output truncated: %v (len %d)", derr, len(result.stdout))
	}
	if pad, _ := object["pad"].(string); len(pad) != 4<<20 {
		t.Fatalf("payload size wrong: %d", len(pad))
	}
}

func TestRemoteExecRunnerOverflowKillsPromptly(t *testing.T) {
	origOut, origErr := remoteStdoutLimit, remoteStderrLimit
	defer func() { remoteStdoutLimit, remoteStderrLimit = origOut, origErr }()

	// Stdout overflow: the child keeps writing but is killed immediately.
	remoteStdoutLimit = 10
	start := time.Now()
	result, err := remoteExecRunner(context.Background(),
		remoteHelperScript(t, "printf 0123456789ABCDE\nsleep 30\necho late\n"), nil, nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("overflow kill not prompt: %v", elapsed)
	}
	if !errors.Is(err, errCaptureTooLarge) {
		t.Fatalf("want capture limit error, got %v", err)
	}
	if len(result.stdout) > 10 {
		t.Fatalf("stdout unbounded: %d", len(result.stdout))
	}

	// Stderr overflow likewise, and the streaming callback saw the bytes.
	remoteStdoutLimit, remoteStderrLimit = origOut, 10
	var seen int
	result, err = remoteExecRunner(context.Background(),
		remoteHelperScript(t, "printf abc >&2\nsleep 0.3\nprintf 0123456789ABCDEF >&2\nsleep 30\n"), nil, func(chunk []byte) { seen += len(chunk) })
	if !errors.Is(err, errCaptureTooLarge) {
		t.Fatalf("want stderr capture limit error, got %v", err)
	}
	if seen == 0 {
		t.Fatal("stderr callback never streamed")
	}
}

func TestRemoteExecRunnerDanglingDescendant(t *testing.T) {
	// The child exits while a grandchild keeps the output pipe open;
	// WaitDelay must still return and reap within a bounded time, and the
	// unconfirmed stream completion must not be reported as success.
	start := time.Now()
	result, err := remoteExecRunner(context.Background(),
		remoteHelperScript(t, "echo '{\"partial\":1}'; (sleep 3) &\n"), nil, nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("dangling descendant not bounded: %v", elapsed)
	}
	if !errors.Is(err, errStreamIncomplete) {
		t.Fatalf("want stream incomplete error, got %v", err)
	}
	if !strings.Contains(string(result.stdout), "partial") {
		t.Fatalf("captured stdout wrong: %q", result.stdout)
	}
}

// TestCliRemoteJSONDescendantHeldPipeNotSuccess runs the full private
// protocol against an own helper child: the child prints a valid JSON
// prefix and exits while a descendant holds the pipe. The valid prefix
// must be rejected as incomplete rather than returned as success.
func TestCliRemoteJSONDescendantHeldPipeNotSuccess(t *testing.T) {
	restore := remoteUseFakeRunner(remoteExecRunner)
	defer restore()
	_, err := cliRemoteJSON(context.Background(),
		remoteHelperScript(t, "printf '{\"result\":\"ok\"'; (sleep 3) &\n"), nil, 10*time.Second)
	if err == nil {
		t.Fatal("valid JSON prefix with unconfirmed stream completion must not succeed")
	}
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() == 0 {
		t.Fatalf("want non-success failure, got %v", err)
	}
	if strings.Contains(err.Error(), "{") {
		t.Fatalf("error leaked captured output: %v", err)
	}
}

func TestRemoteExecRunnerCancel(t *testing.T) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	result, err := remoteExecRunner(ctx, remoteHelperScript(t, "sleep 30\n"), nil, nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancel not prompt: %v", elapsed)
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.code == 0 {
		t.Fatalf("killed child should report failure: %d", result.code)
	}
}

func TestCliDevicesPartialFailureAndConcurrency(t *testing.T) {
	bothInside := make(chan struct{}, 2)
	restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		remote := argv[len(argv)-1]
		bothInside <- struct{}{}
		select {
		case <-bothInside:
		case <-time.After(2 * time.Second):
			return remoteRunResult{}, errors.New("not concurrent")
		}
		if strings.Contains(remote, "ssh 'spark02'") {
			// Nested worker query times out.
			<-ctx.Done()
			return remoteRunResult{}, ctx.Err()
		}
		if !strings.HasPrefix(remote, "~/.local/bin/sparky --json node device") {
			return remoteRunResult{}, errors.New("wrong device command: " + remote)
		}
		return remoteRunResult{stdout: []byte(`{"device":"spark01"}`), code: 0}, nil
	})
	defer restore()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	result, err := cliDevices(ctx, remoteTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	devices, _ := result["devices"].(Object)
	if devices == nil {
		t.Fatalf("devices missing: %v", result)
	}
	if s, _ := devices["spark01"].(Object); s["device"] != "spark01" {
		t.Fatalf("spark01 wrong: %v", devices["spark01"])
	}
	if e, _ := devices["spark02"].(Object); e["error"] != "Device query timed out." {
		t.Fatalf("spark02 wrong: %v", devices["spark02"])
	}
	if result["ok"] != false {
		t.Fatalf("ok should be false: %v", result)
	}

	// Both devices failing cleanly reports per-device errors, ok false.
	restore = remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		return remoteRunResult{stdout: []byte(`{"error":"down"}`), code: 1}, nil
	})
	defer restore()
	result, err = cliDevices(context.Background(), remoteTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["ok"] != false {
		t.Fatalf("ok should be false: %v", result)
	}
	devices = result["devices"].(Object)
	for _, name := range []string{"spark01", "spark02"} {
		if e, _ := devices[name].(Object); e["error"] != "down" {
			t.Fatalf("%s wrong: %v", name, devices[name])
		}
	}

	// Config validation happens before spawning.
	if _, err := cliDevices(context.Background(), clientConfig{}); err == nil {
		t.Fatal("missing hosts accepted")
	}
}

func TestCliAdminInteractiveExitCodeAndStreams(t *testing.T) {
	var gotArgv []string
	var gotIn io.Reader
	var gotOut, gotErr io.Writer
	orig := remoteAdmin
	defer func() { remoteAdmin = orig }()
	remoteAdmin = func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if argv[0] != "ssh" {
			t.Errorf("unexpected command: %v", argv)
		}
		gotArgv, gotIn, gotOut, gotErr = argv, stdin, stdout, stderr
		return 7, nil
	}
	cfg := remoteTestConfig()
	in := strings.NewReader("input")
	var outBuf, errBuf bytes.Buffer
	code, err := cliAdmin(context.Background(), cfg, "spark01", in, &outBuf, &errBuf)
	if err != nil || code != 7 {
		t.Fatalf("spark01: %d %v", code, err)
	}
	if len(gotArgv) != 2 || gotArgv[0] != "ssh" || gotArgv[1] != "spark01" {
		t.Fatalf("spark01 argv: %v", gotArgv)
	}
	if gotIn != in || gotOut != &outBuf || gotErr != &errBuf {
		t.Fatal("supplied streams not carried through")
	}
	code, err = cliAdmin(context.Background(), cfg, "spark02", nil, nil, nil)
	if err != nil || code != 7 {
		t.Fatalf("spark02: %d %v", code, err)
	}
	if len(gotArgv) != 4 || gotArgv[0] != "ssh" || gotArgv[1] != "-t" || gotArgv[2] != "spark01" || gotArgv[3] != "ssh -t 'spark02'" {
		t.Fatalf("spark02 argv: %v", gotArgv)
	}
	if _, err := cliAdmin(context.Background(), cfg, "spark99", nil, nil, nil); err == nil || err.Error() != "Choose spark01 or spark02." {
		t.Fatalf("target validation: %v", err)
	}
	// Empty argv is guarded in the real adapter.
	if code, err := remoteAdminExec(context.Background(), nil, nil, nil, nil); code != 1 || err == nil {
		t.Fatalf("empty argv: %d %v", code, err)
	}
}

func TestCliRemoteCommandRejectsUnknownAction(t *testing.T) {
	cfg := remoteTestConfig()
	if _, err := cliRemoteCommand(cfg, "deploy", "", false, ""); err == nil || err.Error() != "Unsupported remote action." {
		t.Fatalf("unknown action: %v", err)
	}
	for _, action := range []string{"status", "load", "unload", "gateway"} {
		if _, err := cliRemoteCommand(cfg, action, "", false, ""); err != nil {
			t.Fatalf("%s rejected: %v", action, err)
		}
	}
}

func TestCliRemoteOCR(t *testing.T) {
	var gotArgv []string
	var gotStdin []byte
	restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		if argv[0] != "ssh" || !strings.HasPrefix(argv[len(argv)-1], "~/.local/bin/sparky --json node ocr ") {
			t.Errorf("unexpected command: %v", argv)
		}
		gotArgv, gotStdin = argv, stdin
		return remoteRunResult{stdout: []byte(`{}`), code: 0}, nil
	})
	defer restore()

	cfg := remoteTestConfig()
	if _, err := cliRemoteOCR(context.Background(), cfg, "install", "https://git.example/r.git", "main", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotArgv[len(gotArgv)-1] != "~/.local/bin/sparky --json node ocr '--root' '/srv' '--payload' '-'" {
		t.Fatalf("ocr command wrong: %q", gotArgv[len(gotArgv)-1])
	}
	want := `{"action":"install","gateway":"https://gateway.example","rebuild":true,"ref":"main","repo":"https://git.example/r.git"}`
	if string(gotStdin) != want {
		t.Fatalf("ocr payload wrong: %q", gotStdin)
	}

	// Validation before effects.
	if _, err := cliRemoteOCR(context.Background(), cfg, "deploy", "", "", false); err == nil {
		t.Fatal("unknown ocr action accepted")
	}
	if _, err := cliRemoteOCR(context.Background(), cfg, "status", "repo", "", false); err == nil || !strings.Contains(err.Error(), "only valid with 'sparky ocr install'") {
		t.Fatalf("install-only args: %v", err)
	}
	noGateway := clientConfig{Extra: cfg.Extra}
	noGateway.Gateway = ""
	if _, err := cliRemoteOCR(context.Background(), noGateway, "status", "", "", false); err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("gateway validation: %v", err)
	}
}

// lockedWriter is a race-free io.Writer for progress assertions.
type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestCliProgressStop(t *testing.T) {
	origOut, origInterval := cliProgressOut, cliProgressInterval
	defer func() { cliProgressOut, cliProgressInterval = origOut, origInterval }()
	out := &lockedWriter{}
	cliProgressOut = out
	cliProgressInterval = 20 * time.Millisecond

	stop := cliProgress("Working...", true, false)
	if !strings.Contains(out.String(), "Working...") {
		t.Fatalf("first message not immediate: %q", out.String())
	}
	time.Sleep(70 * time.Millisecond)
	if !strings.Contains(out.String(), "Still waiting") {
		t.Fatalf("no follow-up messages: %q", out.String())
	}
	stop()
	final := out.String()
	time.Sleep(60 * time.Millisecond)
	if out.String() != final {
		t.Fatalf("progress continued after stop: %q", out.String())
	}
	stop() // idempotent

	// Disabled or JSON callers get no output at all.
	stop = cliProgress("quiet", false, true)
	if out.String() != final {
		t.Fatalf("disabled progress wrote: %q", out.String())
	}
	stop()
}

func TestCliProgressForcedWording(t *testing.T) {
	origOut, origInterval := cliProgressOut, cliProgressInterval
	defer func() { cliProgressOut, cliProgressInterval = origOut, origInterval }()
	out := &lockedWriter{}
	cliProgressOut = out
	cliProgressInterval = 5 * time.Millisecond
	stop := cliProgress("go", true, true)
	time.Sleep(30 * time.Millisecond)
	stop()
	if !strings.Contains(out.String(), "The forced model operation is still running.") {
		t.Fatalf("forced wording missing: %q", out.String())
	}
	if !strings.Contains(out.String(), "Still waiting") {
		t.Fatalf("waiting text wrong: %q", out.String())
	}
}
