package sparky

// Native lifecycle stage 1: durable state records and strong readiness.
// All effects are local: the backend is an httptest server, the runner is a
// loud fake that refuses unexpected commands, and records live in a temp
// directory. Nothing here ever SSHes, starts units or touches a manager.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func nativeTestConfig(root, backend string) Object {
	return Object{
		"version": 1, "root": root, "controller_port": 8020, "lifecycle": "native",
		"nodes": Object{"spark01": Object{"direct_ip": "10.0.0.1"}, "spark02": Object{"direct_ip": "10.0.0.2"}},
		"policy": Object{"interrupt_active_jobs": false, "protected_models": []any{},
			// Native lifecycle tests never exercise shared image jobs; the
			// cluster validator requires either exclusivity or a nonempty
			// shared allowlist plus a positive memory minimum.
			"comfyui_exclusive_with_llms": true, "comfyui_shared_llms": []any{}, "comfyui_min_available_gib": 18},
		"models": Object{
			"glm": Object{"kind": "llm", "name": "GLM 5.3 Flash", "run_on": "spark01", "keep_loaded": false,
				"reasoning": true, "aliases": []any{},
				"settings": Object{"spark01": Object{
					"enabled": true, "backend_url": backend, "units": Object{"spark01": "glm.service"},
					"context_tokens": 262144, "concurrency": 4,
					"parameters": Object{"tensor-parallel-size": 1}}}},
		},
	}
}

// nativeBackend serves /health and a configurable /v1/models id.
type nativeBackend struct {
	server *httptest.Server
	mu     chan string
}

func newNativeBackend(t *testing.T, id string) *nativeBackend {
	ids := make(chan string, 1)
	ids <- id
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			current := <-ids
			ids <- current
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":` + quoteJSON(current) + `,"object":"model"}]}`))
		default:
			t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return &nativeBackend{server: s, mu: ids}
}

func (b *nativeBackend) setID(id string) {
	<-b.mu
	b.mu <- id
}

// loudRunner answers exactly the small supervisor/unit probes and fails on
// anything else (starts, stops, ssh, docker, ...).
func loudRunner(t *testing.T, unit string) func(*runnerCalls) Runner {
	return func(calls *runnerCalls) Runner {
		return func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
			calls.record(args)
			if args[0] == "systemctl" && args[1] == "show" {
				return CommandResult{Out: "active\n", Code: 0}, nil
			}
			if args[0] == "systemctl" && args[1] == "is-active" && args[len(args)-1] == unit {
				return CommandResult{Code: 0}, nil
			}
			return CommandResult{}, fail("unexpected command: %v", args)
		}
	}
}

type nativeEnv struct {
	t     *testing.T
	rt    *Runtime
	root  string
	calls *runnerCalls
}

func newNativeEnv(t *testing.T, backend *nativeBackend) *nativeEnv {
	t.Helper()
	root := t.TempDir()
	cfg := nativeTestConfig(root, backend.server.URL)
	path := filepath.Join(root, "models.json")
	if err := writeCluster(path, cfg); err != nil {
		t.Fatal(err)
	}
	calls := &runnerCalls{}
	rt := newRuntime(cfg, path)
	rt.Run = loudRunner(t, "glm.service")(calls)
	return &nativeEnv{t: t, rt: rt, root: root, calls: calls}
}

func (e *nativeEnv) begin() Object {
	e.t.Helper()
	cfg := e.rt.config()
	m := obj(obj(cfg["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	rec, err := e.rt.BeginOperation("glm", m, s, []string{"spark01"}, "spark01")
	if err != nil {
		e.t.Fatal(err)
	}
	return rec
}

func (e *nativeEnv) markReady() string {
	rec := e.begin()
	gen := str(rec["generation"])
	if err := e.rt.TransitionState("glm", gen, StateStarting); err != nil {
		e.t.Fatal(err)
	}
	if err := e.rt.TransitionState("glm", gen, StateReady); err != nil {
		e.t.Fatal(err)
	}
	return gen
}

func TestNativeReadyCurrentValidRecord(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	env.markReady()
	cfg := env.rt.config()
	m := obj(obj(cfg["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	if !env.rt.nativeReady(context.Background(), "glm", m, s, []string{"spark01"}, "spark01") {
		t.Fatal("a fully validated ready record must be reported ready")
	}
	for _, args := range env.calls.all() {
		if args[0] != "systemctl" || (args[1] != "show" && args[1] != "is-active") {
			t.Fatalf("readiness must only probe unit metadata, got %v", args)
		}
	}
}

func TestNativeReadyWrongBackendModelID(t *testing.T) {
	backend := newNativeBackend(t, "other-model")
	env := newNativeEnv(t, backend)
	env.markReady()
	cfg := env.rt.config()
	m := obj(obj(cfg["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	if env.rt.nativeReady(context.Background(), "glm", m, s, []string{"spark01"}, "spark01") {
		t.Fatal("a health-200 backend serving the wrong id must never be ready")
	}
	// The exact catalog id is normalized and matched.
	backend.setID("garuda/glm")
	if !env.rt.nativeReady(context.Background(), "glm", m, s, []string{"spark01"}, "spark01") {
		t.Fatal("the exact normalized model id must be accepted")
	}
}

func TestNativeReadyMissingCorruptAndUnknownRecords(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	cfg := env.rt.config()
	m := obj(obj(cfg["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	// Missing record.
	if env.rt.nativeReady(context.Background(), "glm", m, s, []string{"spark01"}, "spark01") {
		t.Fatal("a missing record is never ready")
	}
	// Corrupt record.
	if err := atomicWrite(env.rt.recordPath("glm"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if env.rt.nativeReady(context.Background(), "glm", m, s, []string{"spark01"}, "spark01") {
		t.Fatal("a corrupt record is never ready")
	}
	// Structurally valid but unknown state.
	if err := writeObject(env.rt.recordPath("glm"), Object{
		"name": "glm", "state": "banana", "nodes": []any{"spark01"},
		"units": Object{"spark01": "glm.service"}, "setting": s, "placement": "spark01",
		"generation": "g1", "fingerprint": strings.Repeat("a", 64), "updated_at": now(),
	}); err != nil {
		t.Fatal(err)
	}
	if env.rt.nativeReady(context.Background(), "glm", m, s, []string{"spark01"}, "spark01") {
		t.Fatal("an unknown state is never ready")
	}
}

func TestNativeReadyInactiveSupervisorOrRank(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	env.markReady()
	rt := env.rt
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		env.calls.record(args)
		if args[0] == "systemctl" && args[1] == "show" {
			return CommandResult{Out: "inactive\n", Code: 0}, nil
		}
		return CommandResult{}, fail("unexpected command: %v", args)
	}
	cfg := rt.config()
	m := obj(obj(cfg["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	if rt.nativeReady(context.Background(), "glm", m, s, []string{"spark01"}, "spark01") {
		t.Fatal("an inactive supervisor is never ready")
	}
	// "activating" is only acceptable for startup adoption, never readiness.
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		env.calls.record(args)
		if args[0] == "systemctl" && args[1] == "show" {
			return CommandResult{Out: "activating\n", Code: 0}, nil
		}
		return CommandResult{}, fail("unexpected command: %v", args)
	}
	if rt.nativeReady(context.Background(), "glm", m, s, []string{"spark01"}, "spark01") {
		t.Fatal("an activating supervisor is not strongly ready")
	}
	if ok, err := rt.supervisorActive(context.Background(), "glm"); err != nil || !ok {
		t.Fatal("startup adoption may still accept an activating supervisor")
	}
	// Rank unit inactive (unknown unit never active).
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		env.calls.record(args)
		if args[0] == "systemctl" && args[1] == "show" {
			return CommandResult{Out: "active\n", Code: 0}, nil
		}
		if args[0] == "systemctl" && args[1] == "is-active" {
			return CommandResult{Code: 3}, nil
		}
		return CommandResult{}, fail("unexpected command: %v", args)
	}
	if rt.nativeReady(context.Background(), "glm", m, s, []string{"spark01"}, "spark01") {
		t.Fatal("an inactive rank unit is never ready")
	}
	// Unknown unit names are refused, not probed blindly.
	if _, err := rt.unitActive(context.Background(), "spark01", "../evil"); err == nil {
		t.Fatal("an invalid unit name must be refused")
	}
}

func TestStaleGenerationWriteRefused(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	gen := env.markReady()
	if err := env.rt.TransitionState("glm", gen, StateStopping); err != nil {
		t.Fatal(err)
	}
	if err := env.rt.TransitionState("glm", gen, StateStopped); err != nil {
		t.Fatal(err)
	}
	// A new generation supersedes the old one.
	m := obj(obj(env.rt.config()["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	rec, err := env.rt.BeginOperation("glm", m, s, []string{"spark01"}, "spark01")
	if err != nil {
		t.Fatal(err)
	}
	if str(rec["generation"]) == gen {
		t.Fatal("the replaced record must carry a fresh generation")
	}
	// The stale generation can no longer move the record.
	if err := env.rt.TransitionState("glm", gen, StateStopping); err == nil {
		t.Fatal("a stale generation must not be able to overwrite a newer operation")
	}
	// The record still carries the fresh generation and queued state.
	current, err := env.rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if str(current["state"]) != StateQueued || str(current["generation"]) != str(rec["generation"]) {
		t.Fatalf("the newer operation must be intact, got %v", str(current["state"]))
	}
	// A stale generation must not be able to recreate a deleted record.
	if err := os.Remove(env.rt.recordPath("glm")); err != nil {
		t.Fatal(err)
	}
	if err := env.rt.TransitionState("glm", gen, StateStopping); err == nil {
		t.Fatal("a stale generation must not recreate a vanished record")
	}
	if _, err := os.Stat(env.rt.recordPath("glm")); !os.IsNotExist(err) {
		t.Fatal("no record state may appear from a stale writer")
	}
	// Invalid transitions are refused even with the right generation.
	if err := env.rt.TransitionState("glm", str(rec["generation"]), StateReady); err == nil {
		t.Fatal("queued cannot jump straight to ready")
	}
}

func TestBeginOperationAdoptionAndRefusal(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	first := env.begin()
	m := obj(obj(env.rt.config()["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	// A matching in-progress operation is adopted, not replaced.
	again, err := env.rt.BeginOperation("glm", m, s, []string{"spark01"}, "spark01")
	if err != nil {
		t.Fatal(err)
	}
	if str(again["generation"]) != str(first["generation"]) {
		t.Fatal("a matching in-progress operation must be adopted, not replaced")
	}
	// A changed parameter breaks the fingerprint and is refused.
	changed := copyObject(s)
	obj(changed["parameters"])["tensor-parallel-size"] = 2
	if _, err := env.rt.BeginOperation("glm", m, changed, []string{"spark01"}, "spark01"); err == nil {
		t.Fatal("a different recipe on an in-flight model must be refused")
	}
	// A changed placement also breaks the fingerprint.
	if _, err := env.rt.BeginOperation("glm", m, s, []string{"spark01"}, "both"); err == nil {
		t.Fatal("a different placement must be refused on an in-flight model")
	}
}

func TestFingerprintIgnoresDisplayMetadata(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	m := obj(obj(env.rt.config()["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	base := lifecycleFingerprint("glm", m, s, []string{"spark01"}, "spark01")
	// Display-only metadata changes never move the fingerprint; the model
	// name itself stays lifecycle-relevant because it is the expected
	// backend model id stored in the record.
	display := copyObject(m)
	display["reasoning"] = false
	display["keep_loaded"] = true
	display["aliases"] = []any{"extra"}
	display["name"] = "GLM 5.3 Flash NVIDIA (two Sparks)"
	if lifecycleFingerprint("glm", display, s, []string{"spark01"}, "spark01") != base {
		t.Fatal("display metadata must not change the fingerprint")
	}
	// The human display label is never an identity: the served id is the
	// canonical key, or an explicit model_id override.
	if servedModelID("glm", display) != "glm" || servedModelID("glm", m) != "glm" {
		t.Fatal("the display name must not change the served model id")
	}
	if servedModelID("glm", Object{"model_id": "org/glm"}) != "org/glm" {
		t.Fatal("an explicit model_id must win over the key")
	}
	// Lifecycle-relevant numbers use exact spelling with semantic equality.
	if lifecycleFingerprint("glm", m, s, []string{"spark01"}, "spark01") != base {
		t.Fatal("the fingerprint must be stable")
	}
	changed := copyObject(s)
	changed["context_tokens"] = 131072
	if lifecycleFingerprint("glm", m, changed, []string{"spark01"}, "spark01") == base {
		t.Fatal("a changed context must change the fingerprint")
	}
	// Semantic number equality: 4 and 4.0 fingerprint identically.
	sem := copyObject(s)
	sem["concurrency"] = 4.0
	if lifecycleFingerprint("glm", m, sem, []string{"spark01"}, "spark01") != base {
		t.Fatal("4 and 4.0 must be semantically equal in the fingerprint")
	}
	// Node changes count.
	if lifecycleFingerprint("glm", m, s, []string{"spark02"}, "spark02") == base {
		t.Fatal("a node change must change the fingerprint")
	}
}

func TestBeginOperationReplacesTerminalRecords(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	gen := env.markReady()
	if err := env.rt.TransitionState("glm", gen, StateStopping); err != nil {
		t.Fatal(err)
	}
	// A ready/stopping record blocks a new begin until it is stopped.
	m := obj(obj(env.rt.config()["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	if _, err := env.rt.BeginOperation("glm", m, s, []string{"spark01"}, "spark01"); err == nil {
		t.Fatal("a stopping model must refuse a new operation")
	}
	if err := env.rt.TransitionState("glm", gen, StateStopped); err != nil {
		t.Fatal(err)
	}
	rec, err := env.rt.BeginOperation("glm", m, s, []string{"spark01"}, "spark01")
	if err != nil {
		t.Fatalf("a stopped model must accept a fresh generation: %v", err)
	}
	if str(rec["state"]) != StateQueued || str(rec["generation"]) == gen {
		t.Fatalf("the fresh generation must be queued: %v", rec)
	}
	// A malformed record is refused, never silently overwritten: it might
	// hold real loaded ownership.
	if err := atomicWrite(env.rt.recordPath("glm"), []byte("{oops"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := env.rt.BeginOperation("glm", m, s, []string{"spark01"}, "spark01"); err == nil {
		t.Fatal("a malformed record must be refused, never overwritten")
	}
	// A symlinked record is refused as well. The corrupt record above must
	// be removed first: the fixture owns its own preserved file.
	if err := os.Remove(env.rt.recordPath("glm")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(env.root, "elsewhere.json"), env.rt.recordPath("glm")); err != nil {
		t.Fatal(err)
	}
	if _, err := env.rt.BeginOperation("glm", m, s, []string{"spark01"}, "spark01"); err == nil {
		t.Fatal("a symlinked record must be refused")
	}
	if _, err := env.rt.readRecord("glm"); err == nil {
		t.Fatal("a symlinked record must never be read as state")
	}
}

func TestReadyRechecksDurableGenerationAfterProbes(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	env.markReady()
	cfg := env.rt.config()
	m := obj(obj(cfg["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	// The runner supersedes the record as soon as the small keyed show
	// probe answered, before the backend probes complete: the final recheck
	// must refuse. (Current readiness keys on `systemctl show`; is-active
	// may never be issued.)
	original := env.rt.Run
	var once sync.Once
	env.rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		res, err := original(ctx, args, input)
		once.Do(func() {
			if args[0] == "systemctl" && args[1] == "show" && err == nil && res.Code == 0 &&
				strings.TrimSpace(res.Out) == "active" {
				_ = withFileLock(env.rt.recordPath("glm")+".lock", true, func() error {
					rec, e := env.rt.readRecord("glm")
					if e != nil {
						return e
					}
					rec["state"] = StateStarting
					return writeObject(env.rt.recordPath("glm"), rec)
				})
			}
		})
		return res, err
	}
	done := make(chan bool, 1)
	go func() {
		done <- env.rt.nativeReady(context.Background(), "glm", m, s, []string{"spark01"}, "spark01")
	}()
	select {
	case r := <-done:
		if r {
			t.Fatal("a superseded generation must never be reported ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readiness must not block")
	}
}

// TestLegacyActivityRejectsUnknownMappings keeps malformed inflight rows
// from ever being counted as idle: {}, model:null and non-string models
// without a model_id are unknown, never busy-free.
func TestLegacyActivityRejectsUnknownMappings(t *testing.T) {
	cases := map[string]string{
		"empty mapping":    "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\",\"requests\":[{}]}}\n\n",
		"model null":       "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\",\"requests\":[{\"id\":1,\"model\":null}]}}\n\n",
		"model wrongtype":  "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\",\"requests\":[{\"id\":1,\"model\":7}]}}\n\n",
		"empty model only": "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\",\"requests\":[{\"id\":1,\"model\":\"\"}]}}\n\n",
	}
	for name, body := range cases {
		s := inflightServer(t, body)
		rt := newTestRuntime(t, s.URL)
		if _, err := rt.legacyActivity(context.Background()); err == nil {
			t.Fatalf("%s must never be treated as idle by activity", name)
		}
	}
	// A valid omitempty empty snapshot is still idle.
	s := inflightServer(t, "data: {\"type\":\"inflight\",\"data\":{\"operation\":\"snapshot\"}}\n\n")
	rt := newTestRuntime(t, s.URL)
	counts, err := rt.legacyActivity(context.Background())
	if err != nil || len(counts) != 0 {
		t.Fatalf("an omitted requests field is a valid empty snapshot: %v %v", counts, err)
	}
}

// TestLLMRequestVanishedLeaseIsSafe keeps the lease-bind step from ever
// dereferencing a nil lease when the admission lease is released between
// acquire and the backend bind.
func TestLLMRequestVanishedLeaseIsSafe(t *testing.T) {
	env := newTestEnv(t)
	backend := newNativeBackend(t, "qwen")
	// Point qwen's spark02 recipe at the local backend and mark it ready.
	cfg := env.coord.Config()
	qwen := obj(obj(cfg["models"])["qwen"])
	s := obj(obj(qwen["settings"])["spark02"])
	s["backend_url"] = backend.server.URL
	if err := writeCluster(env.rt.Path, cfg); err != nil {
		t.Fatal(err)
	}
	env.rt.configure(cfg)
	if _, err := env.rt.BeginOperation("qwen", qwen, s, []string{"spark02"}, "spark02"); err != nil {
		t.Fatal(err)
	}
	rec, err := env.rt.readRecord("qwen")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.rt.TransitionState("qwen", str(rec["generation"]), StateStarting); err != nil {
		t.Fatal(err)
	}
	if err := env.rt.TransitionState("qwen", str(rec["generation"]), StateReady); err != nil {
		t.Fatal(err)
	}

	// The runner answers the small probes and deletes any lease on every
	// supervisor probe: the pre-acquire probes find no lease (harmless),
	// while the POST-acquire requireReady probe removes the just-created
	// lease, so the bind step sees a vanished lease and must fail cleanly.
	original := env.rt.Run
	env.rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		if args[0] == "systemctl" && args[1] == "show" {
			env.coord.mu.Lock()
			env.coord.leases = map[string]*lease{}
			env.coord.mu.Unlock()
			return CommandResult{Out: "active\n", Code: 0}, nil
		}
		if args[0] == "systemctl" && args[1] == "is-active" {
			return CommandResult{Code: 0}, nil
		}
		if args[0] == "ssh" && strings.Contains(args[len(args)-1], "is-active") {
			return CommandResult{Code: 0}, nil
		}
		return original(ctx, args, input)
	}

	_, _, err = env.coord.llmRequest(context.Background(), "qwen")
	if err == nil || !strings.Contains(err.Error(), "vanished") {
		t.Fatalf("a vanished lease must fail cleanly, got %v", err)
	}
	env.coord.mu.Lock()
	remaining := len(env.coord.leases)
	env.coord.mu.Unlock()
	if remaining != 0 {
		t.Fatal("no lease may be re-created by the vanished-lease path")
	}
}

// TestCanonicalValueCompactAndRoundtrip pins the canonical JSON contract:
// no trailing newline, identical fingerprints for object literals and JSON
// read roundtrips, and exact numbers (4 == 4.0 == json.Number, full uint64
// range preserved with no float loss).
func TestCanonicalValueCompactAndRoundtrip(t *testing.T) {
	lit := Object{"a": 262144, "b": 4.0, "c": []any{1, Object{"k": 2}}}
	b, err := json.Marshal(lit)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalValue(lit) != canonicalValue(rt) {
		t.Fatalf("object literal and JSON roundtrip must fingerprint identically: %q vs %q", canonicalValue(lit), canonicalValue(rt))
	}
	if strings.ContainsAny(canonicalValue(lit), "\n ") {
		t.Fatalf("the canonical form must be compact: %q", canonicalValue(lit))
	}
	// int, float and json.Number spellings of the same value agree.
	if canonicalValue(4) != canonicalValue(4.0) || canonicalValue(4) != canonicalValue(json.Number("4")) || canonicalValue(json.Number("4.0")) != canonicalValue(4) {
		t.Fatalf("4, 4.0 and json.Number must canonicalize identically: %q %q %q", canonicalValue(4), canonicalValue(4.0), canonicalValue(json.Number("4.0")))
	}
	// The full uint64 range is preserved exactly, no float coercion.
	var maxUint64 uint64 = 18446744073709551615
	if canonicalValue(maxUint64) != "18446744073709551615" {
		t.Fatalf("max uint64 must stay exact, got %q", canonicalValue(maxUint64))
	}
	if canonicalValue(json.Number("18446744073709551615")) != canonicalValue(maxUint64) {
		t.Fatal("a JSON read of max uint64 must fingerprint like the object literal")
	}
	// The same contract holds at the fingerprint level.
	s := Object{"backend_url": "http://x", "units": Object{"spark01": "glm.service"},
		"context_tokens": 262144, "concurrency": 4, "parameters": Object{"tensor-parallel-size": 1}}
	b, _ = json.Marshal(s)
	rt, _ = decode(b)
	base := lifecycleFingerprint("glm", Object{}, s, []string{"spark01"}, "spark01")
	if lifecycleFingerprint("glm", Object{}, obj(rt), []string{"spark01"}, "spark01") != base {
		t.Fatal("a JSON read roundtrip must produce the same fingerprint as the object literal")
	}
	big := copyObject(s)
	big["context_tokens"] = maxUint64
	b, _ = json.Marshal(big)
	rt, _ = decode(b)
	fbig := lifecycleFingerprint("glm", Object{}, big, []string{"spark01"}, "spark01")
	if fbig == base {
		t.Fatal("a changed context must change the fingerprint")
	}
	if lifecycleFingerprint("glm", Object{}, obj(rt), []string{"spark01"}, "spark01") != fbig {
		t.Fatal("max uint64 must survive the JSON roundtrip with no float loss")
	}
}

// ranCall reports whether any recorded command contains sub.
func ranCall(calls *runnerCalls, sub string) bool {
	for _, a := range calls.all() {
		if strings.Contains(strings.Join(a, " "), sub) {
			return true
		}
	}
	return false
}

// startRunner answers exactly the small probes a parent start needs and
// lets the test hook unit-start effects.
type startRunner struct {
	t       *testing.T
	calls   *runnerCalls
	onStart func(rt *Runtime)
}

func (s *startRunner) runner(rt *Runtime) Runner {
	return func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		s.calls.record(args)
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "reset-failed"):
			return CommandResult{Code: 0}, nil
		case strings.Contains(j, "start"):
			if s.onStart != nil {
				s.onStart(rt)
			}
			return CommandResult{Code: 0}, nil
		case strings.Contains(j, "stop"):
			return CommandResult{Code: 0}, nil
		case args[0] == "systemctl" && args[1] == "show":
			return CommandResult{Out: "active\n", Code: 0}, nil
		case args[0] == "systemctl" && args[1] == "is-active":
			return CommandResult{Code: 0}, nil
		}
		return CommandResult{}, fail("unexpected command: %v", args)
	}
}

func (e *nativeEnv) selected() (Object, Object) {
	cfg := e.rt.config()
	m := obj(obj(cfg["models"])["glm"])
	return cfg, obj(obj(m["settings"])["spark01"])
}

// TestStartModelSelectedQueuesRecordBeforeStart asserts the durable queued
// generation exists BEFORE any systemctl start, and a config edit after
// reservation can never redirect the frozen recipe.
func TestStartModelSelectedQueuesRecordBeforeStart(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	rt := env.rt
	cfg, s := env.selected()
	m := obj(obj(cfg["models"])["glm"])
	seen := false
	sr := &startRunner{t: t, calls: env.calls, onStart: func(r *Runtime) {
		rec, err := r.readRecord("glm")
		if err != nil {
			t.Errorf("the queued record must exist before systemctl start: %v", err)
			return
		}
		if str(rec["state"]) != StateQueued {
			t.Errorf("the record must still be queued at start, got %s", str(rec["state"]))
		}
		if err := r.TransitionState("glm", str(rec["generation"]), StateStarting); err != nil {
			t.Error(err)
		}
		_ = r.TransitionState("glm", str(rec["generation"]), StateReady)
		seen = true
	}}
	rt.Run = sr.runner(rt)
	if err := rt.startModelSelected(context.Background(), "glm", cfg); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	if !seen {
		t.Fatal("the fake unit start never ran")
	}
	if !ranCall(env.calls, "reset-failed") || !ranCall(env.calls, "start --no-block") {
		t.Fatalf("expected reset-failed and start --no-block: %v", env.calls.all())
	}
	// A config edit AFTER the reservation cannot redirect the recipe: even
	// with the runtime config pointing elsewhere, a repeat start with the
	// frozen recipe takes the matching-READY fastpath (no new start) and
	// the record keeps the frozen fingerprint.
	edited := copyObject(cfg)
	obj(obj(obj(edited["models"])["glm"])["settings"])["spark01"] = merge(s, Object{"context_tokens": 131072})
	if err := writeCluster(rt.Path, edited); err != nil {
		t.Fatal(err)
	}
	rt.configure(edited)
	before := len(env.calls.all())
	if err := rt.startModelSelected(context.Background(), "glm", cfg); err != nil {
		t.Fatalf("the matching-READY fastpath must succeed: %v", err)
	}
	for _, a := range env.calls.all()[before:] {
		if strings.Contains(strings.Join(a, " "), "start") {
			t.Fatalf("a matching ready model must never be started or restarted again: %v", a)
		}
	}
	rec, err := rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if str(rec["fingerprint"]) != lifecycleFingerprint("glm", m, s, []string{"spark01"}, "spark01") {
		t.Fatal("the record must keep the frozen recipe fingerprint, not the edited config's")
	}
	if str(rec["canonicalkey"]) != "glm" {
		t.Fatal("the record must carry the canonical config key")
	}
	frozen := obj(rec["config"])
	if !isObject(frozen) || str(frozen["root"]) != cfg["root"] ||
		obj(frozen["nodes"]) == nil || obj(frozen["policy"]) == nil ||
		obj(obj(frozen["models"])["glm"]) == nil {
		t.Fatalf("the record must carry the whole defensive config snapshot (root/nodes/models/policy): %v", frozen)
	}
}

// TestStartModelSelectedNotLoadedResetDoesNotBlockFirstStart: a garbage-
// collected inactive instance makes reset-failed exit 1 "not loaded"; with
// a fresh keyed show proving LoadState=loaded AND ActiveState=inactive the
// parent proceeds to start --no-block under the SAME queued generation.
func TestStartModelSelectedNotLoadedResetDoesNotBlockFirstStart(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	rt := env.rt
	cfg, _ := env.selected()
	sr := &startRunner{t: t, calls: env.calls, onStart: func(r *Runtime) {
		rec, err := r.readRecord("glm")
		if err != nil {
			t.Errorf("the queued record must exist at start: %v", err)
			return
		}
		if str(rec["state"]) != StateQueued {
			t.Errorf("the record must still be queued at start, got %s", str(rec["state"]))
		}
		_ = r.TransitionState("glm", str(rec["generation"]), StateStarting)
		_ = r.TransitionState("glm", str(rec["generation"]), StateReady)
	}}
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		j := strings.Join(args, " ")
		if strings.Contains(j, "reset-failed") {
			env.calls.record(args)
			return CommandResult{Code: 1}, nil
		}
		if args[0] == "systemctl" && args[1] == "show" && strings.Contains(j, "LoadState") {
			env.calls.record(args)
			// The keyed supervisor proof: the installed template is loaded,
			// the instance inactive.
			return CommandResult{Out: "LoadState=loaded\nActiveState=inactive\n", Code: 0}, nil
		}
		return sr.runner(rt)(ctx, args, input)
	}
	if err := rt.startModelSelected(context.Background(), "glm", cfg); err != nil {
		t.Fatalf("a not-loaded reset with a loaded+inactive keyed show must not block the first start: %v", err)
	}
	if !ranCall(env.calls, "start --no-block") {
		t.Fatalf("expected start --no-block after the accepted reset: %v", env.calls.all())
	}
	rec, err := rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if str(rec["state"]) != StateReady {
		t.Fatalf("the start must reach ready, got %s", str(rec["state"]))
	}
}

// TestStartModelSelectedUnknownResetRefusesStart: a reset failure that is
// NOT the not-loaded case (unknown unit, error, or a non-inactive state)
// must never start: no start is issued and the generation is honestly
// recorded failed.
func TestStartModelSelectedUnknownResetRefusesStart(t *testing.T) {
	for name, show := range map[string]string{
		"not-found": "LoadState=not-found\nActiveState=inactive\n",
		"active":    "LoadState=loaded\nActiveState=active\n",
		"error":     "",
	} {
		t.Run(name, func(t *testing.T) {
			backend := newNativeBackend(t, "glm")
			env := newNativeEnv(t, backend)
			rt := env.rt
			cfg, _ := env.selected()
			rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
				j := strings.Join(args, " ")
				env.calls.record(args)
				if strings.Contains(j, "reset-failed") {
					return CommandResult{Code: 1}, nil
				}
				if args[0] == "systemctl" && args[1] == "show" && strings.Contains(j, "LoadState") {
					if show == "" {
						return CommandResult{}, fail("probe unreachable")
					}
					return CommandResult{Out: show, Code: 0}, nil
				}
				if strings.Contains(j, "start") {
					return CommandResult{}, fail("an unproven reset must never start: %v", args)
				}
				return CommandResult{}, fail("unexpected command: %v", args)
			}
			if err := rt.startModelSelected(context.Background(), "glm", cfg); err == nil {
				t.Fatal("an unproven reset failure must refuse the start")
			}
			for _, a := range env.calls.all() {
				if strings.Contains(strings.Join(a, " "), "start") {
					t.Fatalf("no start may be issued after an unproven reset: %v", a)
				}
			}
			rec, err := rt.readRecord("glm")
			if err != nil {
				t.Fatal(err)
			}
			if str(rec["state"]) != StateFailed {
				t.Fatalf("the refused start must be honestly recorded failed, got %s", str(rec["state"]))
			}
		})
	}
}

// TestStartModelSelectedAdoptsActiveSupervisor: a matching queued record
// with a live supervisor is adopted and waited; no reset-failed or start
// may be issued and no new generation created.
func TestStartModelSelectedAdoptsActiveSupervisor(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	rt := env.rt
	cfg, _ := env.selected()
	rec := env.begin()
	gen := str(rec["generation"])
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		env.calls.record(args)
		j := strings.Join(args, " ")
		if strings.Contains(j, "reset-failed") || strings.Contains(j, "start") {
			return CommandResult{}, fail("a live supervisor must never be started again: %v", args)
		}
		if args[0] == "systemctl" && args[1] == "show" {
			// The supervisor reports the model ready on the next probe.
			_ = rt.TransitionState("glm", gen, StateStarting)
			_ = rt.TransitionState("glm", gen, StateReady)
			return CommandResult{Out: "active\n", Code: 0}, nil
		}
		if args[0] == "systemctl" && args[1] == "is-active" {
			return CommandResult{Code: 0}, nil
		}
		return CommandResult{}, fail("unexpected command: %v", args)
	}
	if err := rt.startModelSelected(context.Background(), "glm", cfg); err != nil {
		t.Fatalf("adopted start failed: %v", err)
	}
	cur, err := rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if str(cur["generation"]) != gen {
		t.Fatal("adoption must keep the same generation")
	}
}

// TestStartModelSelectedSupersededGenerationFailsPromptly: a stale wait
// must fail as soon as a newer generation owns the record.
func TestStartModelSelectedSupersededGenerationFailsPromptly(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	rt := env.rt
	cfg, _ := env.selected()
	var once sync.Once
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		env.calls.record(args)
		j := strings.Join(args, " ")
		if strings.Contains(j, "reset-failed") || strings.Contains(j, "stop") {
			return CommandResult{Code: 0}, nil
		}
		if strings.Contains(j, "start") {
			// A newer operation takes ownership while the parent waits.
			once.Do(func() {
				_ = withFileLock(rt.recordPath("glm")+".lock", true, func() error {
					rec, e := rt.readRecord("glm")
					if e != nil {
						return e
					}
					rec["generation"] = strings.Repeat("b", 32)
					return writeObject(rt.recordPath("glm"), rec)
				})
			})
			return CommandResult{Code: 0}, nil
		}
		if args[0] == "systemctl" && (args[1] == "show" || args[1] == "is-active") {
			return CommandResult{Out: "active\n", Code: 0}, nil
		}
		return CommandResult{}, fail("unexpected command: %v", args)
	}
	done := make(chan error, 1)
	go func() { done <- rt.startModelSelected(context.Background(), "glm", cfg) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "superseded") {
			t.Fatalf("a superseded generation must fail promptly: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the superseded wait must not run to the health timeout")
	}
}

// TestStartModelSelectedCancelCleansOwnGeneration: cancellation fails
// promptly, marks exactly its own generation failed and never overwrites a
// newer generation.
func TestStartModelSelectedCancelCleansOwnGeneration(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	rt := env.rt
	cfg, _ := env.selected()
	ctx, cancel := context.WithCancel(context.Background())
	var gen string
	var once sync.Once
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		env.calls.record(args)
		j := strings.Join(args, " ")
		if strings.Contains(j, "reset-failed") {
			return CommandResult{Code: 0}, nil
		}
		if strings.Contains(j, "start") {
			once.Do(func() {
				if rec, e := rt.readRecord("glm"); e == nil {
					gen = str(rec["generation"])
					_ = rt.TransitionState("glm", gen, StateStarting)
				}
				cancel()
			})
			return CommandResult{Code: 0}, nil
		}
		if strings.Contains(j, "stop") {
			return CommandResult{Code: 0}, nil
		}
		if args[0] == "systemctl" && (args[1] == "show" || args[1] == "is-active") {
			return CommandResult{Out: "active\n", Code: 0}, nil
		}
		return CommandResult{}, fail("unexpected command: %v", args)
	}
	done := make(chan error, 1)
	go func() { done <- rt.startModelSelected(ctx, "glm", cfg) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("cancellation must fail promptly: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation must not wait out the health timeout")
	}
	rec, err := rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if str(rec["generation"]) != gen {
		t.Fatal("cleanup must own exactly its own generation")
	}
	if str(rec["state"]) != StateFailed {
		t.Fatalf("the cancelled generation must be marked failed, got %s", str(rec["state"]))
	}
}

// TestStartModelSelectedFailedSupervisorPromptsFailure: a failed supervisor
// verdict fails the start promptly, never after a stale health wait.
func TestStartModelSelectedFailedSupervisorPromptsFailure(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	rt := env.rt
	cfg, _ := env.selected()
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		env.calls.record(args)
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "reset-failed") || strings.Contains(j, "start") || strings.Contains(j, "stop"):
			if strings.Contains(j, "start") {
				if rec, e := rt.readRecord("glm"); e == nil {
					_ = rt.TransitionState("glm", str(rec["generation"]), StateStarting)
				}
			}
			return CommandResult{Code: 0}, nil
		case args[0] == "systemctl" && args[1] == "show":
			return CommandResult{Out: "failed\n", Code: 0}, nil
		case args[0] == "systemctl" && args[1] == "is-active":
			return CommandResult{Code: 3}, nil
		}
		return CommandResult{}, fail("unexpected command: %v", args)
	}
	done := make(chan error, 1)
	go func() { done <- rt.startModelSelected(context.Background(), "glm", cfg) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "supervisor") {
			t.Fatalf("a failed supervisor must prompt a failure: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a failed supervisor must not wait out the health timeout")
	}
	rec, err := rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if str(rec["state"]) != StateFailed {
		t.Fatalf("the failed generation must be recorded: %s", str(rec["state"]))
	}
}

// TestBeginOperationStoresWholeFrozenConfig asserts the record's config
// snapshot is the WHOLE validated selected config (root, nodes, policy,
// models), not just the model object, and that it is frozen: later changes
// to the caller's or the base runtime's config never reach the record.
func TestBeginOperationStoresWholeFrozenConfig(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	cfg := env.rt.config()
	obj(obj(cfg["nodes"])["spark01"])["ssh"] = "old-host"
	env.rt.configure(cfg)
	m := obj(obj(cfg["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	rec, err := env.rt.BeginOperation("glm", m, s, []string{"spark01"}, "spark01")
	if err != nil {
		t.Fatal(err)
	}
	frozen := obj(rec["config"])
	if frozen == nil {
		t.Fatal("the record must carry a config snapshot")
	}
	if str(frozen["root"]) != env.root {
		t.Fatal("the record config must carry the whole config root")
	}
	if str(obj(obj(frozen["nodes"])["spark01"])["ssh"]) != "old-host" {
		t.Fatal("the record config must carry the frozen nodes with the ssh host")
	}
	if obj(obj(frozen["models"])["glm"]) == nil {
		t.Fatal("the record config must carry the full model catalog")
	}
	if obj(frozen["policy"]) == nil {
		t.Fatal("the record config must carry the policy")
	}
	if str(obj(rec["setting"])["backend_url"]) != backend.server.URL ||
		str(obj(rec["units"])["spark01"]) == "" || str(rec["placement"]) != "spark01" {
		t.Fatal("the record recipe fields must match the selected cfg snapshot")
	}
	// The frozen snapshot survives base runtime config changes.
	originalPath := env.rt.recordPath("glm") // captured BEFORE the root change
	cfg2 := env.rt.config()
	obj(obj(cfg2["nodes"])["spark01"])["ssh"] = "new-host"
	cfg2["root"] = "/somewhere/else"
	env.rt.configure(cfg2)
	// Read the captured ORIGINAL record path: the base runtime now roots
	// elsewhere and must never be able to redirect record reads.
	current, err := readObject(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	frozen = obj(current["config"])
	if str(obj(obj(frozen["nodes"])["spark01"])["ssh"]) != "old-host" || str(frozen["root"]) != env.root {
		t.Fatal("the record config snapshot must stay frozen, never follow the base runtime")
	}
}

// failedReleaseRunner builds a Runner with independent rank and supervisor
// state fakes: the rank unit answers `systemctl show <unit> -p LoadState
// -p ActiveState` and the supervisor `systemctl show sparky-model@...`.
func failedReleaseRunner(rank, supervisor func() string) func(*runnerCalls) Runner {
	return func(calls *runnerCalls) Runner {
		return func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
			calls.record(args)
			if args[0] == "systemctl" && args[1] == "show" {
				if strings.Contains(strings.Join(args, " "), "sparky-model@") {
					return CommandResult{Out: supervisor() + "\n", Code: 0}, nil
				}
				return CommandResult{Out: rank() + "\n", Code: 0}, nil
			}
			return CommandResult{}, fail("unexpected command: %v", args)
		}
	}
}

// loadedInactive is the property output of a stopped rank unit.
func loadedInactive() string { return "LoadState=loaded\nActiveState=inactive\n" }

// TestFailedRecordReuseRequiresConfirmedRelease asserts a failed record is
// NOT proof of released ownership: with an active rank, an unreachable
// probe, a live listener on the frozen backend port or an active
// supervisor, BeginOperation refuses and no new generation appears. Only a
// confirmed release (all ranks loaded+inactive AND the exact frozen backend
// port refused AND supervisor gone) yields a fresh generation.
func TestFailedRecordReuseRequiresConfirmedRelease(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	gen := env.markReady()
	if err := env.rt.TransitionState("glm", gen, StateFailed); err != nil {
		t.Fatal(err)
	}
	m := obj(obj(env.rt.config()["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	begin := func() (Object, error) {
		return env.rt.BeginOperation("glm", m, s, []string{"spark01"}, "spark01")
	}
	// A rank still active refuses reuse.
	env.rt.Run = failedReleaseRunner(
		func() string { return "LoadState=loaded\nActiveState=active\n" },
		func() string { return "inactive" })(env.calls)
	if _, err := begin(); err == nil {
		t.Fatal("a failed record with an active rank must refuse reuse")
	}
	// A NOT-FOUND unit (LoadState=not-found) is not shutdown proof either.
	env.rt.Run = failedReleaseRunner(
		func() string { return "LoadState=not-found\nActiveState=inactive\n" },
		func() string { return "inactive" })(env.calls)
	if _, err := begin(); err == nil {
		t.Fatal("a failed record with a not-found rank must refuse reuse")
	}
	current, err := env.rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if str(current["generation"]) != gen {
		t.Fatal("a refused reuse must leave the failed generation untouched")
	}
	// The frozen backend port is still LIVE: refused even with dead ranks.
	env.rt.Run = failedReleaseRunner(loadedInactive, func() string { return "inactive" })(env.calls)
	if _, err := begin(); err == nil {
		t.Fatal("a failed record with a live frozen backend port must refuse reuse")
	}
	// Rank inactive but the supervisor is still active: still refused.
	backend.server.Close() // the backend port must be free for later cases
	env.rt.Run = failedReleaseRunner(loadedInactive, func() string { return "active" })(env.calls)
	if _, err := begin(); err == nil {
		t.Fatal("a failed record with a live supervisor must refuse reuse")
	}
	// Fully confirmed release: rank loaded+inactive, frozen port refused,
	// supervisor gone.
	env.rt.Run = failedReleaseRunner(loadedInactive, func() string { return "inactive" })(env.calls)
	rec, err := begin()
	if err != nil {
		t.Fatalf("a confirmed-release failed record must yield a fresh generation: %v", err)
	}
	if str(rec["generation"]) == gen || str(rec["state"]) != StateQueued {
		t.Fatalf("the fresh generation must be queued: %v", rec)
	}
}

// TestSharedStateContractEdges asserts the three contract edges
// (Queued->Stopping, Ready->Failed, Failed->Stopping) are allowed while
// other illegal transitions stay refused.
func TestSharedStateContractEdges(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	env := newNativeEnv(t, backend)
	mk := func() string {
		cfg := env.rt.config()
		m := obj(obj(cfg["models"])["glm"])
		s := obj(obj(m["settings"])["spark01"])
		rec, err := env.rt.BeginOperation("glm", m, s, []string{"spark01"}, "spark01")
		if err != nil {
			t.Fatal(err)
		}
		return str(rec["generation"])
	}
	// Queued -> Stopping (cancel a queued operation), then finish the
	// stopping record: a stopping record refuses any new BeginOperation, so
	// each contract edge needs its own independent valid record.
	gen := mk()
	if err := env.rt.TransitionState("glm", gen, StateStopping); err != nil {
		t.Fatalf("queued must be allowed to stop a cancelled operation: %v", err)
	}
	if err := env.rt.TransitionState("glm", gen, StateStopped); err != nil {
		t.Fatal(err)
	}
	// Ready -> Failed (rank death verdict).
	gen = mk()
	if err := env.rt.TransitionState("glm", gen, StateStarting); err != nil {
		t.Fatal(err)
	}
	if err := env.rt.TransitionState("glm", gen, StateReady); err != nil {
		t.Fatal(err)
	}
	if err := env.rt.TransitionState("glm", gen, StateFailed); err != nil {
		t.Fatalf("ready must be allowed to record a rank death verdict: %v", err)
	}
	// Failed -> Stopping (owned failed cleanup retry) on the SAME failed
	// generation: no new operation is begun over the failed record.
	if err := env.rt.TransitionState("glm", gen, StateStopping); err != nil {
		t.Fatalf("failed must be allowed to retry an owned cleanup: %v", err)
	}
	// Illegal transitions stay refused.
	if err := env.rt.TransitionState("glm", gen, StateStarting); err == nil {
		t.Fatal("stopping must never restart via a raw transition")
	}
	if err := env.rt.TransitionState("glm", gen, StateReady); err == nil {
		t.Fatal("stopping must never become ready via a raw transition")
	}
}

// TestStartModelSelectedRefusesEditedTopology: a direct parent call with the
// same recipe fingerprint but an edited host (ssh, direct IP or root) must
// REFUSE an in-progress record instead of adopting it and waiting out the
// health timeout — with ZERO start/stop commands against the wrong hosts.
func TestStartModelSelectedRefusesEditedTopology(t *testing.T) {
	backend := newNativeBackend(t, "glm")
	for _, tc := range []struct {
		name string
		edit func(Object)
	}{
		{"ssh", func(c Object) { obj(obj(c["nodes"])["spark01"])["ssh"] = "edited-host" }},
		{"direct_ip", func(c Object) { obj(obj(c["nodes"])["spark01"])["direct_ip"] = "10.9.9.9" }},
		{"root", func(c Object) { c["root"] = t.TempDir() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newNativeEnv(t, backend)
			rt := env.rt
			cfg, _ := env.selected()
			env.begin() // a queued in-progress record frozen on the original topology
			edited := copyObject(cfg)
			tc.edit(edited)
			// The BASE runtime keeps the original root: the edited cfg is
			// only the selected snapshot passed to startModelSelected, so
			// a root edit is refused before any clone, record read, probe
			// or write — never mkdir'ing an unowned path.
			rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
				j := strings.Join(args, " ")
				if strings.Contains(j, "start") || strings.Contains(j, "stop") || strings.Contains(j, "reset-failed") {
					return CommandResult{}, fail("an edited host must never be started or stopped: %v", args)
				}
				return CommandResult{}, fail("unexpected command after refusal: %v", args)
			}
			err := rt.startModelSelected(context.Background(), "glm", edited)
			if err == nil || !(strings.Contains(err.Error(), "different host topology") || strings.Contains(err.Error(), "different controller root")) {
				t.Fatalf("an edited host topology must be refused, got %v", err)
			}
			if len(env.calls.all()) != 0 {
				t.Fatalf("a topology refusal must issue zero commands: %v", env.calls.all())
			}
		})
	}
}
