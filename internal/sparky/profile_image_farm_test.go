package sparky

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTwoDeviceImageProfilePlansAndKeepsBothEngines(t *testing.T) {
	cfg := testConfig(t.TempDir())
	comfy := obj(obj(cfg["models"])["comfyui"])
	settings := obj(comfy["settings"])
	settings["both"] = Object{}
	settings["spark01"] = Object{"backend_url": "http://127.0.0.1:8188"}
	path := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nprofiles:\n  farm:\n    capabilities:\n      images:\n        - comfyui: both\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := readProfile(path, "farm", cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes := profileServiceNodes(p.capabilities, "comfyui")
	if !sameStrings(nodes, []string{"spark01", "spark02"}) {
		t.Fatalf("missing engines: %v", nodes)
	}
	if profileServicePlacement(p.capabilities, "comfyui") != "" {
		t.Fatal("multi-device engine must require an explicit device")
	}
	for _, n := range nodes {
		obj(obj(cfg["nodes"])[n])["comfy_unit"] = "comfyui.service"
	}
	snapshot := Object{"nodes": Object{}, "comfyui": Object{}}
	for _, n := range nodes {
		obj(snapshot["nodes"])[n] = Object{"units": Object{"comfyui.service": "inactive"}}
		obj(snapshot["comfyui"])[n] = Object{"reachable": false}
	}
	plan := planProfileServices(cfg, p.capabilities, snapshot)
	if len(plan.start) != 2 || len(plan.stop) != 0 || len(plan.blockers) != 0 {
		t.Fatalf("bad dual start: %+v", plan)
	}
	for _, n := range nodes {
		obj(obj(obj(snapshot["nodes"])[n])["units"])["comfyui.service"] = "active"
		obj(snapshot["comfyui"])[n] = Object{"reachable": true}
	}
	plan = planProfileServices(cfg, p.capabilities, snapshot)
	if len(plan.keep) != 2 || len(plan.stop) != 0 || len(plan.start) != 0 {
		t.Fatalf("engines must stay running: %+v", plan)
	}
	obj(settings["spark01"])["enabled"] = false
	if _, err := readProfile(path, "farm", cfg); err == nil {
		t.Fatal("disabled engine was accepted")
	}
}

func TestTwoDeviceImageProfileSubmitsOnlyToChosenDevice(t *testing.T) {
	env := comfyEnv(t, imageGraph())
	second := newFakeEngine(t, imageGraph())
	second.promptID = "spark01-job"
	cfg := env.coord.Config()
	obj(obj(cfg["nodes"])["spark01"])["comfy_url"] = second.server.URL
	obj(obj(obj(cfg["models"])["comfyui"])["settings"])["spark01"] = Object{"backend_url": second.server.URL}
	if err := writeCluster(env.coord.configPath(), cfg); err != nil {
		t.Fatal(err)
	}
	groups := []any{Object{"name": "images", "providers": []any{Object{"workload": "comfyui", "nodes": []any{"spark01"}}, Object{"workload": "comfyui", "nodes": []any{"spark02"}}}}}
	if err := writeObject(profileStatePath(cfg), Object{"name": "farm", "capabilities": groups}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.coord.SubmitComfy(imageGraph(), ""); err == nil {
		t.Fatal("device choice must be explicit")
	}
	if _, err := env.coord.SubmitComfy(imageGraph(), "both"); err == nil {
		t.Fatal("one workflow cannot target both devices")
	}
	for node, want := range map[string]string{"spark01": "spark01-job", "spark02": "engine-1"} {
		got, err := env.coord.SubmitComfy(imageGraph(), node)
		if err != nil || str(got["node"]) != node || str(got["prompt_id"]) != want {
			t.Fatalf("wrong backend for %s: %v %v", node, got, err)
		}
	}
}
