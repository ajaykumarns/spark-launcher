package sparky

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func profileSnapshot() Object {
	return Object{
		"models": Object{
			"qwen3.8-flash-next": Object{"state": "ready", "kind": "llm", "loaded": true,
				"nodes": []any{"spark01"}},
		},
		"nodes": Object{"spark02": Object{
			"comfyui": Object{"reachable": true, "running": float64(0), "pending": float64(0)},
			"memory_bytes": Object{"MemAvailable": json.Number(
				strconv.FormatInt(112*giB, 10))},
		}},
	}
}

func profileConfig(t *testing.T, root string) Object {
	t.Helper()
	cfg := baseConfig()
	cfg["root"] = root
	cfg["lifecycle"] = "native"
	cfg["controller_port"] = 8020
	cfg["nodes"] = Object{
		"spark01": Object{},
		"spark02": Object{"ssh": "spark02", "comfy_unit": "spark-imagegen.service",
			"comfy_url": "http://worker:8188"},
	}
	cfg["policy"] = Object{"interrupt_active_jobs": false, "comfyui_exclusive_with_llms": false,
		"comfyui_min_available_gib": 21.5, "comfyui_shared_llms": []any{"qwen3.8-flash-next"}}
	cfg["models"] = Object{
		"qwen3.8-flash-next": llmModel("spark01"),
		"comfyui": Object{"kind": "comfyui", "name": "ComfyUI", "run_on": "spark02",
			"settings": Object{"spark02": Object{"parameters": Object{}}}},
	}
	return cfg
}

// profileHarness fakes every effect: the worker runner, the controller HTTP
// surface, and the runtime probe. Unexpected commands and requests fail.
type profileHarness struct {
	mu sync.Mutex

	limits   map[string]string // shared worker cgroup state
	sets     int               // set-property call count
	events   []string
	restarts []string
	requests []string
	commands []string

	fenced        bool   // pre-existing manual fence
	fenceSeen     []bool // nil entries record failed fence writes
	fenceFails    bool   // make fence writes fail
	ramTotalFixed int64  // >= 0 pins the reported ram_total, else dynamic
	failRestore   bool   // fail the rollback set-property
	failFirstSet  bool   // apply the wanted limits, then fail (partial apply)
	invalidMem    bool   // report a malformed MemAvailable reading
	leases        func() Object
	externalEdit  func() // invoked after the first set-property

	rt *fakeProfileRuntime
}

func (h *profileHarness) record(e string) {
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
}

type fakeProfileRuntime struct {
	snap     func(bool) Object
	activity map[string]int
	node     func() Object
	h        *profileHarness
}

func (f *fakeProfileRuntime) Snapshot(_ context.Context, fresh bool) (Object, error) {
	return f.snap(fresh), nil
}
func (f *fakeProfileRuntime) NativeActivity(context.Context) (map[string]int, error) {
	return f.activity, nil
}
func (f *fakeProfileRuntime) ClearComfy(_ context.Context, node string) error {
	f.h.record("clear:" + node)
	return nil
}
func (f *fakeProfileRuntime) InspectNode(_ context.Context, name string) (Object, error) {
	if name != "spark02" {
		return nil, fail("unexpected probe: %s", name)
	}
	return f.node(), nil
}
func (f *fakeProfileRuntime) Unit(_ context.Context, node, action, unit string) error {
	f.h.mu.Lock()
	f.h.restarts = append(f.h.restarts, node+" "+action+" "+unit)
	f.h.mu.Unlock()
	f.h.record("unit:" + action)
	return nil
}

func (h *profileHarness) show() CommandResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	return CommandResult{Code: 0, Out: "MemoryHigh=" + h.limits["MemoryHigh"] +
		"\nMemoryMax=" + h.limits["MemoryMax"] +
		"\nMemorySwapMax=" + h.limits["MemorySwapMax"] + "\n"}
}

func (h *profileHarness) install(t *testing.T) {
	t.Helper()
	prevRun, prevReq, prevRT := imageProfileRun, imageProfileRequest, newImageRuntime
	prevDrain, prevReady := imageDrainAttempts, imageReadinessAttempts
	t.Cleanup(func() {
		imageProfileRun, imageProfileRequest, newImageRuntime = prevRun, prevReq, prevRT
		imageDrainAttempts, imageReadinessAttempts = prevDrain, prevReady
	})
	imageDrainAttempts, imageReadinessAttempts = 3, 3
	imageProfileRun = func(args []string, input []byte, _ time.Duration) (CommandResult, error) {
		h.mu.Lock()
		h.commands = append(h.commands, strings.Join(args, " "))
		h.mu.Unlock()
		if args[0] != "ssh" || len(args) != 5 || args[1] != "-o" || args[2] != "BatchMode=yes" {
			return CommandResult{}, fail("unexpected worker command: %v", args)
		}
		joined := strings.ReplaceAll(args[len(args)-1], "'", "")
		words := strings.Fields(joined)
		if contains(words, "restart") || contains(words, "stop") {
			return CommandResult{}, fail("worker command must never restart or stop: %s", joined)
		}
		if contains(words, "set-property") {
			applied := map[string]string{}
			for _, w := range words {
				if k, v, ok := strings.Cut(w, "="); ok && strings.HasPrefix(k, "Memory") {
					applied[k] = v
				}
			}
			if len(applied) != 3 {
				return CommandResult{}, fail("set-property needs all three properties: %s", joined)
			}
			h.mu.Lock()
			h.sets++
			h.events = append(h.events, "set")
			for k, v := range applied {
				h.limits[k] = v
			}
			edit := h.externalEdit
			failThis := (h.failFirstSet && h.sets == 1) || (h.failRestore && h.sets >= 2)
			h.mu.Unlock()
			if edit != nil {
				edit()
			}
			if failThis {
				return CommandResult{}, fail("injected set-property failure")
			}
			return h.show(), nil
		}
		if contains(words, "show") {
			return h.show(), nil
		}
		return CommandResult{}, fail("unexpected worker words: %s", joined)
	}
	imageProfileRequest = func(_ context.Context, method, url string, payload any, _ time.Duration) (Object, error) {
		h.mu.Lock()
		h.requests = append(h.requests, method+" "+url)
		h.mu.Unlock()
		switch {
		case strings.HasSuffix(url, "/control/leases"):
			// The controller exposes leases as a GET read only.
			if method != "GET" || payload != nil {
				return nil, fail("leases must be a GET read: %s %v", method, payload)
			}
			if h.leases != nil {
				return h.leases(), nil
			}
			// Real Coordinator.admission() shape: no fenced field unless a
			// pre-existing operator fence is simulated.
			leases := Object{"active": []any{}, "waiting": []any{}, "config_transition": false}
			if h.fenced {
				leases["fenced"] = true
			}
			return leases, nil
		case strings.HasSuffix(url, "/control/fence"):
			if method != "POST" {
				return nil, fail("fence must be POST: %s", method)
			}
			enabled := payload.(Object)["enabled"].(bool)
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.fenceFails {
				h.fenceSeen = append(h.fenceSeen, false)
				return nil, fail("fence write failed")
			}
			h.fenceSeen = append(h.fenceSeen, enabled)
			return Object{"fenced": enabled}, nil
		case strings.HasSuffix(url, "/control/config"):
			// /control/config is a GET read in the real server; a POST
			// would always 405 on reload and rollback.
			if method != "GET" || payload != nil {
				return nil, fail("config reload must be a GET read: %s %v", method, payload)
			}
			return Object{}, nil
		case strings.HasSuffix(url, "/system_stats"):
			h.mu.Lock()
			defer h.mu.Unlock()
			total := h.ramTotalFixed
			if total < 0 {
				total = int64(jsonNum(h.limits["MemoryMax"]))
			}
			return Object{"system": Object{"ram_total": json.Number(strconv.FormatInt(total, 10))}}, nil
		case strings.HasSuffix(url, "/sparky/state"):
			return Object{"phase": "idle"}, nil
		}
		return nil, fail("unexpected request: %s %s", method, url)
	}
	newImageRuntime = func(_ Object, _ string) imageRuntime { return h.rt }
}

func jsonNum(s string) float64 {
	n, _ := json.Number(s).Float64()
	return n
}

func newHarness(lowMem int64) *profileHarness {
	h := &profileHarness{
		limits: map[string]string{
			"MemoryHigh":    strconv.FormatInt(14*giB, 10),
			"MemoryMax":     strconv.FormatInt(16*giB, 10),
			"MemorySwapMax": "0",
		},
		ramTotalFixed: -1,
	}
	h.rt = &fakeProfileRuntime{
		snap:     func(bool) Object { return profileSnapshot() },
		activity: map[string]int{},
		node: func() Object {
			node := profileSnapshot()["nodes"].(Object)["spark02"].(Object)
			if h.invalidMem {
				node = copyObject(node)
				node["memory_bytes"] = Object{"MemAvailable": "not-a-number"}
				return node
			}
			if lowMem > 0 && len(h.restarts) == 0 {
				node = copyObject(node)
				node["memory_bytes"] = Object{"MemAvailable": json.Number(strconv.FormatInt(lowMem, 10))}
			}
			return node
		},
		h: h,
	}
	return h
}

// profileTransaction writes a valid central config and wires the fake
// effects. It returns the harness, the root, and the original file bytes.
func profileTransaction(t *testing.T, lowMem int64, configure func(*profileHarness)) (*profileHarness, string, []byte) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config", "models.json")
	cfgJSON, err := json.Marshal(profileConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, cfgJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(lowMem)
	if configure != nil {
		configure(h)
	}
	h.install(t)
	return h, root, cfgJSON
}

func TestEvaluateDedicatedRejectsLanguageModelOnWorker(t *testing.T) {
	s := profileSnapshot()
	obj(s["models"])["qwen3.8-flash-next"].(Object)["nodes"] = []any{"spark01", "spark02"}
	if _, err := EvaluateImageProfile(Object{}, s, "dedicated"); err == nil {
		t.Fatal("dedicated must refuse while an LLM uses Spark02")
	}
}

func TestEvaluateDedicatedRequiresReadyFlashNext(t *testing.T) {
	s := profileSnapshot()
	obj(s["models"])["qwen3.8-flash-next"].(Object)["state"] = "stopped"
	if _, err := EvaluateImageProfile(Object{}, s, "dedicated"); err == nil {
		t.Fatal("dedicated must refuse without ready Flash Next on Spark01")
	}
}

func TestEvaluateRefusesActiveAndUnknownImageWork(t *testing.T) {
	cases := []Object{
		{"reachable": true, "running": float64(1), "pending": float64(0)}, // active
		{"reachable": true, "running": float64(0), "pending": float64(2)}, // queued
		{}, // unknown
		{"running": float64(0), "pending": float64(0)},                     // unreachable
		{"reachable": true, "running": float64(0)},                         // partial
		{"reachable": true, "running": float64(-1), "pending": float64(0)}, // negative
		{"reachable": true, "running": "many", "pending": float64(0)},      // malformed
		{"reachable": true, "running": nil, "pending": float64(0)},         // null
	}
	for i, comfy := range cases {
		s := profileSnapshot()
		obj(s["nodes"])["spark02"].(Object)["comfyui"] = comfy
		if _, err := EvaluateImageProfile(Object{}, s, "shared"); err == nil {
			t.Fatalf("case %d: image work state must be refused, never read as idle", i)
		}
	}
	if _, err := EvaluateImageProfile(Object{}, profileSnapshot(), "turbo"); err == nil {
		t.Fatal("unknown mode must be refused")
	}
	// A valid known-idle Probe shape must pass.
	s := profileSnapshot()
	obj(s["nodes"])["spark02"].(Object)["comfyui"] = Object{
		"reachable": true, "running": float64(0), "pending": float64(0), "guarded": true}
	if _, err := EvaluateImageProfile(Object{}, s, "shared"); err != nil {
		t.Fatalf("known idle state refused: %v", err)
	}
}

func TestProfileDryRunHasNoEffects(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config", "models.json")
	cfgJSON, err := json.Marshal(profileConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, cfgJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(0)
	h.install(t)
	imageProfileRun = func([]string, []byte, time.Duration) (CommandResult, error) {
		return CommandResult{}, fail("dry run must not run commands")
	}
	imageProfileRequest = func(_ context.Context, _, url string, _ any, _ time.Duration) (Object, error) {
		return nil, fail("dry run must not issue requests: %s", url)
	}
	result, err := ActivateImageProfile(root, "shared", true)
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	if result["status"] != "planned" || result["profile"] != "shared" {
		t.Fatalf("unexpected plan: %v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(cfgJSON) {
		t.Fatal("dry run must not touch the central config")
	}
}

func TestProfileShowReadsEngineSystemStatsOnly(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config", "models.json")
	cfgJSON, err := json.Marshal(profileConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, cfgJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(0)
	h.ramTotalFixed = 48 * giB
	h.install(t)
	imageProfileRun = func([]string, []byte, time.Duration) (CommandResult, error) {
		return CommandResult{}, fail("show must not run commands")
	}
	result, err := ActivateImageProfile(root, "show", false)
	if err != nil {
		t.Fatalf("show failed: %v", err)
	}
	if result["status"] != "current" || int64(num(result["service_memory_limit_bytes"])) != 48*giB {
		t.Fatalf("unexpected show result: %v", result)
	}
	if len(h.requests) != 1 || !strings.HasSuffix(h.requests[0], "/system_stats") {
		t.Fatalf("show requests: %v", h.requests)
	}
}

func TestProfileShowRejectsMalformedRamTotal(t *testing.T) {
	for _, bad := range []any{"x", nil, json.Number("-5"), json.Number("1.5")} {
		_, root, _ := profileTransaction(t, 0, nil)
		imageProfileRun = func([]string, []byte, time.Duration) (CommandResult, error) {
			return CommandResult{}, fail("show must not run commands")
		}
		prev := imageProfileRequest
		imageProfileRequest = func(ctx context.Context, method, url string, payload any, timeout time.Duration) (Object, error) {
			if strings.HasSuffix(url, "/system_stats") {
				return Object{"system": Object{"ram_total": bad}}, nil
			}
			return prev(ctx, method, url, payload, timeout)
		}
		if _, err := ActivateImageProfile(root, "show", false); err == nil {
			t.Fatalf("malformed ram_total %v must be rejected", bad)
		}
	}
}

func TestProfileAppliesOnlyLimitsAfterIdleCacheRelease(t *testing.T) {
	h, root, _ := profileTransaction(t, 0, nil)
	result, err := ActivateImageProfile(root, "dedicated", false)
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if result["status"] != "applied" {
		t.Fatalf("unexpected result: %v", result)
	}
	if int64(jsonNum(h.limits["MemoryMax"])) != 48*giB || int64(jsonNum(h.limits["MemoryHigh"])) != 40*giB {
		t.Fatalf("limits not applied: %v", h.limits)
	}
	if boolean(result["image_service_restarted"]) || len(h.restarts) != 0 {
		t.Fatalf("no restart expected: %v", h.restarts)
	}
	for _, cmd := range h.commands {
		if strings.Contains(cmd, " stop ") || strings.Contains(cmd, " restart ") {
			t.Fatalf("service command leaked: %s", cmd)
		}
	}
	doc, err := readCluster(filepath.Join(root, "config", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if num(obj(doc["policy"])["comfyui_min_available_gib"]) != 96 {
		t.Fatalf("policy floor not updated: %v", obj(doc["policy"]))
	}
	if len(h.fenceSeen) != 2 || !h.fenceSeen[0] || h.fenceSeen[1] {
		t.Fatalf("fence sequence: %v", h.fenceSeen)
	}
	clearIdx, setIdx := -1, -1
	for i, e := range h.events {
		if strings.HasPrefix(e, "clear:") && clearIdx == -1 {
			clearIdx = i
		}
		if e == "set" && setIdx == -1 {
			setIdx = i
		}
	}
	if clearIdx == -1 || setIdx == -1 || clearIdx > setIdx {
		t.Fatalf("cache release must precede set-property: %v", h.events)
	}
}

func TestProfileLowSharedRAMRestartsOnlyIdleImageService(t *testing.T) {
	smallMargin := int64(23300000000) // just above the 21.5 GiB floor + 0.5
	for _, low := range []int64{20 * giB, smallMargin} {
		h, root, _ := profileTransaction(t, low, nil)
		result, err := ActivateImageProfile(root, "shared", false)
		if err != nil {
			t.Fatalf("apply failed (low=%d): %v", low, err)
		}
		if !boolean(result["image_service_restarted"]) {
			t.Fatal("idle image service should restart to release context")
		}
		if len(h.restarts) != 1 || h.restarts[0] != "spark02 restart spark-imagegen.service" {
			t.Fatalf("restarts: %v", h.restarts)
		}
		if int64(jsonNum(h.limits["MemoryMax"])) != 16*giB {
			t.Fatalf("limits: %v", h.limits)
		}
		doc, err := readCluster(filepath.Join(root, "config", "models.json"))
		if err != nil {
			t.Fatal(err)
		}
		if num(obj(doc["policy"])["comfyui_min_available_gib"]) != 21.5 {
			t.Fatal("shared floor expected")
		}
	}
}

func TestProfileFailedConfirmationRestoresLimitsConfigAndAdmission(t *testing.T) {
	h, root, original := profileTransaction(t, 0, func(x *profileHarness) { x.ramTotalFixed = 0 })
	if _, err := ActivateImageProfile(root, "dedicated", false); err == nil {
		t.Fatal("failed confirmation must error")
	}
	if int64(jsonNum(h.limits["MemoryMax"])) != 16*giB {
		t.Fatalf("old limits not restored: %v", h.limits)
	}
	got, err := os.ReadFile(filepath.Join(root, "config", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("config not restored: %s", got)
	}
	if len(h.fenceSeen) != 2 || h.fenceSeen[1] != false {
		t.Fatalf("fence not restored: %v", h.fenceSeen)
	}
}

func TestProfileRollbackLimitsFailureStillRestoresConfig(t *testing.T) {
	_, root, original := profileTransaction(t, 0, func(x *profileHarness) {
		x.ramTotalFixed = 0
		x.failRestore = true
	})
	if _, err := ActivateImageProfile(root, "dedicated", false); err == nil {
		t.Fatal("failed confirmation must error")
	}
	got, err := os.ReadFile(filepath.Join(root, "config", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("config restoration must be independent of limits rollback: %s", got)
	}
}

func TestProfileDetectsCentralConfigDrift(t *testing.T) {
	h, root, _ := profileTransaction(t, 0, nil)
	path := filepath.Join(root, "config", "models.json")
	edited := false
	h.mu.Lock()
	h.externalEdit = func() {
		if !edited {
			edited = true
			if err := os.WriteFile(path, []byte(`{"edited":true}`), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	h.mu.Unlock()
	if _, err := ActivateImageProfile(root, "dedicated", false); err == nil {
		t.Fatal("central config drift must be detected")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"edited":true}` {
		t.Fatalf("external edit must not be overwritten: %s", got)
	}
	if int64(jsonNum(h.limits["MemoryMax"])) != 16*giB {
		t.Fatalf("limits not rolled back: %v", h.limits)
	}
}

func TestProfileRefusesExistingManualFenceWithoutWrites(t *testing.T) {
	h, root, _ := profileTransaction(t, 0, func(x *profileHarness) { x.fenced = true })
	if _, err := ActivateImageProfile(root, "dedicated", false); err == nil {
		t.Fatal("a pre-existing manual fence must block the change")
	}
	if len(h.fenceSeen) != 0 || len(h.commands) != 0 || len(h.requests) != 1 {
		t.Fatalf("refused run had effects: fence=%v commands=%v requests=%v",
			h.fenceSeen, h.commands, h.requests)
	}
}

func TestProfileRefusesMalformedLeasesBeforeEffects(t *testing.T) {
	for name, leases := range map[string]Object{
		"no-active":     {"waiting": []any{}, "config_transition": false, "fenced": false},
		"no-waiting":    {"active": []any{}, "config_transition": false, "fenced": false},
		"no-transition": {"active": []any{}, "waiting": []any{}, "fenced": false},
		"transition":    {"active": []any{}, "waiting": []any{}, "config_transition": true, "fenced": false},
	} {
		h, root, _ := profileTransaction(t, 0, func(x *profileHarness) { x.leases = func() Object { return leases } })
		if _, err := ActivateImageProfile(root, "dedicated", false); err == nil {
			t.Fatalf("%s: malformed or transitioning leases must be refused", name)
		}
		if len(h.fenceSeen) != 0 || len(h.commands) != 0 {
			t.Fatalf("%s: refused run had effects", name)
		}
	}
}

func TestProfileInvalidMemoryReadingNeverApplies(t *testing.T) {
	h, root, original := profileTransaction(t, 0, func(x *profileHarness) { x.invalidMem = true })
	if _, err := ActivateImageProfile(root, "dedicated", false); err == nil {
		t.Fatal("an invalid MemAvailable reading must fail the apply")
	}
	if len(h.commands) != 0 {
		t.Fatalf("no worker command may run on an unreadable reading: %v", h.commands)
	}
	got, err := os.ReadFile(filepath.Join(root, "config", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatal("config must be untouched")
	}
}

func TestProfileFenceCleanupFailureSurfaces(t *testing.T) {
	h, root, _ := profileTransaction(t, 0, nil)
	h.fenceFails = true
	if _, err := ActivateImageProfile(root, "dedicated", false); err == nil {
		t.Fatal("fence cleanup failure must surface, never report applied")
	}
}

func TestProfilePreservesOriginalFileMode(t *testing.T) {
	h, root, _ := profileTransaction(t, 0, nil)
	path := filepath.Join(root, "config", "models.json")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateImageProfile(root, "dedicated", false); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("original mode not preserved: %v", info.Mode().Perm())
	}
	_ = h
}

// TestProfileAcceptsRealAdmissionShape is the regression for the live
// Coordinator.admission() payload: exactly active, waiting and
// config_transition, with no fenced field.
func TestProfileAcceptsRealAdmissionShape(t *testing.T) {
	h, root, _ := profileTransaction(t, 0, nil)
	h.leases = func() Object {
		return Object{"active": []any{}, "waiting": []any{}, "config_transition": false}
	}
	result, err := ActivateImageProfile(root, "dedicated", false)
	if err != nil {
		t.Fatalf("the real admission shape must be accepted: %v", err)
	}
	if result["status"] != "applied" {
		t.Fatalf("unexpected result: %v", result)
	}
}

func TestProfileRefusesNonBoolFencedField(t *testing.T) {
	h, root, _ := profileTransaction(t, 0, nil)
	h.leases = func() Object {
		return Object{"active": []any{}, "waiting": []any{},
			"config_transition": false, "fenced": "yes"}
	}
	if _, err := ActivateImageProfile(root, "dedicated", false); err == nil {
		t.Fatal("a non-bool fenced field must be refused, never read as unfenced")
	}
	if len(h.fenceSeen) != 0 || len(h.commands) != 0 {
		t.Fatal("refused run had effects")
	}
}

// TestProfilePartialSetPropertyFailureRestores covers a systemctl
// set-property that applies on the worker and then fails: the original
// cgroup limits must be restored and the error reported.
func TestProfilePartialSetPropertyFailureRestores(t *testing.T) {
	h, root, original := profileTransaction(t, 0, func(x *profileHarness) { x.failFirstSet = true })
	_, err := ActivateImageProfile(root, "dedicated", false)
	if err == nil {
		t.Fatal("a failed set-property must error")
	}
	if strings.Contains(err.Error(), "Restoration needs attention") {
		t.Fatalf("restoration succeeded and must not be reported as needing attention: %v", err)
	}
	if int64(jsonNum(h.limits["MemoryMax"])) != 16*giB {
		t.Fatalf("original limits not restored: %v", h.limits)
	}
	got, err := os.ReadFile(filepath.Join(root, "config", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatal("config must be untouched")
	}
	if len(h.fenceSeen) != 2 || h.fenceSeen[1] {
		t.Fatalf("fence not restored: %v", h.fenceSeen)
	}
}

// TestProfileRollbackFailureReportsAttention covers an independent limits
// rollback failure: the config is still restored, and the failure names what
// needs attention instead of silently claiming success.
func TestProfileRollbackFailureReportsAttention(t *testing.T) {
	_, root, original := profileTransaction(t, 0, func(x *profileHarness) {
		x.ramTotalFixed = 0
		x.failRestore = true
	})
	_, err := ActivateImageProfile(root, "dedicated", false)
	if err == nil {
		t.Fatal("failed confirmation must error")
	}
	if !strings.Contains(err.Error(), "Restoration needs attention") ||
		!strings.Contains(err.Error(), "worker cgroup limits") {
		t.Fatalf("restoration failure must be reported: %v", err)
	}
	got, rerr := os.ReadFile(filepath.Join(root, "config", "models.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(original) {
		t.Fatalf("config restoration must be independent of limits rollback: %s", got)
	}
}

// TestProfileControllerRoutesAreExact asserts the exact method/route wire
// behavior against the real server contract: leases and config are GET
// reads, fence is a POST write.
func TestProfileControllerRoutesAreExact(t *testing.T) {
	h, root, _ := profileTransaction(t, 0, nil)
	if _, err := ActivateImageProfile(root, "dedicated", false); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	leases, config, fences := 0, 0, 0
	for _, r := range h.requests {
		switch {
		case strings.HasSuffix(r, "/control/leases"):
			if !strings.HasPrefix(r, "GET ") {
				t.Fatalf("leases must be GET: %s", r)
			}
			leases++
		case strings.HasSuffix(r, "/control/config"):
			if !strings.HasPrefix(r, "GET ") {
				t.Fatalf("config must be GET (POST would 405): %s", r)
			}
			config++
		case strings.HasSuffix(r, "/control/fence"):
			if !strings.HasPrefix(r, "POST ") {
				t.Fatalf("fence must be POST: %s", r)
			}
			fences++
		}
	}
	if leases < 2 || config < 1 || fences != 2 {
		t.Fatalf("unexpected route counts: leases=%d config=%d fence=%d (%v)",
			leases, config, fences, h.requests)
	}
}

// TestProfileExternalEditBetweenWriteAndFailurePreserved is the regression
// for the expected-bytes rule: the bytes written by writeCluster are the
// rollback candidate. A fresh read after the write must never replace them,
// because a fresh result can be an external edit that rollback would then
// overwrite.
func TestProfileExternalEditBetweenWriteAndFailurePreserved(t *testing.T) {
	h, root, original := profileTransaction(t, 0, func(x *profileHarness) { x.ramTotalFixed = 0 })
	path := filepath.Join(root, "config", "models.json")
	prev := imageProfileRequest
	edited := false
	imageProfileRequest = func(ctx context.Context, method, url string, payload any, timeout time.Duration) (Object, error) {
		// The edit lands after the central config write and before the
		// failing confirmation (the /control/config reload read).
		if strings.HasSuffix(url, "/control/config") && !edited {
			edited = true
			if werr := os.WriteFile(path, []byte("{\"edited\":true}\n"), 0o600); werr != nil {
				t.Error(werr)
			}
		}
		return prev(ctx, method, url, payload, timeout)
	}
	_, err := ActivateImageProfile(root, "dedicated", false)
	if err == nil {
		t.Fatal("a failed confirmation must error")
	}
	if !edited {
		t.Fatal("test setup must have applied the external edit")
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != "{\"edited\":true}\n" {
		t.Fatalf("the external edit must be preserved, never overwritten by rollback: %s", got)
	}
	if int64(jsonNum(h.limits["MemoryMax"])) != 16*giB {
		t.Fatalf("cgroup limits must still be restored: %v", h.limits)
	}
	_ = original
}

// TestProfileBaselineDriftBetweenReadsAbortsBeforeEffects is the regression
// for the single-baseline rule: the initial readCluster config and the later
// original byte read must describe the same content. An edit that arrived
// between the two reads is drift and must abort with zero effects.
func TestProfileBaselineDriftBetweenReadsAbortsBeforeEffects(t *testing.T) {
	h, root, _ := profileTransaction(t, 0, nil)
	path := filepath.Join(root, "config", "models.json")
	prev := h.rt.snap
	edited := false
	h.rt.snap = func(fresh bool) Object {
		if !edited {
			edited = true
			if werr := os.WriteFile(path, []byte("{\"edited\":true}\n"), 0o600); werr != nil {
				t.Error(werr)
			}
		}
		return prev(fresh)
	}
	_, err := ActivateImageProfile(root, "dedicated", false)
	if err == nil {
		t.Fatal("drift between the initial config read and the original byte read must abort")
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != "{\"edited\":true}\n" {
		t.Fatalf("the arriving edit must not be overwritten: %s", got)
	}
	if len(h.fenceSeen) != 0 || len(h.commands) != 0 || len(h.requests) != 0 {
		t.Fatalf("a drift abort must have zero effects: fence=%v commands=%v requests=%v",
			h.fenceSeen, h.commands, h.requests)
	}
}

// TestProfileMaxUint64LimitsRestoredExactly is the regression for old cgroup
// properties beyond the signed 64-bit range: systemd reports its infinity as
// the full unsigned value, so restoration must send the exact original
// strings instead of skipping values that fail ParseInt (which would restore
// the default zero).
func TestProfileMaxUint64LimitsRestoredExactly(t *testing.T) {
	maxUint := "18446744073709551615"
	h, root, original := profileTransaction(t, 0, func(x *profileHarness) {
		x.limits = map[string]string{
			"MemoryHigh": maxUint, "MemoryMax": maxUint, "MemorySwapMax": maxUint}
		x.ramTotalFixed = 0 // fail at the final system_stats confirmation
	})
	_, err := ActivateImageProfile(root, "dedicated", false)
	if err == nil {
		t.Fatal("a failed confirmation must error")
	}
	for _, key := range []string{"MemoryHigh", "MemoryMax", "MemorySwapMax"} {
		if h.limits[key] != maxUint {
			t.Fatalf("the exact original string must be restored for %s: %v", key, h.limits)
		}
	}
	if strings.Contains(err.Error(), "Restoration needs attention") {
		t.Fatalf("restoration succeeded and must not need attention: %v", err)
	}
	got, rerr := os.ReadFile(filepath.Join(root, "config", "models.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(original) {
		t.Fatal("config must be restored to the original baseline")
	}
	if len(h.fenceSeen) != 2 || h.fenceSeen[1] {
		t.Fatalf("fence not restored: %v", h.fenceSeen)
	}
}

// TestProfileMaxUint64RestoreFailureReportsAttention covers a partial
// failure: the exact original strings are attempted but the restore set
// fails, which must be reported rather than silently skipped to defaults.
func TestProfileMaxUint64RestoreFailureReportsAttention(t *testing.T) {
	maxUint := "18446744073709551615"
	h, root, _ := profileTransaction(t, 0, func(x *profileHarness) {
		x.limits = map[string]string{
			"MemoryHigh": maxUint, "MemoryMax": maxUint, "MemorySwapMax": maxUint}
		x.ramTotalFixed = 0
		x.failRestore = true
	})
	_, err := ActivateImageProfile(root, "dedicated", false)
	if err == nil {
		t.Fatal("a failed confirmation must error")
	}
	if !strings.Contains(err.Error(), "Restoration needs attention") ||
		!strings.Contains(err.Error(), "worker cgroup limits") {
		t.Fatalf("the failed restoration must be reported: %v", err)
	}
	if h.limits["MemoryMax"] == "0" {
		t.Fatal("a ParseInt failure must never degrade into the default zero")
	}
	_ = h
}
