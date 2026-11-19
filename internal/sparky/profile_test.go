package sparky

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestProfileApplyNativeLifecycle(t *testing.T) {
	env := newIntegrationEnv(t)
	env.writeCentralPlacements()
	env.installFactory()
	oldInterval := nativeMonitorInterval
	nativeMonitorInterval = 5 * time.Millisecond
	t.Cleanup(func() { nativeMonitorInterval = oldInterval })
	t.Cleanup(env.joinSupervisor)

	path := filepath.Join(filepath.Dir(env.path), "profiles.yaml")
	yaml := "version: 1\nprofiles:\n" +
		"  single:\n    capabilities:\n      chat:\n        - " + integrationModel + ": spark01\n" +
		"  dual:\n    capabilities:\n      chat:\n        - " + integrationModel + ": both\n" +
		"  empty:\n    capabilities: {}\n"
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}

	c, err := newCoordinator(env.path, env.runtime())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := c.Profile("single", false)
	if err != nil || boolean(preview["blocked"]) || len(arr(preview["steps"])) != 1 || env.rankStartsTotal() != 0 {
		t.Fatalf("preview should show one start and change nothing: %v, %v", preview, err)
	}
	if cap := obj(arr(preview["capabilities"])[0]); str(cap["name"]) != "chat" || str(cap["state"]) != "unavailable" {
		t.Fatalf("a planned start must not be advertised as available: %v", cap)
	}
	if _, err := os.Stat(profileStatePath(c.cfg())); !os.IsNotExist(err) {
		t.Fatalf("preview saved a selection: %v", err)
	}
	applied, err := c.Profile("single", true)
	if err != nil || str(applied["status"]) != "applied" || env.rankStartsTotal() != 1 {
		t.Fatalf("apply should load one rank: %v, %v", applied, err)
	}
	if cap := obj(arr(applied["capabilities"])[0]); str(cap["name"]) != "chat" || str(cap["state"]) != "available" {
		t.Fatalf("apply must report chat as available after readiness: %v", cap)
	}
	snapshot, err := c.Snapshot(true)
	if err != nil {
		t.Fatal(err)
	}
	if cap := obj(arr(obj(snapshot["profile"])["capabilities"])[0]); str(cap["state"]) != "available" {
		t.Fatalf("status must report live capability availability: %v", cap)
	}
	before, err := c.runtime.readRecord(integrationModel)
	if err != nil {
		t.Fatal(err)
	}

	applied, err = c.Profile("single", true)
	if err != nil || str(applied["status"]) != "applied" || len(arr(applied["completed"])) != 0 {
		t.Fatalf("second apply should have no steps: %v, %v", applied, err)
	}
	after, err := c.runtime.readRecord(integrationModel)
	if err != nil || before["generation"] != after["generation"] || env.rankStartsTotal() != 1 {
		t.Fatal("a matching profile restarted the model")
	}
	if str(selectedProfile(c.cfg())["name"]) != "single" {
		t.Fatal("successful apply did not save the profile")
	}

	preview, err = c.Profile("dual", false)
	steps := arr(preview["steps"])
	if err != nil || boolean(preview["blocked"]) || len(steps) != 2 ||
		str(obj(steps[0])["action"]) != "stop" || str(obj(steps[1])["action"]) != "start" {
		t.Fatalf("a move must preview stop and start: %v, %v", preview, err)
	}

	applied, err = c.Profile("empty", true)
	if err != nil || str(applied["status"]) != "applied" || len(arr(applied["completed"])) != 1 {
		t.Fatalf("empty profile should unload the managed model: %v, %v", applied, err)
	}
	after, err = c.runtime.readRecord(integrationModel)
	if err != nil || str(after["state"]) != StateStopped || env.dockerOrAudioTouched() {
		t.Fatalf("profile should stop only the native model: %v, %v", after, err)
	}
}

func TestProfilePlanBlocksUnplannedEvictionAndInvalidSharing(t *testing.T) {
	cfg := Object{}
	a := profileModel{name: "base", nodes: []string{"spark01"}, recipe: Object{"shared_device": true}}
	b := profileModel{name: "large", placement: "spark01", nodes: []string{"spark01"}, recipe: Object{
		"shared_device": true, "minimum_available_gib": Object{"spark01": 110},
	}}
	snapshot := Object{
		"models": Object{"base": Object{"kind": "llm", "state": StateReady, "loaded": true, "reserved": true, "nodes": []any{"spark01"}}},
		"nodes":  Object{"spark01": Object{"memory_bytes": Object{"MemAvailable": 30 * gib}}},
	}
	p := clusterProfile{name: "pair", models: []profileModel{a, b}}
	ready := func(m profileModel) bool { return m.name == "base" }
	plan := planProfile(cfg, p, snapshot, ready)
	if !boolean(plan["blocked"]) || len(arr(plan["kept"])) != 1 || len(arr(plan["steps"])) != 1 {
		t.Fatalf("a memory conflict must keep the existing model and block apply: %v", plan)
	}
	b.recipe["shared_device"] = false
	plan = planProfile(cfg, p, snapshot, ready)
	if len(arr(plan["blockers"])) != 2 {
		t.Fatalf("expected memory and sharing conflicts: %v", plan)
	}
}

func TestProfileManagesParakeetWithoutRestartingReadyModel(t *testing.T) {
	env := newImageEnv(t, "native")
	cfg := env.coord.Config()
	obj(cfg["models"])["parakeet"] = Object{
		"kind": "audio", "run_on": "spark02", "keep_loaded": true,
		"settings": Object{"spark02": Object{"containers": []any{"parakeet-engine", "parakeet-gateway"}}},
	}
	obj(cfg["policy"])["protected_models"] = []any{"parakeet"}
	if err := writeCluster(env.path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := env.coord.Refresh(); err != nil {
		t.Fatal(err)
	}
	env.readyRecord()
	before, err := env.rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	content := `version: 1
profiles:
  with-audio:
    capabilities:
      chat:
        - glm: spark01
      images:
        - comfyui: spark02
      transcription:
        - parakeet: spark02
  without-audio:
    capabilities:
      chat:
        - glm: spark01
      images:
        - comfyui: spark02
`
	if err := os.WriteFile(filepath.Join(filepath.Dir(env.path), "profiles.yaml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	// The SSH test double shares the container state between inspection and
	// changes. The native model uses the existing integration test doubles.
	running := map[string]bool{"parakeet-engine": false, "parakeet-gateway": false}
	localProbe := env.rt.ProbeNode
	env.rt.ProbeNode = func(meta Object) (Object, error) {
		state, err := localProbe(meta)
		state["comfyui"] = Object{"reachable": false}
		return state, err
	}
	run := env.rt.Run
	var mutations []string
	env.rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		cmd := args[len(args)-1]
		if args[0] != "ssh" {
			return run(ctx, args, input)
		}
		if strings.Contains(cmd, "node probe") {
			row, err := env.dev.probe(nil)
			containers := Object{}
			for name, active := range running {
				containers[name] = Object{"running": active}
			}
			obj(row["units"])["comfyui.service"] = "active"
			row["containers"] = containers
			return CommandResult{Out: string(jsonBytes(row))}, err
		}
		prefix := shellJoin([]string{"sudo", "-n", "docker"}) + " "
		if !strings.HasPrefix(cmd, prefix) {
			return run(ctx, args, input)
		}
		if args[len(args)-2] != "spark02" {
			t.Fatalf("Parakeet command used the wrong host: %v", args)
		}
		first, _, _ := strings.Cut(strings.TrimPrefix(cmd, prefix), " ")
		verb, name := strings.Trim(first, "'"), ""
		for container := range running {
			if strings.HasSuffix(cmd, " "+shellQuote(container)) {
				name = container
			}
		}
		if name == "" {
			t.Fatalf("unexpected Docker container: %v", args)
		}
		switch verb {
		case "start", "stop":
			mutations = append(mutations, verb+" "+name)
			running[name] = verb == "start"
		case "inspect":
			if !running[name] {
				t.Fatalf("readiness checked before start: %v", args)
			}
			return CommandResult{Out: "healthy\n"}, nil
		default:
			t.Fatalf("unexpected command: %v", args)
		}
		return CommandResult{}, nil
	}

	plan, err := env.coord.Profile("with-audio", false)
	if err != nil || boolean(plan["blocked"]) || len(arr(plan["steps"])) != 1 || len(mutations) != 0 {
		t.Fatalf("preview must show audio start with no effects: %v, %v", plan, err)
	}
	for _, name := range []string{"with-audio", "with-audio", "without-audio", "without-audio"} {
		result, err := env.coord.Profile(name, true)
		if err != nil || str(result["status"]) != "applied" {
			t.Fatalf("apply %s: %v, %v", name, result, err)
		}
	}
	want := []string{"start parakeet-engine", "start parakeet-gateway", "stop parakeet-gateway", "stop parakeet-engine"}
	if !slices.Equal(mutations, want) {
		t.Fatalf("wanted ordered, repeat-safe Docker operations: %v", mutations)
	}
	env.llm.server.Close()
	snapshot, err := env.coord.Snapshot(true)
	if err != nil {
		t.Fatal(err)
	}
	if chat := obj(arr(obj(snapshot["profile"])["capabilities"])[0]); str(chat["state"]) != "unavailable" {
		t.Fatalf("the failed backend must make chat unavailable even with a ready record: %v", chat)
	}
	after, err := env.rt.readRecord("glm")
	if err != nil || before["generation"] != after["generation"] || str(after["state"]) != StateReady {
		t.Fatalf("GLM generation changed: %v, %v", after, err)
	}
	if _, err := env.coord.Unload("parakeet", false); err == nil {
		t.Fatal("profile membership must not remove protection from a direct unload")
	}
}

func TestProfileSwitchesComfyAndOCR(t *testing.T) {
	env := newImageEnv(t, "native")
	ocrInstalled(t, env.root)
	ocrSetHealth(t, Object{"service": "spark-ocr"}, nil)
	var ocrRunning, comfyRunning, imageBusy bool
	var changes []string
	ocrSetStub(t, func(args []string) (CommandResult, error) {
		switch {
		case len(args) > 5 && args[4] == "docker" && args[5] == "compose":
			ocrRunning = true
			changes = append(changes, "start OCR")
		case len(args) > 3 && args[2] == "docker" && args[3] == "stop":
			ocrRunning = false
			changes = append(changes, "stop OCR")
		case len(args) > 3 && args[2] == "docker" && args[3] == "inspect":
			if ocrRunning {
				return CommandResult{Out: ocrInspectOK}, nil
			}
			return CommandResult{Out: `{"status":"exited","running":false}`}, nil
		default:
			t.Fatalf("unexpected OCR command: %v", args)
		}
		return CommandResult{}, nil
	})
	localProbe := env.rt.ProbeNode
	env.rt.ProbeNode = func(meta Object) (Object, error) {
		state, err := localProbe(meta)
		state["comfyui"] = Object{"reachable": false}
		state["containers"] = Object{
			"spark-ocr":      Object{"running": ocrRunning},
			"garuda-gateway": Object{"running": true},
		}
		return state, err
	}
	run := env.rt.Run
	env.rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		cmd := args[len(args)-1]
		if args[0] != "ssh" {
			return run(ctx, args, input)
		}
		switch cmd {
		case shellJoin([]string{"sudo", "-n", "systemctl", "start", "comfyui.service"}):
			comfyRunning = true
			changes = append(changes, "start ComfyUI")
		case shellJoin([]string{"sudo", "-n", "systemctl", "stop", "comfyui.service"}):
			comfyRunning = false
			changes = append(changes, "stop ComfyUI")
		default:
			if !strings.Contains(cmd, "node probe") {
				return run(ctx, args, input)
			}
			state, err := env.dev.probe(nil)
			pending := 0
			if imageBusy {
				pending = 1
			}
			state["comfyui"] = Object{"reachable": comfyRunning, "running": 0, "pending": pending}
			unitState := "inactive"
			if comfyRunning {
				unitState = "active"
			}
			obj(state["units"])["comfyui.service"] = unitState
			return CommandResult{Out: string(jsonBytes(state))}, err
		}
		return CommandResult{}, nil
	}

	profiles := `version: 1
profiles:
  media:
    capabilities:
      images:
        - comfyui: spark02
      ocr:
        - spark-ocr: spark01
  empty:
    capabilities: {}
`
	if err := os.WriteFile(filepath.Join(filepath.Dir(env.path), "profiles.yaml"), []byte(profiles), 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := env.coord.Profile("media", true); err != nil || str(result["status"]) != "applied" {
		t.Fatalf("start services: %v, %v", result, err)
	}
	ocrSetHealth(t, nil, fail("OCR API unavailable"))
	if result, err := env.coord.Profile("media", true); err != nil || !boolean(result["blocked"]) || len(changes) != 2 {
		t.Fatalf("unready OCR must block before changing any service: %v, %v", result, err)
	}
	imageBusy = true
	if result, err := env.coord.Profile("empty", true); err != nil || !boolean(result["blocked"]) || len(changes) != 2 {
		t.Fatalf("busy ComfyUI must block before stopping any service: %v, %v", result, err)
	}
	imageBusy = false
	if result, err := env.coord.Profile("empty", true); err != nil || str(result["status"]) != "applied" {
		t.Fatalf("stop services: %v, %v", result, err)
	}
	if _, err := env.coord.SubmitComfy(Object{"prompt": Object{}}, ""); err == nil || !strings.Contains(err.Error(), "does not include ComfyUI") {
		t.Fatalf("an image request must not start an omitted service: %v", err)
	}
	if want := []string{"start ComfyUI", "start OCR", "stop ComfyUI", "stop OCR"}; !slices.Equal(changes, want) {
		t.Fatalf("unexpected service changes: %v", changes)
	}
}

func TestProfilePlansKokoroLikeOtherAudioServices(t *testing.T) {
	cfg := Object{"models": Object{"kokoro": Object{
		"kind": "audio", "settings": Object{"spark01": Object{"containers": []any{"garuda-kokoro-tts"}}},
	}}}
	state := Object{"running": true}
	snapshot := Object{"nodes": Object{"spark01": Object{"containers": Object{"garuda-kokoro-tts": state}}}}
	groups := []any{Object{"name": "speech", "providers": []any{profileStep("keep", "kokoro", []string{"spark01"})}}}
	if plan := planProfileServices(cfg, groups, snapshot); len(plan.keep) != 1 || len(plan.start)+len(plan.stop) != 0 {
		t.Fatalf("running Kokoro must be kept: %+v", plan)
	}
	if plan := planProfileServices(cfg, nil, snapshot); len(plan.stop) != 1 || len(plan.start) != 0 {
		t.Fatalf("omitted Kokoro must be stopped: %+v", plan)
	}
	state["health"] = "unhealthy"
	if plan := planProfileServices(cfg, groups, snapshot); len(plan.blockers) != 1 || len(plan.keep) != 0 {
		t.Fatalf("unhealthy Kokoro must not be reported ready: %+v", plan)
	}
	state["running"] = false
	if plan := planProfileServices(cfg, groups, snapshot); len(plan.start) != 1 || len(plan.stop) != 0 {
		t.Fatalf("selected stopped Kokoro must be started: %+v", plan)
	}
}

func TestProfileValidatesCapabilitiesAndPreservesModelOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.yaml")
	chat := Object{"kind": "llm", "settings": Object{"both": Object{}}}
	decision := Object{"kind": "llm", "api_kind": "decision", "settings": Object{"spark01": Object{}}}
	cfg := Object{"models": Object{"glm": chat, "knowline": decision}}
	yaml := `version: 1
profiles:
  valid:
    capabilities:
      decisions:
        - knowline: spark01
      chat:
        - glm: both
  wrong:
    capabilities:
      chat:
        - knowline: spark01
`
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := readProfile(path, "valid", cfg)
	if err != nil || len(p.models) != 2 || p.models[0].name != "knowline" || p.models[1].name != "glm" {
		t.Fatalf("expected providers in file order: %v, %v", p.models, err)
	}
	if _, err := readProfile(path, "wrong", cfg); err == nil || !strings.Contains(err.Error(), "does not provide the chat capability") {
		t.Fatalf("wrong capability accepted: %v", err)
	}
}

func TestProfileChecksEveryComfyUnit(t *testing.T) {
	cfg := Object{"nodes": Object{"spark01": Object{"comfy_unit": "comfyui.service"}}}
	units := Object{"comfyui.service": "active"}
	snapshot := Object{
		"nodes":   Object{"spark01": Object{"units": units}},
		"comfyui": Object{"spark01": Object{"reachable": true, "running": 0, "pending": 0}},
	}
	if plan := planProfileServices(cfg, nil, snapshot); len(plan.stop) != 1 || len(plan.blockers) != 0 {
		t.Fatalf("an omitted running unit is managed even without a catalog recipe: %+v", plan)
	}
	units["comfyui.service"] = "inactive"
	if plan := planProfileServices(cfg, nil, snapshot); len(plan.stop) != 0 || len(plan.blockers) != 1 {
		t.Fatalf("an unmanaged engine cannot be stopped through the inactive unit: %+v", plan)
	}
}

func TestProfileCanStopFailedOwnedModel(t *testing.T) {
	row := Object{"kind": "llm", "loaded": true, "state": StateFailed, "reserved": true, "nodes": []any{"spark01"}}
	snapshot := Object{"models": Object{"glm": row}}
	ready := func(profileModel) bool { return false }
	result := planProfile(nil, clusterProfile{}, snapshot, ready)
	steps := arr(result["steps"])
	if boolean(result["blocked"]) || len(steps) != 1 || str(obj(steps[0])["action"]) != "stop" {
		t.Fatalf("a failed model with confirmed ownership can be stopped: %v", result)
	}
	row["reserved"] = false
	result = planProfile(nil, clusterProfile{}, snapshot, ready)
	if !boolean(result["blocked"]) || len(arr(result["steps"])) != 0 {
		t.Fatalf("an unowned model cannot be stopped: %v", result)
	}
}
