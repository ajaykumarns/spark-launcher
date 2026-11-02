package sparky

// Legacy lifecycle tests: the manager sync fence/drain contract through the
// coordinator-owned admission-drain seam. All effects are local httptest
// servers, fake command runners and temp files; no real manager, SSH,
// Docker, systemctl or model is touched.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingRunner records every command and answers the manager validate and
// HUP commands with success; anything else is a loud failure.
type recordingRunner struct {
	mu    sync.Mutex
	argvs [][]string
	fail  map[string]bool // first argv element that must fail
}

func (f *recordingRunner) record(args []string) {
	f.mu.Lock()
	f.argvs = append(f.argvs, args)
	f.mu.Unlock()
}
func (f *recordingRunner) all() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string{}, f.argvs...)
}
func (f *recordingRunner) ran(substr string) bool {
	for _, a := range f.all() {
		if strings.Contains(strings.Join(a, " "), substr) {
			return true
		}
	}
	return false
}

func newFakeRunner() *recordingRunner { return &recordingRunner{fail: map[string]bool{}} }

// legacySyncEnv wires a legacy coordinator whose runtime talks to a local
// SSE manager and a fake command runner.
func legacySyncEnv(t *testing.T, sseBody string) (*Coordinator, *Runtime, *recordingRunner, string) {
	t.Helper()
	root := t.TempDir()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(sseBody))
	}))
	t.Cleanup(s.Close)
	cfg := legacyClusterConfig(root, s.URL)
	path := filepath.Join(root, "models.json")
	if err := writeCluster(path, cfg); err != nil {
		t.Fatal(err)
	}
	rt := newRuntime(cfg, path)
	runner := newFakeRunner()
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		runner.record(args)
		if strings.Contains(strings.Join(args, " "), "--validate") {
			return CommandResult{Code: 0}, nil
		}
		if strings.Contains(strings.Join(args, " "), "HUP") {
			return CommandResult{Code: 0}, nil
		}
		return CommandResult{Code: 0}, nil
	}
	coord, err := newCoordinator(path, rt)
	if err != nil {
		t.Fatal(err)
	}
	return coord, rt, runner, path
}

func managerConfigFile(t *testing.T, root string, content string) string {
	t.Helper()
	dir := filepath.Join(root, "manager")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSyncLegacyHUPsAndWritesCandidate(t *testing.T) {
	coord, rt, runner, _ := legacySyncEnv(t, "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\"}}\n\n")
	root := str(coord.Config()["root"])
	managerConfigFile(t, root, "models:\n  glm: {cmd: /bin/true}\nglobalTTL: 30\n")
	if err := rt.syncLegacy(context.Background()); err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if !runner.ran("--validate") || !runner.ran("HUP") {
		t.Fatalf("expected validate and HUP, ran: %v", runner.all())
	}
	// The candidate config was written and the live path updated.
	if _, err := os.Stat(filepath.Join(root, "controller", "manager-candidate.yaml")); err != nil {
		t.Fatal("the staged candidate was not written")
	}
}

func TestSyncLegacySemanticNoOpNeverHUPs(t *testing.T) {
	coord, rt, runner, _ := legacySyncEnv(t, "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\"}}\n\n")
	_ = str(coord.Config()["root"])
	managerConfigFile(t, str(coord.Config()["root"]), "models:\n  glm: {cmd: /bin/true}\nglobalTTL: 30\n")
	// First sync writes the desired config; the second must be a no-op.
	if err := rt.syncLegacy(context.Background()); err != nil {
		t.Fatalf("first sync failed: %v", err)
	}
	if !runner.ran("HUP") {
		t.Fatal("the first sync must HUP")
	}
	runner.mu.Lock()
	runner.argvs = nil
	runner.mu.Unlock()
	if err := rt.syncLegacy(context.Background()); err != nil {
		t.Fatalf("second sync failed: %v", err)
	}
	if runner.ran("HUP") || runner.ran("--validate") {
		t.Fatalf("an unchanged placement/TTL/concurrency/context must not restart the manager: %v", runner.all())
	}
}

func TestSyncLegacyDrainsAdmissionAndPreservesManualFence(t *testing.T) {
	coord, rt, runner, _ := legacySyncEnv(t, "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\"}}\n\n")
	root := str(coord.Config()["root"])
	managerConfigFile(t, root, "models:\n  glm: {cmd: /bin/true}\nglobalTTL: 30\n")
	// A direct Sparky lease is held; the sync must fence (reloading), wait
	// for the drain, and only then HUP.
	coord.mu.Lock()
	coord.leases["held"] = &lease{ID: "held", Name: "glm"}
	coord.fenced = true // a manual fence predating the reload
	coord.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- rt.syncLegacy(context.Background()) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		coord.mu.Lock()
		reloading := coord.reloading
		coord.mu.Unlock()
		if reloading {
			break
		}
		time.Sleep(time.Millisecond)
	}
	coord.mu.Lock()
	if !coord.reloading {
		coord.mu.Unlock()
		t.Fatal("the sync never fenced admission")
	}
	if runner.ran("HUP") {
		coord.mu.Unlock()
		t.Fatal("the manager was HUPed before the direct lease drained")
	}
	coord.mu.Unlock()
	// New reservations are blocked while fenced for the reload.
	blockCtx, blockCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer blockCancel()
	if _, err := coord.acquire(blockCtx, "glm", []string{"spark01"}, false, 0, "blocked", "", true); err == nil {
		t.Fatal("new reservations must be blocked while the reload fence is held")
	}
	// Release the direct lease; the sync completes and clears only its own
	// reloading flag, preserving the manual fence.
	coord.release("held")
	if err := <-done; err != nil {
		t.Fatalf("sync failed after drain: %v", err)
	}
	coord.mu.Lock()
	defer coord.mu.Unlock()
	if coord.reloading {
		t.Fatal("the reload flag was orphaned")
	}
	if !coord.fenced {
		t.Fatal("the manual fenced flag must be preserved after the reload")
	}
}

func TestSyncLegacyDrainFailureRestoresFlags(t *testing.T) {
	coord, rt, runner, _ := legacySyncEnv(t, "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\"}}\n\n")
	root := str(coord.Config()["root"])
	managerConfigFile(t, root, "models:\n  glm: {cmd: /bin/true}\nglobalTTL: 30\n")
	coord.mu.Lock()
	coord.leases["held"] = &lease{ID: "held", Name: "glm"}
	coord.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := rt.syncLegacy(ctx); err == nil {
		t.Fatal("a stuck drain must fail, never proceed to the HUP")
	}
	if runner.ran("HUP") {
		t.Fatal("no HUP may happen when the drain fails")
	}
	coord.mu.Lock()
	defer coord.mu.Unlock()
	if coord.reloading {
		t.Fatal("the reloading flag must be restored after a failed drain")
	}
}

func TestSyncLegacyValidatedFailureRestoresConfig(t *testing.T) {
	coord, rt, runner, _ := legacySyncEnv(t, "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\"}}\n\n")
	root := str(coord.Config()["root"])
	before := managerConfigFile(t, root, "models:\n  glm: {cmd: /bin/true}\nglobalTTL: 30\n")
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		runner.record(args)
		return CommandResult{Code: 1}, nil // validation fails
	}
	if err := rt.syncLegacy(context.Background()); err == nil {
		t.Fatal("a failed candidate validation must fail loudly")
	}
	if runner.ran("HUP") {
		t.Fatal("no HUP may follow a failed validation")
	}
	after, err := os.ReadFile(before)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "models:\n  glm: {cmd: /bin/true}\nglobalTTL: 30\n" {
		t.Fatal("the live manager config must be untouched after a failed validation")
	}
}

// TestSyncLegacyFailedHUPRestoresOriginalMode pins the rollback contract: a
// failed HUP restores the exact original bytes AND the original file mode,
// and a successful restore is reported as the plain HUP error.
func TestSyncLegacyFailedHUPRestoresOriginalMode(t *testing.T) {
	coord, rt, runner, _ := legacySyncEnv(t, "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\"}}\n\n")
	root := str(coord.Config()["root"])
	live := managerConfigFile(t, root, "models:\n  glm: {cmd: /bin/true}\nglobalTTL: 30\n")
	if err := os.Chmod(live, 0644); err != nil {
		t.Fatal(err)
	}
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		runner.record(args)
		j := strings.Join(args, " ")
		if strings.Contains(j, "--validate") {
			return CommandResult{Code: 0}, nil
		}
		if strings.Contains(j, "HUP") {
			return CommandResult{Code: 1}, fail("HUP rejected")
		}
		return CommandResult{Code: 0}, nil
	}
	err := rt.syncLegacy(context.Background())
	if err == nil || strings.Contains(err.Error(), "could not be restored") {
		t.Fatalf("a failed HUP must surface the reload error itself, got %v", err)
	}
	info, err := os.Stat(live)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("the restore must preserve the original mode, got %v", info.Mode().Perm())
	}
	after, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "models:\n  glm: {cmd: /bin/true}\nglobalTTL: 30\n" {
		t.Fatal("the restore must bring back the exact original bytes")
	}
	if !runner.ran("HUP") {
		t.Fatal("the HUP must have been attempted")
	}
}

// TestSyncLegacyFailedHUPReportsFailedRestore: when the rollback itself
// fails, the error must report the failed restore loudly instead of
// swallowing it — manual recovery is required.
func TestSyncLegacyFailedHUPReportsFailedRestore(t *testing.T) {
	coord, rt, runner, _ := legacySyncEnv(t, "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\"}}\n\n")
	root := str(coord.Config()["root"])
	live := managerConfigFile(t, root, "models:\n  glm: {cmd: /bin/true}\nglobalTTL: 30\n")
	dir := filepath.Dir(live)
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		runner.record(args)
		j := strings.Join(args, " ")
		if strings.Contains(j, "--validate") {
			return CommandResult{Code: 0}, nil
		}
		if strings.Contains(j, "HUP") {
			// Sabotage the restore: the manager directory becomes a
			// regular file, so the rollback cannot recreate the live path.
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dir, []byte("sabotaged"), 0600); err != nil {
				t.Fatal(err)
			}
			return CommandResult{Code: 1}, fail("HUP rejected")
		}
		return CommandResult{Code: 0}, nil
	}
	err := rt.syncLegacy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not be restored") {
		t.Fatalf("a failed restore must be reported loudly, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Native parent stop tests. Everything is local: the runner is a fake that
// records every command, the backend port is a real closed TCP port, the
// lifecycle record is a real file and no live child is ever spawned (the
// fallback helper runs through the faked Run seam).
// ---------------------------------------------------------------------------

type nativeStopEnv struct {
	t    *testing.T
	root string
	path string
	port int
	rt   *Runtime

	mu          sync.Mutex
	calls       []string
	supState    string
	supStopFail bool
	hangHead    bool
	release     chan struct{}
	fallbackRan bool
	onSupStop   func()
}

func newNativeStopEnv(t *testing.T) *nativeStopEnv {
	t.Helper()
	root := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // free again: confirmation must see the port refused
	e := &nativeStopEnv{
		t: t, root: root, port: port,
		path:     filepath.Join(root, "config", "models.json"),
		supState: "inactive",
		release:  make(chan struct{}),
	}
	cfg := Object{
		"version": 1, "root": root, "controller_port": 8020, "lifecycle": "native",
		"nodes": Object{"spark01": Object{"direct_ip": "10.0.0.1"}, "spark02": Object{"direct_ip": "10.0.0.2", "ssh": "frozen-host"}},
		"models": Object{"glm": Object{"kind": "llm", "name": "GLM", "run_on": "both", "keep_loaded": false,
			"settings": Object{"both": Object{
				"enabled": true, "backend_url": fmt.Sprintf("http://127.0.0.1:%d", port),
				"units":          Object{"spark01": "sup-a.service", "spark02": "sup-b.service"},
				"startup_order":  []any{"spark01", "spark02"},
				"context_tokens": 262144, "concurrency": 4, "parameters": Object{},
			}}},
		},
	}
	if err := os.MkdirAll(filepath.Join(root, "controller", "lifecycle"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(e.path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeCluster(e.path, cfg); err != nil {
		t.Fatal(err)
	}
	e.rt = newRuntime(cfg, e.path)
	e.rt.Run = e.runner()
	return e
}

func (e *nativeStopEnv) record(args []string) {
	// Remote commands arrive shell-quoted per token; strip the quotes so
	// assertions can match plain command text.
	e.mu.Lock()
	e.calls = append(e.calls, strings.ReplaceAll(strings.Join(args, " "), "'", ""))
	e.mu.Unlock()
}

func (e *nativeStopEnv) ran(substr string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func (e *nativeStopEnv) runner() Runner {
	return func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		e.record(args)
		j := strings.ReplaceAll(strings.Join(args, " "), "'", "")
		switch {
		case strings.Contains(j, "-p ActiveState --value"):
			return CommandResult{Out: e.supState + "\n", Code: 0}, nil
		case strings.Contains(j, "-p LoadState"):
			return CommandResult{Out: "LoadState=loaded\nActiveState=inactive\n", Code: 0}, nil
		case strings.Contains(j, "is-active"):
			return CommandResult{Code: 1}, nil // inactive
		case strings.Contains(j, "systemctl stop sparky-model@"):
			if e.onSupStop != nil {
				e.onSupStop()
			}
			if e.supStopFail {
				return CommandResult{Code: 1}, nil
			}
			return CommandResult{Code: 0}, nil
		case strings.Contains(j, "systemctl stop sup-a.service") && !strings.Contains(j, "ssh"):
			if e.hangHead {
				<-e.release
				return CommandResult{Code: 1}, nil
			}
			return CommandResult{Code: 0}, nil
		case strings.HasPrefix(j, "env -u INVOCATION_ID"):
			e.mu.Lock()
			e.fallbackRan = true
			e.mu.Unlock()
			return CommandResult{Code: 0}, nil
		}
		return CommandResult{Code: 0}, nil
	}
}

// beginReady creates the durable record and drives it to ready, optionally
// recording durable rank attempts (the generation-owned attempt flags).
func (e *nativeStopEnv) beginReady(attempt bool) string {
	e.t.Helper()
	cfg, err := readCluster(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	rt := newRuntime(cfg, e.path)
	m, s, nodes, placement, err := selectPlacement(cfg, "glm", "")
	if err != nil {
		e.t.Fatal(err)
	}
	rec, err := rt.BeginOperation("glm", m, s, nodes, placement)
	if err != nil {
		e.t.Fatal(err)
	}
	gen := str(rec["generation"])
	if err := rt.TransitionState("glm", gen, StateStarting); err != nil {
		e.t.Fatal(err)
	}
	if attempt {
		if err := recordAttemptedRank(rt, "glm", gen, "spark01", "sup-a.service"); err != nil {
			e.t.Fatal(err)
		}
		if err := recordAttemptedRank(rt, "glm", gen, "spark02", "sup-b.service"); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := rt.TransitionState("glm", gen, StateReady); err != nil {
		e.t.Fatal(err)
	}
	return gen
}

func (e *nativeStopEnv) recordState() string {
	e.t.Helper()
	rec, err := e.rt.readRecord("glm")
	if err != nil {
		e.t.Fatalf("record: %v", err)
	}
	return str(rec["state"])
}

func waitUntil(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestNativeStopMissingRecordZeroStops: a missing record is an error with
// ZERO stop commands; the desired config is never a fallback.
func TestNativeStopMissingRecordZeroStops(t *testing.T) {
	e := newNativeStopEnv(t)
	if err := e.rt.stopModel(context.Background(), "glm"); err == nil {
		t.Fatal("a missing record must be an error")
	}
	if e.ran("systemctl") {
		t.Fatalf("a missing record must cause zero stops: %v", e.calls)
	}
}

// TestNativeStopCorruptRecordZeroStops: a corrupt record is an error with
// ZERO stop commands.
func TestNativeStopCorruptRecordZeroStops(t *testing.T) {
	e := newNativeStopEnv(t)
	if err := os.WriteFile(e.rt.recordPath("glm"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.stopModel(context.Background(), "glm"); err == nil {
		t.Fatal("a corrupt record must be an error")
	}
	if e.ran("systemctl") {
		t.Fatalf("a corrupt record must cause zero stops: %v", e.calls)
	}
}

// TestNativeStopUsesFrozenRecordAfterRecipeDeleted: the recorded units are
// stopped on the FROZEN hosts even though the recipe was deleted from the
// central config afterwards.
func TestNativeStopUsesFrozenRecordAfterRecipeDeleted(t *testing.T) {
	e := newNativeStopEnv(t)
	e.beginReady(true)
	cfg, err := readCluster(e.path)
	if err != nil {
		t.Fatal(err)
	}
	delete(obj(cfg["models"]), "glm") // the recipe is gone
	if err := writeCluster(e.path, cfg); err != nil {
		t.Fatal(err)
	}
	e.rt.invalidate()
	if err := e.rt.stopModel(context.Background(), "glm"); err != nil {
		e.mu.Lock()
		t.Logf("calls: %v", e.calls)
		e.mu.Unlock()
		t.Fatalf("stop failed: %v", err)
	}
	if !(e.ran("frozen-host") && e.ran("systemctl stop sup-b.service")) {
		t.Fatalf("the frozen worker host must be stopped: %v", e.calls)
	}
	if !e.ran("systemctl stop sup-a.service") {
		t.Fatalf("the head rank must be stopped: %v", e.calls)
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the record must be stopped, got %s", st)
	}
}

// TestNativeStopHungHeadDoesNotSkipWorker: the head rank stop hanging can
// never delay or skip the independent worker stop.
func TestNativeStopHungHeadDoesNotSkipWorker(t *testing.T) {
	e := newNativeStopEnv(t)
	e.beginReady(true)
	e.hangHead = true
	done := make(chan error, 1)
	go func() { done <- e.rt.stopModel(context.Background(), "glm") }()
	if !waitUntil(func() bool { return e.ran("systemctl stop sup-b.service") }) {
		close(e.release)
		t.Fatal("the worker stop must run independently of the hung head")
	}
	close(e.release)
	if err := <-done; err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the record must be stopped, got %s", st)
	}
}

// TestNativeStopStaleGenerationNeverCommandsNewUnits: a generation that is
// superseded between the stop steps must never command the new generation's
// units nor overwrite the new record.
func TestNativeStopStaleGenerationNeverCommandsNewUnits(t *testing.T) {
	e := newNativeStopEnv(t)
	e.beginReady(true)
	e.onSupStop = func() {
		rec, err := e.rt.readRecord("glm")
		if err != nil {
			t.Error(err)
			return
		}
		rec["generation"] = token()
		rec["state"] = StateQueued
		rec["units"] = Object{"spark01": "sup-new.service", "spark02": "sup-c.service"}
		rec["attempted"] = nil
		if err := withFileLock(e.rt.recordPath("glm")+".lock", true, func() error {
			return writeObject(e.rt.recordPath("glm"), rec)
		}); err != nil {
			t.Error(err)
		}
	}
	err := e.rt.stopModel(context.Background(), "glm")
	if err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("a superseded stop must be refused, got %v", err)
	}
	if e.ran("stop sup-a.service") || e.ran("stop sup-b.service") {
		t.Fatalf("a stale stop must command zero units: %v", e.calls)
	}
	rec, err := e.rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if str(rec["state"]) != StateQueued || str(obj(rec["units"])["spark01"]) != "sup-new.service" {
		t.Fatalf("the new record must be untouched, got %v", rec)
	}
}

// TestNativeStopFallbackHelperRunsWhenSupervisorStopFails: a failed
// supervisor stop falls back to the REAL owned-generation helper child
// through env -u INVOCATION_ID, then reports stopped only after full
// confirmation.
func TestNativeStopFallbackHelperRunsWhenSupervisorStopFails(t *testing.T) {
	e := newNativeStopEnv(t)
	e.beginReady(true)
	e.supStopFail = true
	if err := e.rt.stopModel(context.Background(), "glm"); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	if !e.fallbackRan {
		t.Fatalf("the owned-generation cleanup helper must run: %v", e.calls)
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the record must be stopped, got %s", st)
	}
}

// TestNativeStopUnconfirmedReleaseRetainsOwnership: an unknown rank probe
// must retain ownership (failed verdict), never claim stopped.
func TestNativeStopUnconfirmedReleaseRetainsOwnership(t *testing.T) {
	e := newNativeStopEnv(t)
	e.beginReady(true)
	e.mu.Lock()
	e.supStopFail = true // trigger the fallback; the fake fallback also "fails"
	e.mu.Unlock()
	// The fallback helper runs through the Run seam and returns success, so
	// instead make the rank probe unknown to break confirmation.
	saved := e.rt.Run
	e.rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		e.record(args)
		j := strings.Join(args, " ")
		if strings.Contains(j, "-p LoadState") {
			return CommandResult{Code: 1}, nil // unknown rank state
		}
		return saved(ctx, args, input)
	}
	err := e.rt.stopModel(context.Background(), "glm")
	if err == nil || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("an unconfirmed release must retain ownership, got %v", err)
	}
	if st := e.recordState(); st != StateFailed {
		t.Fatalf("the record must be failed, got %s", st)
	}
}

// TestSuperviseModelStopFallsBackToRecordOnCorruptCentral: when the central
// cluster file is unreadable, an actual stop through superviseModel must
// still be served by the REAL superviseNative strict root fallback acting
// only on the existing validated durable record — zero starts, zero guessed
// units and the record honestly stopped.
func TestSuperviseModelStopFallsBackToRecordOnCorruptCentral(t *testing.T) {
	e := newNativeStopEnv(t)
	e.beginReady(true)
	// The central cluster file becomes unreadable: the fallback must come
	// from the record's frozen config alone.
	if err := os.WriteFile(e.path, []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	savedFactory := nativeRuntimeFactory
	nativeRuntimeFactory = func(c Object, path string) *Runtime { return e.rt }
	defer func() { nativeRuntimeFactory = savedFactory }()
	if err := superviseModel(context.Background(), e.path, "stop", "glm"); err != nil {
		t.Fatalf("the record-backed stop must succeed through the fallback: %v", err)
	}
	if e.ran("start --no-block") || e.ran("reset-failed") {
		t.Fatalf("the fallback stop must never start anything: %v", e.calls)
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the record must be stopped, got %s", st)
	}
}

// TestSuperviseModelRunFailsClosedOnCorruptCentral: a run has no record to
// serve and fails closed; nothing is ever started from a guessed config.
func TestSuperviseModelRunFailsClosedOnCorruptCentral(t *testing.T) {
	e := newNativeStopEnv(t)
	savedFactory := nativeRuntimeFactory
	nativeRuntimeFactory = func(c Object, path string) *Runtime { return e.rt }
	defer func() { nativeRuntimeFactory = savedFactory }()
	if err := os.WriteFile(e.path, []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := superviseModel(context.Background(), e.path, "run", "glm"); err == nil {
		t.Fatal("a run on an unreadable central file must fail closed")
	}
	if e.ran("start --no-block") {
		t.Fatalf("nothing may be started: %v", e.calls)
	}
}

// TestStopModelRepeatUnloadReleasedGeneration: an idempotent repeat unload
// of the SAME stopped generation succeeds with PROOF (ranks loaded+inactive,
// frozen backend port refused, supervisor gone) and ZERO stop commands.
func TestStopModelRepeatUnloadReleasedGeneration(t *testing.T) {
	e := newNativeStopEnv(t)
	gen := e.beginReady(true)
	if err := e.rt.TransitionState("glm", gen, StateStopping); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.TransitionState("glm", gen, StateStopped); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.stopModel(context.Background(), "glm"); err != nil {
		t.Fatalf("a proven-released stopped generation must unload idempotently: %v", err)
	}
	if e.ran("systemctl stop sup-a.service") || e.ran("systemctl stop sup-b.service") {
		t.Fatalf("a repeat unload must issue no stop commands: %v", e.calls)
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the record must remain stopped, got %s", st)
	}
}

// TestStopModelRepeatUnloadUnprovenRefused: a stopped record whose
// generation is NOT proven released (live supervisor) is never a blind
// success.
func TestStopModelRepeatUnloadUnprovenRefused(t *testing.T) {
	e := newNativeStopEnv(t)
	gen := e.beginReady(true)
	if err := e.rt.TransitionState("glm", gen, StateStopping); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.TransitionState("glm", gen, StateStopped); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.supState = "active" // the supervisor is provably still alive
	e.mu.Unlock()
	if err := e.rt.stopModel(context.Background(), "glm"); err == nil {
		t.Fatal("an unproven stopped generation must refuse blind success")
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the stopped record must be left untouched, got %s", st)
	}
}

// TestNativeStopNoAttemptRecordActiveWorkerRefuses: a queued record with
// ZERO attempted ranks issues zero rank stop commands — the parent and the
// fallback never stop ranks they never started. But the read-only release
// PROOF covers ALL recorded units: a preexisting ACTIVE worker (closed
// frozen backend, inactive supervisor) retains ownership and the stop fails
// instead of a false "stopped". The same holds for an idempotent STOPPED
// repeat, and an unload can never claim done on an active unowned rank.
func TestNativeStopNoAttemptRecordActiveWorkerRefuses(t *testing.T) {
	e := newNativeStopEnv(t)
	// A queued generation whose start was never attempted (empty ledger)
	// but which already recorded both rank units.
	cfg, err := readCluster(e.path)
	if err != nil {
		t.Fatal(err)
	}
	rt := newRuntime(cfg, e.path)
	m, s, nodes, placement, err := selectPlacement(cfg, "glm", "")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := rt.BeginOperation("glm", m, s, nodes, placement)
	if err != nil {
		t.Fatal(err)
	}
	gen := str(rec["generation"])
	// The frozen backend port is closed (newNativeStopEnv freed it) and the
	// supervisor is inactive; only the preexisting unattempted worker rank
	// is still ACTIVE.
	base := e.runner()
	e.rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		j := strings.ReplaceAll(strings.Join(args, " "), "'", "")
		if strings.Contains(j, "sup-b.service") && strings.Contains(j, "-p LoadState") {
			return CommandResult{Out: "LoadState=loaded\nActiveState=active\n", Code: 0}, nil
		}
		return base(ctx, args, input)
	}
	if err := e.rt.stopModel(context.Background(), "glm"); err == nil {
		t.Fatal("an active unattempted worker must refuse a false stopped")
	}
	if e.ran("systemctl stop sup-a.service") || e.ran("systemctl stop sup-b.service") {
		t.Fatalf("a no-attempt generation must command zero rank stops: %v", e.calls)
	}
	if st := e.recordState(); st != StateFailed {
		t.Fatalf("ownership must be retained (failed verdict), got %s", st)
	}
	// The idempotent STOPPED repeat is refused by the same all-units proof.
	if err := e.rt.TransitionState("glm", gen, StateStopping); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.TransitionState("glm", gen, StateStopped); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.stopModel(context.Background(), "glm"); err == nil {
		t.Fatal("an idempotent repeat must refuse while an unowned worker is active")
	}
	if e.ran("systemctl stop sup-a.service") || e.ran("systemctl stop sup-b.service") {
		t.Fatalf("an idempotent repeat must command zero rank stops: %v", e.calls)
	}
}
