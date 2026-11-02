package sparky

// Stage 2 of the public CLI: device operations that reach a Spark over its
// private SSH protocol. The protocol is exactly the one in
// src/spark_launcher/control.py: a local ssh child runs
// "~/.local/bin/sparky --json node ..." remotely and answers with one JSON
// object on stdout. Credentials and raw diagnostics never reach user output.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	remoteConnectTimeoutArg = "ConnectTimeout=10"
	remoteSparkyPrefix      = "~/.local/bin/sparky --json node "
)

// Capture limits are vars only so tests can exercise overflow with a real
// child; production always uses the documented 128MiB/1MiB bounds.
var (
	remoteStdoutLimit int64 = 128 << 20
	remoteStderrLimit int64 = 1 << 20
)

// remoteIdentityScanBound caps the retained stderr scan window for split
// identity URLs.
const remoteIdentityScanBound = 64 << 10

// errRemoteTimeout reports that the local ssh child was stopped because the
// operation deadline passed. Remote work may continue on the device; only
// our own local process was stopped, and it is always reaped.
var errRemoteTimeout = fail("The device command timed out. Remote work may continue on the device.")

// remoteIdentityPattern is the only URL ever copied out of remote stderr.
var remoteIdentityPattern = regexp.MustCompile(`https://login\.tailscale\.com/a/[A-Za-z0-9]+`)

// remoteRunResult is one finished ssh child: bounded captured streams and
// the exit code.
type remoteRunResult struct {
	stdout []byte
	stderr []byte
	code   int
}

// remoteRunner is the narrow seam for spawning the local ssh child. It must
// kill the child when ctx is done, reap it on every path, stream stderr
// chunks to onStderr while running (so an identity link is seen early), and
// bound both captures. Implementations must never include argv in errors.
type remoteRunner func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error)

// remoteAdminRunner is the seam for interactive SSH sessions: the caller
// supplies the terminal streams (production passes os.Stdin/out/err), and
// the child's exit code is returned.
type remoteAdminRunner func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)

var remoteRun remoteRunner = remoteExecRunner
var remoteAdmin remoteAdminRunner = remoteAdminExec

// ---------------------------------------------------------------------------
// Configuration access and validation (before anything is spawned).

func remoteConfigString(cfg clientConfig, key string) string {
	return str(cfg.Extra[key])
}

// remoteSafeHost validates an SSH alias from the site file.
func remoteSafeHost(host string) (string, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]*$`).MatchString(host) {
		return "", fail("Set an existing SSH host alias in the site file.")
	}
	return host, nil
}

func remoteRoot(cfg clientConfig, message string) (string, error) {
	root := remoteConfigString(cfg, "remote_root")
	if root == "" || !strings.HasPrefix(root, "/") {
		return "", errors.New(message)
	}
	return root, nil
}

// cliRemoteActions are the action words the private protocol accepts.
var cliRemoteActions = []string{"status", "load", "unload", "gateway"}

// cliRemoteCommand builds the ssh argv for one remote action word.
// Validation (host alias, absolute root, action word, force rules) happens
// before any process is spawned.
func cliRemoteCommand(cfg clientConfig, action string, model string, force bool) ([]string, error) {
	if !contains(cliRemoteActions, action) {
		return nil, fail("Unsupported remote action.")
	}
	host, err := remoteSafeHost(remoteConfigString(cfg, "admin_ssh_host"))
	if err != nil {
		return nil, err
	}
	root, err := remoteRoot(cfg, "Set an absolute remote_root in the site file.")
	if err != nil {
		return nil, err
	}
	if force && action != "load" && action != "unload" {
		return nil, fail("--force is available only for load and unload.")
	}
	args := []string{action, "--root", root}
	if force {
		args = append(args, "--force")
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	return []string{"ssh", "-o", remoteConnectTimeoutArg, host, remoteSparkyPrefix + shellJoin(args)}, nil
}

// ---------------------------------------------------------------------------
// Local ssh child execution (the concrete seam).

// errCaptureTooLarge is internal: a bounded stream exceeded its limit.
var errCaptureTooLarge = errors.New("capture limit exceeded")

// errStreamIncomplete is internal: the child exited but its output pipe
// completion is unconfirmed (exec.ErrWaitDelay after a dangling descendant
// held the pipe). The captured bytes may be a valid JSON prefix, so they
// must not be treated as a complete result.
// streamIncompleteError is the safe public failure for unconfirmed output:
// the captured bytes may be only a prefix, so they are never returned as
// success. It reports a non-zero exit code like any other failed command.
type streamIncompleteError struct{ message string }

func (e *streamIncompleteError) Error() string { return e.message }
func (e *streamIncompleteError) ExitCode() int { return 1 }

var errStreamIncomplete error = &streamIncompleteError{"output stream incomplete"}

// boundedCapture is an io.Writer that stores at most limit bytes, streams
// every write to onChunk (so partial stderr is inspectable while the child
// runs), and triggers kill immediately on overflow. Writes are silently
// dropped after overflow; the child is stopped by the kill callback so the
// pipe EOFs and os/exec's internal copy goroutine (managed by Cmd.Wait)
// always joins.
type boundedCapture struct {
	limit   int64
	buf     bytes.Buffer
	onChunk func([]byte)
	kill    func()
	over    bool
}

func (b *boundedCapture) Write(p []byte) (int, error) {
	if b.over {
		return len(p), nil
	}
	if int64(b.buf.Len())+int64(len(p)) > b.limit {
		b.over = true
		if b.kill != nil {
			b.kill() // stop the chatty child immediately.
		}
		return len(p), nil
	}
	b.buf.Write(p)
	if b.onChunk != nil {
		b.onChunk(p)
	}
	return len(p), nil
}

// remoteExecRunner spawns the local ssh child. os/exec owns the OS pipes
// and their copy goroutines: Cmd.Wait joins them, enforces WaitDelay, and
// reaps the child on every path. Only our own child is killed; remote work
// may continue. argv never appears in returned errors.
func remoteExecRunner(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
	if len(argv) == 0 {
		return remoteRunResult{}, fail("Missing command.")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// After the kill, force pipe closure so Wait returns and the child is
	// reaped even if a dangling descendant keeps a pipe open.
	cmd.WaitDelay = time.Second
	out := &boundedCapture{limit: remoteStdoutLimit, kill: cancel}
	errw := &boundedCapture{limit: remoteStderrLimit, onChunk: onStderr, kill: cancel}
	cmd.Stdout = out
	cmd.Stderr = errw
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Start(); err != nil {
		return remoteRunResult{}, fail("Could not start the device command.")
	}
	waitErr := cmd.Wait()
	result := remoteRunResult{stdout: out.buf.Bytes(), stderr: errw.buf.Bytes()}
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		result.code = exit.ExitCode()
		waitErr = nil
	}
	if out.over || errw.over {
		return result, errCaptureTooLarge
	}
	// ErrWaitDelay means a dangling descendant kept an output pipe open and
	// os/exec detached the copier: output completion is unconfirmed. A valid
	// JSON prefix must never be reported as complete success, so return a
	// safe stream error (capture overflow already has its own result).
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		return result, errStreamIncomplete
	}
	if waitErr != nil {
		return result, fail("Could not run the device command.")
	}
	return result, nil
}

// remoteAdminExec runs an interactive SSH session on the supplied streams
// (nil falls back to the terminal), returning the child's exit code.
func remoteAdminExec(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 {
		return 1, fail("Missing command.")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	// Supplied non-file streams are os/exec pipes copied by internal
	// goroutines; a dangling descendant holding one would otherwise block
	// Run forever. A WaitDelay detaches such copiers after the child exits.
	// Real terminal files do not need the bound.
	if !isOSFileStream(cmd.Stdin) || !isOSFileStream(cmd.Stdout) || !isOSFileStream(cmd.Stderr) {
		cmd.WaitDelay = 3 * time.Second
	}
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exit.ExitCode(), nil
	}
	return 1, fail("Could not start the SSH session.")
}

// isOSFileStream reports whether the stream is a real operating-system file
// (terminal or regular file) rather than a caller-supplied pipe.
func isOSFileStream(stream any) bool {
	_, ok := stream.(*os.File)
	return ok
}

// ---------------------------------------------------------------------------
// The private protocol: one JSON object over the ssh child.

// cliRemoteJSON runs one remote command and returns its single JSON object.
// payload (possibly nil) is written to the child's stdin as exact bytes and
// never appears in argv or errors. A valid error envelope on a nonzero exit
// propagates only its documented error string; raw diagnostics stay private.
func cliRemoteJSON(ctx context.Context, argv []string, payload []byte, timeout time.Duration) (Object, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var mu sync.Mutex
	var identity string
	var identityBuf bytes.Buffer
	onStderr := func(chunk []byte) {
		mu.Lock()
		defer mu.Unlock()
		if identity != "" {
			return
		}
		identityBuf.Write(chunk)
		// Bound the retained scan window.
		if identityBuf.Len() > remoteIdentityScanBound {
			identityBuf = *bytes.NewBuffer(append([]byte(nil), identityBuf.Bytes()[identityBuf.Len()-remoteIdentityScanBound:]...))
		}
		data := identityBuf.Bytes()
		loc := remoteIdentityPattern.FindIndex(data)
		if loc == nil {
			return
		}
		if loc[1] == len(data) {
			// The auth token may continue in the next write (an
			// earlier chunk ending mid-token must not become a
			// wrong link); wait for a settling byte.
			return
		}
		identity = remoteIdentityMessage(string(data[loc[0]:loc[1]]))
		cancel() // stop waiting; return the safe message promptly.
	}

	result, err := remoteRun(runCtx, argv, payload, onStderr)
	// A found identity wins on every completion and error path.
	mu.Lock()
	found := identity
	mu.Unlock()
	if found != "" {
		return nil, errors.New(found)
	}
	if err != nil {
		if errors.Is(err, errCaptureTooLarge) {
			return nil, fail("The device returned too much data.")
		}
		if errors.Is(err, errStreamIncomplete) {
			return nil, &streamIncompleteError{"The device output ended unexpectedly. Run the command again."}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err() // the caller canceled.
		}
		if runCtx.Err() != nil {
			return nil, errRemoteTimeout // operation deadline.
		}
		return nil, fail("Could not start the device command.")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if runCtx.Err() != nil {
		return nil, errRemoteTimeout
	}
	if result.code != 0 {
		if envelope, derr := decode(result.stdout); derr == nil {
			if e, ok := envelope["error"].(string); ok {
				return nil, errors.New(e)
			}
		}
		return nil, errors.New(remoteFailureMessage(result.code, string(result.stderr)))
	}
	object, derr := decode(result.stdout)
	if derr != nil {
		return nil, fail("The device returned invalid data. Update sparky on both Sparks.")
	}
	return object, nil
}

// remoteIdentityMessage builds the only identity message ever shown.
func remoteIdentityMessage(url string) string {
	return "Tailscale needs an identity check. Open " + url + ", then run the command again."
}

// remoteFailureMessage explains known failures without exposing arbitrary
// remote diagnostics. It mirrors control.py remote_failure_message().
func remoteFailureMessage(code int, stderr string) string {
	if m := remoteIdentityPattern.FindString(stderr); m != "" {
		return remoteIdentityMessage(m)
	}
	if code != 255 {
		return fmt.Sprintf("The device command failed (exit %d). The device did not return an error report. Run sparky doctor to check the installation.", code)
	}
	detail := strings.ToLower(stderr)
	reasons := []struct {
		patterns []string
		message  string
	}{
		{[]string{"host key verification failed", "remote host identification has changed"},
			"SSH could not verify the device identity. Check its host key before changing known_hosts."},
		{[]string{"permission denied", "authentication failed"},
			"SSH authentication failed. Run sparky admin ssh spark01 to check your access."},
		{[]string{"connection timed out", "operation timed out", "no route to host", "network is unreachable"},
			"The SSH connection timed out or the device is unreachable. Check Tailscale and the device network."},
		{[]string{"connection refused"},
			"The device refused the SSH connection. Check that its SSH service is running."},
		{[]string{"could not resolve hostname", "name or service not known"},
			"SSH could not find the configured device address. Check the SSH host alias."},
		{[]string{"connection reset", "connection closed", "broken pipe"},
			"The SSH connection closed before the device returned data. Run the command again."},
	}
	for _, reason := range reasons {
		for _, pattern := range reason.patterns {
			if strings.Contains(detail, pattern) {
				return reason.message
			}
		}
	}
	return "The SSH connection failed (exit 255). Run sparky admin ssh spark01 to see the connection details."
}

// ---------------------------------------------------------------------------
// Public wrappers for Main.

// cliRemoteStatus asks the head device for its status (90s).
func cliRemoteStatus(ctx context.Context, cfg clientConfig) (Object, error) {
	argv, err := cliRemoteCommand(cfg, "status", "", false)
	if err != nil {
		return nil, err
	}
	return cliRemoteJSON(ctx, argv, nil, 90*time.Second)
}

// cliRemoteResult runs a remote load/unload (7230s) or other result action
// (120s).
func cliRemoteResult(ctx context.Context, cfg clientConfig, action string, model string, force bool) (Object, error) {
	argv, err := cliRemoteCommand(cfg, action, model, force)
	if err != nil {
		return nil, err
	}
	timeout := 120 * time.Second
	if action == "load" || action == "unload" {
		timeout = 7230 * time.Second
	}
	return cliRemoteJSON(ctx, argv, nil, timeout)
}

// cliRemoteOperationTimeout returns the documented per-operation timeout.
func cliRemoteOperationTimeout(operation string) time.Duration {
	switch operation {
	case "image-profile":
		return 900 * time.Second
	case "run":
		return 180 * time.Second
	}
	return 120 * time.Second
}

// cliRemoteRPC runs one remote RPC with the payload on stdin as exact JSON.
func cliRemoteRPC(ctx context.Context, cfg clientConfig, operation string, payload Object) (Object, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`).MatchString(operation) {
		return nil, fail("Invalid operation name.")
	}
	host, err := remoteSafeHost(remoteConfigString(cfg, "admin_ssh_host"))
	if err != nil {
		return nil, err
	}
	root, err := remoteRoot(cfg, "Set an absolute remote_root in the client configuration.")
	if err != nil {
		return nil, err
	}
	words := []string{"rpc", operation, "--root", root, "--payload", "-"}
	argv := []string{"ssh", "-o", remoteConnectTimeoutArg, host, remoteSparkyPrefix + shellJoin(words)}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fail("Invalid operation payload.")
	}
	return cliRemoteJSON(ctx, argv, body, cliRemoteOperationTimeout(operation))
}

// cliRemoteOCRActions are the node ocr sub-actions; install-only args are
// rejected elsewhere.
var cliRemoteOCRActions = []string{"install", "status", "start", "stop"}

// cliRemoteOCR sends one node ocr request with the payload on stdin
// (mirroring ocr.py remote_ocr): 7230s for install, 180s otherwise.
func cliRemoteOCR(ctx context.Context, cfg clientConfig, action string, repo string, ref string, rebuild bool) (Object, error) {
	if !contains(cliRemoteOCRActions, action) {
		return nil, fail("Unsupported OCR action.")
	}
	if action != "install" && (repo != "" || ref != "" || rebuild) {
		return nil, fail("--repo, --ref, and --rebuild are only valid with 'sparky ocr install'.")
	}
	host, err := remoteSafeHost(remoteConfigString(cfg, "admin_ssh_host"))
	if err != nil {
		return nil, err
	}
	root, err := remoteRoot(cfg, "Set an absolute remote_root in the client configuration.")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(cfg.Gateway, "https://") {
		return nil, fail("Set gateway to an HTTPS URL in the client configuration.")
	}
	words := []string{"--root", root, "--payload", "-"}
	argv := []string{"ssh", "-o", remoteConnectTimeoutArg, host, remoteSparkyPrefix + "ocr " + shellJoin(words)}
	payload := Object{"action": action, "repo": repo, "ref": ref, "rebuild": rebuild, "gateway": cfg.Gateway}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fail("Invalid OCR payload.")
	}
	timeout := 180 * time.Second
	if action == "install" {
		timeout = 7230 * time.Second
	}
	return cliRemoteJSON(ctx, argv, body, timeout)
}

// cliDevices queries the head and the nested worker concurrently (60s each)
// and reports per-device results plus an overall ok flag.
func cliDevices(ctx context.Context, cfg clientConfig) (Object, error) {
	host, err := remoteSafeHost(remoteConfigString(cfg, "admin_ssh_host"))
	if err != nil {
		return nil, err
	}
	worker, err := remoteSafeHost(remoteConfigString(cfg, "worker_ssh_host"))
	if err != nil {
		return nil, err
	}
	nested := "ssh " + shellQuote(worker) + " " + shellQuote("~/.local/bin/sparky --json node device")
	commands := map[string][]string{
		"spark01": {"ssh", "-o", remoteConnectTimeoutArg, host, "~/.local/bin/sparky --json node device"},
		"spark02": {"ssh", "-o", remoteConnectTimeoutArg, host, nested},
	}
	var mu sync.Mutex
	results := Object{}
	var group sync.WaitGroup
	for name, argv := range commands {
		group.Add(1)
		go func(name string, argv []string) {
			defer group.Done()
			object, err := cliRemoteJSON(ctx, argv, nil, 60*time.Second)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				message := err.Error()
				if errors.Is(err, errRemoteTimeout) || errors.Is(err, context.DeadlineExceeded) {
					message = "Device query timed out."
				}
				results[name] = Object{"error": message}
				return
			}
			results[name] = object
		}(name, argv)
	}
	group.Wait()
	ok := true
	for _, entry := range results {
		if _, has := entry.(Object)["error"]; has {
			ok = false
		}
	}
	return Object{"devices": results, "ok": ok}, nil
}

// cliAdmin opens an interactive SSH session to a device on the supplied
// streams (nil means the terminal) and returns the child's exit code.
// spark02 hops through the head with ssh -t.
func cliAdmin(ctx context.Context, cfg clientConfig, target string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	host, err := remoteSafeHost(remoteConfigString(cfg, "admin_ssh_host"))
	if err != nil {
		return 0, err
	}
	switch target {
	case "spark01":
		return remoteAdmin(ctx, []string{"ssh", host}, stdin, stdout, stderr)
	case "spark02":
		worker, err := remoteSafeHost(remoteConfigString(cfg, "worker_ssh_host"))
		if err != nil {
			return 0, err
		}
		return remoteAdmin(ctx, []string{"ssh", "-t", host, "ssh -t " + shellQuote(worker)}, stdin, stdout, stderr)
	}
	return 0, fail("Choose spark01 or spark02.")
}

// ---------------------------------------------------------------------------
// Progress: occasional human stderr messages while a long operation runs.

// cliProgressInterval is injectable for tests.
var cliProgressInterval = 15 * time.Second

// cliProgressOut receives progress text (stderr in production).
var cliProgressOut io.Writer = os.Stderr

// cliProgressStopper ends the reporter and joins its goroutine.
type cliProgressStopper func()

// cliProgress prints message immediately, then every interval until
// stopped. JSON callers disable it entirely.
func cliProgress(message string, enabled bool, forced bool) cliProgressStopper {
	if !enabled {
		return func() {}
	}
	stop := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once
	detail := "Active work must finish before a device changes."
	if forced {
		detail = "The forced model operation is still running."
	}
	// The first message is printed synchronously so callers can rely on
	// it being visible when cliProgress returns.
	fmt.Fprintln(cliProgressOut, message)
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(cliProgressInterval)
		defer ticker.Stop()
		seconds := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				seconds += int(cliProgressInterval / time.Second)
				fmt.Fprintf(cliProgressOut, "Still waiting (%ds). %s\n", seconds, detail)
			}
		}
	}()
	return func() {
		once.Do(func() { close(stop) })
		<-stopped
	}
}
