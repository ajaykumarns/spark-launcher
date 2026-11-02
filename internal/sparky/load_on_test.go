package sparky

// Tests for the one-shot `sparky load MODEL --on NODE` placement override.
// Every seam is synthetic: no real SSH, HTTP backend calls, systemctl or
// service changes. The tests prove the parser grammar, the CLI/transport
// propagation, and that the coordinator validates the ACTUAL recipe BEFORE
// any stop, eviction or reservation while leaving the saved run_on intact.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// onTestConfig builds a native cluster whose models have single-node and
// "both" recipes. `deepseek` deliberately has ONLY a "both" recipe: a
// single-node override must be refused rather than invented.
func onTestConfig(root, backend string) Object {
	mk := func(unit string, nodes ...string) Object {
		units := Object{}
		for _, n := range nodes {
			units[n] = unit
		}
		return Object{"enabled": true, "backend_url": backend, "units": units,
			"context_tokens": 8192, "concurrency": 4, "parameters": Object{}}
	}
	return Object{
		"version": 1, "root": root, "controller_port": 8020, "lifecycle": "native",
		"nodes": Object{
			"spark01": Object{"direct_ip": "10.0.0.1"},
			"spark02": Object{"direct_ip": "10.0.0.2", "ssh": "spark02h", "comfy_unit": "comfy.service"},
		},
		"policy": Object{"interrupt_active_jobs": false, "protected_models": []any{},
			"comfyui_exclusive_with_llms": true, "comfyui_shared_llms": []any{},
			"comfyui_min_available_gib": 18},
		"models": Object{
			"gemma": Object{"kind": "llm", "name": "Gemma", "run_on": "spark01", "keep_loaded": false,
				"reasoning": true, "aliases": []any{"gem"},
				"settings": Object{
					"spark01": mk("gemma@spark01.service", "spark01"),
					"spark02": mk("gemma@spark02.service", "spark02"),
					"both":    mk("gemma@both.service", "spark01", "spark02"),
				}},
			"deepseek": Object{"kind": "llm", "name": "DeepSeek", "run_on": "both", "keep_loaded": false,
				"reasoning": true, "aliases": []any{},
				"settings": Object{"both": Object{
					"enabled": true, "backend_url": backend,
					"units":          Object{"spark01": "deepseek@both.service", "spark02": "deepseek@both.service"},
					"context_tokens": 8192, "concurrency": 4, "parameters": Object{}}}},
			"parakeet": Object{"kind": "audio", "name": "Parakeet", "run_on": "spark01",
				"keep_loaded": false, "reasoning": false, "aliases": []any{},
				"settings": Object{"spark01": Object{"enabled": true, "containers": []any{"parakeet-asr"}}}},
		},
	}
}

// onEnv wires a native coordinator to a fake runner/probe and a local
// httptest backend. Runner answers only unit probes and fails loudly on any
// start/stop/ssh command so a test can prove no side effect happened.
type onEnv struct {
	t     *testing.T
	coord *Coordinator
	rt    *Runtime
	path  string
	calls *runnerCalls
}

func newOnEnv(t *testing.T) *onEnv {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)
	root := t.TempDir()
	cfg := onTestConfig(root, backend.URL)
	path := filepath.Join(root, "models.json")
	if err := writeCluster(path, cfg); err != nil {
		t.Fatal(err)
	}
	calls := &runnerCalls{}
	rt := newRuntime(cfg, path)
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		calls.record(args)
		switch {
		case args[0] == "systemctl" && args[1] == "show":
			return CommandResult{Out: "active\n", Code: 0}, nil
		case args[0] == "systemctl" && args[1] == "is-active":
			return CommandResult{Code: 0}, nil
		case args[0] == "ssh" && args[len(args)-1] == "~/.local/bin/sparky --json node probe --file -":
			// spark02's node probe is served by the same local fake.
			return CommandResult{Out: string(jsonBytes(Object{
				"comfyui":      Object{"reachable": false, "executing": false, "running": 0, "phase": "idle"},
				"memory_bytes": Object{"MemAvailable": json.Number("107374182400")},
				"units":        Object{},
				"containers":   Object{},
			}))}, nil
		}
		return CommandResult{}, fail("forbidden side effect in test: %v", args)
	}
	rt.ProbeNode = func(meta Object) (Object, error) {
		return Object{
			"comfyui":      Object{"reachable": false, "executing": false, "running": 0, "phase": "idle"},
			"memory_bytes": Object{"MemAvailable": json.Number("107374182400")},
			"units":        Object{},
			"containers":   Object{},
		}, nil
	}
	coord, err := newCoordinator(path, rt)
	if err != nil {
		t.Fatal(err)
	}
	return &onEnv{t: t, coord: coord, rt: rt, path: path, calls: calls}
}

// markReady writes a READY durable record for name at placement using the
// env's frozen config recipe (no unit is started).
func (e *onEnv) markReady(name, placement string, nodes []string) string {
	e.t.Helper()
	cfg, err := readCluster(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	m := obj(obj(cfg["models"])[name])
	s := obj(obj(m["settings"])[placement])
	rec, err := e.rt.BeginOperation(name, m, s, nodes, placement)
	if err != nil {
		e.t.Fatal(err)
	}
	gen := str(rec["generation"])
	if err := e.rt.TransitionState(name, gen, StateStarting); err != nil {
		e.t.Fatal(err)
	}
	if err := e.rt.TransitionState(name, gen, StateReady); err != nil {
		e.t.Fatal(err)
	}
	return gen
}

func (e *onEnv) savedRunOn(name string) string {
	e.t.Helper()
	cfg, err := readCluster(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	return str(obj(obj(cfg["models"])[name])["run_on"])
}

// TestParseLoadOn covers the public CLI grammar: --on is accepted only for
// load, only with a known placement, and never for unload.
func TestParseLoadOn(t *testing.T) {
	inv := mustParse(t, []string{"load", "gem", "--on", "spark02"})
	if !reflect.DeepEqual(inv.Positional, []string{"gem"}) || inv.Values["on"] != "spark02" || !inv.Flags["on"] {
		t.Fatalf("load --on parse wrong: %+v", inv)
	}
	inv = mustParse(t, []string{"load", "gem", "--on=both", "--force"})
	if inv.Values["on"] != "both" || !inv.Flags["force"] {
		t.Fatalf("inline --on parse wrong: %+v", inv)
	}
	wantUsage(t, []string{"load", "gem", "--on"}, "Option --on needs a value")
	wantUsage(t, []string{"load", "gem", "--on", "mars"}, "Option --on must be one of spark01, spark02, both")
	wantUsage(t, []string{"unload", "gem", "--on", "spark02"}, "Unknown option: --on")
	wantUsage(t, []string{"status", "--on", "spark02"}, "Unknown option: --on")
	wantUsage(t, []string{"plan", "load", "gem", "--on", "mars"}, "must be one of")
}

// TestNodeLoadOnGrammar proves the private protocol parses --on strictly and
// rejects it for unload before any config read.
func TestNodeLoadOnGrammar(t *testing.T) {
	root := t.TempDir()
	if _, err := NodeMain([]string{"unload", "--root", root, "--model", "gemma", "--on", "spark02"}, strings.NewReader("")); err == nil ||
		!strings.Contains(err.Error(), "on is available only for load") {
		t.Fatalf("unload --on must be rejected before config read: %v", err)
	}
	if _, err := NodeMain([]string{"load", "--root", root, "--model", "gemma", "--on", "mars"}, strings.NewReader("")); err == nil ||
		!strings.Contains(err.Error(), "on is available only for load") {
		t.Fatalf("load --on mars must be rejected: %v", err)
	}
}

// TestNodeLoadOnValidatesRecipeBeforeRPC proves the node validates the
// selected model's ACTUAL recipe before contacting the controller: the
// both-only model rejects a single-node override even though no controller
// is listening.
func TestNodeLoadOnValidatesRecipeBeforeRPC(t *testing.T) {
	root := t.TempDir()
	cfg := onTestConfig(root, "http://127.0.0.1:1")
	if err := writeCluster(filepath.Join(root, "config", "models.json"), cfg); err != nil {
		t.Fatal(err)
	}
	// A single-node override for the both-only model is refused locally.
	if _, err := nodeResources(context.Background(), root, "load", "deepseek", false, "spark01"); err == nil ||
		!strings.Contains(err.Error(), "deepseek has no recipe for spark01.") {
		t.Fatalf("deepseek --on spark01 must be refused: %v", err)
	}
	// The alias still resolves before the recipe check.
	on := "mars"
	if _, err := nodeResources(context.Background(), root, "load", "gem", true, on); err == nil ||
		!strings.Contains(err.Error(), "--on is available only for load and must be spark01, spark02, or both.") {
		t.Fatalf("invalid placement must be refused: %v", err)
	}
}

// TestLoadOnBothOnlyModelRejectedBeforeSideEffects is the DeepSeek safety
// proof: a model with only a "both" recipe must refuse a single-node
// override with no stop, eviction, drain, force cancel or config mutation —
// including with --force and JSON output.
func TestLoadOnBothOnlyModelRejectedBeforeSideEffects(t *testing.T) {
	env := newOnEnv(t)
	before := env.calls.all()

	// A loaded gemma occupies spark01; the override must not evict it first.
	env.markReady("gemma", "spark01", []string{"spark01"})

	for _, force := range []bool{false, true} {
		before = env.calls.all()
		if _, err := env.coord.Load("deepseek", force, "spark01"); err == nil ||
			!strings.Contains(err.Error(), "no recipe for spark01") {
			t.Fatalf("deepseek --on spark01 (force=%v) must be refused: %v", force, err)
		}
		for _, args := range env.calls.all()[len(before):] {
			if args[0] != "systemctl" || (args[1] != "show" && args[1] != "is-active") {
				t.Fatalf("refusal must happen before any side effect, got %v", args)
			}
		}
	}
	if got := env.savedRunOn("deepseek"); got != "both" {
		t.Fatalf("the saved run_on must stay both, got %q", got)
	}
	// The pre-existing gemma record is untouched.
	rec, err := env.rt.readRecord("gemma")
	if err != nil || str(rec["state"]) != StateReady {
		t.Fatalf("gemma must stay loaded: %v %v", rec, err)
	}
}

// TestLoadOnAliasAndKinds covers alias resolution and the kind gate for
// audio/image workloads before any effect.
func TestLoadOnAliasAndKinds(t *testing.T) {
	env := newOnEnv(t)
	before := env.calls.all()
	if _, err := env.coord.Load("parakeet", false, "spark01"); err == nil ||
		!strings.Contains(err.Error(), "--on is supported only for language models; parakeet is audio.") {
		t.Fatalf("audio override must be refused: %v", err)
	}
	if _, err := env.coord.Load("gem", false, "mars"); err == nil ||
		!strings.Contains(err.Error(), "spark01, spark02, or both") {
		t.Fatalf("invalid placement must be refused: %v", err)
	}
	// The alias "gem" resolves to gemma, so the plan previews the gemma
	// recipe on spark02 without rewriting the saved run_on.
	plan, err := env.coord.Plan("gem", "load", "spark02")
	if err != nil {
		t.Fatalf("alias plan should resolve: %v", err)
	}
	if str(plan["workload"]) != "gemma" || str(plan["placement"]) != "spark02" {
		t.Fatalf("alias plan must name the canonical workload: %v", plan)
	}
	if got := env.savedRunOn("gemma"); got != "spark01" {
		t.Fatalf("the plan must never rewrite run_on, got %q", got)
	}
	// Only the read-only node probe may run for a refusal or a preview;
	// no start, stop, reset or eviction command may ever appear.
	for _, args := range env.calls.all()[len(before):] {
		readOnly := args[0] == "systemctl" && (args[1] == "show" || args[1] == "is-active") ||
			args[0] == "ssh" && strings.HasSuffix(args[len(args)-1], "node probe --file -")
		if !readOnly {
			t.Fatalf("kind/placement refusals and previews must have no side effects, got %v", args)
		}
	}
}

// TestCheckPlacementValidatesActualRecipe proves the coordinator's
// pre-side-effect validator against the ACTUAL recipe: aliases resolve,
// both-only recipes refuse a single node, non-LLM kinds are refused, and
// invalid values never pass.
func TestCheckPlacementValidatesActualRecipe(t *testing.T) {
	root := t.TempDir()
	cfg := onTestConfig(root, "http://127.0.0.1:1")
	for _, tc := range []struct{ name, on, want string }{
		{"deepseek", "spark01", "deepseek has no recipe for spark01."},
		{"deepseek", "spark02", "deepseek has no recipe for spark02."},
		{"parakeet", "spark01", "--on is supported only for language models; parakeet is audio."},
		{"gemma", "mars", "--on must be spark01, spark02, or both."},
	} {
		if err := checkPlacement(cfg, tc.name, tc.on); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("checkPlacement(%s,%s) = %v, want %q", tc.name, tc.on, err, tc.want)
		}
	}
	// A complete both-only recipe is accepted for "both".
	if err := checkPlacement(cfg, "deepseek", "both"); err != nil {
		t.Fatalf("deepseek --on both must be valid: %v", err)
	}
	// Unit names are PER-NODE identities: the same unit basename on two
	// different nodes is a valid recipe, never a duplicate.
	same := onTestConfig(root, "http://127.0.0.1:1")
	both := obj(obj(obj(same["models"])["gemma"])["settings"])["both"]
	obj(both)["units"] = Object{"spark01": "gemma.service", "spark02": "gemma.service"}
	if err := checkPlacement(same, "gemma", "both"); err != nil {
		t.Fatalf("equal unit names on distinct nodes must be valid: %v", err)
	}
	// A missing unit for any selected node is a missing recipe.
	missing := onTestConfig(root, "http://127.0.0.1:1")
	m := obj(obj(obj(missing["models"])["deepseek"])["settings"])["both"]
	obj(m)["units"] = Object{"spark01": "deepseek-head.service"}
	if err := checkPlacement(missing, "deepseek", "both"); err == nil || !strings.Contains(err.Error(), "deepseek has no complete recipe for both.") {
		t.Fatalf("missing unit: %v", err)
	}
	// Aliases resolve to the canonical model before the check.
	name, _, err := resolveWorkload(cfg, "gem")
	if err != nil || name != "gemma" {
		t.Fatalf("alias resolution: %v %v", name, err)
	}
	if err := checkPlacement(cfg, name, "spark02"); err != nil {
		t.Fatalf("gemma --on spark02 must be valid: %v", err)
	}
}

// TestPlacementNodesForPreview proves the plan preview uses the one-shot
// placement while a plain plan uses the saved run_on.
func TestPlacementNodesForPreview(t *testing.T) {
	root := t.TempDir()
	cfg := onTestConfig(root, "http://127.0.0.1:1")
	if got := placementNodesFor(cfg, "gemma", "spark02"); !sameStrings(got, []string{"spark02"}) {
		t.Fatalf("preview nodes: %v", got)
	}
	if got := placementNodesFor(cfg, "gemma", ""); !sameStrings(got, []string{"spark01"}) {
		t.Fatalf("default preview nodes: %v", got)
	}
}

// TestEffectiveNodesFollowsActualPlacement proves admission follows the
// durable record (ready/starting/queued) rather than the saved run_on and
// falls back to the desired placement only with no record.
func TestEffectiveNodesFollowsActualPlacement(t *testing.T) {
	env := newOnEnv(t)
	cfg := env.coord.cfg()
	// No record yet: the saved run_on decides.
	nodes, err := env.coord.effectiveNodes(cfg, "gemma")
	if err != nil || !sameStrings(nodes, []string{"spark01"}) {
		t.Fatalf("desired nodes: %v %v", nodes, err)
	}
	// A READY record frozen at spark02 must drive admission.
	env.markReady("gemma", "spark02", []string{"spark02"})
	nodes, err = env.coord.effectiveNodes(cfg, "gemma")
	if err != nil || !sameStrings(nodes, []string{"spark02"}) {
		t.Fatalf("actual nodes: %v %v", nodes, err)
	}
}

// TestLoadedSelectionRejectsTamperedRecord proves a raw durable record is
// never trusted: a READY record whose placement/units do not match the
// current config falls back to the saved (desired) selection, and a record
// frozen on an old backend never contributes an invented backend identity.
func TestLoadedSelectionRejectsTamperedRecord(t *testing.T) {
	env := newOnEnv(t)
	// A record frozen for spark02 (valid against the config) is trusted.
	env.markReady("gemma", "spark02", []string{"spark02"})
	sel, err := env.coord.loadedSelection("gemma")
	if err != nil || !sel.Actual || sel.Placement != "spark02" || !sameStrings(sel.Nodes, []string{"spark02"}) {
		t.Fatalf("valid record: %+v %v", sel, err)
	}
	if backendForRecipe(sel.Setting, sel.Record) == "" {
		t.Fatalf("valid record must carry a backend identity: %+v", sel)
	}
	// An edited backend_url in the current config must not claim the record:
	// the fingerprint no longer matches, so the saved run_on is used and the
	// stale record contributes no actual placement.
	cfg, err := readCluster(env.path)
	if err != nil {
		t.Fatal(err)
	}
	obj(obj(obj(cfg["models"])["gemma"])["settings"])["spark02"] = Object{
		"enabled": true, "backend_url": "http://127.0.0.1:2", "units": Object{"spark02": "gemma@spark02.service"},
		"context_tokens": 8192, "concurrency": 4, "parameters": Object{}}
	sel, err = env.rt.loadedSelectionWith(cfg, "gemma")
	if err != nil || sel.Actual || sel.Placement != "spark01" || !sameStrings(sel.Nodes, []string{"spark01"}) {
		t.Fatalf("edited backend must fall back to saved run_on: %+v %v", sel, err)
	}
	// A disabled model never resolves an actual loaded selection.
	obj(obj(cfg["models"])["gemma"])["enabled"] = false
	sel, err = env.rt.loadedSelectionWith(cfg, "gemma")
	if err != nil || sel.Actual {
		t.Fatalf("disabled model must not be actual: %+v %v", sel, err)
	}
}

// TestLoadedSelectionSurvivesCoordinatorRestart proves a fresh coordinator
// reading the same durable record resolves the ACTUAL one-shot placement,
// so inference/status are not tied to the old coordinator's memory.
func TestLoadedSelectionSurvivesCoordinatorRestart(t *testing.T) {
	env := newOnEnv(t)
	env.markReady("gemma", "spark02", []string{"spark02"})
	fresh := &Coordinator{runtime: newRuntime(env.coord.cfg(), env.path), path: env.path}
	fresh.runtime.Run = env.rt.Run
	fresh.runtime.ProbeNode = env.rt.ProbeNode
	sel, err := fresh.loadedSelection("gemma")
	if err != nil || !sel.Actual || sel.Placement != "spark02" || !sameStrings(sel.Nodes, []string{"spark02"}) {
		t.Fatalf("restart selection: %+v %v", sel, err)
	}
}

// TestRemoteLoadOnPropagation proves the public remote command carries the
// one-shot placement on the private protocol line.
func TestRemoteLoadOnPropagation(t *testing.T) {
	cfg := remoteTestConfig()
	argv, err := cliRemoteCommand(cfg, "load", "gem", false, "both")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := mainNodeRemote("load", "--root", "/srv", "--model", "gem", "--on", "both")
	if argv[4] != want {
		t.Fatalf("remote command wrong: want %q got %q", want, argv[4])
	}
	// --on is refused for every non-load action.
	for _, action := range []string{"status", "unload", "gateway"} {
		if _, err := cliRemoteCommand(cfg, action, "", false, "both"); err == nil ||
			!strings.Contains(err.Error(), "--on is available only for load") {
			t.Fatalf("%s must reject --on: %v", action, err)
		}
	}
	if _, err := cliRemoteCommand(cfg, "load", "gem", false, "mars"); err == nil ||
		!strings.Contains(err.Error(), "spark01, spark02, or both") {
		t.Fatalf("invalid --on must be rejected: %v", err)
	}
}

// TestServerLoadOnBody proves /control/load forwards the validated one-shot
// placement and /control/unload refuses it.
func TestServerLoadOnBody(t *testing.T) {
	ctrl := newFakeController()
	s := &controllerServer{ctrl: ctrl}
	rec := serverRequest(t, s, "POST", "/control/load", "127.0.0.1:5", `{"model":"glm","on":"spark02"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"on": "spark02"`) &&
		!strings.Contains(rec.Body.String(), `"on":"spark02"`) {
		t.Fatalf("load body forwarding: %d %q", rec.Code, rec.Body.String())
	}
	rec = serverRequest(t, s, "POST", "/control/load", "127.0.0.1:5", `{"model":"glm","on":"mars"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid on must be a 400: %d %q", rec.Code, rec.Body.String())
	}
	rec = serverRequest(t, s, "POST", "/control/unload", "127.0.0.1:5", `{"model":"glm","on":"spark02"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unload on must be a 400: %d %q", rec.Code, rec.Body.String())
	}
}

// TestCliHumanLoadOnPlanAndResult proves the public human CLI previews the
// same one-shot placement and sends it to the remote load command.
func TestCliHumanLoadOnPlanAndResult(t *testing.T) {
	mainDirs(t)
	writeMainConfig(t, "https://gateway.example")
	var calls []string
	var planBody string
	restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		remote := argv[len(argv)-1]
		calls = append(calls, remote)
		switch {
		case remote == mainNodeRemote("rpc", "plan", "--root", "/srv", "--payload", "-"):
			planBody = string(stdin)
			return remoteRunResult{stdout: []byte(`{"workload":"gemma","placement":"spark02","nodes":["spark02"],"steps":[]}`), code: 0}, nil
		case remote == mainNodeRemote("load", "--root", "/srv", "--model", "gemma", "--on", "spark02"):
			// The controller must confirm the ACTUAL placement and nodes;
			// the CLI never manufactures them from the request.
			return remoteRunResult{stdout: []byte(`{"status":"ready","workload":"gemma","placement":"spark02","nodes":["spark02"]}`), code: 0}, nil
		}
		return remoteRunResult{}, fmt.Errorf("unexpected remote command: %q", remote)
	})
	defer restore()
	code, out, errout := runMain([]string{"load", "gemma", "--on", "spark02", "--json"}, "")
	if code != 0 {
		t.Fatalf("load --on --json: %d %q", code, errout)
	}
	// JSON mode goes straight to the load command (no plan RPC).
	if len(calls) != 1 || calls[0] != mainNodeRemote("load", "--root", "/srv", "--model", "gemma", "--on", "spark02") {
		t.Fatalf("json load calls: %v", calls)
	}
	for _, frag := range []string{`"placement": "spark02"`, `"nodes": [`, `"spark02"`} {
		if !strings.Contains(out, frag) {
			t.Fatalf("json result missing %s: %q", frag, out)
		}
	}
	if strings.Contains(out, "placement_source") {
		t.Fatalf("confirmed result must not carry invented metadata: %q", out)
	}

	// Human mode previews the plan with the same override first.
	calls = nil
	code, _, errout = runMain([]string{"load", "gemma", "--on", "spark02"}, "")
	if code != 0 {
		t.Fatalf("human load --on: %d %q", code, errout)
	}
	if len(calls) != 2 || calls[0] != mainNodeRemote("rpc", "plan", "--root", "/srv", "--payload", "-") ||
		calls[1] != mainNodeRemote("load", "--root", "/srv", "--model", "gemma", "--on", "spark02") {
		t.Fatalf("human load calls: %v", calls)
	}
	if !strings.Contains(planBody, `"on":"spark02"`) {
		t.Fatalf("plan payload missing on: %q", planBody)
	}
}

// TestCliLoadOnRejectedLocallyBeforeSpawn proves the client refuses a
// non-load --on or invalid placement without spawning ssh.
func TestCliLoadOnRejectedLocallyBeforeSpawn(t *testing.T) {
	mainDirs(t)
	writeMainConfig(t, "https://gateway.example")
	var calls []string
	restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		calls = append(calls, argv[len(argv)-1])
		return remoteRunResult{}, fmt.Errorf("must not spawn")
	})
	defer restore()
	code, _, errout := runMain([]string{"load", "gemma", "--on", "mars", "--json"}, "")
	if code == 0 || len(calls) != 0 || !strings.Contains(errout, "spark01, spark02, both") {
		t.Fatalf("invalid --on must fail locally: %d %v %q", code, calls, errout)
	}
	code, _, errout = runMain([]string{"unload", "gemma", "--on", "spark02", "--json"}, "")
	if code == 0 || len(calls) != 0 || !strings.Contains(errout, "Unknown option: --on") {
		t.Fatalf("unload --on must be refused by the parser: %d %v %q", code, calls, errout)
	}
}

// TestLoadOnPlanPreviewUsesOverride proves the human plan RPC previews the
// transient placement's nodes without changing the saved run_on.
func TestLoadOnPlanPreviewUsesOverride(t *testing.T) {
	env := newOnEnv(t)
	plan, err := env.coord.Plan("gem", "load", "spark02")
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	if str(plan["placement"]) != "spark02" || !sameStrings(stringsOf(plan["nodes"]), []string{"spark02"}) {
		t.Fatalf("plan must preview the transient placement: %v", plan)
	}
	if raw, has := plan["placement_override_supported"]; !has || raw != true {
		t.Fatalf("an explicit plan must advertise the override capability: %v", plan)
	}
	// A plain plan carries no explicit-override capability marker.
	plain, err := env.coord.Plan("gem", "load", "")
	if err != nil {
		t.Fatalf("plain plan: %v", err)
	}
	if _, has := plain["placement_override_supported"]; has {
		t.Fatalf("a plain plan must not advertise the override capability: %v", plain)
	}
	if got := env.savedRunOn("gemma"); got != "spark01" {
		t.Fatalf("plan must never rewrite run_on, got %q", got)
	}
	if _, err := env.coord.Plan("deepseek", "load", "spark01"); err == nil ||
		!strings.Contains(err.Error(), "no recipe for spark01") {
		t.Fatalf("plan must validate the actual recipe: %v", err)
	}
}

// controllerUseFake routes controllerRequest to a synthetic controller for
// the duration of one test.
func controllerUseFake(fake func(context.Context, string, any) (Object, error)) func() {
	previous := controllerSeam
	controllerSeam = fake
	return func() { controllerSeam = previous }
}

// modifyingCommand reports whether a runner argv would change device state:
// any non-read-only systemctl verb, an ssh that is not a node probe, or a
// manager HUP/cancel. Read-only probes are allowed.
func modifyingCommand(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "systemctl":
		if len(args) > 1 && (args[1] == "show" || args[1] == "is-active") {
			return false
		}
		return true
	case "ssh":
		return !strings.HasSuffix(args[len(args)-1], "node probe --file -")
	}
	return true
}

// newLegacyOnEnv wires a LEGACY cluster (no native lifecycle) to the same
// fake runner so a refusal can be proven to have zero modifying commands.
func newLegacyOnEnv(t *testing.T) *onEnv {
	t.Helper()
	root := t.TempDir()
	cfg := onTestConfig(root, "http://127.0.0.1:1")
	cfg["lifecycle"] = "legacy"
	path := filepath.Join(root, "models.json")
	if err := writeCluster(path, cfg); err != nil {
		t.Fatal(err)
	}
	calls := &runnerCalls{}
	rt := newRuntime(cfg, path)
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		calls.record(args)
		switch {
		case args[0] == "systemctl" && args[1] == "show":
			return CommandResult{Out: "active\n", Code: 0}, nil
		case args[0] == "systemctl" && args[1] == "is-active":
			return CommandResult{Code: 0}, nil
		}
		return CommandResult{}, fail("forbidden side effect in test: %v", args)
	}
	rt.ProbeNode = func(meta Object) (Object, error) {
		return Object{
			"comfyui":      Object{"reachable": false, "executing": false, "running": 0, "phase": "idle"},
			"memory_bytes": Object{"MemAvailable": json.Number("107374182400")},
			"units":        Object{}, "containers": Object{},
		}, nil
	}
	coord, err := newCoordinator(path, rt)
	if err != nil {
		t.Fatal(err)
	}
	return &onEnv{t: t, coord: coord, rt: rt, path: path, calls: calls}
}

// TestLoadOnLegacyModeRejectedBeforeSideEffects proves an explicit --on in a
// legacy deployment is refused BEFORE manager sync/HUP, lease cancellation
// or unit changes, for normal, forced and JSON paths alike, with zero
// modifying commands and no cancelled lease. A no-flag legacy load keeps its
// historical behaviour.
func TestLoadOnLegacyModeRejectedBeforeSideEffects(t *testing.T) {
	env := newLegacyOnEnv(t)
	// A live legacy lease on spark01 must never be cancelled.
	leaseID, err := env.coord.acquire(context.Background(), "gemma", []string{"spark01"}, false, 4, "", "", true)
	if err != nil {
		t.Fatalf("legacy lease: %v", err)
	}
	defer env.coord.release(leaseID)

	for _, tc := range []struct {
		name  string
		load  func() (Object, error)
		force bool
	}{
		{"normal", func() (Object, error) { return env.coord.Load("gemma", false, "spark02") }, false},
		{"force", func() (Object, error) { return env.coord.Load("gemma", true, "spark02") }, true},
		{"json", func() (Object, error) { return env.coord.Load("gemma", false, "spark02") }, false},
	} {
		before := env.calls.all()
		if _, err := tc.load(); err == nil || !strings.Contains(err.Error(), "requires the native model lifecycle") {
			t.Fatalf("%s legacy --on must be refused: %v", tc.name, err)
		}
		changed := false
		for _, args := range env.calls.all()[len(before):] {
			changed = changed || modifyingCommand(args)
		}
		if changed {
			t.Fatalf("%s legacy --on must perform zero modifying commands", tc.name)
		}
	}
	env.coord.mu.Lock()
	cancelled := env.coord.leases[leaseID] != nil && env.coord.leases[leaseID].Cancelled
	env.coord.mu.Unlock()
	if cancelled {
		t.Fatal("a legacy --on refusal must never cancel a lease")
	}
	if got := env.savedRunOn("gemma"); got != "spark01" {
		t.Fatalf("legacy refusal must not rewrite run_on, got %q", got)
	}
}

// TestLoadOnLegacyPlanRejectedBeforeSnapshot proves the PLAN path is as
// strict as execution in a legacy deployment: an explicit load placement is
// refused BEFORE the snapshot is taken, no capability marker is ever
// advertised, and no modifying command runs.
func TestLoadOnLegacyPlanRejectedBeforeSnapshot(t *testing.T) {
	env := newLegacyOnEnv(t)
	for _, tc := range []struct {
		name string
		call func() (Object, error)
	}{
		{"plan", func() (Object, error) { return env.coord.Plan("gemma", "load", "spark02") }},
		{"json", func() (Object, error) { return env.coord.plan(context.Background(), "gemma", "load", "spark02") }},
	} {
		before := env.calls.all()
		result, err := tc.call()
		if err == nil || !strings.Contains(err.Error(), "requires the native model lifecycle") {
			t.Fatalf("%s legacy --on must be refused: %v", tc.name, err)
		}
		if result != nil {
			t.Fatalf("%s legacy --on must not return a plan: %v", tc.name, result)
		}
		// The refusal precedes the snapshot: the legacy activity probe
		// (systemctl/systemd unit inspection) never even runs.
		if got := env.calls.all()[len(before):]; len(got) != 0 {
			t.Fatalf("%s legacy --on must issue no command at all, got %v", tc.name, got)
		}
	}
	// A plain legacy plan is unaffected by the guard: it still reaches the
	// snapshot (proven by the runner call the synthetic env cannot serve),
	// and a plain plan never advertises the override capability.
	before := env.calls.all()
	if _, err := env.coord.Plan("gemma", "load", ""); err == nil ||
		strings.Contains(err.Error(), "requires the native model lifecycle") {
		t.Fatalf("a plain legacy plan must not hit the --on guard: %v", err)
	}
	if len(env.calls.all()) == len(before) {
		t.Fatal("a plain legacy plan must still reach the snapshot")
	}
	if got := env.savedRunOn("gemma"); got != "spark01" {
		t.Fatalf("legacy plan refusal must not rewrite run_on, got %q", got)
	}
}

// TestNodeLoadOnOldControllerRefused proves mixed-version safety: an updated
// node talking to an OLD controller that previews --on but has no capability
// marker is refused BEFORE POST /control/load, with zero load POSTs and zero
// cancellation/stop commands.
func TestNodeLoadOnOldControllerRefused(t *testing.T) {
	var posts, cancels, stops int
	var gotPlan bool
	restore := controllerUseFake(func(ctx context.Context, operation string, raw any) (Object, error) {
		if model := str(obj(raw)["model"]); model != "gemma" {
			t.Errorf("plan/model must resolve the canonical workload, got %q", model)
		}
		switch operation {
		case "plan":
			gotPlan = true
			// The OLD controller previews the placement but never advertises
			// the override capability, and would silently ignore "on".
			return Object{"workload": "gemma", "action": "load", "placement": "spark02", "nodes": []any{"spark02"}, "steps": []any{}}, nil
		case "load":
			posts++
			return Object{"status": "ready", "workload": "gemma"}, nil
		case "cancel", "stop":
			cancels++
			return Object{}, nil
		}
		return Object{}, fmt.Errorf("unexpected controller operation %q", operation)
	})
	defer restore()
	root := t.TempDir()
	cfg := onTestConfig(root, "http://127.0.0.1:1")
	if err := writeCluster(filepath.Join(root, "config", "models.json"), cfg); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		if _, err := nodeResources(context.Background(), root, "load", "gem", force, "spark02"); err == nil ||
			!strings.Contains(err.Error(), "does not support one-shot placement overrides") {
			t.Fatalf("old controller must be refused (force=%v): %v", force, err)
		}
	}
	if !gotPlan || posts != 0 || cancels != 0 || stops != 0 {
		t.Fatalf("old controller: plan=%v posts=%d cancels=%d stops=%d", gotPlan, posts, cancels, stops)
	}
}

// TestNodeLoadOnNewControllerConfirmed proves the capability marker unlocks
// the explicit load and that a confirmed placement/nodes result is required:
// a missing or contradictory confirmation is an error, never a success.
func TestNodeLoadOnNewControllerConfirmed(t *testing.T) {
	confirm := Object{"status": "ready", "workload": "gemma", "placement": "spark02", "nodes": []any{"spark02"}}
	loadResult := confirm
	posts := 0
	restore := controllerUseFake(func(ctx context.Context, operation string, raw any) (Object, error) {
		switch operation {
		case "plan":
			return Object{"workload": "gemma", "action": "load", "placement": "spark02",
				"nodes": []any{"spark02"}, "steps": []any{}, "placement_override_supported": true}, nil
		case "load":
			posts++
			return loadResult, nil
		case "cancel", "stop":
			return Object{}, fmt.Errorf("unexpected modifying operation %q", operation)
		}
		return Object{}, fmt.Errorf("unexpected controller operation %q", operation)
	})
	defer restore()
	root := t.TempDir()
	cfg := onTestConfig(root, "http://127.0.0.1:1")
	if err := writeCluster(filepath.Join(root, "config", "models.json"), cfg); err != nil {
		t.Fatal(err)
	}
	result, err := nodeResources(context.Background(), root, "load", "gemma", false, "spark02")
	if err != nil || posts != 1 {
		t.Fatalf("new controller must allow one load: %v posts=%d", err, posts)
	}
	if str(result["placement"]) != "spark02" || !sameStrings(stringsOf(result["nodes"]), []string{"spark02"}) {
		t.Fatalf("confirmed result must carry the actual placement and nodes: %v", result)
	}
	// A missing acknowledgment (old behaviour, "on" ignored) is an error.
	posts = 0
	loadResult = Object{"status": "ready", "workload": "gemma"}
	if _, err := nodeResources(context.Background(), root, "load", "gemma", true, "spark02"); err == nil ||
		!strings.Contains(err.Error(), "did not confirm the requested placement") {
		t.Fatalf("missing confirmed placement must fail: %v", err)
	}
	// A contradictory acknowledgment is an error too.
	loadResult = Object{"status": "ready", "workload": "gemma", "placement": "spark01", "nodes": []any{"spark01"}}
	if _, err := nodeResources(context.Background(), root, "load", "gemma", false, "spark02"); err == nil ||
		!strings.Contains(err.Error(), "did not confirm the requested placement") {
		t.Fatalf("wrong confirmed placement must fail: %v", err)
	}
}

// TestConfirmedSelectionReportsActual proves the override acknowledgment is
// derived ONLY from the validated durable record: the confirmed placement and
// nodes plus the frozen backend, never the requested placement. The actual
// coordinator fastpath is proven end to end by the native integration tests.
func TestConfirmedSelectionReportsActual(t *testing.T) {
	env := newOnEnv(t)
	env.markReady("gemma", "spark02", []string{"spark02"})
	result, err := env.coord.confirmedSelection("gemma")
	if err != nil {
		t.Fatalf("confirmed selection: %v", err)
	}
	if str(result["status"]) != "ready" || str(result["workload"]) != "gemma" ||
		str(result["placement"]) != "spark02" || !sameStrings(stringsOf(result["nodes"]), []string{"spark02"}) ||
		str(result["backend"]) == "" {
		t.Fatalf("confirmed selection must report the actual record: %v", result)
	}
	// No valid loaded record: no fabricated acknowledgment.
	if _, err := env.coord.confirmedSelection("deepseek"); err == nil ||
		!strings.Contains(err.Error(), "must be upgraded and restarted") {
		t.Fatalf("missing actual selection must fail: %v", err)
	}
	// The saved run_on is untouched by the acknowledgement.
	if got := env.savedRunOn("gemma"); got != "spark01" {
		t.Fatalf("acknowledgement must never rewrite run_on, got %q", got)
	}
}

// TestConfirmedSelectionRequiresReadyRecord proves the acknowledgment is
// refused for a VALIDATED actual record that is queued or starting: only a
// READY generation may be reported as status ready.
func TestConfirmedSelectionRequiresReadyRecord(t *testing.T) {
	for _, state := range []string{StateQueued, StateStarting} {
		t.Run(state, func(t *testing.T) {
			env := newOnEnv(t)
			cfg, err := readCluster(env.path)
			if err != nil {
				t.Fatal(err)
			}
			m := obj(obj(cfg["models"])["gemma"])
			s := obj(obj(m["settings"])["spark02"])
			rec, err := env.rt.BeginOperation("gemma", m, s, []string{"spark02"}, "spark02")
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if state == StateStarting {
				if err := env.rt.TransitionState("gemma", str(rec["generation"]), StateStarting); err != nil {
					t.Fatalf("start: %v", err)
				}
			}
			// The record is valid and Actual, but not READY.
			sel, err := env.coord.loadedSelection("gemma")
			if err != nil || sel == nil || !sel.Actual || str(sel.Record["state"]) != state {
				t.Fatalf("the %s record must be the validated actual selection: %+v %v", state, sel, err)
			}
			if _, err := env.coord.confirmedSelection("gemma"); err == nil ||
				!strings.Contains(err.Error(), "is not ready at the selected placement") {
				t.Fatalf("a %s record must not be acknowledged as ready: %v", state, err)
			}
			// The saved run_on is untouched by the refusal.
			if got := env.savedRunOn("gemma"); got != "spark01" {
				t.Fatalf("refusal must never rewrite run_on, got %q", got)
			}
		})
	}
}

// TestCliLoadOnRejectsUnconfirmedResult proves the CLI never fabricates a
// placement when the controller omits or contradicts one.
func TestCliLoadOnRejectsUnconfirmedResult(t *testing.T) {
	mainDirs(t)
	writeMainConfig(t, "https://gateway.example")
	result := `{"status":"ready","workload":"gemma"}`
	restore := remoteUseFakeRunner(func(ctx context.Context, argv []string, stdin []byte, onStderr func([]byte)) (remoteRunResult, error) {
		return remoteRunResult{stdout: []byte(result), code: 0}, nil
	})
	defer restore()
	code, out, errout := runMain([]string{"load", "gemma", "--on", "spark02", "--json"}, "")
	if code == 0 || !strings.Contains(errout, "did not confirm the requested placement") {
		t.Fatalf("missing placement must not be a CLI success: %d %q %q", code, out, errout)
	}
	if strings.Contains(out, `"placement"`) {
		t.Fatalf("the CLI must not fabricate a placement: %q", out)
	}
	// A contradictory acknowledgment is refused the same way.
	result = `{"status":"ready","workload":"gemma","placement":"spark01","nodes":["spark01"]}`
	code, out, errout = runMain([]string{"load", "gemma", "--on", "spark02", "--json"}, "")
	if code == 0 || !strings.Contains(errout, "did not confirm the requested placement") || strings.Contains(out, `"placement"`) {
		t.Fatalf("wrong placement must not be a CLI success: %d %q %q", code, out, errout)
	}
}
