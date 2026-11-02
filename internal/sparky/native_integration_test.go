package sparky

// One real cross-component native lifecycle round trip. The ONLY faked
// things are the transports: the fake Runner models real systemd/SSH
// commands and FAILS on any unexpected command, and the head rank binds a
// real local HTTP backend on a reserved loopback port. Every control-flow
// component is the REAL production code: the coordinator producer writes
// the durable record with the whole frozen config, the REAL superviseNative
// is launched as the model supervisor (own context, real OS flock locks,
// real rank starts through the fake transport, and IT writes the READY
// record itself — never a fake READY), the real controllerServer serves the
// chat request against the canonical backend and cleans up the lease, a
// SECOND coordinator adopts the running model without any second rank
// start or generation, and the real stop/cleanup hooks tear everything
// down. No SSH, no systemd, no remote hosts, no credentials.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	integrationModel    = "glm"
	integrationHead     = "glm-head.service"
	integrationWorker   = "glm-worker.service"
	integrationParakeet = "parakeet"
	integrationBothOnly = "bothonly"
	integrationBothHead = "bothonly@both.service"
	integrationBothWork = "bothonly@both.service"
)

// integrationEnv is the whole fake world: central file under a temp
// canonical ROOT/config/models.json, a reserved loopback backend port that
// starts CLOSED, shared fake unit state under a mutex, and the real
// superviseNative launched through the factory seam.
type integrationEnv struct {
	t    *testing.T
	root string
	path string
	port int

	mu          sync.Mutex
	running     map[string]bool
	startCounts map[string]int
	stopCounts  map[string]int
	commands    [][]string
	supLaunches int
	chatHits    int
	// singlePort is the DISTINCT backend the one-shot spark01 recipe binds
	// (0 until writeCentralPlacements reserves it).
	singlePort int
	// singleHits counts requests served by the one-shot backend.
	singleHits int

	ln  net.Listener
	srv *http.Server
	// singleLn/singleSrv are the one-shot backend's own listener/server.
	singleLn  net.Listener
	singleSrv *http.Server
	supCancel context.CancelFunc
	supDone   chan struct{}
	supErr    error
}

func newIntegrationEnv(t *testing.T) *integrationEnv {
	t.Helper()
	root := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // free again: the preflight must see the port refused
	e := &integrationEnv{
		t: t, root: root, port: port,
		path:        filepath.Join(root, "config", "models.json"),
		running:     map[string]bool{},
		startCounts: map[string]int{},
		stopCounts:  map[string]int{},
	}
	e.writeCentral()
	if err := os.MkdirAll(filepath.Join(root, "controller", "lifecycle"), 0700); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *integrationEnv) backend() string {
	return fmt.Sprintf("http://127.0.0.1:%d", e.port)
}

// writeCentral writes the canonical cluster fixture: two nodes, one
// canonical glm LLM with a DISTINCT human display name, a valid protected
// Parakeet audio model with fake metadata, and auto routing to glm.
func (e *integrationEnv) writeCentral() {
	units := Object{"spark01": integrationHead, "spark02": integrationWorker}
	// A REAL recipe env file on disk: the frozen context_tokens binding must
	// actually match an existing file when the supervisor renders the head
	// node locally. The real render rewrites it in place.
	envPath := filepath.Join(e.root, "config", "recipe.env")
	// The config dir is created later in this function; the env fixture
	// must not fail before that, so ensure the parent dir exists first.
	if err := os.MkdirAll(filepath.Dir(envPath), 0700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("CONTEXT_TOKENS=4096\n"), 0600); err != nil {
		e.t.Fatal(err)
	}
	binding := func() Object {
		return Object{
			"path":   envPath,
			"format": "env",
			"fields": Object{"context_tokens": "CONTEXT_TOKENS"},
		}
	}
	recipe := func() Object {
		return Object{
			"enabled": true, "backend_url": e.backend(), "units": units,
			"context_tokens": 8192, "concurrency": 2, "parameters": Object{},
			"parameter_bindings": Object{
				"spark01": []any{binding()},
				"spark02": []any{binding()},
			},
			"startup_order": []any{"spark01", "spark02"},
		}
	}
	parakeetSettings := func(on string) Object {
		return Object{"enabled": true, "containers": []any{"parakeet-asr"}}
	}
	cfg := Object{
		"version": 1, "root": e.root, "controller_port": 8020, "lifecycle": "native",
		"nodes": Object{
			"spark01": Object{"direct_ip": "10.0.0.1", "comfy_unit": "comfy.service"},
			"spark02": Object{"direct_ip": "10.0.0.2", "ssh": "spark02h", "comfy_unit": "comfy.service"},
		},
		"policy": Object{
			"interrupt_active_jobs": false, "protected_models": []any{integrationParakeet},
			"comfyui_exclusive_with_llms": true, "comfyui_shared_llms": []any{},
			"comfyui_min_available_gib": 18,
		},
		"models": Object{
			integrationModel: Object{
				"kind": "llm", "name": "GLM 4.7 (canonical glm identity)", "run_on": "both",
				"keep_loaded": false, "reasoning": true, "aliases": []any{},
				"settings": Object{"both": recipe()},
			},
			integrationParakeet: Object{
				"kind": "audio", "name": "Protected Parakeet", "run_on": "spark01",
				"keep_loaded": true, "reasoning": false, "aliases": []any{},
				"id": "parakeet-92", "started_at": 1720000000,
				"settings": Object{
					"spark01": parakeetSettings("spark01"),
					"spark02": parakeetSettings("spark02"),
				},
			},
		},
		"auto": Object{"targets": []any{integrationModel}, "default": integrationModel},
	}
	if err := os.MkdirAll(filepath.Dir(e.path), 0700); err != nil {
		e.t.Fatal(err)
	}
	if err := writeCluster(e.path, cfg); err != nil {
		e.t.Fatal(err)
	}
}

// writeCentralPlacements rewrites the canonical glm fixture with BOTH a
// single-node spark01 recipe and a two-rank "both" recipe while the SAVED
// run_on stays "both". It is the fixture for the one-shot `--on spark01`
// integration proof: the single-node recipe runs one rank and serves the
// same shared backend identity.
func (e *integrationEnv) writeCentralPlacements() {
	cfg, err := readCluster(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	m := obj(obj(cfg["models"])[integrationModel])
	both := obj(obj(m["settings"])["both"])
	// DIFFERENT backend identities per placement: the frozen one-shot
	// spark01 recipe binds its own port, so admission/readiness must use
	// the ACTUAL record's backend, never the saved both recipe's.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	e.mu.Lock()
	e.singlePort = probe.Addr().(*net.TCPAddr).Port
	e.mu.Unlock()
	_ = probe.Close()
	single := Object{
		"enabled": true, "backend_url": fmt.Sprintf("http://127.0.0.1:%d", e.singlePort),
		"units": Object{"spark01": integrationHead},
		// The one-shot recipe reuses the frozen spark01 parameter binding:
		// the supervisor's render check must match on spark01.
		"parameter_bindings": Object{"spark01": obj(both["parameter_bindings"])["spark01"]},
		"context_tokens":     8192, "concurrency": 2, "parameters": Object{},
		"startup_order": []any{"spark01"},
	}
	m["settings"] = Object{"both": both, "spark01": single}
	if err := writeCluster(e.path, cfg); err != nil {
		e.t.Fatal(err)
	}
}

// writeCentralBothOnly adds a BOTH-ONLY language model (a two-rank recipe
// with a head unit on spark01 and a worker unit on spark02, the same unit
// basename on both nodes) to the canonical fixture. It has no single-node
// recipe, so a one-shot --on spark01/spark02 must be refused before any
// cancel, stop or start.
func (e *integrationEnv) writeCentralBothOnly() {
	cfg, err := readCluster(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	// The canonical glm fixture becomes both-only (the one-shot move tests
	// rewrite it themselves). Its saved run_on stays both.
	obj(obj(cfg["models"])[integrationModel])["settings"] = Object{"both": func() Object {
		m := obj(obj(cfg["models"])[integrationModel])
		return copyObject(obj(obj(m["settings"])["both"]))
	}()}
	envPath := filepath.Join(e.root, "config", "recipe.env")
	binding := func() []any {
		return []any{Object{"path": envPath, "format": "env",
			"fields": Object{"context_tokens": "CONTEXT_TOKENS"}}}
	}
	obj(cfg["models"])[integrationBothOnly] = Object{
		"kind": "llm", "name": "Both Only", "run_on": "both",
		"keep_loaded": false, "reasoning": true, "aliases": []any{},
		"settings": Object{"both": Object{
			"enabled": true, "backend_url": e.backend(),
			"units":              Object{"spark01": integrationBothHead, "spark02": integrationBothWork},
			"parameter_bindings": Object{"spark01": binding(), "spark02": binding()},
			"context_tokens":     8192, "concurrency": 2, "parameters": Object{},
			"startup_order": []any{"spark01", "spark02"},
		}},
	}
	if err := writeCluster(e.path, cfg); err != nil {
		e.t.Fatal(err)
	}
}

// commandsSnapshot copies the fake transport's command log.
func (e *integrationEnv) commandsSnapshot() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]string, len(e.commands))
	for i, args := range e.commands {
		out[i] = append([]string{}, args...)
	}
	return out
}

// parakeetMeta returns the frozen protected Parakeet fake metadata.
func (e *integrationEnv) parakeetMeta(t *testing.T) string {
	t.Helper()
	cfg, err := readCluster(e.path)
	if err != nil {
		t.Fatal(err)
	}
	return canonicalValue(obj(cfg["models"])[integrationParakeet])
}

// runtime builds a REAL runtime wired to the fake transports. Used for both
// coordinators; the supervisor gets the same seams through the factory.
func (e *integrationEnv) runtime() *Runtime {
	cfg, err := readCluster(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	rt := newRuntime(cfg, e.path)
	rt.Run = e.runner()
	rt.ProbeNode = func(meta Object) (Object, error) { return e.probe(meta), nil }
	return rt
}

func (e *integrationEnv) installFactory() {
	e.t.Helper()
	saved := nativeRuntimeFactory
	e.t.Cleanup(func() { nativeRuntimeFactory = saved })
	nativeRuntimeFactory = func(c Object, p string) *Runtime {
		rt := newRuntime(c, p)
		rt.Run = e.runner()
		rt.ProbeNode = func(meta Object) (Object, error) { return e.probe(meta), nil }
		return rt
	}
}

// probe returns a fully idle node inventory: every unit inactive (a
// confirmed INACTIVE comfy unit), no ComfyUI work, plenty of memory and no
// containers running.
func (e *integrationEnv) probe(meta Object) Object {
	units := Object{}
	for u := range obj(meta["units"]) {
		units[u] = "inactive"
	}
	if cu := str(meta["comfy_unit"]); cu != "" {
		units[cu] = "inactive"
	}
	meta["units"] = units
	meta["comfyui"] = Object{"executing": false, "running": 0, "phase": "idle"}
	meta["memory_bytes"] = Object{"MemAvailable": 1 << 40}
	meta["containers"] = Object{}
	return meta
}

func (e *integrationEnv) record(command []string) {
	e.mu.Lock()
	e.commands = append(e.commands, command)
	e.mu.Unlock()
}

func (e *integrationEnv) isRunning(unit string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[unit]
}

// startRank models a real rank start: the unit becomes active and the head
// rank binds the REAL shared backend on the reserved loopback port.
func (e *integrationEnv) startRank(unit string) error {
	e.mu.Lock()
	e.startCounts[unit]++
	e.running[unit] = true
	ln := e.ln
	e.mu.Unlock()
	if unit == integrationHead && ln == nil {
		e.startBackend()
		e.startSingleBackend()
	}
	return nil
}

// stopRank models a real rank stop: the unit goes inactive and, when no
// rank runs anymore, the backend port is closed again.
func (e *integrationEnv) stopRank(unit string) {
	e.mu.Lock()
	e.stopCounts[unit]++
	e.running[unit] = false
	ln := e.ln
	srv := e.srv
	idle := true
	for _, r := range e.running {
		if r {
			idle = false
		}
	}
	if idle {
		e.ln = nil
		e.srv = nil
	}
	e.mu.Unlock()
	// A real process stop kills ALL connections: Server.Close closes the
	// listener AND every accepted keep-alive connection, so no stale
	// backend can keep answering after the last rank went inactive.
	if idle && srv != nil {
		_ = srv.Close()
	} else if idle && ln != nil {
		_ = ln.Close()
	}
	if idle {
		e.mu.Lock()
		singleLn, singleSrv := e.singleLn, e.singleSrv
		e.singleLn, e.singleSrv = nil, nil
		e.mu.Unlock()
		if singleSrv != nil {
			_ = singleSrv.Close()
		} else if singleLn != nil {
			_ = singleLn.Close()
		}
	}
}

// startSingleBackend binds the DISTINCT one-shot backend port and serves the
// same canonical model id. It is opened by the head rank start and closed
// when the last rank stops, exactly like the shared backend.
func (e *integrationEnv) startSingleBackend() {
	port := e.singlePort
	if port == 0 {
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.singleHits++
		e.mu.Unlock()
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + integrationModel + `","object":"model"}]}`))
	})
	srv := &http.Server{Handler: mux}
	e.mu.Lock()
	e.singleLn = ln
	e.singleSrv = srv
	e.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
}

// startBackend binds the recipe port and serves /health, the canonical
// model id on /v1/models and a synthetic chat completion — exactly like the
// real vLLM head rank. The port starts CLOSED and is only opened BY the
// head rank start.
func (e *integrationEnv) startBackend() {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(e.port))
	if err != nil {
		return // port taken: readiness will simply never confirm
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + integrationModel + `","object":"model"}]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.chatHits++
		e.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"chat-1","object":"chat.completion","model":"` + integrationModel + `","choices":[{"index":0,"message":{"role":"assistant","content":"synthetic ok"},"finish_reason":"stop"}]}`))
	})
	srv := &http.Server{Handler: mux}
	e.mu.Lock()
	e.ln = ln
	e.srv = srv
	e.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
}

// launchSupervisor verifies the actual queued producer record exists and
// then launches the REAL superviseNative with its own context via the
// factory-installed fake runtime. The supervisor reads the SAME producer
// record, acquires the REAL OS locks, starts the ranks through the fake
// transport and writes READY itself.
func (e *integrationEnv) launchSupervisor() error {
	rt := e.runtime()
	rec, err := rt.readRecord(integrationModel)
	if err != nil {
		return fail("the queued producer record must exist before the supervisor is launched: %v", err)
	}
	if str(rec["state"]) != StateQueued {
		return fail("the supervisor must only be launched for a queued record, got %s", str(rec["state"]))
	}
	e.mu.Lock()
	e.supLaunches++
	e.supErr = nil
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	e.supCancel = cancel
	e.supDone = done
	e.mu.Unlock()
	go func() {
		err := superviseNative(ctx, e.path, "run", integrationModel)
		e.mu.Lock()
		e.supErr = err // recorded BEFORE done fires
		e.mu.Unlock()
		close(done) // CLOSED, never sent to: the completion is reusable
	}()
	return nil
}

// waitSupFinished waits (bounded) for the real supervisor goroutine to
// finish WITHOUT consuming its completion: supDone is CLOSED by the
// goroutine (never sent to) and supErr is stored under the mutex, so the
// completion signal stays observable by any number of later waiters and
// observing the error can never consume the completion.
func (e *integrationEnv) waitSupFinished(timeout time.Duration) error {
	e.mu.Lock()
	done := e.supDone
	e.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
	case <-time.After(timeout):
		return fail("the real supervisor goroutine did not finish within %s", timeout)
	}
	e.mu.Lock()
	err := e.supErr // set before done was closed
	e.mu.Unlock()
	if err != nil {
		return fail("supervisor: %v", err)
	}
	return nil
}

// supervisorFailure returns the real supervisor goroutine's own error (if
// any) after waiting for it to finish, so a generic parent verdict is never
// reported without the goroutine's cause.
func (e *integrationEnv) supervisorFailure() string {
	if err := e.waitSupFinished(10 * time.Second); err != nil {
		return err.Error()
	}
	return ""
}

// emulateStop runs the REAL native stop action (actual stop/cleanup hooks:
// marker, real rank stops through the fake transport, real confirmation and
// CAS to stopped), then signals the supervisor context and waits for the
// REAL run function to finish. No fake record writes.
func (e *integrationEnv) emulateStop() error {
	err := superviseNative(context.Background(), e.path, "stop", integrationModel)
	e.mu.Lock()
	cancel := e.supCancel
	e.supCancel = nil
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// supDone is reusable: waiting again later observes the SAME closed
	// channel instead of blocking on an empty consumed one.
	if werr := e.waitSupFinished(30 * time.Second); werr != nil {
		return werr
	}
	return err
}

// joinSupervisor is the GUARANTEED cleanup: it cancels the supervisor
// context and JOINS the real goroutine (bounded) before any global
// (monitor interval, native runtime factory) is restored, so a still-running
// superviseNative can never race a restored nativeMonitorInterval. It also
// makes a best-effort attempt to stop any rank still recorded running and
// to close the shared backend port. Registered as the LAST t.Cleanup so
// LIFO teardown runs it FIRST on every exit path, including any t.Fatal.
func (e *integrationEnv) joinSupervisor() {
	e.mu.Lock()
	cancel := e.supCancel
	e.supCancel = nil
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	_ = e.waitSupFinished(5 * time.Second)
	for _, unit := range []string{integrationHead, integrationWorker} {
		if e.isRunning(unit) {
			e.stopRank(unit)
		}
	}
	e.mu.Lock()
	srv := e.srv
	ln := e.ln
	singleSrv, singleLn := e.singleSrv, e.singleLn
	e.srv, e.ln, e.singleSrv, e.singleLn = nil, nil, nil, nil
	e.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	} else if ln != nil {
		_ = ln.Close()
	}
	if singleSrv != nil {
		_ = singleSrv.Close()
	} else if singleLn != nil {
		_ = singleLn.Close()
	}
}

// runner is the fake transport: it models real systemd and SSH commands and
// FAILS LOUDLY on anything unexpected. There is no docker and no audio.
func (e *integrationEnv) runner() Runner {
	return func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		e.record(args)
		j := strings.Join(args, " ")
		switch {
		case args[0] == "sudo" && len(args) >= 4 && args[2] == "systemctl":
			action, unit := args[3], args[len(args)-1]
			switch {
			case action == "reset-failed":
				return CommandResult{Code: 0}, nil
			case action == "start" && unit == "sparky-model@"+integrationModel+".service":
				if err := e.launchSupervisor(); err != nil {
					return CommandResult{}, err
				}
				return CommandResult{Code: 0}, nil
			case action == "stop" && unit == "sparky-model@"+integrationModel+".service":
				if err := e.emulateStop(); err != nil {
					return CommandResult{}, err
				}
				return CommandResult{Code: 0}, nil
			case action == "start":
				return CommandResult{}, e.startRank(unit)
			case action == "stop":
				e.stopRank(unit)
				return CommandResult{Code: 0}, nil
			}
		case args[0] == "systemctl" && args[1] == "show" && strings.Contains(j, "--value"):
			// The model supervisor unit is exactly active while the real
			// superviseNative function is running.
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.supCancel != nil {
				return CommandResult{Out: "active\n", Code: 0}, nil
			}
			return CommandResult{Out: "inactive\n", Code: 0}, nil
		case args[0] == "systemctl" && args[1] == "show":
			// Keyed LoadState=/ActiveState= probe of a local rank unit.
			unit := args[2]
			e.mu.Lock()
			defer e.mu.Unlock()
			return CommandResult{Out: "LoadState=loaded\nActiveState=" + activeWord(e.running[unit]) + "\n", Code: 0}, nil
		case args[0] == "systemctl" && args[1] == "is-active":
			if e.isRunning(args[len(args)-1]) {
				return CommandResult{Code: 0}, nil
			}
			return CommandResult{Code: 3}, nil
		case args[0] == "ssh":
			// shellJoin single-quotes every argument, so the joined remote
			// command must be unquoted before substring matching; otherwise
			// even a correctly issued start/stop is never dispatched.
			last := strings.ReplaceAll(args[len(args)-1], "'", "")
			switch {
			case strings.Contains(last, "node render"):
				// Execute the actual render payload with the real
				// renderer: a payload the real renderer refuses fails
				// the start, exactly like a real worker.
				spec, derr := decode(input)
				if derr != nil {
					return CommandResult{}, derr
				}
				if _, rerr := RenderParameters(spec); rerr != nil {
					return CommandResult{}, rerr
				}
				return CommandResult{Code: 0}, nil
			case strings.Contains(last, "probe --file"):
				var meta Object
				if err := json.Unmarshal(input, &meta); err != nil {
					return CommandResult{}, err
				}
				return CommandResult{Out: string(jsonBytes(e.probe(meta))), Code: 0}, nil
			case strings.Contains(last, "LoadState"):
				unit := remoteShowUnit(last)
				e.mu.Lock()
				defer e.mu.Unlock()
				return CommandResult{Out: "LoadState=loaded\nActiveState=" + activeWord(e.running[unit]) + "\n", Code: 0}, nil
			case strings.Contains(last, "is-active"):
				unit := remoteUnit(last)
				if e.isRunning(unit) {
					return CommandResult{Code: 0}, nil
				}
				return CommandResult{Code: 3}, nil
			case strings.Contains(last, " start "):
				return CommandResult{}, e.startRank(remoteUnit(last))
			case strings.Contains(last, " stop "):
				e.stopRank(remoteUnit(last))
				return CommandResult{Code: 0}, nil
			}
		}
		return CommandResult{}, fail("unexpected command in the fake transport: %v", args)
	}
}

func activeWord(running bool) string {
	if running {
		return "active"
	}
	return "inactive"
}

// remoteUnit extracts the unit from a shell-joined remote command line.
func remoteUnit(joined string) string {
	fields := strings.Fields(strings.NewReplacer("'", "", `"`, "").Replace(joined))
	return fields[len(fields)-1]
}

// remoteShowUnit extracts the unit from a remote `systemctl show unit -p
// LoadState -p ActiveState` line: the token right after "show".
func remoteShowUnit(joined string) string {
	fields := strings.Fields(strings.NewReplacer("'", "", `"`, "").Replace(joined))
	for i, f := range fields {
		if f == "show" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func (e *integrationEnv) startCount(unit string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.startCounts[unit]
}

func (e *integrationEnv) supLaunchCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.supLaunches
}

func (e *integrationEnv) rankStartsTotal() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.startCounts {
		n += c
	}
	return n
}

func (e *integrationEnv) dockerOrAudioTouched() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, args := range e.commands {
		j := strings.Join(args, " ")
		if strings.Contains(j, "docker") || strings.Contains(j, "parakeet-asr") {
			return true
		}
	}
	return false
}

func integrationRecord(t *testing.T, e *integrationEnv) Object {
	t.Helper()
	rec, err := e.runtime().readRecord(integrationModel)
	if err != nil {
		t.Fatalf("lifecycle record: %v", err)
	}
	return rec
}

func backendClosed(t *testing.T, port int) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the shared backend port must be closed after the teardown")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("the backend port must be refused, got %v", err)
	}
}

// TestNativeLifecycleIntegrationRoundTrip is the ONE real round trip:
// producer record with the whole frozen config -> real supervisor writes
// READY -> real httptest controller serves garuda/spark-auto chat on the
// canonical backend and cleans the lease -> a second coordinator adopts
// without a second start or generation -> real stop: both ranks inactive,
// port closed, record stopped -> reload gets a NEW generation -> cleanup.
func TestNativeLifecycleIntegrationRoundTrip(t *testing.T) {
	e := newIntegrationEnv(t)
	e.installFactory()
	// Shorten only the available poll seams; nothing else is no-op'd.
	savedInterval := nativeMonitorInterval
	nativeMonitorInterval = 5 * time.Millisecond
	t.Cleanup(func() { nativeMonitorInterval = savedInterval })
	// Registered LAST so the LIFO teardown JOINS the real supervisor
	// goroutine FIRST, before the monitor interval or the runtime factory
	// global are restored. t.Cleanup runs on every exit path, so any
	// t.Fatal still tears down the goroutine before the globals move.
	t.Cleanup(func() { e.joinSupervisor() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cfgBefore := e.runtime().Config
	parakeetBefore := e.parakeetMeta(t)

	// --- Producer: the real coordinator Load writes the queued record with
	// the WHOLE frozen config, then the REAL supervisor makes it READY.
	c, err := newCoordinator(e.path, e.runtime())
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	res, err := c.load(ctx, integrationModel, false)
	if err != nil {
		if s := e.supervisorFailure(); s != "" {
			t.Fatalf("the real native load must succeed: %v (%s)", err, s)
		}
		t.Fatalf("the real native load must succeed: %v", err)
	}
	if str(res["status"]) != "ready" || str(res["workload"]) != integrationModel {
		t.Fatalf("unexpected load result: %v", res)
	}

	rec := integrationRecord(t, e)
	if str(rec["state"]) != StateReady {
		t.Fatalf("the REAL supervisor must write the READY record itself, got %s", str(rec["state"]))
	}
	if gen := str(rec["generation"]); len(gen) != 32 {
		t.Fatalf("the record needs a durable generation token, got %q", gen)
	}
	// The frozen config snapshot in the record must be the WHOLE central
	// config the coordinator selected at reservation time — parakeet, auto,
	// policy, nodes and all.
	frozen := obj(rec["config"])
	if frozen == nil {
		t.Fatal("the record must carry the frozen config snapshot")
	}
	if canonicalValue(frozen) != canonicalValue(cfgBefore) {
		t.Fatal("the record's frozen config must equal the whole selected coordinator config")
	}
	// The selected recipe snapshot is frozen too. selectPlacement returns
	// the MODEL first and the SETTING second, so capture the setting slot.
	_, sel, nodes, placement, err := selectPlacement(cfgBefore, integrationModel, "")
	if err != nil {
		t.Fatal(err)
	}
	if canonicalValue(obj(rec["setting"])) != canonicalValue(sel) {
		t.Fatal("the record's frozen setting must equal the selected recipe")
	}
	if !sameStrings(stringsOf(rec["nodes"]), nodes) || str(rec["placement"]) != placement {
		t.Fatal("the record must freeze the selected nodes and placement")
	}
	if e.startCount(integrationHead) != 1 || e.startCount(integrationWorker) != 1 {
		t.Fatalf("exactly one start per rank is expected, got %+v", e.startCounts)
	}
	if e.dockerOrAudioTouched() {
		t.Fatal("the native LLM path must never touch docker or the protected audio model")
	}

	// --- Real HTTP controller: POST chat as garuda/spark-auto, routed to
	// the canonical glm backend with lease cleanup.
	s := &controllerServer{ctrl: c}
	ts := httptest.NewServer(s)
	defer ts.Close()
	chatBody := `{"model":"garuda/spark-auto","messages":[{"role":"user","content":"hi"}]}`
	httpRes, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("the real controller chat request failed: %v", err)
	}
	var chat map[string]any
	if err := json.NewDecoder(httpRes.Body).Decode(&chat); err != nil {
		t.Fatalf("chat response decode: %v", err)
	}
	httpRes.Body.Close()
	if httpRes.StatusCode != 200 {
		t.Fatalf("chat must be served, got status %d: %v", httpRes.StatusCode, chat)
	}
	if str(chat["model"]) != integrationModel {
		t.Fatalf("the inference must reach the canonical glm backend, got model %v", chat["model"])
	}
	if e.chatHits == 0 {
		t.Fatal("the real backend must have served the synthetic chat completion")
	}
	if adm := c.Admission(); len(arr(adm["active"])) != 0 {
		t.Fatalf("the inference lease must be cleaned up after the chat, got %v", adm)
	}

	// --- Adoption: a NEW coordinator/runtime over the SAME files while the
	// model supervisor is still running. No second rank start, no new
	// generation, no record rewrite.
	gen1 := str(rec["generation"])
	supLaunches1 := e.supLaunchCount()
	rankStarts1 := e.rankStartsTotal()
	c2, err := newCoordinator(e.path, e.runtime())
	if err != nil {
		t.Fatalf("second coordinator: %v", err)
	}
	res2, err := c2.load(ctx, integrationModel, false)
	if err != nil {
		if s := e.supervisorFailure(); s != "" {
			t.Fatalf("the second coordinator must adopt the running model: %v (%s)", err, s)
		}
		t.Fatalf("the second coordinator must adopt the running model: %v", err)
	}
	if str(res2["status"]) != "ready" {
		t.Fatalf("unexpected adoption result: %v", res2)
	}
	rec2 := integrationRecord(t, e)
	if str(rec2["generation"]) != gen1 {
		t.Fatal("adoption must never mint a new generation")
	}
	if str(rec2["state"]) != StateReady {
		t.Fatalf("the adopted record must still be ready, got %s", str(rec2["state"]))
	}
	if e.supLaunchCount() != supLaunches1 {
		t.Fatal("adoption must never launch a second model supervisor")
	}
	if got := e.rankStartsTotal(); got != rankStarts1 {
		t.Fatalf("adoption must never start a rank again: %d != %d", got, rankStarts1)
	}
	// Adoption must also serve the chat path without any manager call.
	httpRes2, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("adopted chat failed: %v", err)
	}
	httpRes2.Body.Close()
	if httpRes2.StatusCode != 200 {
		t.Fatalf("the adopted coordinator must serve chat, got %d", httpRes2.StatusCode)
	}

	// --- Real stop: the actual native stop hooks run, the real run function
	// finishes, both ranks go inactive and the backend port closes.
	if _, err := c.unload(ctx, integrationModel, false); err != nil {
		t.Fatalf("the real native unload must succeed: %v", err)
	}
	rec3 := integrationRecord(t, e)
	if str(rec3["state"]) != StateStopped {
		t.Fatalf("the record must be stopped after the unload, got %s", str(rec3["state"]))
	}
	if str(rec3["generation"]) != gen1 {
		t.Fatal("the stop must not mint or change a generation")
	}
	if e.isRunning(integrationHead) || e.isRunning(integrationWorker) {
		t.Fatal("both recorded ranks must be inactive after the stop")
	}
	backendClosed(t, e.port)
	if e.dockerOrAudioTouched() {
		t.Fatal("the stop must never touch docker or the protected audio model")
	}
	// The protected Parakeet fake metadata is untouched.
	if e.parakeetMeta(t) != parakeetBefore {
		t.Fatal("the protected Parakeet metadata (id/started_at) must be unchanged")
	}

	// --- Reload the SAME fixture model: a NEW generation and a valid ready.
	res3, err := c.load(ctx, integrationModel, false)
	if err != nil {
		if s := e.supervisorFailure(); s != "" {
			t.Fatalf("the reload must succeed: %v (%s)", err, s)
		}
		t.Fatalf("the reload must succeed: %v", err)
	}
	if str(res3["status"]) != "ready" {
		t.Fatalf("unexpected reload result: %v", res3)
	}
	rec4 := integrationRecord(t, e)
	gen2 := str(rec4["generation"])
	if gen2 == gen1 || len(gen2) != 32 {
		t.Fatalf("the reload must mint a NEW durable generation, got %q (was %q)", gen2, gen1)
	}
	if str(rec4["state"]) != StateReady {
		t.Fatalf("the reloaded model must be ready again, got %s", str(rec4["state"]))
	}
	if e.startCount(integrationHead) != 2 || e.startCount(integrationWorker) != 2 {
		t.Fatalf("the reload must start each rank exactly once more, got %+v", e.startCounts)
	}

	// --- Cleanup at the end: the real stop plus the idempotent cleanup for
	// its own generation.
	if err := e.emulateStop(); err != nil {
		t.Fatalf("the final real stop failed: %v", err)
	}
	if err := superviseNative(ctx, e.path, "cleanup", integrationModel); err != nil {
		t.Fatalf("the final cleanup for the current generation failed: %v", err)
	}
	if st := integrationRecord(t, e); str(st["state"]) != StateStopped {
		t.Fatalf("the record must remain stopped after cleanup, got %s", str(st["state"]))
	}
	if e.isRunning(integrationHead) || e.isRunning(integrationWorker) {
		t.Fatal("no rank may run after the final cleanup")
	}
	backendClosed(t, e.port)
}

// TestNativeLoadOnIntegrationOneShotPlacement is the real round trip for
// the one-shot `--on spark01` override: the coordinator freezes a spark01
// recipe, the REAL supervisor starts exactly the spark01 rank and writes
// READY under the spark01 placement; the saved run_on stays "both" and a
// later plain load applies the saved default (both). Admission and the
// plan preview follow the ACTUAL loaded nodes, and a plain load while the
// one-shot placement is ready reports keep_running rather than a restart.
func TestNativeLoadOnIntegrationOneShotPlacement(t *testing.T) {
	e := newIntegrationEnv(t)
	e.writeCentralPlacements()
	e.installFactory()
	savedInterval := nativeMonitorInterval
	nativeMonitorInterval = 5 * time.Millisecond
	t.Cleanup(func() { nativeMonitorInterval = savedInterval })
	t.Cleanup(func() { e.joinSupervisor() })

	savedRunOn := func() string {
		cfg, err := readCluster(e.path)
		if err != nil {
			t.Fatal(err)
		}
		return str(obj(obj(cfg["models"])[integrationModel])["run_on"])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, err := newCoordinator(e.path, e.runtime())
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}

	// The plan preview validates the actual recipe and previews spark01
	// before anything changes.
	plan, err := c.plan(ctx, integrationModel, "load", "spark01")
	if err != nil {
		t.Fatalf("plan --on spark01: %v", err)
	}
	if str(plan["placement"]) != "spark01" || !sameStrings(stringsOf(plan["nodes"]), []string{"spark01"}) {
		t.Fatalf("plan must preview the one-shot placement: %v", plan)
	}
	// The READ-ONLY plan advertises the override capability the node
	// preflight requires before sending an explicit load.
	if raw, has := plan["placement_override_supported"]; !has || raw != true {
		t.Fatalf("plan must advertise placement_override_supported: %v", plan)
	}
	if got := savedRunOn(); got != "both" {
		t.Fatalf("the plan must never rewrite run_on: %q", got)
	}

	res, err := c.Load(integrationModel, false, "spark01")
	if err != nil {
		if s := e.supervisorFailure(); s != "" {
			t.Fatalf("the one-shot load must succeed: %v (%s)", err, s)
		}
		t.Fatalf("the one-shot load must succeed: %v", err)
	}
	if str(res["status"]) != "ready" || str(res["placement"]) != "spark01" {
		t.Fatalf("unexpected one-shot load result: %v", res)
	}
	// The acknowledgment must be the ACTUAL confirmed selection, including
	// the distinct backend the frozen spark01 recipe binds.
	if !sameStrings(stringsOf(res["nodes"]), []string{"spark01"}) ||
		str(res["backend"]) != fmt.Sprintf("http://127.0.0.1:%d", e.singlePort) {
		t.Fatalf("the one-shot load must acknowledge the confirmed selection: %v", res)
	}
	rec := integrationRecord(t, e)
	if str(rec["state"]) != StateReady || str(rec["placement"]) != "spark01" {
		t.Fatalf("the REAL supervisor must write a READY spark01 record, got %v", rec)
	}
	if !sameStrings(stringsOf(rec["nodes"]), []string{"spark01"}) {
		t.Fatalf("the record must freeze only the spark01 node: %v", rec["nodes"])
	}
	if e.startCount(integrationHead) != 1 || e.startCount(integrationWorker) != 0 {
		t.Fatalf("exactly the head rank may start, got %+v", e.startCounts)
	}
	if got := savedRunOn(); got != "both" {
		t.Fatalf("the saved run_on must stay both after a one-shot load: %q", got)
	}

	// Admission follows the ACTUAL frozen nodes.
	nodes, err := c.effectiveNodes(c.cfg(), integrationModel)
	if err != nil || !sameStrings(nodes, []string{"spark01"}) {
		t.Fatalf("admission nodes must follow the frozen record: %v %v", nodes, err)
	}

	// A fresh coordinator (same durable record, new in-memory state) must
	// resolve the ACTUAL spark01 selection and bind the spark01 recipe's
	// OWN backend on inference — never the saved both backend.
	singleHits := func() int { e.mu.Lock(); defer e.mu.Unlock(); return e.singleHits }
	before := singleHits()
	fresh, err := newCoordinator(e.path, e.runtime())
	if err != nil {
		t.Fatalf("fresh coordinator: %v", err)
	}
	name, lease, err := fresh.LLMRequest(ctx, integrationModel)
	if err != nil {
		t.Fatalf("inference after a one-shot load must be admitted: %v", err)
	}
	defer fresh.Release(lease)
	if name != integrationModel {
		t.Fatalf("inferred model: %q", name)
	}
	if got := fresh.leaseBackend(lease); got != fmt.Sprintf("http://127.0.0.1:%d", e.singlePort) {
		t.Fatalf("the lease must bind the ACTUAL spark01 backend, got %q", got)
	}
	if singleHits() <= before {
		t.Fatal("readiness must probe the ACTUAL spark01 backend, not the saved both backend")
	}
	// Auto admission resolves the same actual selection.
	if got, err := fresh.resolveAuto(ctx); err != nil || got != integrationModel {
		t.Fatalf("auto admission: %q %v", got, err)
	}

	// The plain plan targets the SAVED default exactly: both.
	plan2, err := c.plan(ctx, integrationModel, "load", "")
	if err != nil {
		t.Fatalf("plain plan: %v", err)
	}
	if str(plan2["placement"]) != "both" || !sameStrings(stringsOf(plan2["nodes"]), []string{"spark01", "spark02"}) {
		t.Fatalf("plain plan must target the saved both default: %v", plan2)
	}

	// REPEATING the same explicit override while the model is already READY is
	// the native already-ready fastpath: it must acknowledge the SAME actual
	// confirmed selection (placement spark01, nodes [spark01]) and the distinct
	// spark01 backend, and start no new rank.
	startsBefore := len(e.commandsSnapshot())
	again, err := c.Load(integrationModel, false, "spark01")
	if err != nil {
		if s := e.supervisorFailure(); s != "" {
			t.Fatalf("the already-ready override must succeed: %v (%s)", err, s)
		}
		t.Fatalf("the already-ready override must succeed: %v", err)
	}
	if str(again["status"]) != "ready" || str(again["placement"]) != "spark01" ||
		!sameStrings(stringsOf(again["nodes"]), []string{"spark01"}) ||
		str(again["backend"]) != fmt.Sprintf("http://127.0.0.1:%d", e.singlePort) {
		t.Fatalf("the already-ready fastpath must acknowledge the confirmed selection: %v", again)
	}
	for _, args := range e.commandsSnapshot()[startsBefore:] {
		if strings.Contains(strings.Join(args, " "), " start ") {
			t.Fatalf("an already-ready override must start no rank, got %v", args)
		}
	}
	if e.startCount(integrationHead) != 1 || e.startCount(integrationWorker) != 0 {
		t.Fatalf("an already-ready override must not restart a rank, got %+v", e.startCounts)
	}

	// Unload the one-shot placement: the frozen record is stopped and the
	// single rank goes inactive.
	if _, err := c.unload(ctx, integrationModel, false); err != nil {
		t.Fatalf("the one-shot unload must succeed: %v", err)
	}
	if e.isRunning(integrationHead) {
		t.Fatal("the head rank must be inactive after the unload")
	}
	if st := integrationRecord(t, e); str(st["state"]) != StateStopped {
		t.Fatalf("the record must be stopped, got %s", str(st["state"]))
	}

	// A later PLAIN load applies the saved "both" default: both ranks start.
	res3, err := c.load(ctx, integrationModel, false)
	if err != nil {
		if s := e.supervisorFailure(); s != "" {
			t.Fatalf("the plain reload must succeed: %v (%s)", err, s)
		}
		t.Fatalf("the plain reload must succeed: %v", err)
	}
	if str(res3["status"]) != "ready" {
		t.Fatalf("a plain load must succeed: %v", res3)
	}
	rec3 := integrationRecord(t, e)
	if str(rec3["placement"]) != "both" || !sameStrings(stringsOf(rec3["nodes"]), []string{"spark01", "spark02"}) {
		t.Fatalf("the plain load must freeze the saved both placement: %v", rec3)
	}
	if e.startCount(integrationHead) != 2 || e.startCount(integrationWorker) != 1 {
		t.Fatalf("the plain reload must start both ranks once more, got %+v", e.startCounts)
	}

	if err := e.emulateStop(); err != nil {
		t.Fatalf("final stop: %v", err)
	}
	if err := superviseNative(ctx, e.path, "cleanup", integrationModel); err != nil {
		t.Fatalf("final cleanup: %v", err)
	}
	backendClosed(t, e.port)
}

// TestNativeLoadOnIntegrationMoveWithoutUnload proves a one-shot placement
// switch while the previous placement is loaded targets EXACTLY the
// requested nodes: the old generation is stopped and the new spark01
// generation starts only the head rank. Reservation nodes (both) never leak
// into the target recipe, its readiness or its durable record, and the
// saved run_on stays both.
func TestNativeLoadOnIntegrationMoveWithoutUnload(t *testing.T) {
	e := newIntegrationEnv(t)
	e.writeCentralPlacements()
	e.installFactory()
	savedInterval := nativeMonitorInterval
	nativeMonitorInterval = 5 * time.Millisecond
	t.Cleanup(func() { nativeMonitorInterval = savedInterval })
	t.Cleanup(func() { e.joinSupervisor() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, err := newCoordinator(e.path, e.runtime())
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	// Load the saved default (both) first.
	if _, err := c.load(ctx, integrationModel, false); err != nil {
		t.Fatalf("the default load must succeed: %v (%s)", err, e.supervisorFailure())
	}
	rec := integrationRecord(t, e)
	if str(rec["placement"]) != "both" || !sameStrings(stringsOf(rec["nodes"]), []string{"spark01", "spark02"}) {
		t.Fatalf("the default load must freeze both: %v", rec)
	}
	if e.startCount(integrationHead) != 1 || e.startCount(integrationWorker) != 1 {
		t.Fatalf("the default load must start both ranks, got %+v", e.startCounts)
	}

	// A transient spark01 load while both is loaded moves the model.
	res, err := c.Load(integrationModel, false, "spark01")
	if err != nil {
		t.Fatalf("the one-shot move must succeed: %v (%s)", err, e.supervisorFailure())
	}
	if str(res["placement"]) != "spark01" || !sameStrings(stringsOf(res["nodes"]), []string{"spark01"}) {
		t.Fatalf("the move result must report only spark01: %v", res)
	}
	rec = integrationRecord(t, e)
	if str(rec["placement"]) != "spark01" || !sameStrings(stringsOf(rec["nodes"]), []string{"spark01"}) {
		t.Fatalf("the moved record must freeze only spark01: %v", rec)
	}
	if e.isRunning(integrationWorker) {
		t.Fatal("the worker rank must be stopped by the placement move")
	}
	// The head is restarted for the new generation and the worker is never
	// started by the spark01 recipe: exactly one more head start.
	if e.startCount(integrationHead) != 2 || e.startCount(integrationWorker) != 1 {
		t.Fatalf("the move must start only the head once more, got %+v", e.startCounts)
	}
	cfg, err := readCluster(e.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := str(obj(obj(cfg["models"])[integrationModel])["run_on"]); got != "both" {
		t.Fatalf("the saved run_on must stay both: %q", got)
	}
	// Inference is admitted and bound to the spark01 recipe's own backend.
	if _, lease, err := c.LLMRequest(ctx, integrationModel); err != nil {
		t.Fatalf("inference after the move: %v", err)
	} else {
		if got := c.leaseBackend(lease); got != fmt.Sprintf("http://127.0.0.1:%d", e.singlePort) {
			t.Fatalf("the lease must bind the spark01 backend, got %q", got)
		}
		c.Release(lease)
	}

	if _, err := c.unload(ctx, integrationModel, false); err != nil {
		t.Fatalf("the moved unload must succeed: %v", err)
	}
	if e.isRunning(integrationHead) || e.isRunning(integrationWorker) {
		t.Fatal("no rank may run after the moved unload")
	}
	if err := e.emulateStop(); err != nil {
		t.Fatalf("final stop: %v", err)
	}
	if err := superviseNative(ctx, e.path, "cleanup", integrationModel); err != nil {
		t.Fatalf("final cleanup: %v", err)
	}
	backendClosed(t, e.port)
}

// TestNativeLoadOnIntegrationBothOnlyRefused is the real-supervisor proof
// that a BOTH-ONLY model rejects a single-node one-shot placement BEFORE
// any side effect: the plan and both the plain and forced loads are refused,
// the fake transport sees no start/stop/cancel command, no durable record
// exists and the loaded glm model keeps running.
func TestNativeLoadOnIntegrationBothOnlyRefused(t *testing.T) {
	e := newIntegrationEnv(t)
	e.writeCentralBothOnly()
	e.installFactory()
	savedInterval := nativeMonitorInterval
	nativeMonitorInterval = 5 * time.Millisecond
	t.Cleanup(func() { nativeMonitorInterval = savedInterval })
	t.Cleanup(func() { e.joinSupervisor() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, err := newCoordinator(e.path, e.runtime())
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	// glm is both-only now: load it with the default placement.
	if _, err := c.load(ctx, integrationModel, false); err != nil {
		t.Fatalf("the both-only default load must succeed: %v (%s)", err, e.supervisorFailure())
	}
	before := len(e.commandsSnapshot())
	for _, on := range []string{"spark01", "spark02"} {
		want := integrationBothOnly + " has no recipe for " + on + "."
		if _, err := c.plan(ctx, integrationBothOnly, "load", on); err == nil || err.Error() != want {
			t.Fatalf("plan %s: %v, want %q", on, err, want)
		}
		if _, err := c.Load(integrationBothOnly, false, on); err == nil || err.Error() != want {
			t.Fatalf("load %s: %v, want %q", on, err, want)
		}
		if _, err := c.Load(integrationBothOnly, true, on); err == nil || err.Error() != want {
			t.Fatalf("forced load %s: %v, want %q", on, err, want)
		}
	}
	// No mutating command may run: the refusal precedes every stop/cancel.
	for _, args := range e.commandsSnapshot()[before:] {
		j := strings.Join(args, " ")
		for _, effect := range []string{" start ", " stop ", "reset-failed", "cancel"} {
			if strings.Contains(j, effect) {
				t.Fatalf("a refused one-shot placement must run no mutating command, got %v", args)
			}
		}
	}
	if _, err := e.runtime().readRecord(integrationBothOnly); err == nil {
		t.Fatal("a refused one-shot placement must not create a lifecycle record")
	}
	// The already-loaded glm model is untouched.
	rec := integrationRecord(t, e)
	if str(rec["state"]) != StateReady || str(rec["placement"]) != "both" {
		t.Fatalf("the loaded model must stay READY on both: %v", rec)
	}
	if !e.isRunning(integrationHead) || !e.isRunning(integrationWorker) {
		t.Fatal("the loaded both ranks must stay running")
	}
	// The saved run_on is never rewritten.
	cfg, err := readCluster(e.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := str(obj(obj(cfg["models"])[integrationBothOnly])["run_on"]); got != "both" {
		t.Fatalf("the saved run_on must stay both: %q", got)
	}
	// A both placement is valid for the both-only recipe (validation only).
	if _, err := c.plan(ctx, integrationBothOnly, "load", "both"); err != nil {
		t.Fatalf("plan both must be valid for a both-only model: %v", err)
	}

	if _, err := c.unload(ctx, integrationModel, false); err != nil {
		t.Fatalf("the final unload must succeed: %v", err)
	}
	if err := e.emulateStop(); err != nil {
		t.Fatalf("final stop: %v", err)
	}
	if err := superviseNative(ctx, e.path, "cleanup", integrationModel); err != nil {
		t.Fatalf("final cleanup: %v", err)
	}
	backendClosed(t, e.port)
}
