package sparky

// Coordinator tests: admission queues and the ComfyUI integration. All
// device and engine effects are local: the ComfyUI engine is an httptest
// server, device probes and command runners are exact injected fakes, and
// the cluster config lives in a temp directory. Synchronization uses
// channels and bounded polling, never arbitrary long sleeps.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const gib = 1024.0 * 1024.0 * 1024.0

func testConfig(root string) Object {
	settings := func(name, url string) Object {
		unit := name + ".service"
		mk := func(nodes ...string) Object {
			units := Object{}
			for _, n := range nodes {
				units[n] = unit
			}
			return Object{"backend_url": url, "units": units, "context_tokens": 123456, "concurrency": 8}
		}
		return Object{"spark01": mk("spark01"),
			"spark02": mk("spark02"),
			"both":    mk("spark01", "spark02")}
	}
	return Object{
		"version": 1, "root": root, "controller_port": 8020, "lifecycle": "native",
		"nodes": Object{
			"spark01": Object{"direct_ip": "10.0.0.1"},
			"spark02": Object{"direct_ip": "10.0.0.2", "ssh": "spark02", "comfy_url": "", "comfy_unit": "comfyui.service"},
		},
		"policy": Object{"interrupt_active_jobs": false, "protected_models": []any{},
			"comfyui_exclusive_with_llms": false,
			"comfyui_shared_llms":         []any{"qwen"},
			"comfyui_min_available_gib":   18},
		"models": Object{
			"qwen":     Object{"kind": "llm", "name": "qwen", "run_on": "spark02", "settings": settings("qwen", "http://127.0.0.1:8001"), "aliases": []any{}},
			"nemotron": Object{"kind": "llm", "name": "nemotron", "run_on": "spark01", "settings": settings("nemotron", "http://127.0.0.1:8001"), "aliases": []any{}},
			"comfyui":  Object{"kind": "comfyui", "name": "ComfyUI", "run_on": "spark02", "settings": settings("comfyui", "")},
		},
	}
}

// fakeDevice is the injected node probe. It is safe for concurrent use so
// snapshot's parallel node queries and tests can share it.
type fakeDevice struct {
	mu        sync.Mutex
	Mem       float64
	Units     map[string]string
	Reachable bool
}

func (f *fakeDevice) probe(Object) (Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	units := Object{}
	for name, state := range f.Units {
		units[name] = state
	}
	return Object{
		"comfyui":      Object{"reachable": f.Reachable, "executing": false, "running": 0, "phase": "idle"},
		"memory_bytes": Object{"MemAvailable": json.Number(fmt.Sprintf("%d", int64(f.Mem)))},
		"units":        units,
		"containers":   Object{},
	}, nil
}

// runner returns an injected Runner that answers the ssh probe and systemctl
// state queries without touching the real system.
func fakeRunner(dev *fakeDevice) Runner {
	return func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		switch args[0] {
		case "ssh":
			probe, _ := dev.probe(nil)
			return CommandResult{Out: string(jsonBytes(probe))}, nil
		case "systemctl":
			return CommandResult{Out: "inactive\n", Code: 0}, nil
		}
		return CommandResult{}, fmt.Errorf("unexpected command in test: %v", args)
	}
}

type testEnv struct {
	t      *testing.T
	coord  *Coordinator
	rt     *Runtime
	dev    *fakeDevice
	engine *fakeEngine
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	cfg := testConfig(root)
	path := filepath.Join(root, "models.json")
	if err := writeCluster(path, cfg); err != nil {
		t.Fatal(err)
	}
	dev := &fakeDevice{Mem: 23 * gib, Units: map[string]string{}, Reachable: true}
	rt := newRuntime(cfg, path)
	rt.Run = fakeRunner(dev)
	rt.ProbeNode = dev.probe
	coord, err := newCoordinator(path, rt)
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, coord: coord, rt: rt, dev: dev}
}

// waitForAdmission blocks until the named workload appears in the waiting
// queue, proving the goroutine reached the FIFO before the test proceeds.
func (e *testEnv) waitForAdmission(name string) {
	e.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		admission := e.coord.Admission()
		for _, row := range arr(admission["waiting"]) {
			if str(obj(row)["workload"]) == name {
				return
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s never reached the queue: %v", name, admission)
		}
		time.Sleep(time.Millisecond)
	}
}

func leaseCount(c *Coordinator) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.leases)
}

func leaseNames(c *Coordinator) map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for _, l := range c.leases {
		out[l.Name] = true
	}
	return out
}

func TestOverlappingDeviceExclusivity(t *testing.T) {
	env := newTestEnv(t)
	id, err := env.coord.acquire(context.Background(), "qwen", []string{"spark01"}, true, 0, "a1", "", true)
	if err != nil {
		t.Fatal(err)
	}
	// A nonblocking exclusive request on the same device must not be admitted
	// and must leave exactly one waiting entry.
	got, err := env.coord.acquire(context.Background(), "nemotron", []string{"spark01"}, false, 0, "a2", "", false)
	if err != nil || got != "" {
		t.Fatalf("conflicting admission must wait, got %q %v", got, err)
	}
	if waiting := len(env.coord.Admission()["waiting"].([]any)); waiting != 1 {
		t.Fatalf("expected one waiting entry, got %v", env.coord.Admission()["waiting"])
	}
	env.coord.Release(id)
	// After release the blocked request is admitted without a duplicate lease.
	id2, err := env.coord.acquire(context.Background(), "nemotron", []string{"spark01"}, false, 0, "a2", "", false)
	if err != nil || id2 == "" {
		t.Fatalf("release must unblock the queued request: %q %v", id2, err)
	}
	if n := leaseCount(env.coord); n != 1 {
		t.Fatalf("exactly one lease expected, got %d", n)
	}
}

func TestSameModelConcurrencyLimit(t *testing.T) {
	env := newTestEnv(t)
	ids := []string{}
	for i := 0; i < 3; i++ {
		id, err := env.coord.acquire(context.Background(), "qwen", []string{"spark02"}, false, 3, fmt.Sprintf("c%d", i), "", true)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if got, err := env.coord.acquire(context.Background(), "qwen", []string{"spark02"}, false, 3, "c4", "", false); err != nil || got != "" {
		t.Fatalf("fourth lease over the limit must wait: %q %v", got, err)
	}
	for _, id := range ids[:2] {
		env.coord.Release(id)
	}
	if got, err := env.coord.acquire(context.Background(), "qwen", []string{"spark02"}, false, 3, "c4", "", false); err != nil || got == "" {
		t.Fatalf("under-limit request must be admitted: %q %v", got, err)
	}
}

func TestDisjointWorkAdmittedIndependently(t *testing.T) {
	env := newTestEnv(t)
	id, err := env.coord.acquire(context.Background(), "qwen", []string{"spark01"}, true, 0, "d1", "", true)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := env.coord.acquire(context.Background(), "nemotron", []string{"spark02"}, true, 0, "d2", "", true)
	if err != nil {
		t.Fatalf("disjoint devices must both be admitted: %v", err)
	}
	if id == "" || id2 == "" || id == id2 {
		t.Fatalf("disjoint work must get distinct tokens: %q %q", id, id2)
	}
}

func TestFIFOFairnessNoStarvation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	holder, _ := env.coord.acquire(ctx, "qwen", []string{"spark01"}, true, 0, "h", "", true)
	firstDone := make(chan string, 1)
	secondDone := make(chan string, 1)
	// The trailing waiter is cancellable so the test never leaks a blocked
	// goroutine; any lease it is granted is released before returning.
	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	go func() {
		id, err := env.coord.acquire(ctx, "first", []string{"spark01"}, true, 0, "f1", "", true)
		if err != nil {
			firstDone <- "err"
			return
		}
		firstDone <- id
	}()
	env.waitForAdmission("first")
	go func() {
		id, err := env.coord.acquire(wctx, "second", []string{"spark01"}, true, 0, "f2", "", true)
		if err != nil {
			secondDone <- "err"
			return
		}
		secondDone <- id
	}()
	env.waitForAdmission("second")
	env.coord.Release(holder)
	// The earlier request must be admitted; the later one keeps waiting.
	select {
	case id := <-firstDone:
		if id == "err" {
			t.Fatal("the head of the queue failed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the head of the queue starved")
	}
	select {
	case id := <-secondDone:
		if id != "" {
			t.Fatalf("the later request must keep waiting FIFO, got %q", id)
		}
	case <-time.After(100 * time.Millisecond):
	}
	// Unblock and join the trailing waiter, releasing any lease it owned.
	wcancel()
	select {
	case id := <-secondDone:
		if id != "" && id != "err" {
			env.coord.Release(id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the trailing waiter never returned after cancellation")
	}
}

func TestCancellationWhileQueued(t *testing.T) {
	env := newTestEnv(t)
	holder, _ := env.coord.acquire(context.Background(), "qwen", []string{"spark01"}, true, 0, "h", "", true)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, err := env.coord.acquire(ctx, "qwen", []string{"spark01"}, true, 0, "w1", "", true)
		done <- err
	}()
	env.waitForAdmission("qwen")
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled queued request must fail")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued cancellation must not hang")
	}
	if waiting := len(env.coord.Admission()["waiting"].([]any)); waiting != 0 {
		t.Fatalf("cancelled entry must be cleaned from the queue: %v", env.coord.Admission()["waiting"])
	}
	env.coord.Release(holder)
	_ = holder
}

func TestCancelJobRemovesOnlyMatchingWaitingEntry(t *testing.T) {
	env := newTestEnv(t)
	holder, _ := env.coord.acquire(context.Background(), "qwen", []string{"spark01"}, true, 0, "h", "", true)
	go func() {
		_, _ = env.coord.acquire(context.Background(), "comfyui@spark02", []string{"spark01"}, true, 0, "spark02:job-1", "", true)
	}()
	env.waitForAdmission("comfyui@spark02")
	env.coord.CancelJob("spark02", "job-1")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(env.coord.Admission()["waiting"].([]any)) > 0 {
		time.Sleep(time.Millisecond)
	}
	if waiting := len(env.coord.Admission()["waiting"].([]any)); waiting != 0 {
		t.Fatalf("cancelled pending request must be removed: %v", env.coord.Admission()["waiting"])
	}
	// The active unrelated lease is untouched.
	if n := leaseCount(env.coord); n != 1 {
		t.Fatalf("unrelated lease must survive a job cancel: %d", n)
	}
	env.coord.Release(holder)
}

func TestRequestIDRetryDeduplication(t *testing.T) {
	env := newTestEnv(t)
	holder, _ := env.coord.acquire(context.Background(), "qwen", []string{"spark01"}, true, 0, "h", "", true)
	results := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func() {
			id, err := env.coord.acquire(context.Background(), "qwen", []string{"spark01"}, false, 0, "spark02:dup", "", true)
			if err != nil {
				results <- "err"
				return
			}
			results <- id
		}()
	}
	env.waitForAdmission("qwen")
	env.coord.Release(holder)
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case id := <-results:
			seen[id] = true
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent retries deadlocked")
		}
	}
	if len(seen) != 1 {
		t.Fatalf("two retries of one prompt must share one lease: %v", seen)
	}
	if n := leaseCount(env.coord); n != 1 {
		t.Fatalf("exactly one lease for the retried prompt: %d", n)
	}
}

func TestFenceAndForceWakeups(t *testing.T) {
	env := newTestEnv(t)
	holder, _ := env.coord.acquire(context.Background(), "qwen", []string{"spark01"}, true, 0, "h", "", true)
	done := make(chan string, 1)
	go func() {
		id, err := env.coord.acquire(context.Background(), "qwen", []string{"spark01"}, true, 0, "w", "", true)
		if err != nil {
			done <- "err"
			return
		}
		done <- id
	}()
	env.waitForAdmission("qwen")
	env.coord.Fence(true)
	env.coord.Release(holder) // release alone must not admit past a fence
	select {
	case id := <-done:
		t.Fatalf("fencing must keep the queue blocked, got %q", id)
	case <-time.After(100 * time.Millisecond):
	}
	env.coord.Fence(false) // unfencing wakes the queued request for admission
	select {
	case id := <-done:
		if id == "err" || id == "" {
			t.Fatalf("the queued request must be admitted after the fence lifts: %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unfencing must wake the queued request")
	}
}

// fakeEngine is a local stand-in for a real ComfyUI engine: /queue,
// /sparky/state, /prompt and /sparky/free.
type fakeEngine struct {
	mu       sync.Mutex
	server   *httptest.Server
	graph    any
	state    Object
	promptID string
	lastBody Object
	down     bool
}

func newFakeEngine(t *testing.T, graph Object) *fakeEngine {
	e := &fakeEngine{graph: graph, promptID: "engine-1",
		state: Object{"executing": false, "prompt_id": nil, "phase": "idle"}}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		down, state, graph, promptID, lastBody := e.down, e.state, e.graph, e.promptID, e.lastBody
		_ = lastBody
		e.mu.Unlock()
		switch {
		case down:
			w.WriteHeader(http.StatusBadGateway)
		case strings.HasSuffix(r.URL.Path, "/queue"):
			if graph == nil {
				_, _ = w.Write([]byte(`{"queue_running":[]}`))
				return
			}
			out, _ := json.Marshal(Object{"queue_running": []any{[]any{0, "image-job", graph}}})
			_, _ = w.Write(out)
		case strings.HasSuffix(r.URL.Path, "/sparky/state"):
			out, _ := json.Marshal(state)
			_, _ = w.Write(out)
		case strings.HasSuffix(r.URL.Path, "/sparky/free"):
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/prompt"):
			body := Object{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			e.mu.Lock()
			e.lastBody = body
			e.mu.Unlock()
			if promptID == "" {
				out, _ := json.Marshal(Object{})
				_, _ = w.Write(out)
				return
			}
			out, _ := json.Marshal(Object{"prompt_id": promptID})
			_, _ = w.Write(out)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(e.server.Close)
	return e
}

func (e *fakeEngine) setState(state Object) {
	e.mu.Lock()
	e.state = state
	e.mu.Unlock()
}

// comfyEnv builds a coordinator whose spark02 points at the fake engine.
func comfyEnv(t *testing.T, graph Object) *testEnv {
	env := newTestEnv(t)
	engine := newFakeEngine(t, graph)
	cfg := env.coord.Config()
	obj(obj(cfg["nodes"])["spark02"])["comfy_url"] = engine.server.URL
	obj(obj(cfg["models"])["comfyui"])["run_on"] = "spark02"
	obj(obj(obj(cfg["models"])["comfyui"])["settings"])["spark02"] = Object{"backend_url": engine.server.URL, "units": Object{}}
	if err := writeCluster(env.coord.configPath(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := env.coord.Refresh(); err != nil {
		t.Fatal(err)
	}
	env.engine = engine
	return env
}

func imageGraph() Object { return Object{"1": Object{"class_type": "UNETLoader", "inputs": Object{}}} }

func (e *testEnv) admit() Object {
	result, err := e.coord.ComfyAcquire("spark02", "image-job", false)
	if err != nil {
		e.t.Fatalf("ComfyAcquire failed: %v", err)
	}
	return result
}

func TestEightAllowedLLMLeasesShareImage(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	env.dev.Units = map[string]string{"qwen.service": "active"}
	for i := 0; i < 8; i++ {
		if _, err := env.coord.acquire(context.Background(), "qwen", []string{"spark01", "spark02"}, false, 0, fmt.Sprintf("l%d", i), "", true); err != nil {
			t.Fatal(err)
		}
	}
	result := env.admit()
	if result["shared"] != true || result["status"] != "admitted" {
		t.Fatalf("shared image admission expected: %v", result)
	}
	if leaseCount(env.coord) != 9 {
		t.Fatalf("eight LLM leases plus one image lease expected: %d", leaseCount(env.coord))
	}
	// The shared image still blocks new exclusive or same-model jobs.
	if got, _ := env.coord.acquire(context.Background(), "comfyui@spark02", []string{"spark02"}, true, 0, "spark02:x1", "", false); got != "" {
		t.Fatal("a second image job on the same device must wait")
	}
	env.waitForAdmission("comfyui@spark02")
	// FIFO correctly keeps the LLM behind the queued exclusive image, so the
	// image job is cancelled (as the engine would for a finished prompt)
	// before the allowed LLM can share the device.
	env.coord.CancelJob("spark02", "x1")
	if _, err := env.coord.acquire(context.Background(), "qwen", []string{"spark02"}, false, 0, "x2", "", true); err != nil {
		t.Fatalf("allowed LLMs keep sharing: %v", err)
	}
}

func TestUnapprovedLoadedLLMNeverUnloaded(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	env.dev.Units = map[string]string{"nemotron.service": "active"}
	result := env.admit()
	if result["reason"] != "unapproved_loaded_llm" || result["token"] != nil {
		t.Fatalf("unapproved LLM must block the shared image: %v", result)
	}
	if leaseCount(env.coord) != 0 {
		t.Fatalf("no lease may survive a refused admission: %d", leaseCount(env.coord))
	}
}

func TestLowMemoryWaitsAndClearsIdleCacheOnly(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	env.dev.Units = map[string]string{"qwen.service": "active"}
	env.dev.Mem = 12 * gib
	result := env.admit()
	if result["reason"] != "low_available_memory" || result["token"] != nil {
		t.Fatalf("low memory must wait: %v", result)
	}
	if leaseCount(env.coord) != 0 {
		t.Fatalf("no lease may survive low memory: %d", leaseCount(env.coord))
	}
	// After the idle cache is cleared the recheck sees the freed memory and
	// admits the job without touching any model.
	env.dev.Mem = 23 * gib
	result = env.admit()
	if result["status"] != "admitted" || result["shared"] != true {
		t.Fatalf("freed memory must admit the shared job: %v", result)
	}
}

func TestExclusiveImageRefusesWhileLLMLoaded(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	env.dev.Units = map[string]string{"qwen.service": "active"}
	cfg := env.coord.Config()
	obj(cfg["policy"])["comfyui_exclusive_with_llms"] = true
	if err := writeCluster(env.coord.configPath(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := env.coord.Refresh(); err != nil {
		t.Fatal(err)
	}
	result, err := env.coord.ComfyAcquire("spark02", "image-job", false)
	if err != nil {
		t.Fatal(err)
	}
	if result["reason"] != "loaded_llm_requires_explicit_unload" {
		t.Fatalf("exclusive image job must wait for an explicit unload: %v", result)
	}
	if leaseCount(env.coord) != 0 {
		t.Fatalf("no lease may survive: %d", leaseCount(env.coord))
	}
}

func TestFakeCPUFlagCannotBypassGPU(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	if _, err := env.coord.ComfyAcquire("spark02", "image-job", true); err == nil {
		t.Fatal("a client CPU flag must not bypass GPU admission")
	}
	if leaseCount(env.coord) != 0 {
		t.Fatalf("no lease may be created: %d", leaseCount(env.coord))
	}
	if _, err := env.coord.ComfyAcquire("spark09", "image-job", false); err == nil {
		t.Fatal("unknown device must fail safely")
	}
}

func TestCPUProbeUsesNoGPUNodes(t *testing.T) {
	env := comfyEnv(t, Object{"1": Object{"class_type": "SparkyProbe", "inputs": Object{}}})
	result := env.admit()
	if result["cpu_only"] != true || result["status"] != "admitted" {
		t.Fatalf("SparkyProbe-only workflow must be CPU-only: %v", result)
	}
	if !leaseNames(env.coord)["cpu-probe@spark02"] {
		t.Fatalf("cpu-probe lease expected: %v", leaseNames(env.coord))
	}
	// A CPU probe reserves no device: an LLM is admitted concurrently.
	if got, err := env.coord.acquire(context.Background(), "qwen", []string{"spark02"}, false, 0, "cpu-llm", "", false); err != nil || got == "" {
		t.Fatalf("CPU probe must not block GPU workloads: %q %v", got, err)
	}
}

func TestComfyRetryDedupAndWaitingShape(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	// An exclusive lease on the device blocks the image job, so both
	// retries park in the FIFO as exactly one pending entry.
	holder, err := env.coord.acquire(context.Background(), "nemotron", []string{"spark02"}, true, 0, "h", "", true)
	if err != nil {
		t.Fatal(err)
	}
	first, err := env.coord.ComfyAcquire("spark02", "image-job", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.coord.ComfyAcquire("spark02", "image-job", false)
	if err != nil {
		t.Fatal(err)
	}
	if first["status"] != "waiting" || first["retry_after"] == nil {
		t.Fatalf("waiting shape expected: %v", first)
	}
	if second["status"] != "waiting" {
		t.Fatalf("retry must wait promptly: %v", second)
	}
	// Exactly one pending entry is retained for the prompt.
	env.coord.mu.Lock()
	pending := len(env.coord.pending)
	env.coord.mu.Unlock()
	if pending != 1 {
		t.Fatalf("exactly one pending entry expected: %d", pending)
	}
	// Once the blocker is gone the retained entry admits with one lease.
	env.coord.Release(holder)
	result, err := env.coord.ComfyAcquire("spark02", "image-job", false)
	if err != nil || result["status"] != "admitted" {
		t.Fatalf("the retained entry must admit later: %v %v", result, err)
	}
}

func TestReconcileUnknownStateKeepsLeases(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	env.admit()
	env.engine.mu.Lock()
	env.engine.down = true
	env.engine.mu.Unlock()
	env.coord.ReconcileJobs()
	if leaseCount(env.coord) != 1 {
		t.Fatalf("unknown engine state must keep reservations: %d", leaseCount(env.coord))
	}
}

func TestReconcileAdoptsExecutingEngineJob(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	env.engine.setState(Object{"executing": true, "prompt_id": "image-job", "phase": "executing"})
	env.coord.ReconcileJobs()
	if !leaseNames(env.coord)["comfyui@spark02"] {
		t.Fatalf("executing engine job must be adopted: %v", leaseNames(env.coord))
	}
	// The adopted shared job coexists with allowed LLM leases but not with
	// exclusive ones under the default policy.
	if got, _ := env.coord.acquire(context.Background(), "qwen", []string{"spark02"}, false, 0, "after-adopt", "", false); got == "" {
		t.Fatal("allowed LLM must share with the adopted job")
	}
	if got, _ := env.coord.acquire(context.Background(), "qwen", []string{"spark02"}, true, 0, "after-adopt-x", "", false); got != "" {
		t.Fatal("exclusive LLM start must wait for the adopted job")
	}
	// A confirmed finished job releases the obsolete adoption lease.
	env.engine.setState(Object{"executing": false, "prompt_id": nil, "phase": "idle"})
	env.coord.ReconcileJobs()
	if leaseNames(env.coord)["comfyui@spark02"] {
		t.Fatalf("confirmed finished job must release the adoption: %v", leaseNames(env.coord))
	}
}

func TestSubmitComfyWrapsAndValidates(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	result, err := env.coord.SubmitComfy(imageGraph(), "")
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	if result["node"] != "spark02" || result["prompt_id"] != "engine-1" {
		t.Fatalf("submit result shape mismatch: %v", result)
	}
	env.engine.mu.Lock()
	wrapped := env.engine.lastBody
	env.engine.mu.Unlock()
	if _, hasPrompt := wrapped["prompt"]; !hasPrompt {
		t.Fatalf("the workflow must be wrapped exactly once: %v", wrapped)
	}
	// A missing engine prompt_id is an error, never a fake success.
	env.engine.promptID = ""
	if _, err := env.coord.SubmitComfy(imageGraph(), ""); err == nil {
		t.Fatal("a missing prompt_id must fail")
	}
	env.engine.promptID = "engine-1"
	// Multi-device submissions are refused.
	cfg := env.coord.Config()
	obj(obj(cfg["models"])["comfyui"])["run_on"] = "both"
	if err := writeCluster(env.coord.configPath(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := env.coord.SubmitComfy(imageGraph(), "both"); err == nil {
		t.Fatal("submitting to two devices must fail")
	}
}

// coordinatorLeaseExists reports whether the lease token still exists.
func coordinatorLeaseExists(c *Coordinator, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.leases[id]
	return ok
}

// A lease removed (reconciled away) while its validation is blocked must
// never come back as an admitted orphan token.
func TestComfyValidationCommitRequiresSurvivingLease(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	hold := make(chan struct{})
	base := env.rt.ProbeNode
	env.rt.ProbeNode = func(meta Object) (Object, error) {
		<-hold
		return base(meta)
	}
	done := make(chan Object, 1)
	errs := make(chan error, 1)
	go func() {
		r, err := env.coord.ComfyAcquire("spark02", "image-job", false)
		done <- r
		errs <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	var id string
	for time.Now().Before(deadline) && id == "" {
		env.coord.mu.Lock()
		for lid, l := range env.coord.leases {
			if l.Name == "comfyui@spark02" && l.Validating {
				id = lid
			}
		}
		env.coord.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	if id == "" {
		t.Fatal("the validation never started")
	}
	// Reconcile removes the provisional lease while validation is blocked.
	env.coord.mu.Lock()
	delete(env.coord.leases, id)
	env.coord.mu.Unlock()
	close(hold)
	if err := <-errs; err != nil {
		t.Fatalf("a superseded validation must not error the poller: %v", err)
	}
	result := <-done
	if result["status"] == "admitted" || result["token"] != nil {
		t.Fatalf("no admitted orphan token may survive: %v", result)
	}
	if result["status"] != "waiting" {
		t.Fatalf("a superseded validation must ask for a retry: %v", result)
	}
	if coordinatorLeaseExists(env.coord, id) {
		t.Fatal("the removed lease must stay removed")
	}
}

// A failed device probe propagates its error and never starts any unit.
func TestSubmitComfyPropagatesProbeFailureWithoutStarting(t *testing.T) {
	env := newTestEnv(t)
	var mu sync.Mutex
	var calls [][]string
	base := fakeRunner(env.dev)
	env.rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		mu.Lock()
		calls = append(calls, args)
		mu.Unlock()
		if args[0] == "ssh" {
			return CommandResult{}, fail("probe exploded")
		}
		return base(ctx, args, input)
	}
	_, err := env.coord.SubmitComfy(imageGraph(), "spark02")
	if err == nil {
		t.Fatal("the probe failure must propagate")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, args := range calls {
		if args[0] == "systemctl" || args[0] == "sudo" {
			t.Fatalf("a failed probe must never start a unit: %v", calls)
		}
	}
}

func TestCheckJobLeaseValidatesNode(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	result := env.admit()
	token := str(result["token"])
	if err := env.coord.CheckJobLease(token, "spark02"); err != nil {
		t.Fatalf("matching node lease must validate: %v", err)
	}
	if err := env.coord.CheckJobLease(token, "spark01"); err == nil {
		t.Fatal("a lease of another workload must be refused")
	}
	// A missing token is idempotent, matching the Python controller.
	if err := env.coord.CheckJobLease("missing", "spark02"); err != nil {
		t.Fatalf("missing token must stay idempotent: %v", err)
	}
}

func TestReconcileMalformedStateKeepsLeases(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	env.admit()
	// Malformed bodies are unknown states, never a confirmed idle cleanup.
	for _, state := range []Object{
		{},
		Object{"executing": "yes", "prompt_id": "image-job"},
		Object{"executing": true, "prompt_id": nil},
		Object{"executing": true, "prompt_id": 7},
	} {
		env.engine.setState(state)
		env.coord.ReconcileJobs()
		if leaseCount(env.coord) != 1 {
			t.Fatalf("malformed state %v must keep the reservation", state)
		}
	}
	// A confirmed idle state still cleans up.
	env.engine.setState(Object{"executing": false, "prompt_id": nil, "phase": "idle"})
	env.coord.ReconcileJobs()
	if leaseCount(env.coord) != 0 {
		t.Fatalf("confirmed idle must release the lease: %d", leaseCount(env.coord))
	}
}

// Concurrent retries of the same prompt must share one committed lease;
// only the validating owner may release it.
func TestConcurrentComfyRetriesShareOneLease(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	env.dev.Units = map[string]string{"qwen.service": "active"}
	const tries = 4
	results := make(chan Object, tries)
	var wg sync.WaitGroup
	for i := 0; i < tries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := env.coord.ComfyAcquire("spark02", "image-job", false)
			if err != nil {
				r = Object{"status": "error", "error": err.Error()}
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	token := ""
	for r := range results {
		if r["status"] != "admitted" {
			continue
		}
		if token == "" {
			token = str(r["token"])
		} else if str(r["token"]) != token {
			t.Fatal("concurrent retries must share one committed token")
		}
	}
	if token == "" {
		t.Fatal("at least one retry must be admitted")
	}
	if leaseCount(env.coord) != 1 || !leaseNames(env.coord)["comfyui@spark02"] {
		t.Fatalf("exactly one image lease may survive: %d %v", leaseCount(env.coord), leaseNames(env.coord))
	}
}

// When validation fails (low memory), the owner releases the provisional
// lease and no duplicate may keep or re-release it.
func TestConcurrentRetriesReleaseOnceOnFailure(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	env.dev.Units = map[string]string{"qwen.service": "active"}
	env.dev.Mem = 12 * gib
	const tries = 4
	results := make(chan Object, tries)
	var wg sync.WaitGroup
	for i := 0; i < tries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := env.coord.ComfyAcquire("spark02", "image-job", false)
			if err != nil {
				r = Object{"status": "error", "error": err.Error()}
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	for r := range results {
		if r["status"] == "admitted" {
			t.Fatalf("low memory must not admit any retry: %v", r)
		}
	}
	if leaseCount(env.coord) != 0 {
		t.Fatalf("the failed validation must leave no lease: %d", leaseCount(env.coord))
	}
}

// New entries are really inserted into the FIFO: after the holder releases,
// the first waiter wins and later waiters stay queued behind it in order.
func TestPendingFIFOOrder(t *testing.T) {
	env := newTestEnv(t)
	holder, _ := env.coord.acquire(context.Background(), "qwen", []string{"spark01"}, true, 0, "h", "", true)
	type res struct{ rid, id string }
	done := make(chan res, 3)
	for _, rid := range []string{"spark01:w1", "spark01:w2", "spark01:w3"} {
		go func(rid string) {
			id, _ := env.coord.acquire(context.Background(), "qwen", []string{"spark01"}, true, 0, rid, "", true)
			done <- res{rid, id}
		}(rid)
		env.waitForAdmission("qwen")
	}
	env.coord.Release(holder)
	first := <-done
	if first.rid != "spark01:w1" || first.id == "" {
		t.Fatalf("the first waiter must win the FIFO: %v", first)
	}
	if waiting := len(env.coord.Admission()["waiting"].([]any)); waiting != 2 {
		t.Fatalf("later waiters must stay queued: %v", env.coord.Admission()["waiting"])
	}
	env.coord.CancelJob("spark01", "w2")
	env.coord.Release(first.id)
	ids := map[string]string{"spark01:w1": first.id}
	for i := 0; i < 2; i++ {
		r := <-done
		ids[r.rid] = r.id
	}
	if ids["spark01:w2"] != "" {
		t.Fatalf("the cancelled waiter must not be admitted: %v", ids)
	}
	if ids["spark01:w3"] == "" {
		t.Fatal("the waiter behind the cancelled one must be admitted")
	}
}

// A graph row that is not a real object is refused, never admitted.
func TestComfyAcquireRefusesMalformedGraph(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	env.engine.mu.Lock()
	env.engine.graph = "not-an-object"
	env.engine.mu.Unlock()
	if _, err := env.coord.ComfyAcquire("spark02", "image-job", false); err == nil {
		t.Fatal("a malformed graph row must fail admission")
	}
	if leaseCount(env.coord) != 0 {
		t.Fatalf("no lease may survive a malformed graph: %d", leaseCount(env.coord))
	}
}

// ---------------------------------------------------------------------------
// Deleted/disabled model unload through the durable record. All physical
// effects are fakes: the device probe is fakeDevice, commands run through
// fakeRunner plus a sudo stub, and the frozen backend port (127.0.0.1:8001)
// is a closed port in tests.
// ---------------------------------------------------------------------------

// recordEnv creates a testEnv plus a valid durable record for qwen whose
// recipe is then deleted from the central config.
func recordEnv(t *testing.T) *testEnv {
	env := newTestEnv(t)
	cfg := env.rt.config()
	m := obj(obj(cfg["models"])["qwen"])
	s := obj(obj(m["settings"])["spark02"])
	rec, err := env.rt.BeginOperation("qwen", m, s, []string{"spark02"}, "spark02")
	if err != nil {
		t.Fatal(err)
	}
	gen := str(rec["generation"])
	if err := env.rt.TransitionState("qwen", gen, StateStarting); err != nil {
		t.Fatal(err)
	}
	if err := env.rt.TransitionState("qwen", gen, StateReady); err != nil {
		t.Fatal(err)
	}
	// The recipe disappears from the current central config. The central
	delete(obj(cfg["models"]), "qwen")
	// config must stay VALID after the deletion: the shared allowlist may
	// only name configured language models, and an empty allowlist is only
	// valid in exclusive mode. The validator is never weakened — the frozen
	// record retains the original whole cfg/policy.
	shared := []any{}
	for _, n := range stringsOf(obj(cfg["policy"])["comfyui_shared_llms"]) {
		if n != "qwen" {
			shared = append(shared, n)
		}
	}
	obj(cfg["policy"])["comfyui_shared_llms"] = shared
	if len(shared) == 0 {
		obj(cfg["policy"])["comfyui_exclusive_with_llms"] = true
	}
	if err := writeCluster(env.rt.Path, cfg); err != nil {
		t.Fatal(err)
	}
	env.rt.invalidate()
	// Accept parent/unit stop and frozen rank probes without spawning
	// anything; everything else (node probes) goes through the fakeRunner.
	base := env.rt.Run
	env.rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		// ssh args carry a shellJoin'ed remote script: strip the single
		// quotes so the property probes below match the REAL command
		// contract (the same normalization the CLI/Device fakes use).
		j := strings.ReplaceAll(strings.Join(args, " "), "'", "")
		switch {
		case args[0] == "sudo":
			return CommandResult{Code: 0}, nil
		case args[0] == "ssh" && strings.Contains(j, "systemctl"):
			// Rank probes: the unit must be provably free.
			if strings.Contains(j, "-p LoadState") {
				return CommandResult{Out: "LoadState=loaded\nActiveState=inactive\n", Code: 0}, nil
			}
			return CommandResult{Code: 0}, nil
		case args[0] == "systemctl" && strings.Contains(j, "-p ActiveState --value"):
			// The supervisor show is answered exactly: inactive.
			return CommandResult{Out: "inactive\n", Code: 0}, nil
		}
		return base(ctx, args, input)
	}
	if err := env.coord.refresh(); err != nil {
		t.Fatal(err)
	}
	return env
}

// TestUnloadDeletedModelUsesDurableRecord: a model deleted from the current
// config is still unloadable through its valid durable record, and the
// record is honestly stopped. Without a record the unload is refused.
func TestUnloadDeletedModelUsesDurableRecord(t *testing.T) {
	env := recordEnv(t)
	out, err := env.coord.Unload("qwen", false)
	if err != nil {
		t.Fatalf("a deleted model with a valid record must unload: %v", err)
	}
	if str(out["status"]) != "unloaded" {
		t.Fatalf("unexpected result: %v", out)
	}
	rec, err := env.rt.readRecord("qwen")
	if err != nil || str(rec["state"]) != StateStopped {
		t.Fatalf("the record must be stopped: %v %v", rec, err)
	}
	// No record and no current recipe: refused.
	if _, err := env.coord.Unload("ghost", false); err == nil {
		t.Fatal("an unknown workload without a record must be refused")
	}
}

// TestUnloadProtectedRecordRefused: keep_loaded in the frozen record config
// protects a deleted model exactly like a current model.
func TestUnloadProtectedRecordRefused(t *testing.T) {
	env := recordEnv(t)
	rec, err := env.rt.readRecord("qwen")
	if err != nil {
		t.Fatal(err)
	}
	obj(obj(obj(rec["config"])["models"])["qwen"])["keep_loaded"] = true
	if err := writeObject(env.rt.recordPath("qwen"), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := env.coord.Unload("qwen", false); err == nil || !strings.Contains(err.Error(), "must stay loaded") {
		t.Fatalf("a protected record must refuse the unload, got %v", err)
	}
}

// TestForceUnloadDeletedModelUsesDurableRecord: the forced path resolves a
// deleted model through its valid record too and stops the recorded
// generation; no protected or image-job rule is bypassed.
func TestForceUnloadDeletedModelUsesDurableRecord(t *testing.T) {
	env := recordEnv(t)
	out, err := env.coord.Unload("qwen", true)
	if err != nil {
		t.Fatalf("a forced unload of a deleted model with a record must work: %v", err)
	}
	if str(out["status"]) != "unloaded" || !boolean(out["forced"]) {
		t.Fatalf("unexpected result: %v", out)
	}
	rec, err := env.rt.readRecord("qwen")
	if err != nil || str(rec["state"]) != StateStopped {
		t.Fatalf("the record must be stopped: %v %v", rec, err)
	}
}

// TestForceProtectedRecordRefused: --force must not bypass a protected
// record either.
func TestForceProtectedRecordRefused(t *testing.T) {
	env := recordEnv(t)
	rec, err := env.rt.readRecord("qwen")
	if err != nil {
		t.Fatal(err)
	}
	obj(obj(obj(rec["config"])["models"])["qwen"])["keep_loaded"] = true
	if err := writeObject(env.rt.recordPath("qwen"), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := env.coord.Unload("qwen", true); err == nil || !strings.Contains(err.Error(), "must stay loaded") {
		t.Fatalf("a protected record must refuse even --force, got %v", err)
	}
}

// TestLoadRefusesDeletedProtectedLoadedModel: a loaded model deleted from
// the central config but protected by its FROZEN record keep_loaded must
// refuse an explicit load of a conflicting other model — without any
// unload, start or eviction, and the deleted model's record stays ready.
// The frozen protected LLM is never stopped just because its current
// config entry is gone.
func TestLoadRefusesDeletedProtectedLoadedModel(t *testing.T) {
	env := newTestEnv(t)
	cfg := env.rt.config()
	// The old model is protected and its durable record is ready on
	// spark02; the explicitly loaded model is moved onto the same device.
	obj(obj(cfg["models"])["qwen"])["keep_loaded"] = true
	obj(obj(cfg["models"])["nemotron"])["run_on"] = "spark02"
	if err := writeCluster(env.rt.Path, cfg); err != nil {
		t.Fatal(err)
	}
	// The runtime must be configured with the SAME valid config that was
	// written to disk BEFORE the operation begins, so the frozen record
	// captures keep_loaded=true (otherwise the whole stale env config is
	// stored). The coordinator refresh uses the same config.
	env.rt.configure(cfg)
	if err := env.coord.refresh(); err != nil {
		t.Fatal(err)
	}
	m := obj(obj(cfg["models"])["qwen"])
	s := obj(obj(m["settings"])["spark02"])
	rec, err := env.rt.BeginOperation("qwen", m, s, []string{"spark02"}, "spark02")
	if err != nil {
		t.Fatal(err)
	}
	gen := str(rec["generation"])
	// The frozen record must have captured the actual keep_loaded=true
	// policy that was selected for this operation.
	if !boolean(obj(obj(obj(rec["config"])["models"])["qwen"])["keep_loaded"]) {
		t.Fatalf("the frozen record must contain keep_loaded=true: %v", rec)
	}
	if err := env.rt.TransitionState("qwen", gen, StateStarting); err != nil {
		t.Fatal(err)
	}
	if err := env.rt.TransitionState("qwen", gen, StateReady); err != nil {
		t.Fatal(err)
	}
	// The protected recipe disappears from the current central config; the
	// central config must stay VALID after the deletion (the shared
	// allowlist may only name configured language models). The frozen
	// record retains the original whole cfg/policy with keep_loaded=true.
	delete(obj(cfg["models"]), "qwen")
	obj(cfg["policy"])["comfyui_shared_llms"] = []any{}
	obj(cfg["policy"])["comfyui_exclusive_with_llms"] = true
	if err := writeCluster(env.rt.Path, cfg); err != nil {
		t.Fatal(err)
	}
	env.rt.invalidate()
	if err := env.coord.refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := env.coord.Load("nemotron", false); err == nil || !strings.Contains(err.Error(), "must stay loaded") {
		t.Fatalf("a deleted protected loaded model must refuse the conflicting load, got %v", err)
	}
	// Nothing was evicted or started: the protected record is still ready
	// and no new operation exists for the requested model.
	qr, err := env.rt.readRecord("qwen")
	if err != nil || str(qr["state"]) != StateReady || str(qr["generation"]) != gen {
		t.Fatalf("the protected record must stay ready and untouched: %v %v", qr, err)
	}
	if _, err := env.rt.readRecord("nemotron"); !os.IsNotExist(err) {
		t.Fatalf("no operation may be started for the requested model: %v", err)
	}
}

// TestLegacyCoordinatorLoadUsesManagerHealth: a LEGACY deployment's
// Coordinator.Load must dispatch through the real legacy manager path —
// the /upstream/<name>/health activation — and never through the native
// start path: zero systemctl commands, zero lifecycle records.
func TestLegacyCoordinatorLoadUsesManagerHealth(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	delete(cfg, "lifecycle") // a legacy deployment: no native lifecycle.
	health := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/running":
			_ = json.NewEncoder(w).Encode(Object{"running": []any{
				Object{"model": "qwen", "state": "ready"},
			}})
		case "/upstream/qwen/health":
			health <- "hit"
			// The manager activates the model: the running marker and the
			// ready state are written by the real manager on activation.
			if err := writeObject(filepath.Join(root, "controller/running-qwen.json"),
				Object{"nodes": []any{"spark02"}, "units": Object{"spark02": "qwen.service"}}); err != nil {
				t.Error(err)
			}
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg["manager_url"] = srv.URL
	if err := os.MkdirAll(filepath.Join(root, "vllm"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vllm/api.env"), []byte("VLLM_API_KEY=test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// A manager config that already matches the desired sync: no HUP, no
	// llama-swap validation run is ever needed.
	manifest := "globalTTL: 0\n" +
		"routing:\n  router:\n    use: matrix\n    settings:\n      matrix:\n        sets:\n          placement_1: \"qwen & nemotron\"\n" +
		"models:\n" +
		"  qwen:\n    proxy: http://127.0.0.1:8001\n    concurrencyLimit: 8\n    ttl: 0\n    capabilities:\n      context: 123456\n" +
		"  nemotron:\n    proxy: http://127.0.0.1:8001\n    concurrencyLimit: 8\n    ttl: 0\n    capabilities:\n      context: 123456\n"
	if err := os.MkdirAll(filepath.Join(root, "manager"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manager/config.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "models.json")
	if err := writeCluster(path, cfg); err != nil {
		t.Fatal(err)
	}
	dev := &fakeDevice{Mem: 23 * gib, Units: map[string]string{}, Reachable: true}
	rt := newRuntime(cfg, path)
	rt.ProbeNode = dev.probe
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		j := strings.Join(args, " ")
		if strings.Contains(j, "systemctl") || strings.Contains(j, "lifecycle") || strings.Contains(j, "llama-swap") || strings.Contains(j, "reset-failed") {
			return CommandResult{}, fail("a legacy load must never run a native or manager command: %v", args)
		}
		if args[0] == "ssh" {
			probe, _ := dev.probe(nil)
			return CommandResult{Out: string(jsonBytes(probe))}, nil
		}
		return CommandResult{}, fmt.Errorf("unexpected command in test: %v", args)
	}
	coord, err := newCoordinator(path, rt)
	if err != nil {
		t.Fatal(err)
	}
	out, err := coord.Load("qwen", false)
	if err != nil {
		t.Fatalf("a legacy load must succeed through the manager path: %v", err)
	}
	if str(out["status"]) != "ready" {
		t.Fatalf("unexpected result: %v", out)
	}
	select {
	case <-health:
	default:
		t.Fatal("the manager /upstream/qwen/health activation was never called")
	}
	// ZERO native lifecycle records were ever created.
	if _, err := os.Stat(filepath.Join(root, "controller/lifecycle")); !os.IsNotExist(err) {
		t.Fatalf("a legacy load must never create a lifecycle record: %v", err)
	}
}
