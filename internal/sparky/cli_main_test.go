package sparky

// Public Main dispatch tests. Everything runs through Main with injected
// seams: temp config/state dirs, an httptest gateway for the catalog, fake
// remote runners, an own helper child for real harness launches, and a
// bound NodeDispatch. No real SSH, services, or saved sessions.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mainDirs points the config and state at temp files and returns their dir.
func mainDirs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SPARKY_CONFIG", filepath.Join(dir, "client.json"))
	t.Setenv("SPARKY_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("COLUMNS", "")
	return dir
}

// writeMainConfig creates a valid client configuration.
func writeMainConfig(t *testing.T, gateway string) {
	t.Helper()
	raw := Object{
		"version": 1, "gateway": gateway, "model_prefix": "garuda/",
		"timeout_seconds": 7200, "compact_percent": 95,
		"admin_ssh_host": "spark01", "worker_ssh_host": "spark02",
		"remote_root": "/srv", "admin_url": "https://ui.example/ui",
	}
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(clientPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// mainCatalogGateway serves exactly the one public catalog endpoint and
// fails loudly on anything else, so an accidental inference call cannot
// succeed behind a catch-all handler.
func mainCatalogGateway(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sparky/catalog" {
			http.Error(w, `{"error":"unexpected path"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"garuda/glm","name":"GLM","aliases":["glm"],"context_length":40960},
			{"id":"garuda/spark-auto","context_length":40960}]}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func runMain(args []string, in string) (int, string, string) {
	var outBuf, errBuf bytes.Buffer
	code := Main(args, strings.NewReader(in), &outBuf, &errBuf)
	return code, outBuf.String(), errBuf.String()
}

func TestMainHelpAndVersionNeedNoConfig(t *testing.T) {
	mainDirs(t)
	for _, args := range [][]string{{}, {"--help"}, {"--version"}, {"status", "--help"}} {
		code, out, errout := runMain(args, "")
		if code != 0 || errout != "" {
			t.Fatalf("%v: code %d stderr %q", args, code, errout)
		}
		if len(args) == 1 && args[0] == "--version" {
			// Compare against the rendered line rather than a literal: a
			// release build injects cliVersion/cliCommit with -ldflags -X, and
			// this test must stay about dispatch, not about the format.
			if strings.TrimSpace(out) != cliVersionLine() {
				t.Fatalf("version output: %q", out)
			}
		} else if !strings.Contains(out, "Commands:") {
			t.Fatalf("help output missing: %q", out)
		}
	}
}

func TestMainJSONErrorEnvelopeRecognizesTokenRole(t *testing.T) {
	mainDirs(t)
	// --json seen before the failing option: one safe envelope on stderr.
	code, out, errout := runMain([]string{"status", "--json", "--bogus"}, "")
	if code != 2 || out != "" {
		t.Fatalf("code %d stdout %q", code, out)
	}
	var envelope Object
	if err := json.Unmarshal([]byte(errout), &envelope); err != nil {
		t.Fatalf("stderr not one JSON envelope: %q", errout)
	}
	if envelope["code"] != float64(2) || !strings.Contains(str(envelope["error"]), "Unknown option") {
		t.Fatalf("envelope wrong: %v", envelope)
	}

	// A value spelled --json is not an output request: human error only.
	code, out, errout = runMain([]string{"--project", "--json", "status"}, "")
	if code != 2 {
		t.Fatalf("code %d", code)
	}
	if strings.Contains(errout, `"code"`) || strings.Contains(out, `"code"`) {
		t.Fatalf("json leaked for value-role token: %q %q", out, errout)
	}

	// After a delimiter the word belongs to the tail, and --json before it
	// still selects JSON mode. Two model arguments are one too many, so the
	// excess positional is a usage error, not a config failure.
	code, _, errout = runMain([]string{"load", "--json", "--", "glm", "extra"}, "")
	if code != 2 || !strings.Contains(errout, `"code": 2`) && !strings.Contains(errout, `"code":2`) {
		t.Fatalf("tail positional with json: %d %q", code, errout)
	}
}

func TestMainMissingConfigAndRuntimeCodes(t *testing.T) {
	mainDirs(t)
	code, out, errout := runMain([]string{"status"}, "")
	if code != 1 || out != "" {
		t.Fatalf("code %d stdout %q", code, out)
	}
	if !strings.Contains(errout, "sparky: error") || strings.Contains(errout, `"`) {
		t.Fatalf("human error expected: %q", errout)
	}
	// Usage errors stay 2.
	if code, _, _ := runMain([]string{"load"}, ""); code != 2 {
		t.Fatalf("usage code %d", code)
	}
}

func TestMainInitExclusiveAndPrivate(t *testing.T) {
	dir := mainDirs(t)
	// The product creates a NEW site directory with 0700; an existing
	// parent (like the temp root) must never be chmodded.
	site := filepath.Join(dir, "site")
	t.Setenv("SPARKY_CONFIG", filepath.Join(site, "client.json"))
	parentBefore, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errout := runMain([]string{"init", "--gateway", "http://127.0.0.1:8443"}, "")
	if code != 0 {
		t.Fatalf("init failed: %d %q", code, errout)
	}
	if !strings.Contains(out, "Client configuration created") {
		t.Fatalf("human init output: %q", out)
	}
	path := filepath.Join(site, "client.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode: %v %v", info, err)
	}
	newSite, err := os.Stat(site)
	if err != nil || newSite.Mode().Perm() != 0o700 {
		t.Fatalf("site dir mode: %v %v", newSite, err)
	}
	parentAfter, err := os.Stat(dir)
	if err != nil || parentAfter.Mode().Perm() != parentBefore.Mode().Perm() {
		t.Fatalf("parent dir chmodded: %v -> %v", parentBefore.Mode(), parentAfter)
	}

	// Second init refuses and preserves the file.
	before, _ := os.ReadFile(path)
	code, _, errout = runMain([]string{"init", "--gateway", "http://127.0.0.1:8443"}, "")
	if code != 1 || !strings.Contains(errout, "already exists") {
		t.Fatalf("second init: %d %q", code, errout)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("init overwrote the configuration")
	}

	// A broken symlink is also protected.
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(filepath.Join(dir, "missing.json"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPARKY_CONFIG", link)
	code, _, errout = runMain([]string{"init", "--gateway", "http://127.0.0.1:8443"}, "")
	if code != 1 || !strings.Contains(errout, "already exists") {
		t.Fatalf("broken symlink not protected: %d %q", code, errout)
	}
	t.Setenv("SPARKY_CONFIG", filepath.Join(dir, "client.json"))

	// JSON mode reports the created path only.
	other := filepath.Join(dir, "other.json")
	t.Setenv("SPARKY_CONFIG", other)
	code, out, errout = runMain([]string{"init", "--gateway", "https://spark.example", "--json"}, "")
	if code != 0 {
		t.Fatalf("json init: %d %q", code, errout)
	}
	var created Object
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("init json: %v %q", err, out)
	}
	if created["status"] != "created" || created["path"] != other {
		t.Fatalf("init json wrong: %v", created)
	}
}

func TestMainLogsSkipsCorruptAndLimits(t *testing.T) {
	mainDirs(t)
	launches := filepath.Join(stateDir(), "launches")
	if err := os.MkdirAll(launches, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(launches, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("b.json", `{"started_at":"2026-01-02T00:00:00Z","harness":"claude"}`)
	write("a.json", `{"started_at":"2026-01-01T00:00:00Z","harness":"codex"}`)
	write("corrupt.json", `{not json`)
	write("wrong.json", `{"no":"started_at"}`)
	for _, name := range []string{"ignore.txt", "nested"} {
		if err := os.WriteFile(filepath.Join(launches, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errout := runMain([]string{"logs", "--limit", "1", "--json"}, "")
	if code != 0 {
		t.Fatalf("logs: %d %q", code, errout)
	}
	var data Object
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("logs json: %v %q", err, out)
	}
	records := arr(data["records"])
	if len(records) != 1 || str(obj(records[0])["harness"]) != "claude" {
		t.Fatalf("records newest first: %v", records)
	}
	skipped := stringsOf(data["skipped_files"])
	if len(skipped) != 2 || !contains(skipped, "corrupt.json") || !contains(skipped, "wrong.json") {
		t.Fatalf("skipped wrong: %v", skipped)
	}
}

// mainNodeRemote builds the exact remote command string the real runner
// sends: the unquoted sparky prefix followed by every word single-quoted by
// shellJoin. Tests must match the actual quoting contract, not substrings.
func mainNodeRemote(words ...string) string {
	return remoteSparkyPrefix + shellJoin(words)
}

func TestMainRemoteCommandsThroughFakeRunner(t *testing.T) {
	mainDirs(t)
	writeMainConfig(t, "https://gateway.example")
	var calls []string
	restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		if len(argv) == 0 || argv[0] != "ssh" {
			return remoteRunResult{}, fmt.Errorf("unexpected argv: %v", argv)
		}
		remote := argv[len(argv)-1]
		calls = append(calls, remote+" stdin="+string(stdin))
		respond := func(s string) (remoteRunResult, error) {
			return remoteRunResult{stdout: []byte(s), code: 0}, nil
		}
		switch remote {
		case mainNodeRemote("status", "--root", "/srv"):
			return respond(`{"memory":"12 GB"}`)
		case mainNodeRemote("rpc", "plan", "--root", "/srv", "--payload", "-"):
			return respond(`{"workload":"glm","steps":[{"action":"unload","workload":"other"}]}`)
		case mainNodeRemote("load", "--root", "/srv", "--model", "glm"):
			return respond(`{"status":"loading"}`)
		case mainNodeRemote("load", "--root", "/srv", "--force", "--model", "glm"):
			return respond(`{"status":"loading"}`)
		case mainNodeRemote("rpc", "image-profile", "--root", "/srv", "--payload", "-"):
			return respond(`{"mode":"shared"}`)
		case mainNodeRemote("rpc", "placement", "--root", "/srv", "--payload", "-"):
			return respond(`{"model":"glm","on":"spark02"}`)
		case mainNodeRemote("rpc", "run", "--root", "/srv", "--payload", "-"):
			// The workflow travels in the payload on stdin.
			if !strings.Contains(string(stdin), "class_type") {
				return remoteRunResult{}, fmt.Errorf("run payload missing workflow: %q", stdin)
			}
			return respond(`{"prompt_id":"abc"}`)
		case remoteSparkyPrefix + "ocr " + shellJoin([]string{"--root", "/srv", "--payload", "-"}):
			if !strings.Contains(string(stdin), `"action":"install"`) {
				return remoteRunResult{}, fmt.Errorf("ocr payload missing action: %q", stdin)
			}
			return respond(`{"ocr":"installed"}`)
		case mainNodeRemote("gateway", "--root", "/srv"):
			return respond(`{"routes":4}`)
		}
		return remoteRunResult{}, fmt.Errorf("unexpected remote command: %q stdin %q", remote, stdin)
	})
	defer restore()

	// status --json
	code, out, errout := runMain([]string{"status", "--json"}, "")
	if code != 0 || !strings.Contains(out, `"memory": "12 GB"`) || errout != "" {
		t.Fatalf("status json: %d %q %q", code, out, errout)
	}
	// Human status uses the terminal renderer, not raw JSON.
	code, out, _ = runMain([]string{"status"}, "")
	if code != 0 || strings.Contains(out, `"memory"`) {
		t.Fatalf("status human: %d %q", code, out)
	}
	// ocr install carries the payload on stdin.
	code, out, _ = runMain([]string{"ocr", "install", "--repo", "ssh://repo", "--json"}, "")
	if code != 0 || !strings.Contains(out, "ocr") {
		t.Fatalf("ocr: %d %q", code, out)
	}
	// image-profile result view.
	code, out, _ = runMain([]string{"image-profile", "shared", "--json"}, "")
	if code != 0 || !strings.Contains(out, "shared") {
		t.Fatalf("image-profile: %d %q", code, out)
	}
	// config placement.
	code, out, _ = runMain([]string{"config", "placement", "glm", "spark02", "--json"}, "")
	if code != 0 || !strings.Contains(out, "placement") && !strings.Contains(out, "glm") {
		t.Fatalf("config placement: %d %q", code, out)
	}
	// admin enable-harnesses result.
	code, out, _ = runMain([]string{"admin", "enable-harnesses", "--json"}, "")
	if code != 0 || out == "" {
		t.Fatalf("enable-harnesses: %d %q", code, out)
	}

	// Human load prints the plan note then the result; the plan RPC ran first.
	calls = nil
	code, out, errout = runMain([]string{"load", "glm"}, "")
	if code != 0 {
		t.Fatalf("load: %d %q", code, errout)
	}
	if len(calls) != 2 || !strings.Contains(calls[0], `'rpc' 'plan'`) || !strings.Contains(calls[1], `'load' '--root'`) {
		t.Fatalf("load calls: %v", calls)
	}
	if !strings.Contains(errout, "Will unload after active requests finish: other.") {
		t.Fatalf("conflict note missing: %q", errout)
	}

	// Forced JSON load goes straight to the remote command.
	calls = nil
	code, out, errout = runMain([]string{"load", "glm", "--force", "--json"}, "")
	if code != 0 || len(calls) != 1 || !strings.Contains(calls[0], "--force") {
		t.Fatalf("force json load: %d %v %q", code, calls, errout)
	}
	if strings.Contains(errout, "Will unload") {
		t.Fatal("forced load printed the plan note")
	}

	// run comfyui submits a bounded API workflow with the prompt_id check.
	workflow := filepath.Join(t.TempDir(), "api.json")
	if err := os.WriteFile(workflow, []byte(`{"3":{"class_type":"KSampler","inputs":{"seed":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errout = runMain([]string{"run", "comfyui", workflow, "--on", "spark02", "--json"}, "")
	if code != 0 || !strings.Contains(out, `"prompt_id": "abc"`) {
		t.Fatalf("run: %d %q %q", code, out, errout)
	}

	// The UI-format workflow is rejected before any SSH call.
	ui := filepath.Join(t.TempDir(), "ui.json")
	if err := os.WriteFile(ui, []byte(`{"nodes":[],"links":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(calls)
	code, _, errout = runMain([]string{"run", "comfyui", ui, "--json"}, "")
	if code != 1 || !strings.Contains(errout, "API workflow") || len(calls) != before {
		t.Fatalf("ui workflow accepted: %d %q", code, errout)
	}
}

func TestMainDevicesExitCode(t *testing.T) {
	mainDirs(t)
	writeMainConfig(t, "https://gateway.example")
	restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		if strings.Contains(argv[len(argv)-1], "node device") {
			return remoteRunResult{stdout: []byte(`{"device":"spark01"}`), code: 0}, nil
		}
		return remoteRunResult{}, fmt.Errorf("unexpected: %s", argv[len(argv)-1])
	})
	defer restore()
	code, out, _ := runMain([]string{"devices", "--json"}, "")
	if code != 0 || !strings.Contains(out, `"ok": true`) {
		t.Fatalf("devices: %d %q", code, out)
	}
	restore2 := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		return remoteRunResult{stdout: []byte(`{"error":"down"}`), code: 1}, nil
	})
	defer restore2()
	code, out, _ = runMain([]string{"devices", "--json"}, "")
	if code != 1 || !strings.Contains(out, `"ok": false`) {
		t.Fatalf("devices failure: %d %q", code, out)
	}
}

func TestMainDoctorNeverInfers(t *testing.T) {
	dir := mainDirs(t)
	server := mainCatalogGateway(t)
	writeMainConfig(t, server.URL)
	// Deterministic missing binaries: an isolated empty PATH and home, so
	// the developer machine's installed harnesses are invisible.
	emptyBin := filepath.Join(dir, "empty-bin")
	if err := os.MkdirAll(emptyBin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", emptyBin)
	t.Setenv("HOME", dir)
	code, out, errout := runMain([]string{"doctor", "--json"}, "")
	if code != 1 { // no harness binaries are installed here
		t.Fatalf("doctor code: %d %q", code, errout)
	}
	var data Object
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("doctor json: %v %q", err, out)
	}
	if data["inference"] != false || data["ok"] != false {
		t.Fatalf("doctor shape: %v", data)
	}
	checks := arr(data["checks"])
	if len(checks) != 5 {
		t.Fatalf("checks: %v", checks)
	}
	if first := obj(checks[0]); first["name"] != "gateway" || first["ok"] != true {
		t.Fatalf("gateway check: %v", first)
	}
}

func TestMainUIJSONAndBrowserSeam(t *testing.T) {
	mainDirs(t)
	writeMainConfig(t, "https://gateway.example")
	code, out, errout := runMain([]string{"ui", "--json"}, "")
	if code != 0 || strings.TrimSpace(out) != "{\n  \"url\": \"https://ui.example/ui\"\n}" || errout != "" {
		t.Fatalf("ui json: %d %q %q", code, out, errout)
	}
	origOpen := uiOpenBrowser
	defer func() { uiOpenBrowser = origOpen }()
	uiOpenBrowser = func(string) bool { return true }
	code, out, _ = runMain([]string{"ui"}, "")
	if code != 0 || !strings.Contains(out, "Opened gateway UI") {
		t.Fatalf("ui human opened: %d %q", code, out)
	}
	uiOpenBrowser = func(string) bool { return false }
	code, out, _ = runMain([]string{"ui"}, "")
	if code != 0 || !strings.Contains(out, "Open this gateway UI URL in your browser") {
		t.Fatalf("ui human fallback: %d %q", code, out)
	}
}

func TestMainAdminSSHRejectsJSON(t *testing.T) {
	mainDirs(t)
	writeMainConfig(t, "https://gateway.example")
	code, _, errout := runMain([]string{"admin", "ssh", "spark01", "--json"}, "")
	if code != 2 || !strings.Contains(errout, "--json") {
		t.Fatalf("admin ssh json: %d %q", code, errout)
	}
}

func TestMainAdminSSHThreadsInput(t *testing.T) {
	mainDirs(t)
	writeMainConfig(t, "https://gateway.example")
	origAdmin := remoteAdmin
	defer func() { remoteAdmin = origAdmin }()
	var seen string
	var seenHost string
	remoteAdmin = func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		data, _ := io.ReadAll(stdin)
		seen = string(data)
		seenHost = argv[len(argv)-1]
		return 7, nil
	}
	code, out, errout := runMain([]string{"admin", "ssh", "spark01"}, "session-input")
	if code != 7 || out != "" || errout != "" {
		t.Fatalf("admin ssh passthrough: %d %q %q", code, out, errout)
	}
	if seen != "session-input" || !strings.Contains(seenHost, "spark01") {
		t.Fatalf("admin stdin/host: %q %q", seen, seenHost)
	}
}

func TestMainNodeBinding(t *testing.T) {
	mainDirs(t)
	// The private dispatcher is bound by default and answers with one JSON
	// envelope on stdout, success or failure; Main never installs the public
	// signal handler for it.
	if NodeDispatch == nil {
		t.Fatal("NodeDispatch is not bound")
	}

	// Malformed syntax (unknown action) is typed usage: exit 2 on stdout.
	code, out, errout := runMain([]string{"node", "bogus"}, "")
	if code != 2 || errout != "" {
		t.Fatalf("node usage: %d %q %q", code, out, errout)
	}
	var envelope Object
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("node envelope: %v %q", err, out)
	}
	if envelope["code"] != float64(2) || !strings.Contains(str(envelope["error"]), "Unknown node action") {
		t.Fatalf("usage envelope wrong: %v", envelope)
	}

	// Malformed flags are also usage errors before any effect.
	code, out, _ = runMain([]string{"node", "device", "--bogus"}, "")
	if code != 2 || !strings.Contains(out, "Unknown option") {
		t.Fatalf("node flag usage: %d %q", code, out)
	}

	// Runtime failures (unreadable payload) stay exit 1, still one envelope.
	code, out, _ = runMain([]string{"node", "memory"}, "not json")
	if code != 1 || !strings.Contains(out, "payload") {
		t.Fatalf("node runtime: %d %q", code, out)
	}

	// The seam receives the raw private words with global options stripped.
	orig := NodeDispatch
	defer func() { NodeDispatch = orig }()
	NodeDispatch = func(args []string, in io.Reader) (any, error) {
		data, _ := io.ReadAll(in)
		if len(args) != 1 || args[0] != "memory" || string(data) != "payload" {
			return nil, fmt.Errorf("bad dispatch args %v input %q", args, data)
		}
		return Object{"node": "memory"}, nil
	}
	code, out, errout = runMain([]string{"node", "memory"}, "payload")
	if code != 0 || !strings.Contains(out, `"node": "memory"`) || errout != "" {
		t.Fatalf("bound node: %d %q %q", code, out, errout)
	}
}

func TestMainHarnessDryRunPrepareAndChildCode(t *testing.T) {
	mainDirs(t)
	server := mainCatalogGateway(t)
	writeMainConfig(t, server.URL)
	hermes := t.TempDir()
	t.Setenv("HERMES_HOME", hermes)

	// Dry-run writes no settings and prints the plan.
	code, out, errout := runMain([]string{"hermes", "--dry-run", "--json"}, "")
	if code != 0 || !strings.Contains(out, `"harness": "hermes"`) || !strings.Contains(out, `"prepared": false`) {
		t.Fatalf("dry run: %d %q %q", code, out, errout)
	}
	if _, err := os.Stat(filepath.Join(hermes, "config.yaml")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote settings")
	}

	// Prepare writes the Hermes provider without starting anything.
	code, out, errout = runMain([]string{"hermes", "--prepare", "--json"}, "")
	if code != 0 || !strings.Contains(out, `"prepared": true`) {
		t.Fatalf("prepare: %d %q %q", code, out, errout)
	}
	raw, err := os.ReadFile(filepath.Join(hermes, "config.yaml"))
	if err != nil || !strings.Contains(string(raw), "spark-gateway") {
		t.Fatalf("prepare settings: %v %s", err, raw)
	}

	// A real launch returns the own helper child's exact exit code (143).
	bin := filepath.Join(t.TempDir(), "fakeclaude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 143\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(clientPath())
	var cfg Object
	if err := json.Unmarshal(raw2, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["binaries"] = Object{"claude": bin}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(clientPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errout = runMain([]string{"claude"}, "")
	if code != 143 {
		t.Fatalf("child code not preserved: %d %q", code, errout)
	}
}

func TestMainInterruptReporting(t *testing.T) {
	mainDirs(t)
	// A canceled remote wait with an interrupt cause becomes the typed
	// failure: "work may still continue", exit 130.
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errInterruptCause)
	err := cliInterruptErr(ctx, context.Canceled)
	var interrupt *cliInterruptError
	if !errors.As(err, &interrupt) || interrupt.ExitCode() != 130 {
		t.Fatalf("interrupt mapping: %v", err)
	}
	if !strings.Contains(err.Error(), "Remote work may still continue") {
		t.Fatalf("interrupt message: %v", err)
	}
	// SIGTERM cause maps to 143 on its own fresh context: WithCancelCause
	// keeps only the first cause, so causes are never reused.
	termCtx, termCancel := context.WithCancelCause(context.Background())
	termCancel(errTerminatedCause)
	if got := cliInterruptErr(termCtx, context.Canceled); got != nil {
		var term *cliInterruptError
		if !errors.As(got, &term) || term.ExitCode() != 143 {
			t.Fatalf("terminate mapping: %v", got)
		}
	}
	plain, plainCancel := context.WithCancelCause(context.Background())
	defer plainCancel(nil)
	if got := cliInterruptErr(plain, context.Canceled); got == nil {
		t.Fatal("unrelated cancellation must stay an error")
	}
	// JSON public errors carry the code; private node errors go to stdout.
	var errBuf, outBuf bytes.Buffer
	inv := &cliInvocation{Command: "status", JSON: true, Values: map[string]string{}, Flags: map[string]bool{}}
	if code := cliErrorExit(inv, &cliInterruptError{}, &outBuf, &errBuf); code != 130 || outBuf.Len() != 0 {
		t.Fatalf("json interrupt: %d %q", code, outBuf.String())
	}
	if !strings.Contains(errBuf.String(), `"code": 130`) && !strings.Contains(errBuf.String(), `"code":130`) {
		t.Fatalf("interrupt envelope: %q", errBuf.String())
	}
	errBuf.Reset()
	node := &cliInvocation{Command: "node", Values: map[string]string{}, Flags: map[string]bool{}}
	if code := cliErrorExit(node, &usageError{msg: "bad"}, &outBuf, &errBuf); code != 2 || errBuf.Len() != 0 {
		t.Fatalf("node error stream: %d %q", code, errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "bad") {
		t.Fatalf("node envelope: %q", outBuf.String())
	}
}
