package sparky

import (
	"os"
	"path/filepath"
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

	path := filepath.Join(filepath.Dir(env.path), "profiles.json")
	if err := writeObject(path, Object{"version": 1, "profiles": Object{
		"single": Object{"models": []any{Object{"model": integrationModel, "on": "spark01"}}},
		"dual":   Object{"models": []any{Object{"model": integrationModel, "on": "both"}}},
		"empty":  Object{"models": []any{}},
	}}); err != nil {
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
	if _, err := os.Stat(profileStatePath(c.cfg())); !os.IsNotExist(err) {
		t.Fatalf("preview saved a selection: %v", err)
	}
	applied, err := c.Profile("single", true)
	if err != nil || str(applied["status"]) != "applied" || env.rankStartsTotal() != 1 {
		t.Fatalf("apply should load one rank: %v, %v", applied, err)
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
