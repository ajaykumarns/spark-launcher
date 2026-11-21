package sparky

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProfileServiceReservesDeviceBeforeCheckingMemory(t *testing.T) {
	env := newIntegrationEnv(t)
	cfg, err := readCluster(env.path)
	if err != nil {
		t.Fatal(err)
	}
	obj(obj(obj(obj(cfg["models"])["parakeet"])["settings"])["spark02"])["minimum_available_gib"] = Object{"spark02": 30}
	if err := writeCluster(env.path, cfg); err != nil {
		t.Fatal(err)
	}
	rt := env.runtime()
	base := rt.Run
	var free atomic.Int64
	free.Store(100)
	started := make(chan struct{}, 1)
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		if args[0] == "ssh" && strings.Contains(args[len(args)-1], "probe --file") {
			var meta Object
			if err := json.Unmarshal(input, &meta); err != nil {
				return CommandResult{}, err
			}
			state := env.probe(meta)
			state["memory_bytes"] = Object{"MemAvailable": free.Load() * (1 << 30)}
			return CommandResult{Out: string(jsonBytes(state))}, nil
		}
		if args[0] == "ssh" && strings.Contains(args[len(args)-1], "'docker' 'start'") {
			started <- struct{}{}
			return CommandResult{}, errors.New("test startup refused")
		}
		return base(ctx, args, input)
	}
	c, err := newCoordinator(env.path, rt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lease, err := c.acquire(ctx, "comfyui@spark02", []string{"spark02"}, true, 0, "active-image", "", true)
	if err != nil {
		t.Fatal(err)
	}
	step := profileStep("start", "parakeet", []string{"spark02"})
	step["service_kind"], step["containers"] = "audio", []any{"parakeet-asr"}
	done := make(chan error, 1)
	go func() {
		_, _, err := c.applyProfileSteps(ctx, cfg, []any{step}, func() error { return nil })
		done <- err
	}()
	premature := false
	waiting := time.After(time.Second)
wait:
	for len(arr(c.Admission()["waiting"])) == 0 {
		select {
		case <-started:
			premature = true
			break wait
		case <-waiting:
			t.Error("service neither waited for the device nor reported a start")
			break wait
		case <-time.After(time.Millisecond):
		}
	}
	// The first reading would permit startup. Memory changes while the
	// device is busy, so the check must run after the lease becomes free.
	free.Store(5)
	c.release(lease)
	err = <-done
	if premature {
		t.Error("service started while an image job held the device")
	}
	if err == nil || !strings.Contains(err.Error(), "spark02 has 5.0 GiB available") {
		t.Errorf("service did not recheck memory after acquiring the device: %v", err)
	}
	if state := c.Admission(); len(arr(state["active"])) != 0 || len(arr(state["waiting"])) != 0 {
		t.Errorf("service startup left admission state behind: %v", state)
	}
}

func TestProfilePlanChecksServiceMemoryBeforeUnrelatedStops(t *testing.T) {
	cfg := Object{"models": Object{
		"old": Object{"kind": "llm"},
		"audio": Object{"kind": "audio", "settings": Object{"spark02": Object{
			"containers": []any{"engine"}, "minimum_available_gib": Object{"spark02": 30},
		}}},
	}}
	p := clusterProfile{name: "audio", capabilities: []any{Object{"name": "transcription", "providers": []any{profileStep("keep", "audio", []string{"spark02"})}}}}
	snapshot := Object{
		"models": Object{"old": Object{"kind": "llm", "loaded": true, "reserved": true, "nodes": []any{"spark01"}}},
		"nodes": Object{
			"spark01": Object{"memory_bytes": Object{"MemAvailable": 100 * gib}},
			"spark02": Object{"memory_bytes": Object{"MemAvailable": 5 * gib}, "containers": Object{"engine": Object{"running": false}}},
		},
	}
	plan := planProfile(cfg, p, snapshot, func(profileModel) bool { return false })
	if !boolean(plan["blocked"]) {
		t.Errorf("plan would stop a working model before discovering an impossible service start: %v", plan)
	}
	var start Object
	for _, raw := range arr(plan["steps"]) {
		if step := obj(raw); str(step["workload"]) == "audio" {
			start = step
		}
	}
	if num(obj(start["minimum_available_gib"])["spark02"]) != 30 {
		t.Errorf("plan omitted the service startup memory floor: %v", start)
	}
}

func TestProfileSchedulerReportsEveryConcurrentFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var entered atomic.Int32
	barrier := make(chan struct{})
	_, err := scheduleProfileStarts(ctx, []profileStartTask{
		schedulerTask(0, "first", "spark01"), schedulerTask(1, "second", "spark02"),
	}, func(ctx context.Context, task profileStartTask) ([]any, error) {
		if entered.Add(1) == 2 {
			close(barrier)
		}
		select {
		case <-barrier:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return nil, errors.New(str(task.step["workload"]) + " failed")
	})
	if err == nil || !strings.Contains(err.Error(), "first failed") || !strings.Contains(err.Error(), "second failed") {
		t.Fatalf("parallel startup discarded a failure needed for recovery: %v", err)
	}
}

func TestProfileFailureDisplayShowsCompletedEffectsOnly(t *testing.T) {
	result := Object{"profile": "parallel", "status": "failed", "steps": []any{
		profileStep("start", "failed", []string{"spark01"}),
		profileStep("start", "ready", []string{"spark02"}),
		profileStep("start", "not-started", []string{"spark01"}),
	}, "completed": []any{profileStep("start", "ready", []string{"spark02"})}, "error": "failed did not become ready"}
	output := renderProfile(result)
	if !strings.Contains(output, "Completed steps\n  start ready on spark02") || strings.Contains(output, "  start not-started") || strings.Contains(output, "  start failed") {
		t.Fatalf("failure display presents uncompleted steps as effects: %s", output)
	}
}

func TestProfileModelWaitsForDeviceBeforeStartupMemoryCheck(t *testing.T) {
	env := newIntegrationEnv(t)
	env.writeCentralPlacements()
	cfg, err := readCluster(env.path)
	if err != nil {
		t.Fatal(err)
	}
	obj(obj(obj(obj(cfg["models"])["glm"])["settings"])["spark01"])["minimum_available_gib"] = Object{"spark01": 80}
	if err := writeCluster(env.path, cfg); err != nil {
		t.Fatal(err)
	}
	rt := env.runtime()
	var free atomic.Int64
	free.Store(5)
	rt.ProbeNode = func(meta Object) (Object, error) {
		state := env.probe(meta)
		state["memory_bytes"] = Object{"MemAvailable": free.Load() * (1 << 30)}
		return state, nil
	}
	var attempted atomic.Bool
	base := rt.Run
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		if strings.Join(args, " ") == "sudo -n systemctl start --no-block sparky-model@glm.service" {
			attempted.Store(true)
			return CommandResult{}, errors.New("test transport refused startup")
		}
		return base(ctx, args, input)
	}
	c, err := newCoordinator(env.path, rt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lease, err := c.acquire(ctx, "comfyui@spark01", []string{"spark01"}, true, 0, "active-image", "", true)
	if err != nil {
		t.Fatal(err)
	}
	step := profileStep("start", "glm", []string{"spark01"})
	step["on"], step["minimum_available_gib"] = "spark01", Object{"spark01": 80}
	done := make(chan error, 1)
	go func() {
		_, _, err := c.applyProfileSteps(ctx, cfg, []any{step}, func() error { return nil })
		done <- err
	}()
	var early error
	waiting := time.After(time.Second)
wait:
	for len(arr(c.Admission()["waiting"])) == 0 {
		select {
		case early = <-done:
			break wait
		case <-waiting:
			t.Error("model did not wait for its device")
			break wait
		case <-time.After(time.Millisecond):
		}
	}
	free.Store(100)
	c.release(lease)
	if early != nil {
		t.Fatalf("transient image-job memory caused an early model failure: %v", early)
	}
	err = <-done
	if !attempted.Load() || err == nil || strings.Contains(err.Error(), "has 5.0 GiB available") {
		t.Errorf("model did not reach startup after the image job finished: attempted=%v err=%v", attempted.Load(), err)
	}
}

func TestProfileNativeMemoryFloorRemainsEnforced(t *testing.T) {
	env := newSupEnv(t, "spark01")
	cfg, err := readCluster(env.path)
	if err != nil {
		t.Fatal(err)
	}
	obj(obj(obj(obj(cfg["models"])["glm"])["settings"])["spark01"])["minimum_available_gib"] = Object{"spark01": 30}
	if err := writeCluster(env.path, cfg); err != nil {
		t.Fatal(err)
	}
	env.begin()
	env.installFactory()
	nativeRuntimeFactory = func(cfg Object, path string) *Runtime {
		rt := newRuntime(cfg, path)
		rt.Run = env.runner()
		rt.ProbeNode = func(Object) (Object, error) {
			state := env.probe()
			state["memory_bytes"] = Object{"MemAvailable": 5 * gib}
			return state, nil
		}
		return rt
	}
	err = env.supervise(context.Background(), "run")
	if err == nil || !strings.Contains(err.Error(), "spark01 has 5.0 GiB available") || env.recordState() != StateFailed {
		t.Fatalf("native startup accepted insufficient device memory: %v", err)
	}
	if len(env.startedList()) != 0 || len(env.stoppedList()) != 0 {
		t.Fatalf("low-memory preflight changed model units: starts=%v stops=%v", env.startedList(), env.stoppedList())
	}
}
