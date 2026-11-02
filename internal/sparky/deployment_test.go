package sparky

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// deploymentConfig is the minimal cluster fixture for the deployment tests.
func deploymentConfig(root string) Object {
	return Object{
		"root": root,
		"nodes": Object{
			"spark01": Object{},
			"spark02": Object{"ssh": "spark02"},
		},
	}
}

type deploymentFakeRun struct {
	mu     sync.Mutex
	calls  []string
	inputs [][]byte
	script func(args []string, input []byte) (CommandResult, error)
}

func (f *deploymentFakeRun) run(_ context.Context, args []string, input []byte) (CommandResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(args, " "))
	f.inputs = append(f.inputs, append([]byte{}, input...))
	f.mu.Unlock()
	if f.script != nil {
		return f.script(args, input)
	}
	return CommandResult{Code: 0}, nil
}

func (f *deploymentFakeRun) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

func (f *deploymentFakeRun) inputAt(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inputs[i]
}

func deploymentFake(t *testing.T) *deploymentFakeRun {
	t.Helper()
	fake := &deploymentFakeRun{}
	prev := deploymentRun
	deploymentRun = fake.run
	t.Cleanup(func() { deploymentRun = prev })
	return fake
}

// deploymentFastClocks shrinks the wait cadences so tests run on real local
// httptest servers without sleeping on the production clocks.
func deploymentFastClocks(t *testing.T) {
	t.Helper()
	prevPoll, prevIdle := httpWaitPoll, comfyIdlePoll
	httpWaitPoll, comfyIdlePoll = time.Millisecond, time.Millisecond
	t.Cleanup(func() { httpWaitPoll, comfyIdlePoll = prevPoll, prevIdle })
}

// journalFake is the journal worker fake: the captured command run plus the
// isolated canonical temporary tree that backs the fake worker's real
// protocol state. The head fixture and the worker state never share files.
type journalFake struct {
	*deploymentFakeRun
	root   string
	worker string
}

// journalWorkerPath maps one head path to its isolated worker location for
// one node: unit paths under /etc/systemd/system and files inside the head
// root each map into that node's own worker tree, so the fake never writes
// /etc, the user home, or the head fixture itself, and one node's committed
// state is never visible to another node's protocol traffic. Anything else
// is refused.
func journalWorkerPath(worker, root, node, path string) (string, bool) {
	if path == "/etc/systemd/system" || strings.HasPrefix(path, "/etc/systemd/system/") {
		return filepath.Join(worker, node, "etc", "systemd", "system", filepath.Base(path)), true
	}
	if rel, err := filepath.Rel(root, path); err == nil && rel != "." &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.Join(worker, node, "files", rel), true
	}
	return "", false
}

// journalWorkerNode derives the node from the dispatched transport argv:
// local root commands run under sudo on spark01, worker commands run under
// ssh with the pinned flag layout "-o x -o y <host> ...", so the host alias
// is the fifth argument.
func journalWorkerNode(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if args[0] == "sudo" {
		return "spark01"
	}
	if args[0] == "ssh" && len(args) >= 6 {
		return args[5]
	}
	return ""
}

// journalWorkerFake fakes deploymentRun with real isolated protocol state:
// every captured command runs the actual node file protocol against a
// canonical temporary worker tree. Head root unit commands (sudo) and worker
// transport commands (ssh) are mapped into that tree; any other command
// fails, so unexpected effects can never pass silently. The payload never
// appears on argv.
func journalWorkerFake(t *testing.T, root string) *journalFake {
	t.Helper()
	fake := &journalFake{deploymentFakeRun: &deploymentFakeRun{}, root: root, worker: canonicalTempDir(t)}
	fake.script = func(args []string, input []byte) (CommandResult, error) {
		meta, err := decode(input)
		if err != nil {
			return CommandResult{Code: 2}, nil
		}
		if len(args) == 0 || (args[0] != "sudo" && args[0] != "ssh") {
			return CommandResult{Code: 2}, nil
		}
		node := journalWorkerNode(args)
		if node != "spark01" && node != "spark02" {
			return CommandResult{Code: 2}, nil
		}
		switch op := str(meta["operation"]); op {
		case "inspect", "write", "remove", "":
		default:
			return CommandResult{Code: 2}, nil
		}
		mapped, ok := journalWorkerPath(fake.worker, root, node, str(meta["path"]))
		if !ok {
			return CommandResult{Code: 2}, nil
		}
		meta["path"] = mapped
		result, err := nodeWriteFile(bytes.NewReader(jsonBytes(meta)))
		if err != nil {
			return CommandResult{Code: 1}, nil
		}
		return CommandResult{Code: 0, Out: string(jsonBytes(result))}, nil
	}
	prev := deploymentRun
	deploymentRun = fake.run
	t.Cleanup(func() { deploymentRun = prev })
	return fake
}

// TestWaitComfyIdleThreeConfirmedSamples covers the idle rule: exactly three
// consecutive confirmed idle arrays; missing, null, wrong-type, or failed
// samples reset the count.
func TestWaitComfyIdleThreeConfirmedSamples(t *testing.T) {
	deploymentFastClocks(t)
	idle := `{"queue_running":[],"queue_pending":[]}`
	bad := []string{
		`{"queue_running":null,"queue_pending":[]}`,
		`{"queue_pending":[]}`,
		`{}`,
		`{"queue_running":[1],"queue_pending":[]}`,
		`{"queue_running":[],"queue_pending":"many"}`,
		`not json`,
	}
	for _, shape := range bad {
		var n int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n++
			if n <= 2 {
				_, _ = w.Write([]byte(idle))
				return
			}
			_, _ = w.Write([]byte(shape))
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if err := waitComfyIdle(ctx, server.URL, 5*time.Second); err == nil {
			server.Close()
			t.Fatalf("a non-idle sample must reset the count: %s", shape)
		}
		server.Close()
	}
	// Three confirmed idle samples in a row succeed.
	var n int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		_, _ = w.Write([]byte(idle))
	}))
	defer server.Close()
	if err := waitComfyIdle(context.Background(), server.URL, 10*time.Second); err != nil {
		t.Fatalf("confirmed idle must succeed: %v", err)
	}
	if n != 3 {
		t.Fatalf("exactly three samples expected: %d", n)
	}
	// A persistent HTTP failure is never idle.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer broken.Close()
	if err := waitComfyIdle(context.Background(), broken.URL, 2*time.Second); err == nil {
		t.Fatal("HTTP failures must never read as idle")
	}
	// Cancellation surfaces as an error, not idle.
	held := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(idle))
	}))
	defer held.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitComfyIdle(ctx, held.URL, time.Minute); err == nil {
		t.Fatal("a cancelled wait must not report idle")
	}
}

// TestDrainComfyUsesDrainEndpointThenQueue covers the exported drain: the
// /sparky/drain attempt is best effort and idle comes only from the queue.
func TestDrainComfyUsesDrainEndpointThenQueue(t *testing.T) {
	deploymentFastClocks(t)
	var drains, queues int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/sparky/drain":
			drains++
			w.WriteHeader(http.StatusNotFound) // engine without the endpoint
		case r.Method == "GET" && r.URL.Path == "/queue":
			queues++
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	dir := canonicalTempDir(t)
	file := filepath.Join(dir, "node.json")
	if err := os.WriteFile(file, []byte(`{"comfy_url":"`+server.URL+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Unbounded exported drain: bound the queue wait through the fixture by
	// using a context-aware call with a generous timeout instead.
	if err := drainComfy(context.Background(), file); err != nil {
		t.Fatalf("drain failed: %v", err)
	}
	if drains != 1 || queues < 3 {
		t.Fatalf("drains=%d queues=%d", drains, queues)
	}
	// A node file without an address is refused.
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DrainComfy(empty); err == nil {
		t.Fatal("a node file without comfy_url must be refused")
	}
	if err := DrainComfy(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("a missing node file must be refused")
	}
}

// TestWaitHTTPBoundedAndContextAware covers readiness waits.
func TestWaitHTTPBoundedAndContextAware(t *testing.T) {
	deploymentFastClocks(t)
	var n int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ready":true}`))
	}))
	defer server.Close()
	got, err := waitHTTP(context.Background(), server.URL+"/health", 10*time.Second)
	if err != nil || !got["ready"].(bool) {
		t.Fatalf("readiness: %v %v", got, err)
	}
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer dead.Close()
	if _, err := waitHTTP(context.Background(), dead.URL, 2*time.Second); err == nil {
		t.Fatal("a timeout must surface, never read as ready")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waitHTTP(ctx, dead.URL, time.Minute); err == nil {
		t.Fatal("a cancelled wait must error")
	}
}

// deploymentValidConfig is a full valid cluster fixture: it passes the real
// central validation, unlike the minimal journal fixture, so tests that
// persist a cluster file never weaken the validator.
func deploymentValidConfig(root string) Object {
	c := baseConfig()
	c["root"] = root
	obj(c["nodes"])["spark02"] = Object{"ssh": "spark02"}
	return c
}

// TestRuntimeActivityIdle uses the concrete Runtime with a fake Runner: the
// native lifecycle is instantly idle, and a failing activity query is an
// error, never idle.
func TestRuntimeActivityIdle(t *testing.T) {
	root := canonicalTempDir(t)
	cfg := deploymentValidConfig(root)
	cfg["lifecycle"] = "native"
	path := filepath.Join(root, "models.json")
	if err := writeCluster(path, cfg); err != nil {
		t.Fatal(err)
	}
	rt := newRuntime(cfg, path)
	rt.Run = func(context.Context, []string, []byte) (CommandResult, error) {
		return CommandResult{}, fail("unexpected command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtimeActivityIdle(ctx, rt); err != nil {
		t.Fatalf("native runtime must be idle: %v", err)
	}
	// Legacy runtime with an unreachable manager: the query failure must
	// surface, never read as idle.
	legacy := deploymentValidConfig(root)
	legacy["lifecycle"] = "legacy"
	legacy["manager_url"] = "http://127.0.0.1:1"
	legacyPath := filepath.Join(root, "legacy.json")
	if err := writeCluster(legacyPath, legacy); err != nil {
		t.Fatal(err)
	}
	lrt := newRuntime(legacy, legacyPath)
	lrt.Run = func(context.Context, []string, []byte) (CommandResult, error) {
		return CommandResult{}, fail("unexpected command")
	}
	if err := runtimeActivityIdle(ctx, lrt); err == nil {
		t.Fatal("a failed activity query must never read as idle")
	}
}

// qwenFixtures writes the exact expected source layouts.
func qwenFixtures(t *testing.T, root string, mutate func(dir string)) {
	t.Helper()
	extra := filepath.Join(root, "extra")
	if err := os.MkdirAll(filepath.Join(extra, "nvidia-recipes"), 0o700); err != nil {
		t.Fatal(err)
	}
	recipe := `{
  "serve": {
    "command": "python3 serve.py --model qwen3.8-flash-next --tensor-parallel-size 1 --max-num-seqs 256 --gpu-memory-utilization 0.9 --quantization fp4 --enable-prefix-caching --mtp-enable"
  }
}`
	source := `import os
` + qwenRankCheck + `recipe = json.load(open(model + ".json"))
env = dict(os.environ)
`
	launcher := `#!/usr/bin/env bash
` + qwenLauncherBlock + qwenMemoryBlock + `cache="$base/extra/cache/$model-fp4-local"
exec /service/serve-nvidia.py
`
	writes := []struct {
		path string
		data string
		mode os.FileMode
	}{
		{filepath.Join(extra, "nvidia-recipes", "qwen3.8-flash-next.json"), recipe, 0o600},
		{filepath.Join(extra, "serve-nvidia.py"), source, 0o644},
		{filepath.Join(extra, "start-local-nvidia.sh"), launcher, 0o755},
	}
	for _, w := range writes {
		if err := os.WriteFile(w.path, []byte(w.data), w.mode); err != nil {
			t.Fatal(err)
		}
	}
	// The drift mutation runs after the source layout exists, so it edits the
	// real files instead of being silently overwritten by the fixture.
	if mutate != nil {
		mutate(extra)
	}
}

// TestPrepareQwenDualGoldenPreservation is the golden test: the pinned
// rewrites, the kept flags, the worker rank and headless mode, the master
// placeholders, and the Restart=no unit without any enable hook.
func TestPrepareQwenDualGoldenPreservation(t *testing.T) {
	root := canonicalTempDir(t)
	cfg := deploymentConfig(root)
	qwenFixtures(t, root, nil)
	fake := journalWorkerFake(t, root)
	stage := filepath.Join(root, "controller", "staging")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := prepareQwenDual(ctx, cfg, stage); err != nil {
		t.Fatalf("prepare failed: %v", err)
	}
	// The dual recipe keeps every original flag and carries the pinned
	// values plus master placeholders.
	recipe, err := readObject(filepath.Join(root, "extra", "nvidia-recipes", "qwen3.8-flash-next-dual.json"))
	if err != nil {
		t.Fatal(err)
	}
	command := str(obj(recipe["serve"])["command"])
	for _, flag := range []string{
		"--quantization fp4", "--enable-prefix-caching", "--mtp-enable",
		"--tensor-parallel-size 2", "--max-num-seqs 4", "--gpu-memory-utilization 0.75",
		"--nnodes 2 --node-rank 0 --master-addr \"$HEAD_IP\" --master-port \"$MASTER_PORT\"",
	} {
		if !strings.Contains(command, flag) {
			t.Fatalf("dual recipe command missing %q: %s", flag, command)
		}
	}
	additional := arr(obj(recipe["serve"])["additional_commands"])
	if len(additional) != 1 {
		t.Fatalf("one worker command expected: %v", additional)
	}
	worker := str(obj(additional[0])["command"])
	if !strings.Contains(worker, "--node-rank 1") || !strings.Contains(worker, "--headless") {
		t.Fatalf("worker command: %s", worker)
	}
	// The engine script keeps the source semantics with the pinned edits.
	source, err := os.ReadFile(filepath.Join(root, "extra", "serve-qwen-dual.py"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`(model + "-dual.json")`, `env["B12X_COMPILE_WORKERS"] = "1"`,
		`env["PYTORCH_CUDA_ALLOC_CONF"] = "expandable_segments:True"`} {
		if !strings.Contains(string(source), fragment) {
			t.Fatalf("engine script missing %q", fragment)
		}
	}
	if strings.Contains(string(source), "spark01 only") {
		t.Fatal("the single-device guard must be removed")
	}
	launcher, err := os.ReadFile(filepath.Join(root, "extra", "start-qwen-dual.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(launcher), "$model-fp4-local-dual") ||
		!strings.Contains(string(launcher), "/service/serve-qwen-dual.py") {
		t.Fatalf("launcher: %s", launcher)
	}
	// The unit is Restart=no with no install section: never enabled.
	for _, call := range fake.all() {
		if strings.Contains(call, "enable") {
			t.Fatalf("no enable command may run: %s", call)
		}
	}
	// Both nodes received the unit through the CAS protocol under sudo:
	// exactly one unit write per node, plus bounded inspect calls.
	unitWrites := 0
	for i := range fake.inputs {
		meta, err := decode(fake.inputAt(i))
		if err != nil {
			continue
		}
		if str(meta["operation"]) == "write" && strings.HasSuffix(str(meta["path"]), "sparky-qwen-dual.service") {
			unitWrites++
		}
	}
	if unitWrites != 2 {
		t.Fatalf("both nodes need exactly one unit write: %v", fake.all())
	}
	// The root worker command must be the absolute quoted helper.
	expectedRoot := shellJoin([]string{"sudo", "-n", journalHelper(), "--json", "node", "write-file"})
	rootSeen := false
	for _, call := range fake.all() {
		if strings.Contains(call, "spark02") && strings.HasSuffix(call, expectedRoot) {
			rootSeen = true
		}
	}
	if !rootSeen {
		t.Fatalf("the root worker command must use the absolute helper: %v", fake.all())
	}
	// The unit text carries Restart=no, no install hook, a colon-prefixed
	// quoted ExecStart, and a bare unquoted User.
	var unitText string
	for i := range fake.inputs {
		meta, err := decode(fake.inputAt(i))
		if err != nil {
			continue
		}
		if !strings.HasSuffix(str(meta["path"]), "sparky-qwen-dual.service") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(str(meta["data_base64"]))
		if err != nil {
			t.Fatalf("unit metadata payload: %v", err)
		}
		unitText = string(raw)
	}
	if unitText == "" {
		t.Fatal("no unit write was captured")
	}
	if !strings.Contains(unitText, "Restart=no") ||
		strings.Contains(unitText, "[Install]") ||
		strings.Contains(unitText, "WantedBy") {
		t.Fatalf("unit text must carry Restart=no and no install hook: %s", unitText)
	}
	if !strings.Contains(unitText, "ExecStart=:\"") {
		t.Fatalf("ExecStart must carry the literal colon prefix: %s", unitText)
	}
	if strings.Contains(unitText, "User=\"") {
		t.Fatalf("User must be a bare unquoted name: %s", unitText)
	}
	if !strings.Contains(unitText, "User="+filepath.Base(home())+"\n") {
		t.Fatalf("unit text must carry the real service user: %s", unitText)
	}
}

// TestPrepareQwenDualLayoutDriftZeroEffects asserts that any changed source
// layout is rejected before a single write or command.
func TestPrepareQwenDualLayoutDriftZeroEffects(t *testing.T) {
	drifts := []func(dir string){
		func(dir string) { // recipe without one pinned flag
			path := filepath.Join(dir, "nvidia-recipes", "qwen3.8-flash-next.json")
			data, _ := os.ReadFile(path)
			_ = os.WriteFile(path, []byte(strings.Replace(string(data), "--max-num-seqs 256", "", 1)), 0o600)
		},
		func(dir string) { // duplicated pinned flag
			path := filepath.Join(dir, "nvidia-recipes", "qwen3.8-flash-next.json")
			data, _ := os.ReadFile(path)
			_ = os.WriteFile(path, []byte(strings.Replace(string(data), "--tensor-parallel-size 1",
				"--tensor-parallel-size 1 --tensor-parallel-size 1", 1)), 0o600)
		},
		func(dir string) { // engine script without the rank guard
			path := filepath.Join(dir, "serve-nvidia.py")
			data, _ := os.ReadFile(path)
			_ = os.WriteFile(path, []byte(strings.Replace(string(data), qwenRankCheck, "", 1)), 0o644)
		},
		func(dir string) { // launcher without the memory block
			path := filepath.Join(dir, "start-local-nvidia.sh")
			data, _ := os.ReadFile(path)
			_ = os.WriteFile(path, []byte(strings.Replace(string(data), qwenMemoryBlock, "", 1)), 0o755)
		},
		func(dir string) { // launcher without the cache line
			path := filepath.Join(dir, "start-local-nvidia.sh")
			data, _ := os.ReadFile(path)
			_ = os.WriteFile(path, []byte(strings.Replace(string(data),
				`cache="$base/extra/cache/$model-fp4-local"`, "cache=other", 1)), 0o755)
		},
	}
	for i, drift := range drifts {
		root := canonicalTempDir(t)
		cfg := deploymentConfig(root)
		qwenFixtures(t, root, drift)
		fake := deploymentFake(t)
		if err := prepareQwenDual(context.Background(), cfg, filepath.Join(root, "staging")); err == nil {
			t.Fatalf("drift %d must be rejected", i)
		}
		// Zero mutations: every dispatched command's actual protocol
		// operation is examined, and no write or remove may run on an
		// invalid layout. A readonly inspect would be tolerable, but the
		// source validates every layout before any dispatch, so zero
		// commands of any kind is the asserted promise.
		for j := range fake.inputs {
			if meta, err := decode(fake.inputAt(j)); err == nil {
				if op := str(meta["operation"]); op == "write" || op == "remove" || op == "" {
					t.Fatalf("drift %d must never mutate: %s", i, fake.all())
				}
			}
		}
		if calls := fake.all(); len(calls) != 0 {
			t.Fatalf("drift %d must have zero effects: %v", i, calls)
		}
		for _, created := range []string{
			filepath.Join(root, "extra", "nvidia-recipes", "qwen3.8-flash-next-dual.json"),
			filepath.Join(root, "extra", "serve-qwen-dual.py"),
			filepath.Join(root, "extra", "start-qwen-dual.sh"),
		} {
			if _, err := os.Lstat(created); !os.IsNotExist(err) {
				t.Fatalf("drift %d must not create %s", i, created)
			}
		}
	}
}

// TestPrepareQwenDualRollbackAttempts covers a partial failure: the already
// written local files are removed or restored and the error surfaces.
func TestPrepareQwenDualRollbackAttempts(t *testing.T) {
	root := canonicalTempDir(t)
	cfg := deploymentConfig(root)
	qwenFixtures(t, root, nil)
	fake := journalWorkerFake(t, root)
	fake.mu.Lock()
	prevScript := fake.script
	fake.script = func(args []string, input []byte) (CommandResult, error) {
		if meta, err := decode(input); err == nil &&
			str(meta["operation"]) == "write" && strings.HasSuffix(str(meta["path"]), "serve-qwen-dual.py") {
			return CommandResult{Code: 1}, nil
		}
		return prevScript(args, input)
	}
	fake.mu.Unlock()
	if err := prepareQwenDual(context.Background(), cfg, filepath.Join(root, "staging")); err == nil {
		t.Fatal("a failed upload must error")
	}
	for _, created := range []string{
		filepath.Join(root, "extra", "nvidia-recipes", "qwen3.8-flash-next-dual.json"),
		filepath.Join(root, "extra", "serve-qwen-dual.py"),
	} {
		if _, err := os.Lstat(created); !os.IsNotExist(err) {
			t.Fatalf("created file must be rolled back: %s", created)
		}
	}
	// The untouched launcher keeps its original bytes and mode.
	launcher, err := os.ReadFile(filepath.Join(root, "extra", "start-local-nvidia.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(launcher), qwenLauncherBlock) {
		t.Fatal("an unwritten file must keep its original content")
	}
	info, err := os.Stat(filepath.Join(root, "extra", "start-local-nvidia.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("source file must be untouched: %v", err)
	}
}
