package sparky

// Stage 2 supervisor tests. Everything is local: the shared backend is a
// simulated listener started BY the fake unit-start (exactly like vLLM
// binding its port), the runner is a loud fake, and the device locks are
// real flock files contended from the same process. No SSH, no services,
// no credentials.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// supEnv wires the frozen-record supervisor against a fully fake world.
type supEnv struct {
	t       *testing.T
	root    string
	path    string
	port    int
	calls   *runnerCalls
	gen     string
	modelID string

	mu         sync.Mutex
	running    map[string]bool
	loadStates map[string]string // unit -> forced LoadState
	started    []string
	stopped    []string
	onStart    map[string]func() error
	onStop     map[string]func() error
	healthHits int
	simLn      net.Listener
}

func newSupEnv(t *testing.T, placement string) *supEnv {
	t.Helper()
	root := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // free again: preflight must see the port refused
	e := &supEnv{
		t: t, root: root, port: port,
		path:       filepath.Join(root, "config", "models.json"),
		calls:      &runnerCalls{},
		running:    map[string]bool{},
		loadStates: map[string]string{},
		onStart:    map[string]func() error{},
		onStop:     map[string]func() error{},
		modelID:    "glm",
	}
	e.writeCentral(placement)
	if err := os.MkdirAll(filepath.Join(root, "controller", "lifecycle"), 0700); err != nil {
		t.Fatal(err)
	}
	return e
}

// writeCentral writes the central cluster file. The per-node unit names are
// distinctive so frozen-record behavior is observable against any later
// central edit.
func (e *supEnv) writeCentral(placement string) {
	// Each settings entry's units must match its own placement; the record
	// freezes the run_on recipe.
	perPlacement := map[string]Object{
		"both":    {"spark01": "sup-a.service", "spark02": "sup-b.service"},
		"spark01": {"spark01": "sup-a.service"},
		"spark02": {"spark02": "sup-b.service"},
	}
	perOrder := map[string][]any{
		"both":    {"spark01", "spark02"},
		"spark01": {"spark01"},
		"spark02": {"spark02"},
	}
	// A REAL installed recipe file on disk with a REAL env binding for
	// context_tokens: the renderer must match every parameter it is given,
	// so the frozen recipe carries a valid binding against an actual file.
	recipeEnv := filepath.Join(e.root, "recipe.env")
	if err := os.WriteFile(recipeEnv, []byte("CTX=0\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	binding := Object{"path": recipeEnv, "format": "env", "fields": Object{"context_tokens": "CTX"}}
	recipe := func(on string) Object {
		bindings := Object{}
		for _, node := range placements[on] {
			bindings[node] = []any{binding}
		}
		return Object{
			"enabled": true, "backend_url": e.backend(), "units": perPlacement[on],
			"context_tokens": 262144, "concurrency": 4, "parameters": Object{},
			"parameter_bindings": bindings,
			"startup_order":      perOrder[on],
		}
	}
	settings := Object{"both": recipe("both"), "spark01": recipe("spark01"), "spark02": recipe("spark02")}
	cfg := Object{
		"version": 1, "root": e.root, "controller_port": 8020, "lifecycle": "native",
		"nodes": Object{"spark01": Object{"direct_ip": "10.0.0.1"}, "spark02": Object{"direct_ip": "10.0.0.2", "ssh": "spark02h"}},
		"policy": Object{"interrupt_active_jobs": false, "protected_models": []any{},
			"comfyui_exclusive_with_llms": true, "comfyui_shared_llms": []any{}, "comfyui_min_available_gib": 18},
		"models": Object{"glm": Object{"kind": "llm", "name": "GLM", "run_on": placement,
			"keep_loaded": false, "reasoning": true, "aliases": []any{}, "settings": settings}},
	}
	if err := os.MkdirAll(filepath.Dir(e.path), 0700); err != nil {
		e.t.Fatal(err)
	}
	if err := writeCluster(e.path, cfg); err != nil {
		e.t.Fatal(err)
	}
}

func (e *supEnv) backend() string { return fmt.Sprintf("http://127.0.0.1:%d", e.port) }

func (e *supEnv) probe() Object {
	return Object{
		"units":        Object{"": "inactive"},
		"comfyui":      Object{"executing": false, "running": 0, "phase": "idle"},
		"memory_bytes": Object{"MemAvailable": 1 << 40},
	}
}

// begin creates the durable queued record from the CURRENT central config
// and remembers its generation.
func (e *supEnv) begin() {
	e.t.Helper()
	cfg, err := readCluster(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	rt := newRuntime(cfg, e.path)
	m := obj(obj(cfg["models"])["glm"])
	s := obj(obj(m["settings"])[str(m["run_on"])])
	nodes := placements[str(m["run_on"])]
	rec, err := rt.BeginOperation("glm", m, s, nodes, str(m["run_on"]))
	if err != nil {
		e.t.Fatal(err)
	}
	e.gen = str(rec["generation"])
	if !isObject(rec["config"]) {
		e.t.Fatal("BeginOperation must store the defensive frozen config snapshot")
	}
}

func (e *supEnv) centralRuntime() *Runtime {
	cfg, err := readCluster(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	return newRuntime(cfg, e.path)
}

// supervise runs the REAL superviseNative entry with the injected factory.
func (e *supEnv) supervise(ctx context.Context, action string) error {
	return superviseNative(ctx, e.path, action, "glm")
}

func (e *supEnv) installFactory() {
	e.t.Helper()
	saved := nativeRuntimeFactory
	e.t.Cleanup(func() { nativeRuntimeFactory = saved })
	nativeRuntimeFactory = func(c Object, p string) *Runtime {
		rt := newRuntime(c, p)
		rt.Run = e.runner()
		rt.ProbeNode = func(Object) (Object, error) { return e.probe(), nil }
		return rt
	}
}

func (e *supEnv) recordState() string {
	e.t.Helper()
	rec, err := e.centralRuntime().readRecord("glm")
	if err != nil {
		e.t.Fatalf("record: %v", err)
	}
	return str(rec["state"])
}

// recordStateRootFallback reads the glm record through the STRICT root-only
// fallback runtime — exactly what production superviseNative uses when the
// central file is unreadable — never through the corrupt central file.
func (e *supEnv) recordStateRootFallback() string {
	e.t.Helper()
	fb, ok := nativeRootFallback(e.path)
	if !ok {
		e.t.Fatal("the fixture path is not a root fallback location")
	}
	rt := newRuntime(fb, e.path)
	rec, err := rt.readRecord("glm")
	if err != nil {
		e.t.Fatalf("record: %v", err)
	}
	return str(rec["state"])
}

func (e *supEnv) isRunning(unit string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[unit]
}

func (e *supEnv) startEffects(unit string) error {
	e.mu.Lock()
	hook := e.onStart[unit]
	e.mu.Unlock()
	if hook != nil {
		if err := hook(); err != nil {
			return err
		}
	}
	e.mu.Lock()
	e.started = append(e.started, unit)
	e.running[unit] = true
	ln := e.simLn
	e.mu.Unlock()
	if ln == nil {
		e.startSim()
	}
	return nil
}

func (e *supEnv) stopEffects(unit string) error {
	e.mu.Lock()
	hook := e.onStop[unit]
	e.mu.Unlock()
	if hook != nil {
		if err := hook(); err != nil {
			return err
		}
	}
	e.mu.Lock()
	e.stopped = append(e.stopped, unit)
	e.running[unit] = false
	idle := true
	for _, r := range e.running {
		if r {
			idle = false
		}
	}
	ln := e.simLn
	if idle {
		e.simLn = nil
	}
	e.mu.Unlock()
	if idle && ln != nil {
		_ = ln.Close()
	}
	return nil
}

// startSim binds the recipe port and serves the exact model id, exactly
// like the real ranks binding vLLM after their start.
func (e *supEnv) startSim() {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(e.port))
	if err != nil {
		return // port taken: awaitReady will simply never succeed
	}
	e.mu.Lock()
	e.simLn = ln
	e.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.healthHits++
		e.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + e.modelID + `","object":"model"}]}`))
	})
	go func() { _ = http.Serve(ln, mux) }()
}

func (e *supEnv) healthHitCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.healthHits
}

// systemctl applies a start/stop to the fake world.
func (e *supEnv) systemctl(action, unit string) (CommandResult, error) {
	switch action {
	case "start":
		if err := e.startEffects(unit); err != nil {
			return CommandResult{}, err
		}
	case "stop":
		if err := e.stopEffects(unit); err != nil {
			return CommandResult{}, err
		}
	default:
		return CommandResult{}, fail("unexpected systemctl action: %s", action)
	}
	return CommandResult{Code: 0}, nil
}

func (e *supEnv) runner() Runner {
	return func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		e.calls.record(args)
		j := strings.Join(args, " ")
		switch {
		case args[0] == "systemctl" && args[1] == "show" && strings.Contains(j, "--value"):
			return CommandResult{Out: "active\n", Code: 0}, nil
		case args[0] == "systemctl" && args[1] == "show":
			unit := args[2]
			e.mu.Lock()
			load := e.loadStates[unit]
			active := "inactive"
			if e.running[unit] {
				active = "active"
			}
			e.mu.Unlock()
			if load == "" {
				load = "loaded"
			}
			return CommandResult{Out: "LoadState=" + load + "\nActiveState=" + active + "\n", Code: 0}, nil
		case args[0] == "systemctl" && args[1] == "is-active":
			if e.isRunning(args[len(args)-1]) {
				return CommandResult{Code: 0}, nil
			}
			return CommandResult{Code: 3}, nil
		case args[0] == "sudo" && len(args) == 5 && args[2] == "systemctl":
			return e.systemctl(args[3], args[4])
		case args[0] == "ssh":
			// shellJoin single-quotes every argument, so the joined remote
			// command must be unquoted before substring matching; otherwise
			// even a correctly issued stop command is never dispatched.
			last := strings.ReplaceAll(args[len(args)-1], "'", "")
			switch {
			case strings.Contains(last, "node render"):
				// The fake worker transport EXECUTES the actual render
				// payload with the real renderer: a payload the real
				// renderer refuses fails the start, exactly like a worker.
				spec, derr := decode(input)
				if derr != nil {
					return CommandResult{}, derr
				}
				if _, rerr := RenderParameters(spec); rerr != nil {
					return CommandResult{}, rerr
				}
				return CommandResult{Code: 0}, nil
			case strings.Contains(last, "probe --file"):
				return CommandResult{Out: string(jsonBytes(e.probe())), Code: 0}, nil
			case strings.Contains(last, "sport ="):
				return CommandResult{Out: "LISTEN\n", Code: 0}, nil
			case strings.Contains(last, "LoadState"):
				e.mu.Lock()
				active := "inactive"
				if e.running["sup-b.service"] {
					active = "active"
				}
				e.mu.Unlock()
				return CommandResult{Out: "LoadState=loaded\nActiveState=" + active + "\n", Code: 0}, nil
			case strings.Contains(last, "is-active"):
				f := strings.Fields(last)
				if e.isRunning(f[len(f)-1]) {
					return CommandResult{Code: 0}, nil
				}
				return CommandResult{Code: 3}, nil
			case strings.Contains(last, " start "):
				f := strings.Fields(last)
				return e.systemctl("start", f[len(f)-1])
			case strings.Contains(last, " stop "):
				f := strings.Fields(last)
				return e.systemctl("stop", f[len(f)-1])
			}
		case args[0] == "ss":
			return CommandResult{Out: "LISTEN\n", Code: 0}, nil
		}
		return CommandResult{}, fail("unexpected command: %v", args)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// waitForReady observes BOTH the ready state and an early supervisor exit:
// if the entry fails before ready, the test fails immediately with the real
// cause instead of hanging until the deadline.
func waitForReady(t *testing.T, e *supEnv, done chan error, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("supervisor exited before ready: %v", err)
		default:
		}
		if e.recordState() == StateReady {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// slowMonitor keeps the rank-death monitor from firing during a test
// window so assertion windows are deterministic. The restore func MUST be
// called only after every supervisor goroutine of the test has joined.
func slowMonitor(t *testing.T) func() {
	t.Helper()
	saved := nativeMonitorInterval
	nativeMonitorInterval = time.Hour
	return func() { nativeMonitorInterval = saved }
}

// startRunReady spawns the REAL superviseNative run in a goroutine under
// its own cancellable context — a successful run is a full-lifetime
// supervisor and must never block the test thread — and waits for the
// durable READY record, failing fast if the supervisor exits first.
func (e *supEnv) startRunReady(t *testing.T) (context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.supervise(ctx, "run") }()
	waitForReady(t, e, done, 10*time.Second)
	return cancel, done
}

// joinSupervisor cancels the supervisor's own context and joins the
// goroutine with a bounded deadline: the real teardown (stop, confirm,
// durable verdict) must finish, never hang.
func joinSupervisor(t *testing.T, cancel context.CancelFunc, done chan error) error {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("the supervisor teardown must not hang")
		return nil
	}
}

func unitIn(list []string, unit string) bool {
	for _, u := range list {
		if u == unit {
			return true
		}
	}
	return false
}

func (e *supEnv) startedList() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.started...)
}

func (e *supEnv) stoppedList() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.stopped...)
}

func (e *supEnv) anyStartCommand() bool {
	for _, args := range e.calls.all() {
		if strings.Contains(strings.Join(args, " "), " start ") {
			return true
		}
	}
	return false
}

// TestSuperviseNativePreflightStalePortZeroEffects: an occupied recipe port
// refuses BEFORE any rank start AND no stop command is ever issued.
func TestSuperviseNativePreflightStalePortZeroEffects(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	e.installFactory()
	e.startSim() // something already answers on the recipe port
	err := e.supervise(context.Background(), "run")
	if err == nil || !strings.Contains(err.Error(), "still in use") {
		t.Fatalf("an occupied recipe port must refuse the start: %v", err)
	}
	if e.anyStartCommand() {
		t.Fatal("a preflight failure must never start a rank")
	}
	for _, args := range e.calls.all() {
		if strings.Contains(strings.Join(args, " "), " stop ") {
			t.Fatalf("a preflight failure must have zero stop effects: %v", args)
		}
	}
	if st := e.recordState(); st != StateFailed {
		t.Fatalf("the durable state must be failed without touching processes, got %s", st)
	}
}

// TestSuperviseNativeFrozenConfigAfterCentralEdit: the run uses ONLY the
// record's frozen units/setting; a central edit after the reservation is
// invisible, including the backend the supervisor probes.
func TestSuperviseNativeFrozenConfigAfterCentralEdit(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	// A later central edit: renamed units AND a different backend URL.
	raw, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	edited, err := decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	g := obj(obj(obj(edited["models"])["glm"])["settings"])
	// Each changed recipe stays VALID for its own placement; the frozen
	// record keeps the original "both" units and backend.
	changed := map[string]Object{
		"both":    {"spark01": "renamed-a.service", "spark02": "renamed-b.service"},
		"spark01": {"spark01": "renamed-a.service"},
		"spark02": {"spark02": "renamed-b.service"},
	}
	for _, k := range []string{"both", "spark01", "spark02"} {
		s := obj(g[k])
		s["units"] = changed[k]
		s["backend_url"] = "http://127.0.0.1:1"
	}
	if err := writeCluster(e.path, edited); err != nil {
		t.Fatal(err)
	}
	e.installFactory()
	// A successful run is a full-lifetime supervisor: it is spawned for
	// real in a goroutine under its own context, the assertions run WHILE
	// it is ready, and it is then actually stopped and joined bounded.
	cancel, done := e.startRunReady(t)
	started := e.startedList()
	if !unitIn(started, "sup-a.service") || !unitIn(started, "sup-b.service") {
		t.Fatalf("the frozen units must be started, got %v", started)
	}
	if e.healthHitCount() == 0 {
		t.Fatal("readiness must be confirmed against the FROZEN backend URL")
	}
	if st := e.recordState(); st != StateReady {
		t.Fatalf("the model must be ready, got %s", st)
	}
	if err := joinSupervisor(t, cancel, done); err != nil {
		t.Fatalf("the frozen-record supervisor must exit cleanly on stop: %v", err)
	}
}

// TestSuperviseNativeCorrectOrderRendezvous: the recipe order starts head
// before worker and the rendezvous probe follows the head rank.
func TestSuperviseNativeCorrectOrderRendezvous(t *testing.T) {
	e := newSupEnv(t, "both")
	// Add a rendezvous on the head node to the recipe BEFORE the record is
	// frozen, so the frozen setting carries it.
	raw, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	s := obj(obj(obj(doc["models"])["glm"])["settings"])["both"].(Object)
	s["rendezvous"] = Object{"node": "spark01", "port": e.port + 7}
	if err := writeCluster(e.path, doc); err != nil {
		t.Fatal(err)
	}
	e.begin()
	e.installFactory()
	var mu sync.Mutex
	var seq []string
	e.mu.Lock()
	e.onStart["sup-a.service"] = func() error { mu.Lock(); seq = append(seq, "head"); mu.Unlock(); return nil }
	e.onStart["sup-b.service"] = func() error { mu.Lock(); seq = append(seq, "worker"); mu.Unlock(); return nil }
	e.mu.Unlock()
	cancel, done := e.startRunReady(t)
	mu.Lock()
	if len(seq) != 2 || seq[0] != "head" || seq[1] != "worker" {
		mu.Unlock()
		t.Fatalf("the frozen startup order must be respected, got %v", seq)
	}
	mu.Unlock()
	if err := joinSupervisor(t, cancel, done); err != nil {
		t.Fatalf("the ordered supervisor must exit cleanly on stop: %v", err)
	}
}

// TestSuperviseNativePartialStartCleanup: a failed head start cleans up
// ONLY the attempted head rank; the worker whose start was never attempted
// is never stopped. A rank whose start WAS attempted is always stopped
// (see TestSuperviseNativeFailedStartEffectStillOwned).
func TestSuperviseNativePartialStartCleanup(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	e.installFactory()
	e.mu.Lock()
	e.onStart["sup-a.service"] = func() error { return fail("head boot failed") }
	e.mu.Unlock()
	err := e.supervise(context.Background(), "run")
	if err == nil {
		t.Fatal("a failed head start must fail the startup")
	}
	if !unitIn(e.stoppedList(), "sup-a.service") {
		t.Fatal("the attempted head rank must be cleaned up")
	}
	if unitIn(e.stoppedList(), "sup-b.service") {
		t.Fatal("a never-attempted rank must never receive a stop command")
	}
	if st := e.recordState(); st != StateFailed {
		t.Fatalf("the failed startup must be recorded, got %s", st)
	}
}

// TestSuperviseNativeFailedStartEffectStillOwned: a start command that
// applies its side effect and THEN fails must still own the rank: the
// attempted node is recorded durably BEFORE the command and the effect is
// stopped, while the never-attempted worker is never touched.
func TestSuperviseNativeFailedStartEffectStillOwned(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	e.installFactory()
	e.mu.Lock()
	e.onStart["sup-a.service"] = func() error {
		// The side effect lands (a process may already exist) and only then
		// the command fails.
		e.mu.Lock()
		e.running["sup-a.service"] = true
		e.mu.Unlock()
		return fail("start command failed after applying its effect")
	}
	e.mu.Unlock()
	err := e.supervise(context.Background(), "run")
	if err == nil {
		t.Fatal("a failed start must fail the startup")
	}
	// The attempted rank is durable BEFORE the command, so the ledger and
	// the in-memory view agree.
	rec, rerr := e.centralRuntime().readRecord("glm")
	if rerr != nil {
		t.Fatal(rerr)
	}
	att := obj(rec["attempted"])
	if str(att["spark01"]) != "sup-a.service" {
		t.Fatalf("the attempted rank must be recorded durably, got %v", att)
	}
	if _, ok := att["spark02"]; ok {
		t.Fatal("a never-attempted rank must never be recorded as attempted")
	}
	// The failed command's effect is still cleaned up.
	if !unitIn(e.stoppedList(), "sup-a.service") {
		t.Fatal("the failed-but-effective start must be stopped")
	}
	if unitIn(e.stoppedList(), "sup-b.service") {
		t.Fatal("a never-attempted worker must never receive a stop command")
	}
	if st := e.recordState(); st != StateFailed {
		t.Fatalf("the failed startup must be recorded, got %s", st)
	}
}

// TestSuperviseNativeLoadedCancelHoldsLocksThroughCleanup: cancellation
// while loaded stops the OWN ranks, confirms them, records the verdict and
// only THEN releases the device locks. A competing lock contender cannot
// acquire the device while the ranks may still run.
func TestSuperviseNativeLoadedCancelHoldsLocksThroughCleanup(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	e.installFactory()
	savedInterval := nativeMonitorInterval
	nativeMonitorInterval = 5 * time.Millisecond
	t.Cleanup(func() { nativeMonitorInterval = savedInterval })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.supervise(ctx, "run") }()
	waitForReady(t, e, done, 10*time.Second)

	// Compete for the head device lock from now on.
	var mu sync.Mutex
	acquired := false
	go func() {
		for {
			f, err := os.OpenFile(filepath.Join(e.root, "controller", "lifecycle", "owner.spark01.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				return
			}
			if er := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); er == nil {
				mu.Lock()
				acquired = true
				mu.Unlock()
				_ = f.Close()
				return
			}
			_ = f.Close()
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// While the rank is still marked running, the stop hook must observe
	// the competitor locked OUT: locks are held through the whole teardown.
	e.mu.Lock()
	e.onStop["sup-a.service"] = func() error {
		mu.Lock()
		defer mu.Unlock()
		if acquired {
			t.Error("the device lock was released before the owned rank shutdown finished")
		}
		return nil
	}
	e.mu.Unlock()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("an interrupted-but-confirmed supervisor must exit cleanly: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancellation while loaded must not hang")
	}
	stopped := e.stoppedList()
	if !unitIn(stopped, "sup-a.service") || !unitIn(stopped, "sup-b.service") {
		t.Fatalf("both owned ranks must be stopped on cancellation: %v", stopped)
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("a fully confirmed interrupted teardown is honestly stopped, got %s", st)
	}
	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return acquired
	})
}

// TestSuperviseNativeRankDeathCleansRemaining: one dead rank triggers the
// release of the remaining owned ranks and an honest durable verdict.
func TestSuperviseNativeRankDeathCleansRemaining(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	e.installFactory()
	savedInterval := nativeMonitorInterval
	nativeMonitorInterval = 5 * time.Millisecond
	t.Cleanup(func() { nativeMonitorInterval = savedInterval })
	done := make(chan error, 1)
	go func() { done <- e.supervise(context.Background(), "run") }()
	waitForReady(t, e, done, 10*time.Second)
	// The head rank dies.
	e.mu.Lock()
	e.running["sup-a.service"] = false
	e.mu.Unlock()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Fatalf("a rank death must be reported: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("rank death must not hang the supervisor")
	}
	if !unitIn(e.stoppedList(), "sup-b.service") {
		t.Fatal("the remaining rank must be released")
	}
	if st := e.recordState(); st != StateStopped && st != StateFailed {
		t.Fatalf("an honest verdict is required, got %s", st)
	}
}

// TestNativeStopRanksWorkerIndependentOfHungHead: every rank is stopped in
// parallel with its own bounded context; the worker's stop completes and is
// observed BEFORE the hung head is released.
func TestNativeStopRanksWorkerIndependentOfHungHead(t *testing.T) {
	e := newSupEnv(t, "both")
	e.installFactory()
	release := make(chan struct{})
	headDone := make(chan error, 1)
	workerStopped := make(chan struct{}, 1)
	e.mu.Lock()
	e.onStop["sup-a.service"] = func() error { <-release; return nil }
	e.onStop["sup-b.service"] = func() error {
		workerStopped <- struct{}{}
		return nil
	}
	e.mu.Unlock()
	units := Object{"spark01": "sup-a.service", "spark02": "sup-b.service"}
	rt := e.centralRuntime()
	rt.Run = e.runner()
	go func() { headDone <- nativeStopRanks(rt, units) }()
	select {
	case <-workerStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker stop must complete independently of a hung head")
	}
	select {
	case err := <-headDone:
		t.Fatalf("the head stop returned while still hung: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-headDone:
		if err != nil {
			t.Fatalf("parallel stop must aggregate nothing after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the head stop must finish once released")
	}
}

// TestSuperviseNativeFailedOwnershipBlocksReuse: a FAILED record's recorded
// units are probed as real ownership; a live unit on a required node blocks
// the start with zero effects, and a malformed record refuses too.
func TestSuperviseNativeFailedOwnershipBlocksReuse(t *testing.T) {
	e := newSupEnv(t, "spark01")
	e.begin()
	e.installFactory()
	// An orphan failed record occupying another unit on the same node. It
	// is produced by the REAL producer path: a whole frozen config snapshot
	// with its own model key, recipe and unit mapping written through the
	// real BeginOperation, then failed WITHOUT any physical effects.
	// The side central config is CLONED from the already-valid central
	// file — both nodes, policy, version and root defaults intact — so no
	// mandatory element is hand-missed. The only addition is a valid
	// "other" model derived from glm's own spark01 recipe, with an orphan
	// unit and its own matching backend + parameter binding.
	rawOther, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	cfgOther, err := decode(rawOther)
	if err != nil {
		t.Fatal(err)
	}
	glmSpark01 := Object(obj(obj(obj(obj(cfgOther["models"])["glm"])["settings"])["spark01"]))
	otherSpark01 := make(Object, len(glmSpark01))
	for k, v := range glmSpark01 {
		otherSpark01[k] = v
	}
	otherSpark01["units"] = Object{"spark01": "orphan.service"}
	otherSpark01["backend_url"] = "http://127.0.0.1:3"
	cfgOther["models"] = Object{"glm": obj(cfgOther["models"])["glm"], "other": Object{
		"kind": "llm", "name": "Other", "run_on": "spark01",
		"keep_loaded": false, "reasoning": true, "aliases": []any{},
		"settings": Object{"spark01": otherSpark01},
	}}
	otherPath := filepath.Join(e.root, "other-central.json")
	if err := writeCluster(otherPath, cfgOther); err != nil {
		t.Fatal(err)
	}
	otherCfg, err := readCluster(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	otherRt := newRuntime(otherCfg, otherPath)
	om := obj(obj(otherCfg["models"])["other"])
	oSetting := obj(obj(om["settings"])["spark01"])
	if _, err := otherRt.BeginOperation("other", om, oSetting, placements["spark01"], "spark01"); err != nil {
		t.Fatal(err)
	}
	// Fail the queued record durably with zero physical effects: the
	// reserved ownership is exactly what must block the reuse below.
	if err := withFileLock(otherRt.recordPath("other")+".lock", true, func() error {
		rec, rerr := otherRt.readRecord("other")
		if rerr != nil {
			return rerr
		}
		rec["state"] = StateFailed
		rec["updated_at"] = now()
		if err := validateRecord(rec, "other"); err != nil {
			return err
		}
		return writeObject(otherRt.recordPath("other"), rec)
	}); err != nil {
		t.Fatal(err)
	}
	// The current central config never carried "other": the failed record
	// is a true orphan there, exactly the reuse-blocking scenario.
	// The orphan unit answers active.
	e.mu.Lock()
	e.running["orphan.service"] = true
	e.mu.Unlock()
	if err := e.supervise(context.Background(), "run"); err == nil || !strings.Contains(err.Error(), "occupies") {
		t.Fatalf("a failed record's live unit must block reuse: %v", err)
	}
	if e.anyStartCommand() {
		t.Fatal("a blocked start must have zero effects")
	}
	// A malformed record is unknown ownership and refuses as well. This is
	// a FRESH isolated environment: the first run already durably failed
	// glm's own generation above, and that generation legitimately has no
	// queued operation anymore — it must not mask the malformed-record scan.
	e2 := newSupEnv(t, "spark01")
	e2.begin()
	e2.installFactory()
	if err := atomicWrite(filepath.Join(e2.root, "controller", "lifecycle", "bad.json"), []byte("{oops"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e2.supervise(context.Background(), "run"); err == nil || !strings.Contains(err.Error(), "Cannot confirm ownership") {
		t.Fatalf("a malformed record must block the start: %v", err)
	}
}

// TestSuperviseNativeStaleInvocationAndGenerationZeroEffects: a stale
// invocation guard and a mismatched cleanup marker cause ZERO stop effects.
func TestSuperviseNativeStaleInvocationAndGenerationZeroEffects(t *testing.T) {
	e := newSupEnv(t, "spark01")
	e.begin()
	e.installFactory()
	// Load the model first so a stop would have real targets. The success
	// run is a real lifetime supervisor spawned in a goroutine and then
	// actually stopped and joined, so the monitor can never undo the
	// zero-effects checks below.
	restore := slowMonitor(t)
	cancel, done := e.startRunReady(t)
	if err := joinSupervisor(t, cancel, done); err != nil {
		t.Fatalf("the loaded supervisor must exit cleanly on stop: %v", err)
	}
	restore()
	// Store a foreign invocation id in the record.
	if err := withFileLock(e.centralRuntime().recordPath("glm")+".lock", true, func() error {
		rec, err := e.centralRuntime().readRecord("glm")
		if err != nil {
			return err
		}
		rec["invocation_id"] = "inv-old"
		return writeObject(e.centralRuntime().recordPath("glm"), rec)
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INVOCATION_ID", "inv-new")
	before := len(e.calls.all())
	if err := e.supervise(context.Background(), "stop"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("a stale invocation must never stop a newer operation: %v", err)
	}
	if len(e.calls.all()) != before {
		t.Fatal("a refused stop must have zero effects")
	}
	// A cleanup whose marker does not carry the current generation is refused.
	if err := os.WriteFile(filepath.Join(e.root, "controller", "lifecycle", "stopping.glm."+e.gen), []byte("wronggen"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.supervise(context.Background(), "cleanup"); err == nil {
		t.Fatal("a cleanup for a foreign generation must be refused")
	}
}

// TestSuperviseNativeConfirmUnknownStaysFailed: a unit that systemd no
// longer knows is NOT shutdown proof; the record keeps the failed verdict.
func TestSuperviseNativeConfirmUnknownStaysFailed(t *testing.T) {
	e := newSupEnv(t, "spark01")
	e.begin()
	e.installFactory()
	restore := slowMonitor(t)
	cancel, done := e.startRunReady(t)
	// Make the unit unknown for BOTH the supervisor's own teardown and the
	// explicit stop: an unknown unit is never shutdown proof.
	e.mu.Lock()
	e.loadStates["sup-a.service"] = "not-found"
	e.mu.Unlock()
	// The real supervisor's own teardown legitimately fails to confirm an
	// unknown unit; the durable failed verdict is what matters.
	_ = joinSupervisor(t, cancel, done)
	restore()
	if st := e.recordState(); st != StateFailed {
		t.Fatalf("an unconfirmable teardown must retain failed, got %s", st)
	}
	if err := e.supervise(context.Background(), "stop"); err == nil {
		t.Fatal("an unknown unit must never count as shutdown proof")
	}
	if st := e.recordState(); st != StateFailed {
		t.Fatalf("an unconfirmable shutdown must retain failed, got %s", st)
	}
}

// TestSuperviseNativeRootFallbackStop: with an invalid central file at
// ROOT/config/models.json, stop/cleanup still work from the validated
// record alone via the strict root fallback.
func TestSuperviseNativeRootFallbackStop(t *testing.T) {
	e := newSupEnv(t, "spark01")
	e.begin()
	e.installFactory()
	// The monitor is kept quiet so its own teardown can never race the
	// explicit stop's durable CAS below; it is joined bounded afterwards.
	restore := slowMonitor(t)
	cancel, done := e.startRunReady(t)
	// Corrupt the central file: only the strict fallback remains.
	if err := atomicWrite(e.path, []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.supervise(context.Background(), "stop"); err != nil {
		t.Fatalf("the strict root fallback must serve an existing validated record: %v", err)
	}
	if st := e.recordStateRootFallback(); st != StateStopped {
		t.Fatalf("the record must be stopped, got %s", st)
	}
	if !unitIn(e.stoppedList(), "sup-a.service") {
		t.Fatal("the recorded unit must be stopped from the frozen record")
	}
	// The now-redundant lifetime supervisor is stopped for real and joined:
	// its own teardown touches nothing the durable verdict above already
	// recorded, so any error there must not weaken the assertions.
	_ = joinSupervisor(t, cancel, done)
	restore()
}

// TestSuperviseNativeFailedRecoveryStop: a same-generation FAILED record is
// recovered by stop. It needs the Core-owned Failed->Stopping edge and is
// skipped until that transition lands.
func TestSuperviseNativeFailedRecoveryStop(t *testing.T) {
	e := newSupEnv(t, "spark01")
	e.begin()
	e.installFactory()
	e.mu.Lock()
	e.onStart["sup-a.service"] = func() error { return fail("boot failed") }
	e.mu.Unlock()
	if err := e.supervise(context.Background(), "run"); err == nil {
		t.Fatal("the failed boot must fail the startup")
	}
	if st := e.recordState(); st != StateFailed {
		t.Fatalf("the failed startup must be recorded, got %s", st)
	}
	// Probe the Core-owned edge; skip while it is missing.
	if err := e.centralRuntime().TransitionState("glm", e.gen, StateStopping); err != nil {
		t.Skipf("awaiting the Core Failed->Stopping edge: %v", err)
	}
	if err := e.supervise(context.Background(), "stop"); err != nil {
		t.Fatalf("an owned failed generation must be recoverable: %v", err)
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the recovered generation must be stopped, got %s", st)
	}
}
