package sparky

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSharedDeviceNeedsNativeAndMemoryChecks(t *testing.T) {
	c := baseConfig()
	m := llmModel("spark01")
	c["models"] = Object{"glm": m}
	s := obj(obj(m["settings"])["spark01"])
	s["shared_device"] = true
	if validateCluster(c) == nil {
		t.Fatal("legacy sharing must fail")
	}
	c["lifecycle"] = "native"
	if validateCluster(c) == nil {
		t.Fatal("missing memory check must fail")
	}
	s["minimum_available_gib"] = Object{"spark01": 30}
	if err := validateCluster(c); err != nil {
		t.Fatal(err)
	}
}

func TestSharedDeviceLocksAllowPeersButExcludeExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.lock")
	a, err := nativeLockPathsMode(context.Background(), []string{path}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer nativeCloseLocks(a)
	b, err := nativeLockPathsMode(context.Background(), []string{path}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer nativeCloseLocks(b)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	x, err := nativeLockPaths(ctx, []string{path})
	if err == nil {
		nativeCloseLocks(x)
		t.Fatal("exclusive lock admitted alongside shared owners")
	}
}

func TestSharedDeviceUsesFrozenOwnershipAndReadyState(t *testing.T) {
	rt, _ := inventoryEnv(t, false)
	cfg := rt.config()
	s := obj(obj(obj(obj(cfg["models"])["glm"])["settings"])["both"])
	s["shared_device"] = true
	rt.configure(cfg)
	rec := inventoryBegin(t, rt)
	inventoryTo(t, rt, rec, StateStarting)
	if nativeNoOwnedOverlap(context.Background(), rt, "other", []string{"spark01"}, true) == nil {
		t.Fatal("starting owner must block")
	}
	inventoryTo(t, rt, rec, StateReady)
	if err := nativeNoOwnedOverlap(context.Background(), rt, "other", []string{"spark01"}, true); err != nil {
		t.Fatal(err)
	}
	if nativeNoOwnedOverlap(context.Background(), rt, "other", []string{"spark01"}, false) == nil {
		t.Fatal("exclusive target must block")
	}
	// Changing live config must not change a running owner's frozen policy.
	delete(s, "shared_device")
	rt.configure(cfg)
	if err := nativeNoOwnedOverlap(context.Background(), rt, "other", []string{"spark01"}, true); err != nil {
		t.Fatal(err)
	}
	inventoryTo(t, rt, rec, StateStopping)
	if nativeNoOwnedOverlap(context.Background(), rt, "other", []string{"spark01"}, true) == nil {
		t.Fatal("stopping owner must block")
	}
}

func TestSharedDevicePlanPreservesEveryReadyPeer(t *testing.T) {
	c := baseConfig()
	target := llmModel("spark01")
	obj(obj(target["settings"])["spark01"])["shared_device"] = true
	c["models"] = Object{"target": target, "one": llmModel("spark01"), "two": llmModel("spark01"), "three": llmModel("spark01")}
	rows := Object{}
	for _, name := range []string{"one", "two", "three"} {
		rows[name] = Object{"kind": "llm", "loaded": true, "nodes": []any{"spark01"}, "shared_device": true, "state": StateReady}
	}
	snap := Object{"models": rows, "comfyui": Object{"spark01": Object{}}}
	plan, err := makePlan(c, "target", snap, "load", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range arr(plan["steps"]) {
		if str(obj(step)["action"]) == "unload" {
			t.Fatalf("shared peer was evicted: %v", step)
		}
	}
	obj(rows["two"])["shared_device"] = false
	plan, err = makePlan(c, "target", snap, "load", "")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, step := range arr(plan["steps"]) {
		if str(obj(step)["action"]) == "unload" {
			count++
			if str(obj(step)["workload"]) != "two" {
				t.Fatal(step)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected one exclusive peer, got %d", count)
	}
}

func TestSharedDeviceAdmissionRequiresBothActualRecipes(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	models := obj(cfg["models"])
	for _, name := range []string{"qwen", "nemotron"} {
		m := obj(models[name])
		m["run_on"] = "spark02"
		obj(obj(m["settings"])["spark02"])["shared_device"] = true
	}
	rt := newRuntime(cfg, filepath.Join(root, "models.json"))
	co := &Coordinator{config: cfg, runtime: rt}
	for _, name := range []string{"qwen", "nemotron"} {
		m := obj(models[name])
		setting := obj(obj(m["settings"])["spark02"])
		rec, err := rt.BeginOperation(name, m, setting, []string{"spark02"}, "spark02")
		if err != nil {
			t.Fatal(err)
		}
		if err := rt.TransitionState(name, str(rec["generation"]), StateStarting); err != nil {
			t.Fatal(err)
		}
		if err := rt.TransitionState(name, str(rec["generation"]), StateReady); err != nil {
			t.Fatal(err)
		}
	}
	nodes := []string{"spark02"}
	if co.conflicts("qwen", nodes, false, "nemotron", nodes, false) {
		t.Fatal("ready sharing models must be admitted together")
	}
	if !co.conflicts("qwen", nodes, true, "nemotron", nodes, false) {
		t.Fatal("exclusive operation must block")
	}
	changed := copyObject(cfg)
	delete(obj(obj(obj(obj(changed["models"])["qwen"])["settings"])["spark02"]), "shared_device")
	rt.configure(changed)
	co.config = changed
	if !co.conflicts("qwen", nodes, false, "nemotron", nodes, false) {
		t.Fatal("changed recipe must fail closed")
	}
}
