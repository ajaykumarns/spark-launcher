package sparky

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func workerRequestEnv(t *testing.T) (*Coordinator, *Runtime, *nativeBackend, *runnerCalls) {
	t.Helper()
	b := newNativeBackend(t, "glm")
	cfg := nativeTestConfig(t.TempDir(), b.server.URL)
	obj(obj(cfg["nodes"])["spark02"])["ssh"] = "spark02"
	m := obj(obj(cfg["models"])["glm"])
	s := obj(obj(m["settings"])["spark01"])
	s["units"] = Object{"spark02": "glm.service"}
	s["concurrency"] = 1
	m["run_on"], m["settings"] = "spark02", Object{"spark02": s}
	path := filepath.Join(str(cfg["root"]), "models.json")
	if err := writeCluster(path, cfg); err != nil {
		t.Fatal(err)
	}
	rt := newRuntime(cfg, path)
	calls := &runnerCalls{}
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		calls.record(args)
		if args[0] == "systemctl" && args[1] == "show" {
			return CommandResult{Out: "active\n"}, nil
		}
		if args[0] == "ssh" {
			return CommandResult{Code: 3}, nil
		}
		return CommandResult{}, fail("unexpected command: %v", args)
	}
	rec, err := rt.BeginOperation("glm", m, s, []string{"spark02"}, "spark02")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{StateStarting, StateReady} {
		if err := rt.TransitionState("glm", str(rec["generation"]), state); err != nil {
			t.Fatal(err)
		}
	}
	c, err := newCoordinator(path, rt)
	if err != nil {
		t.Fatal(err)
	}
	return c, rt, b, calls
}

func TestRequestReadinessNoWorkerSSHRetainsCapacity(t *testing.T) {
	c, _, b, calls := workerRequestEnv(t)
	_, token, err := c.LLMRequest(context.Background(), "glm")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release(token)
	if origin, err := c.InferenceOrigin(token); err != nil || origin != b.server.URL {
		t.Fatalf("admission must bind the model backend: %q %v", origin, err)
	}
	// Capacity is still one: a second request must wait and cancel without
	// acquiring a second lease or releasing the first request's reservation.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, second, err := c.LLMRequest(ctx, "glm")
		if err == nil {
			c.Release(second)
		}
		done <- err
	}()
	deadline := time.After(2 * time.Second)
	for {
		c.mu.Lock()
		queued := len(c.pending) == 1
		c.mu.Unlock()
		if queued {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("second request returned before queuing: %v", err)
		case <-deadline:
			t.Fatal("second request did not wait for capacity")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a second request must not bypass the occupied capacity")
	}
	c.mu.Lock()
	active, waiting := len(c.leases), len(c.pending)
	c.mu.Unlock()
	if active != 1 || waiting != 0 {
		t.Fatalf("cancelled waiter must preserve only the first lease: %d active, %d waiting", active, waiting)
	}
	for _, args := range calls.all() {
		if args[0] == "ssh" {
			t.Fatalf("inference must not open worker SSH sessions: %v", args)
		}
	}
	c.Release(token)
	_, next, err := c.LLMRequest(context.Background(), "glm")
	if err != nil {
		t.Fatalf("released capacity must admit the next request: %v", err)
	}
	c.Release(next)
}

func TestRequestReadinessAfterReservationFailureReleasesCapacity(t *testing.T) {
	c, rt, b, calls := workerRequestEnv(t)
	original := rt.Run
	checks := 0
	rt.Run = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		if args[0] == "systemctl" && args[1] == "show" {
			checks++
			if checks == 2 {
				b.setID("different-model")
			}
		}
		return original(ctx, args, input)
	}
	if _, _, err := c.LLMRequest(context.Background(), "glm"); err == nil {
		t.Fatal("a backend identity change after reservation must reject the request")
	}
	c.mu.Lock()
	active := len(c.leases)
	c.mu.Unlock()
	if active != 0 {
		t.Fatal("failed post-reservation readiness must release capacity")
	}
	for _, args := range calls.all() {
		if args[0] == "ssh" {
			t.Fatalf("post-reservation validation must not use SSH: %v", args)
		}
	}
}

func TestRequestReadinessStillRejectsWrongBackendAndStoppedRecord(t *testing.T) {
	c, rt, b, _ := workerRequestEnv(t)
	b.setID("different-model")
	if _, _, err := c.LLMRequest(context.Background(), "glm"); err == nil {
		t.Fatal("a healthy backend serving the wrong model must not be admitted")
	}
	b.setID("glm")
	rec, err := rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.TransitionState("glm", str(rec["generation"]), StateStopping); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.LLMRequest(context.Background(), "glm"); err == nil {
		t.Fatal("a stopping generation must not be admitted")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.leases) != 0 {
		t.Fatal("failed readiness must not leave a reservation")
	}
}

func TestLifecycleReadinessStillChecksWorkerUnits(t *testing.T) {
	_, rt, _, calls := workerRequestEnv(t)
	sel, err := rt.loadedSelection("glm")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ready(context.Background(), "glm", sel.Setting, sel.Nodes) {
		t.Fatal("lifecycle readiness must reject the inactive worker unit")
	}
	for _, args := range calls.all() {
		if args[0] == "ssh" && strings.Contains(strings.Join(args, " "), "is-active") {
			return
		}
	}
	t.Fatal("lifecycle readiness must still query the worker unit")
}
