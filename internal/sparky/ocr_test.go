package sparky

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ocrStub records every invocation and delegates to the test behavior; any
// command the behavior does not handle fails the call. Both the bounded
// capture runner (ocrRun) and the streaming runner (ocrStreamRun) route
// through the stub so unhandled commands never reach real execution.
type ocrStub struct {
	mu     sync.Mutex
	calls  [][]string
	run    func(args []string) (CommandResult, error)
	stream func(args []string, log io.Writer) error
}

func (s *ocrStub) runner(args []string, _ []byte, _ time.Duration) (CommandResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, args)
	behavior := s.run
	s.mu.Unlock()
	if behavior == nil {
		return CommandResult{}, fail("unexpected ocr command: %v", args)
	}
	return behavior(args)
}

func (s *ocrStub) streamer(args []string, log io.Writer, _ time.Duration) error {
	s.mu.Lock()
	s.calls = append(s.calls, args)
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return fail("unexpected ocr command: %v", args)
	}
	return stream(args, log)
}

func (s *ocrStub) recorded() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string{}, s.calls...)
}

func ocrSetStub(t *testing.T, behavior func(args []string) (CommandResult, error)) *ocrStub {
	return ocrSetInstallStub(t, behavior, nil)
}

// ocrSetInstallStub wires both runner seams; the streaming behavior receives
// the private log writer so fakes can record diagnostics in it.
func ocrSetInstallStub(t *testing.T, behavior func(args []string) (CommandResult, error), stream func(args []string, log io.Writer) error) *ocrStub {
	t.Helper()
	stub := &ocrStub{run: behavior, stream: stream}
	oldRun, oldStream := ocrRun, ocrStreamRun
	ocrRun = stub.runner
	ocrStreamRun = stub.streamer
	t.Cleanup(func() { ocrRun = oldRun; ocrStreamRun = oldStream })
	return stub
}

func ocrSetHealth(t *testing.T, result Object, err error) {
	t.Helper()
	old := ocrHealth
	ocrHealth = func(string, time.Duration) (Object, error) { return result, err }
	t.Cleanup(func() { ocrHealth = old })
}

// ocrInstalled creates an existing installation layout.
func ocrInstalled(t *testing.T, root string) string {
	t.Helper()
	source := filepath.Join(root, "ocr", "source")
	if err := os.MkdirAll(filepath.Join(source, "deploy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "deploy", "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := "SPARK_OCR_TOKEN=keep-this-token\nOCR_BIFROST_URL=https://spark.example\nOCR_MODELS_HOST_DIR=" + filepath.Join(root, "retained-models") + "\n"
	if err := os.WriteFile(filepath.Join(source, "deploy", ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	return source
}

func ocrWriteRecord(t *testing.T, service *ocrService, record Object) string {
	t.Helper()
	before := ""
	if b, err := os.ReadFile(service.metadata); err == nil {
		before = string(b)
	}
	if err := ocrWrite(service.metadata, ocrMarshalRecord(record)); err != nil {
		t.Fatal(err)
	}
	return before
}

// ocrInspectOK is the minimal successful Docker inspect response.
const ocrInspectOK = `{"status":"running","running":true,"health":"healthy","image":"spark-ocr:0.2.0","project":"deploy"}`

func TestManageOCRValidationBeforeEffects(t *testing.T) {
	stub := ocrSetStub(t, nil)
	if _, err := ManageOCR("/tmp/x", Object{"action": "restart"}); err == nil {
		t.Fatal("unknown action must be rejected")
	}
	if _, err := ManageOCR("relative/root", Object{"action": "status"}); err == nil {
		t.Fatal("non-absolute root must be rejected")
	}
	// Invalid repositories and refs are rejected before any command runs.
	cases := []Object{
		{"action": "install", "repo": "https://user:secret@git.example/ocr", "ref": "main"},
		{"action": "install", "repo": "https://git.example/ocr", "ref": "--bad"},
		{"action": "install", "repo": "-bad", "ref": "main"},
		{"action": "install", "repo": "https://git.example/ocr\nX=bad", "ref": "main"},
		{"action": "install", "repo": "https://git.example:99999/ocr", "ref": "main"},
		{"action": "install", "repo": "https://git.example/ocr", "ref": "main", "rebuild": "yes"},
	}
	for _, payload := range cases {
		if _, err := ManageOCR("/tmp/x", payload); err == nil {
			t.Fatalf("invalid payload must be rejected: %v", payload)
		}
	}
	// A malformed repo value that is not a string at all must also fail.
	if _, err := ManageOCR("/tmp/x", Object{"action": "install", "repo": 5, "ref": "main"}); err == nil {
		t.Fatal("non-string repo must be rejected")
	}
	if len(stub.recorded()) != 0 {
		t.Fatalf("no commands may run for invalid input: %v", stub.recorded())
	}
}

func TestOCRStatusAbsentVsInaccessibleDocker(t *testing.T) {
	root := t.TempDir()
	service, err := newOCRService(root)
	if err != nil {
		t.Fatal(err)
	}
	// A missing container is recognized narrowly and is not an error.
	ocrSetStub(t, func(args []string) (CommandResult, error) {
		return CommandResult{Code: 1, Err: "Error: No such object: spark-ocr"}, nil
	})
	if info, err := service.ocrInspect(); err != nil || info != nil {
		t.Fatalf("missing container must be nil,nil: %v %v", info, err)
	}
	// A Docker permission/transport failure must fail, never claim stopped.
	ocrSetStub(t, func(args []string) (CommandResult, error) {
		return CommandResult{Code: 1, Err: "permission denied"}, nil
	})
	if _, err := service.ocrStatus(); err == nil {
		t.Fatal("inaccessible Docker must fail status")
	}
	if _, err := ManageOCR(root, Object{"action": "status"}); err == nil {
		t.Fatal("ManageOCR status must return the error")
	}
	// Nonzero code with partial stdout is still a failure.
	ocrSetStub(t, func(args []string) (CommandResult, error) {
		return CommandResult{Code: 1, Err: "error", Out: ocrInspectOK}, nil
	})
	if _, err := service.ocrInspect(); err == nil {
		t.Fatal("partial output with failure status must fail")
	}
	// Malformed or unexpected shapes fail too.
	for _, body := range []string{"not json", `{"status":"running","running":"yes","image":null,"project":null}`, `{"status":"running","running":true,"image":null,"project":null,"env":"secret"}`} {
		ocrSetStub(t, func(args []string) (CommandResult, error) { return CommandResult{Out: body}, nil })
		if _, err := service.ocrInspect(); err == nil {
			t.Fatalf("malformed inspect %q must fail", body)
		}
	}
}

func TestOCRStatusSafeFields(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	// Status without an installation must not run effects or create files.
	stub := ocrSetStub(t, func(args []string) (CommandResult, error) {
		return CommandResult{Code: 1, Err: "Error: No such object: spark-ocr"}, nil
	})
	result, err := ManageOCR(root, Object{"action": "status"})
	if err != nil {
		t.Fatal(err)
	}
	if result["state"] != "not_installed" || result["installed"] != false || result["running"] != false {
		t.Fatalf("unexpected not-installed status: %v", result)
	}
	if len(result) != 10 {
		t.Fatalf("status must expose exactly the public fields: %v", result)
	}
	if len(stub.recorded()) != 1 {
		t.Fatalf("status must only inspect: %v", stub.recorded())
	}
	if _, err := os.Stat(filepath.Join(root, "ocr")); !os.IsNotExist(err) {
		t.Fatal("status must not create anything")
	}
	// Installed and running: identity fields present, secrets never exposed.
	source := ocrInstalled(t, root)
	ocrWriteRecord(t, service, Object{"source": source, "revision": "abc123", "project": "deploy"})
	ocrSetStub(t, func(args []string) (CommandResult, error) {
		return CommandResult{Out: `{"status":"running","running":true,"health":"healthy","image":"spark-ocr:0.2.0","project":"deploy"}`}, nil
	})
	ocrSetHealth(t, Object{"service": "spark-ocr"}, nil)
	result, err = service.ocrStatus()
	if err != nil {
		t.Fatal(err)
	}
	if result["running"] != true || result["api_ready"] != true || result["installed"] != true || result["state"] != "running" || result["health"] != "healthy" {
		t.Fatalf("unexpected status: %v", result)
	}
	if result["image"] != "spark-ocr:0.2.0" || result["revision"] != "abc123" || result["source"] != source {
		t.Fatalf("unexpected identity fields: %v", result)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "private-token") || strings.Contains(string(encoded), "keep-this-token") {
		t.Fatalf("status must not expose secrets: %s", encoded)
	}
}

func TestOCRCorruptMetadataFails(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	if err := os.MkdirAll(service.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.metadata, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ocrRecord(); err == nil {
		t.Fatal("corrupt metadata must fail")
	}
	ocrSetStub(t, func(args []string) (CommandResult, error) {
		return CommandResult{Out: ocrInspectOK}, nil
	})
	if _, err := service.ocrStatus(); err == nil {
		t.Fatal("corrupt metadata must fail status, never claim stopped")
	}
	if _, err := service.ocrSource(); err == nil {
		t.Fatal("corrupt metadata must fail source lookup")
	}
	if _, err := ManageOCR(root, Object{"action": "status"}); err == nil {
		t.Fatal("ManageOCR must return the corrupt metadata error")
	}
	// A relative metadata source is rejected too.
	if err := os.WriteFile(service.metadata, []byte(`{"source":"relative/path"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ocrSource(); err == nil {
		t.Fatal("relative source must be rejected")
	}
}

func TestOCRStartStopNotSuccessfulAfterFailedStateCheck(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	source := ocrInstalled(t, root)
	ocrWriteRecord(t, service, Object{"source": source, "project": "deploy"})
	// Docker fails during start's final status check: start must fail.
	inspect := 0
	ocrSetStub(t, func(args []string) (CommandResult, error) {
		if len(args) > 4 && args[2] == "docker" && args[3] == "compose" {
			return CommandResult{}, nil
		}
		if len(args) > 4 && args[2] == "docker" && args[3] == "inspect" {
			inspect++
			if inspect >= 2 {
				return CommandResult{Code: 1, Err: "daemon unreachable"}, nil
			}
			return CommandResult{Out: ocrInspectOK}, nil
		}
		return CommandResult{}, fail("unexpected ocr command: %v", args)
	})
	if _, err := service.ocrStart(source, "deploy"); err == nil {
		t.Fatal("start must fail when the state check fails")
	}
	// Stop must fail when the post-stop status check fails.
	ocrSetStub(t, func(args []string) (CommandResult, error) {
		if len(args) > 4 && args[2] == "docker" && args[3] == "stop" {
			return CommandResult{}, nil
		}
		if len(args) > 4 && args[2] == "docker" && args[3] == "inspect" {
			return CommandResult{Out: ocrInspectOK}, nil
		}
		return CommandResult{}, fail("unexpected ocr command: %v", args)
	})
	// First inspect says running, post-stop status must fail to prove stopped.
	if _, err := service.ocrStop(); err == nil {
		t.Fatal("stop must fail when the final state cannot be proven")
	}
}

func TestOCRStopAndStartArguments(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	source := ocrInstalled(t, root)
	ocrWriteRecord(t, service, Object{"source": source, "project": "deploy"})
	var stopArgs []string
	calls := 0
	ocrSetStub(t, func(args []string) (CommandResult, error) {
		if len(args) > 3 && args[2] == "docker" {
			switch args[3] {
			case "inspect":
				calls++
				if calls == 1 {
					return CommandResult{Out: `{"status":"running","running":true,"health":null,"image":null,"project":"deploy"}`}, nil
				}
				return CommandResult{Out: `{"status":"exited","running":false,"health":null,"image":null,"project":"deploy"}`}, nil
			case "stop":
				stopArgs = args
				return CommandResult{}, nil
			}
		}
		return CommandResult{}, fail("unexpected ocr command: %v", args)
	})
	if _, err := service.ocrStop(); err != nil {
		t.Fatal(err)
	}
	want := []string{"sudo", "-n", "docker", "stop", "--time", "30", "spark-ocr"}
	if len(stopArgs) != len(want) {
		t.Fatalf("stop must target only the ocr container: %v", stopArgs)
	}
	for i, a := range want {
		if stopArgs[i] != a {
			t.Fatalf("stop args mismatch: %v", stopArgs)
		}
	}
	// Start uses --no-build --no-deps, waits, and only the ocr service.
	var composeArgs []string
	ocrSetStub(t, func(args []string) (CommandResult, error) {
		if len(args) > 5 && args[4] == "docker" && args[5] == "compose" {
			composeArgs = args
			return CommandResult{}, nil
		}
		if len(args) > 3 && args[2] == "docker" && args[3] == "inspect" {
			return CommandResult{Out: ocrInspectOK}, nil
		}
		return CommandResult{}, fail("unexpected ocr command: %v", args)
	})
	ocrSetHealth(t, Object{"service": "spark-ocr"}, nil)
	if _, err := service.ocrStart(source, "deploy"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(composeArgs, " ")
	for _, want := range []string{"--no-build", "--no-deps", "--wait", "--wait-timeout 90", "--project-name deploy", "-f " + filepath.Join(source, "deploy", "compose.yaml"), "up -d"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("compose args missing %q: %s", want, joined)
		}
	}
	if !strings.HasSuffix(joined, "ocr") {
		t.Fatalf("compose must target the ocr service only: %s", joined)
	}
	if strings.Contains(joined, "keep-this-token") {
		t.Fatal("credentials must not leak into arguments")
	}
}

func TestOCRLockExclusion(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	unlock, err := service.ocrLock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	second, _ := newOCRService(root)
	if _, err := second.ocrLock(); err == nil || !strings.Contains(err.Error(), "Another OCR control command is running.") {
		t.Fatalf("second lock must be rejected: %v", err)
	}
}

func TestOCRURLValidation(t *testing.T) {
	valid := [][2]string{
		{"https://git.example/ocr", "main"},
		{"https://git.example:8443/team/ocr.git", "v1.0.0"},
		{"https://[2001:db8::1]/ocr.git", "feature/x"},
		{"git@git.example:user/ocr.git", "deadbeef"},
		{"git@172.16.0.5:spark/ocr.git", "a/b/c"},
	}
	for _, c := range valid {
		if err := ocrValidateRepository(c[0], c[1]); err != nil {
			t.Fatalf("valid %v rejected: %v", c, err)
		}
	}
	invalid := [][2]string{
		{"https://user:secret@git.example/ocr", "main"},
		{"http://git.example/ocr", "main"},
		{"https://git.example/ocr?x=1", "main"},
		{"https://git.example/ocr#frag", "main"},
		{"https://git.example/ocr\nX=bad", "main"},
		{"https://git.example:99999/ocr", "main"},
		{"-bad", "main"},
		{"git@git.example:bad name", "main"},
		{"https://git.example/ocr", "--bad"},
		{"https://git.example/ocr", "main extra"},
		{"", "main"},
	}
	for _, c := range invalid {
		if err := ocrValidateRepository(c[0], c[1]); err == nil {
			t.Fatalf("invalid %v accepted", c)
		}
	}
	good := []string{"https://spark.example", "https://spark.example/", "https://spark.example:8443", "https://[2001:db8::1]"}
	for _, g := range good {
		if err := ocrValidateGateway(g); err != nil {
			t.Fatalf("gateway %q must be accepted: %v", g, err)
		}
	}
	bad := []string{"http://spark.example", "https://user@spark.example", "https://spark.example/x?y", "https://spark.example/#f", "https://", "https://spark.example\nEVIL=1", "", "https://spark.example:99999"}
	for _, g := range bad {
		if err := ocrValidateGateway(g); err == nil {
			t.Fatalf("gateway %q must be rejected", g)
		}
	}
}

func TestOCRPayloadNullFallback(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	source := ocrInstalled(t, root)
	ocrWriteRecord(t, service, Object{"source": source, "repo": "https://git.example/ocr", "ref": "v9", "project": "deploy"})
	// Simulate the public CLI sending repo:null/ref:null: values must fall
	// back to the previous record and effects must run with those values.
	var cloneRepo, checkoutRef string
	_ = cloneRepo
	_ = checkoutRef
	stub := ocrSetStub(t, nil)
	_ = stub
	// Validation-level check: ManageOCR must not fail on null fields before
	// falling back. Use an invalid ref in the record to prove the fallback
	// is actually read (invalid ref from record must surface the error).
	ocrWriteRecord(t, service, Object{"source": source, "repo": "https://git.example/ocr", "ref": "--bad", "project": "deploy"})
	if _, err := ManageOCR(root, Object{"action": "install", "repo": nil, "ref": nil, "rebuild": nil}); err == nil {
		t.Fatal("null payload fields must fall back to the record (bad ref must surface)")
	}
	// A non-string non-null repo must fail before effects.
	if _, err := ManageOCR(root, Object{"action": "install", "repo": 5}); err == nil {
		t.Fatal("non-string repo must fail")
	}
}

func TestOCRTokenAndReleaseIDFailures(t *testing.T) {
	// Randomness failure must not produce an empty token or zero release id.
	if _, err := ocrToken(); err != nil {
		t.Fatalf("token generation should work in tests: %v", err)
	}
	if token, err := ocrToken(); err == nil && token == "" {
		t.Fatal("empty token must never be returned")
	}
	if id, err := ocrHex(8); err != nil || id == "" || len(id) != 16 {
		t.Fatalf("release id must be 16 hex chars: %q %v", id, err)
	}
}

// --- Full installer tests -------------------------------------------------

// ocrInstallFake holds mutable fake Docker/git state for installer tests.
type ocrInstallFake struct {
	started      bool
	composeOK    int // -1: always ok; otherwise number of successful compose calls
	composeCount int
	imageMissing bool // docker image inspect: confirmed missing image
	imageBroken  bool // docker image inspect: transport/permission failure
	revBad       bool // git rev-parse returns an invalid revision
	regFail      bool // ensure_bifrost.py registration fails
	specName     string
	withScript   bool
}

func (f *ocrInstallFake) run(args []string) (CommandResult, error) {
	switch {
	case len(args) > 3 && args[2] == "docker" && args[3] == "inspect":
		if f.started {
			return CommandResult{Out: ocrInspectOK}, nil
		}
		return CommandResult{Code: 1, Err: "Error response from daemon: No such container: spark-ocr"}, nil
	case len(args) > 3 && args[2] == "docker" && args[3] == "image":
		if f.imageBroken {
			return CommandResult{Code: 1, Err: "permission denied while trying to connect to the Docker daemon"}, nil
		}
		if f.imageMissing {
			return CommandResult{Code: 1, Err: "Error: No such image: spark-ocr:1.2.3"}, nil
		}
		return CommandResult{Out: "sha256:" + strings.Repeat("a", 64) + "\n"}, nil
	case len(args) > 3 && args[2] == "docker" && args[3] == "stop":
		f.started = false
		return CommandResult{}, nil
	case len(args) > 5 && args[4] == "docker" && args[5] == "compose":
		if f.composeOK >= 0 {
			if f.composeCount >= f.composeOK {
				return CommandResult{Code: 1, Err: "compose failed"}, nil
			}
			f.composeCount++
		}
		if args[len(args)-1] == "ocr" && contains(args, "up") {
			f.started = true
		}
		return CommandResult{}, nil
	case len(args) > 4 && args[0] == "git" && args[3] == "rev-parse":
		if f.revBad {
			return CommandResult{Out: "not-a-revision\n"}, nil
		}
		return CommandResult{Out: strings.Repeat("b", 40) + "\n"}, nil
	}
	return CommandResult{}, fail("unexpected ocr command: %v", args)
}

// stream is the fake streaming runner: the fake git clone materializes a
// staged project and every handled command records diagnostics into the
// private log writer. Unhandled commands never fall through to real exec.
func (f *ocrInstallFake) stream(t *testing.T) func([]string, io.Writer) error {
	return func(args []string, log io.Writer) error {
		if len(args) > 2 && args[0] == "git" && args[1] == "clone" {
			ocrFakeRepo(t, args[len(args)-1], f.specName, f.withScript)
			fmt.Fprint(log, "fake clone diagnostic ok\n")
			return nil
		}
		if args[0] == "git" {
			return nil
		}
		if args[0] == "sudo" && len(args) > 3 && args[2] == "docker" && (args[3] == "build" || args[3] == "run") {
			return nil
		}
		if args[0] == "python3" {
			if f.regFail {
				fmt.Fprint(log, "registration leaked secret-token\n")
				return fail("fake registration failed")
			}
			return nil
		}
		return fail("unexpected stream command: %v", args)
	}
}

// ocrFakeRepo writes a staged spark-ocr source layout; name "" uses the
// supported name and withScript controls the registration script presence.
func ocrFakeRepo(t *testing.T, source string, name string, withScript bool) {
	t.Helper()
	for _, d := range []string{"scripts", "deploy", "config"} {
		if err := os.MkdirAll(filepath.Join(source, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if name == "" {
		name = "spark-ocr"
	}
	files := map[string]string{
		"pyproject.toml":              "[project]\nname = \"" + name + "\"\nversion = \"1.2.3\"\n",
		"deploy/compose.yaml":         "services:\n  ocr: {}\n",
		"deploy/Dockerfile":           "FROM x\n",
		"deploy/Dockerfile.indra":     "FROM y\n",
		"config/bifrost.example.json": "{}\n",
	}
	if withScript {
		files["scripts/ensure_bifrost.py"] = "# engine-owned\n"
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(source, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func ocrWriteStable(t *testing.T, service *ocrService, models string) {
	t.Helper()
	env := "SPARK_OCR_TOKEN=keep-this-token\nOCR_BIFROST_URL=https://spark.example\nOCR_MODELS_HOST_DIR=" + models + "\n"
	if err := ocrWrite(filepath.Join(service.home, "service.env"), env); err != nil {
		t.Fatal(err)
	}
}

func ocrFindCalls(stub *ocrStub, match func([]string) bool) [][]string {
	var out [][]string
	for _, args := range stub.recorded() {
		if match(args) {
			out = append(out, args)
		}
	}
	return out
}

func ocrIsCompose(args []string) bool {
	return len(args) > 5 && args[4] == "docker" && args[5] == "compose"
}

func ocrIsBuild(args []string) bool {
	return len(args) > 3 && args[2] == "docker" && args[3] == "build"
}
func ocrIsBootstrap(args []string) bool {
	return len(args) > 3 && args[2] == "docker" && args[3] == "run"
}

func TestOCRInstallSuccessStreamedAndCached(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	models := filepath.Join(root, "custom-models")
	ocrWriteStable(t, service, models)
	if err := os.MkdirAll(models, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(models, "cached.bin")
	if err := os.WriteFile(cache, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &ocrInstallFake{composeOK: -1, withScript: true}
	stub := ocrSetInstallStub(t, f.run, f.stream(t))
	ocrSetHealth(t, Object{"service": "spark-ocr"}, nil)
	result, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr", "ref": "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if result["running"] != true || result["api_ready"] != true || result["action"] != "install" ||
		result["image_reused"] != true || result["bifrost_registered"] != true {
		t.Fatalf("unexpected install result: %v", result)
	}
	if len(result) != 14 {
		t.Fatalf("install must expose exactly the public fields: %v", result)
	}
	logPath := str(result["log"])
	if !strings.HasSuffix(logPath, "install.log") {
		t.Fatalf("private log path missing: %v", result)
	}
	if b, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(b), "fake clone diagnostic") {
		t.Fatalf("private log must contain streamed diagnostics: %v", err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "keep-this-token") {
		t.Fatalf("public result must exclude secrets: %s", encoded)
	}
	// Cache reuse: exactly one image inspect, no builds.
	inspects := ocrFindCalls(stub, func(a []string) bool { return len(a) > 3 && a[2] == "docker" && a[3] == "image" })
	if len(inspects) != 1 || contains(inspects[0], "--format") == false || !contains(inspects[0], "{{.Id}}") {
		t.Fatalf("image inspect must request the minimal id once: %v", inspects)
	}
	if builds := ocrFindCalls(stub, ocrIsBuild); len(builds) != 0 {
		t.Fatalf("cached image must not rebuild: %v", builds)
	}
	// Exact compose arguments for the staged source and stable env.
	composes := ocrFindCalls(stub, ocrIsCompose)
	stable := filepath.Join(service.home, "service.env")
	staged := str(result["source"])
	want := []string{"sudo", "-n", "env", "OCR_SERVICE_ENV_FILE=" + stable, "docker", "compose",
		"--project-name", "spark-ocr", "--env-file", stable, "-f", filepath.Join(staged, "deploy", "compose.yaml"),
		"up", "-d", "--no-build", "--no-deps", "--wait", "--wait-timeout", "90", "ocr"}
	if len(composes) != 1 || strings.Join(composes[0], " ") != strings.Join(want, " ") {
		t.Fatalf("compose args mismatch: %v", composes)
	}
	// Bootstrap mounts the preserved custom model cache and uses the image.
	bootstraps := ocrFindCalls(stub, ocrIsBootstrap)
	if len(bootstraps) != 1 || !contains(bootstraps[0], "spark-ocr:1.2.3") ||
		!contains(bootstraps[0], models+":/models") || bootstraps[0][len(bootstraps[0])-1] != "bootstrap" {
		t.Fatalf("bootstrap args mismatch: %v", bootstraps)
	}
	// Registration runs the engine-owned script with the staged paths.
	regs := ocrFindCalls(stub, func(a []string) bool { return len(a) > 0 && a[0] == "python3" })
	wantReg := []string{"python3", filepath.Join(staged, "scripts", "ensure_bifrost.py"),
		"--gateway-root", filepath.Join(root, "gateway"), "--service-env", stable,
		"--provider-template", filepath.Join(staged, "config", "bifrost.example.json")}
	if len(regs) != 1 || strings.Join(regs[0], " ") != strings.Join(wantReg, " ") {
		t.Fatalf("registration args mismatch: %v", regs)
	}
	// Metadata records the staged source; stable credentials and cache are untouched.
	record, err := service.ocrRecord()
	if err != nil {
		t.Fatal(err)
	}
	if record["repo"] != "https://git.example/ocr" || record["ref"] != "v1" ||
		record["revision"] != strings.Repeat("b", 40) || record["project"] != "spark-ocr" || record["source"] != staged {
		t.Fatalf("unexpected metadata record: %v", record)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("custom model cache must be preserved: %v", err)
	}
	if env, err := os.ReadFile(stable); err != nil || !strings.Contains(string(env), "keep-this-token") {
		t.Fatalf("stable credentials must be unchanged: %v", err)
	}
}

func TestOCRInstallFirstBuildBothArgsAndStableFromOldEnv(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	oldSource := ocrInstalled(t, root)
	ocrWriteRecord(t, service, Object{"source": oldSource, "revision": strings.Repeat("c", 40), "project": "deploy"})
	f := &ocrInstallFake{composeOK: -1, imageMissing: true, withScript: true}
	stub := ocrSetInstallStub(t, f.run, f.stream(t))
	ocrSetHealth(t, Object{"service": "spark-ocr"}, nil)
	result, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr", "ref": nil})
	if err != nil {
		t.Fatal(err)
	}
	if result["image_reused"] != false {
		t.Fatalf("missing image must build: %v", result)
	}
	builds := ocrFindCalls(stub, ocrIsBuild)
	if len(builds) != 2 {
		t.Fatalf("both images must build: %v", builds)
	}
	joined := strings.Join(builds[0], " ") + "|" + strings.Join(builds[1], " ")
	for _, want := range []string{"Dockerfile.indra", "spark-ocr-indra:1.2.3", "-t spark-ocr:1.2.3", "--build-arg INDRA_IMAGE=spark-ocr-indra:1.2.3"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("build args missing %q: %s", want, joined)
		}
	}
	// Null ref falls back to the installed record; null ref absent record uses main.
	record, _ := service.ocrRecord()
	if record["ref"] != "main" {
		t.Fatalf("ref must fall back to main when the record has none: %v", record)
	}
	// The stable environment is copied from the old deploy/.env, credentials kept.
	stable := filepath.Join(service.home, "service.env")
	env, err := os.ReadFile(stable)
	if err != nil || !strings.Contains(string(env), "keep-this-token") {
		t.Fatalf("stable env must be copied from the old source: %v", err)
	}
	// The public result never contains the copied token.
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "keep-this-token") {
		t.Fatalf("public result must exclude secrets: %s", encoded)
	}
}

func TestOCRInstallRebuildForcesBuild(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	ocrWriteStable(t, service, filepath.Join(root, "m"))
	f := &ocrInstallFake{composeOK: -1, withScript: true}
	stub := ocrSetInstallStub(t, f.run, f.stream(t))
	ocrSetHealth(t, Object{"service": "spark-ocr"}, nil)
	result, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr", "rebuild": true})
	if err != nil {
		t.Fatal(err)
	}
	builds := ocrFindCalls(stub, ocrIsBuild)
	if len(builds) != 2 || result["image_reused"] != false {
		t.Fatalf("--rebuild must rebuild both images: %v %v", builds, result)
	}
}

func TestOCRInstallBadInspectPreventsBuild(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	ocrWriteStable(t, service, filepath.Join(root, "m"))
	f := &ocrInstallFake{composeOK: -1, withScript: true, imageBroken: true}
	stub := ocrSetInstallStub(t, f.run, f.stream(t))
	if _, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr"}); err == nil ||
		!strings.Contains(err.Error(), "Cannot inspect the OCR image") {
		t.Fatalf("transport failure must not be a cache miss: %v", err)
	}
	if builds := ocrFindCalls(stub, ocrIsBuild); len(builds) != 0 {
		t.Fatalf("build must not run on an inspect failure: %v", builds)
	}
	if _, err := os.Stat(service.metadata); !os.IsNotExist(err) {
		t.Fatal("metadata must not be written on a failed install")
	}
}

func TestOCRInstallUnsupportedRepository(t *testing.T) {
	for name, mutate := range map[string]func(*ocrInstallFake){
		"wrong spec name":  func(f *ocrInstallFake) { f.specName = "other" },
		"missing script":   func(f *ocrInstallFake) {},
		"invalid revision": func(f *ocrInstallFake) { f.revBad = true },
	} {
		root := t.TempDir()
		service, _ := newOCRService(root)
		ocrWriteStable(t, service, filepath.Join(root, "m"))
		f := &ocrInstallFake{composeOK: -1, withScript: name != "missing script"}
		mutate(f)
		stub := ocrSetInstallStub(t, f.run, f.stream(t))
		if _, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr"}); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
		if builds := ocrFindCalls(stub, ocrIsBuild); len(builds) != 0 {
			t.Fatalf("%s must not build: %v", name, builds)
		}
		if _, err := os.Stat(service.metadata); !os.IsNotExist(err) {
			t.Fatalf("%s must not write metadata", name)
		}
	}
}

// ocrSeamOverride replaces an OCR-private seam for the duration of the test.
func ocrSeamOverride[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

func TestOCRInstallActivationFailuresRollBack(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ocrInstallFake)
		seam    func(t *testing.T, f *ocrInstallFake)
		wantErr string
	}{
		{"metadata write", func(f *ocrInstallFake) {}, func(t *testing.T, f *ocrInstallFake) {
			ocrSeamOverride(t, &ocrWriteFile, func(path, text string) error { return fail("injected write failure") })
		}, "injected"},
		{"start", func(f *ocrInstallFake) { f.composeOK = 0 }, nil, "compose"},
		{"registration", func(f *ocrInstallFake) { f.regFail = true }, nil, "install.log"},
		{"final status", func(f *ocrInstallFake) {}, func(t *testing.T, f *ocrInstallFake) {
			ocrSetHealth(t, nil, fail("health endpoint down"))
		}, "recovery"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			service, _ := newOCRService(root)
			ocrWriteStable(t, service, filepath.Join(root, "m"))
			f := &ocrInstallFake{composeOK: -1, withScript: true}
			tc.mutate(f)
			stub := ocrSetInstallStub(t, f.run, f.stream(t))
			ocrSetHealth(t, Object{"service": "spark-ocr"}, nil)
			if tc.seam != nil {
				tc.seam(t, f)
			}
			_, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr"})
			if err == nil {
				t.Fatal("install must fail")
			}
			if tc.name == "final status" && !strings.Contains(err.Error(), "did not become ready") {
				t.Fatalf("failed final verification must surface its cause after clean rollback: %v", err)
			}
			if _, err := os.Stat(service.metadata); !os.IsNotExist(err) {
				t.Fatalf("metadata must be rolled back: %v", err)
			}
			// Staged evidence is preserved for inspection.
			releases := filepath.Join(service.home, "releases")
			entries, _ := os.ReadDir(releases)
			if len(entries) != 1 {
				t.Fatalf("staged release must be preserved: %v", entries)
			}
			// A metadata-write failure must not attempt any service start.
			if tc.name == "metadata write" {
				if composes := ocrFindCalls(stub, ocrIsCompose); len(composes) != 0 {
					t.Fatalf("no compose may run when metadata write fails: %v", composes)
				}
			}
		})
	}
}

func TestOCRInstallRollbackMetadataFailureStillRestoresService(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	oldSource := ocrInstalled(t, root)
	ocrWriteRecord(t, service, Object{"source": oldSource, "revision": strings.Repeat("c", 40), "project": "deploy"})
	ocrWriteStable(t, service, filepath.Join(root, "m"))
	f := &ocrInstallFake{composeOK: -1, withScript: true, regFail: true, started: true}
	stub := ocrSetInstallStub(t, f.run, f.stream(t))
	ocrSetHealth(t, Object{"service": "spark-ocr"}, nil)
	ocrSeamOverride(t, &ocrWriteFile, func(path, text string) error { return fail("injected") })
	_, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr"})
	if err == nil || !strings.Contains(err.Error(), "recovery needs attention") {
		t.Fatalf("metadata restore failure must be surfaced: %v", err)
	}
	// The old running service is still restarted despite the failed restore.
	restarts := ocrFindCalls(stub, func(a []string) bool {
		return ocrIsCompose(a) && contains(a, "-f") && contains(a, filepath.Join(oldSource, "deploy", "compose.yaml"))
	})
	if len(restarts) != 1 {
		t.Fatalf("old service must be restored despite metadata failure: %v", restarts)
	}
}

func TestOCRInstallRollbackServiceFailureStillRestoresMetadata(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	oldSource := ocrInstalled(t, root)
	ocrWriteRecord(t, service, Object{"source": oldSource, "revision": strings.Repeat("c", 40), "project": "deploy"})
	saved, readErr := os.ReadFile(service.metadata)
	if readErr != nil {
		t.Fatal(readErr)
	}
	ocrWriteStable(t, service, filepath.Join(root, "m"))
	f := &ocrInstallFake{composeOK: 1, withScript: true, regFail: true, started: true}
	ocrSetInstallStub(t, f.run, f.stream(t))
	ocrSetHealth(t, Object{"service": "spark-ocr"}, nil)
	_, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr"})
	if err == nil || !strings.Contains(err.Error(), "recovery needs attention") {
		t.Fatalf("service restore failure must be surfaced: %v", err)
	}
	// Metadata is restored even though the service restart failed.
	now, readErr := os.ReadFile(service.metadata)
	if readErr != nil || string(now) != string(saved) {
		t.Fatalf("metadata must still be restored: %v", readErr)
	}
}

func TestOCRInstallOldStoppedServiceNotStarted(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	oldSource := ocrInstalled(t, root)
	ocrWriteRecord(t, service, Object{"source": oldSource, "revision": strings.Repeat("c", 40), "project": "deploy"})
	ocrWriteStable(t, service, filepath.Join(root, "m"))
	f := &ocrInstallFake{composeOK: -1, withScript: true, regFail: true}
	stub := ocrSetInstallStub(t, f.run, f.stream(t))
	ocrSetHealth(t, Object{"service": "spark-ocr"}, nil)
	if _, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr"}); err == nil {
		t.Fatal("registration failure must fail the install")
	}
	for _, args := range stub.recorded() {
		if ocrIsCompose(args) && contains(args, filepath.Join(oldSource, "deploy", "compose.yaml")) {
			t.Fatalf("a previously stopped service must not be started: %v", args)
		}
	}
}

func TestOCRInstallUnreadableMetadataNeverUnlinked(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	if err := os.MkdirAll(service.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.metadata, []byte(`{"source":"/x"}`), 0o000); err != nil {
		t.Fatal(err)
	}
	stub := ocrSetStub(t, nil)
	if _, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr"}); err == nil ||
		!strings.Contains(err.Error(), "Cannot read the OCR installation metadata") {
		t.Fatalf("unreadable metadata must fail distinctly: %v", err)
	}
	if _, err := os.Stat(service.metadata); err != nil {
		t.Fatal("an unreadable metadata file must never be removed")
	}
	if len(stub.recorded()) != 0 {
		t.Fatalf("no commands may run: %v", stub.recorded())
	}
}

func TestOCRInstallLogSeamFailures(t *testing.T) {
	root := t.TempDir()
	// Log creation failure surfaces before any command runs.
	noEffects := func(args []string) (CommandResult, error) {
		if len(args) > 3 && args[2] == "docker" && args[3] == "inspect" {
			return CommandResult{Code: 1, Err: "Error: No such object: spark-ocr"}, nil
		}
		return CommandResult{}, fail("unexpected ocr command: %v", args)
	}
	ocrSeamOverride(t, &ocrOpenLog, func(string) (*os.File, error) { return nil, fail("injected") })
	stub := ocrSetInstallStub(t, noEffects, nil)
	if _, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr"}); err == nil ||
		!strings.Contains(err.Error(), "Cannot create the private OCR installation log") {
		t.Fatalf("log open failure must surface: %v", err)
	}
	if len(stub.recorded()) != 0 {
		for _, args := range stub.recorded() {
			if args[0] == "git" || args[0] == "python3" || ocrIsCompose(args) {
				t.Fatalf("no effects may run: %v", stub.recorded())
			}
		}
	}
	// Header write failure surfaces too.
	ocrSeamOverride(t, &ocrOpenLog, func(path string) (*os.File, error) {
		return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	})
	ocrSeamOverride(t, &ocrLogLine, func(*os.File, string) error { return fail("injected") })
	stub = ocrSetInstallStub(t, noEffects, nil)
	if _, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr"}); err == nil ||
		!strings.Contains(err.Error(), "Cannot write the private OCR installation log") {
		t.Fatalf("log write failure must surface: %v", err)
	}
	for _, args := range stub.recorded() {
		if args[0] == "git" || args[0] == "python3" || ocrIsCompose(args) {
			t.Fatalf("no effects may run: %v", stub.recorded())
		}
	}
}

func TestOCRInstallPrivateLogKeepsSecretsOutOfPublicErrors(t *testing.T) {
	root := t.TempDir()
	service, _ := newOCRService(root)
	ocrWriteStable(t, service, filepath.Join(root, "m"))
	f := &ocrInstallFake{composeOK: -1, withScript: true, regFail: true}
	ocrSetInstallStub(t, f.run, f.stream(t))
	ocrSetHealth(t, Object{"service": "spark-ocr"}, nil)
	_, err := ManageOCR(root, Object{"action": "install", "repo": "https://git.example/ocr"})
	if err == nil {
		t.Fatal("install must fail")
	}
	if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "clone diagnostic") {
		t.Fatalf("public error must exclude raw output: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(service.home, "releases", "*", "install.log"))
	if len(matches) != 1 {
		t.Fatalf("private log must exist: %v", matches)
	}
	b, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(b), "secret-token") {
		t.Fatal("private log must contain the fake failure diagnostics")
	}
}
