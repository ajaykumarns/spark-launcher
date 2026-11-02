package sparky

// Bounded regression tests for the generation-checked IDEMPOTENT teardown
// completion (nativeCompleteStopped): a same-generation record already
// confirmed stopped must complete with nil and no further CAS, a wrong
// generation must be refused, and a REAL concurrent owned stop (CLI stop
// racing the supervisor's own teardown) must never fail with a false
// "Cannot move ... to failed" error.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNativeCompleteStoppedAlreadyStoppedSameGen: a fully confirmed
// same-generation stopped record is completed idempotently with nil and
// zero further effects; the verdict is honestly stopped, never failed.
func TestNativeCompleteStoppedAlreadyStoppedSameGen(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	rt := e.centralRuntime()
	for _, st := range []string{StateStarting, StateReady, StateStopping, StateStopped} {
		if err := rt.TransitionState("glm", e.gen, st); err != nil {
			t.Fatalf("fixture transition to %s: %v", st, err)
		}
	}
	if err := nativeCompleteStopped(rt, "glm", e.gen); err != nil {
		t.Fatalf("an already confirmed stopped same-generation record must complete with nil: %v", err)
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the record must remain stopped, got %s", st)
	}
}

// TestNativeCompleteStoppedFromStoppingSameGen: a stopping record of the
// same generation moves directly to stopped through the real edge.
func TestNativeCompleteStoppedFromStoppingSameGen(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	rt := e.centralRuntime()
	for _, st := range []string{StateStarting, StateReady, StateStopping} {
		if err := rt.TransitionState("glm", e.gen, st); err != nil {
			t.Fatalf("fixture transition to %s: %v", st, err)
		}
	}
	if err := nativeCompleteStopped(rt, "glm", e.gen); err != nil {
		t.Fatalf("a stopping record must complete to stopped: %v", err)
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the record must be stopped, got %s", st)
	}
}

// TestNativeCompleteStoppedWrongGenerationRefused: a newer generation's
// record is never modified — the completion is refused and the state is
// retained, never claimed as a stopped verdict.
func TestNativeCompleteStoppedWrongGenerationRefused(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	rt := e.centralRuntime()
	for _, st := range []string{StateStarting, StateReady, StateStopping} {
		if err := rt.TransitionState("glm", e.gen, st); err != nil {
			t.Fatalf("fixture transition to %s: %v", st, err)
		}
	}
	if err := nativeCompleteStopped(rt, "glm", "bogus-generation"); err == nil {
		t.Fatal("a wrong generation must be refused")
	} else if !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("the refusal must name the supersession: %v", err)
	}
	if st := e.recordState(); st != StateStopping {
		t.Fatalf("a newer generation's record must be untouched, got %s", st)
	}
}

// TestNativeStopConcurrentOwnedTeardownNoFalseFailure: the REAL race — the
// CLI stop (which confirms ranks inactive and the port closed, then CASes
// stopped) runs concurrently with the supervisor's own cancellation-driven
// teardown (which then completes the SAME generation). Neither side may
// fail with "Cannot move ... to failed", and the record must end stopped.
func TestNativeStopConcurrentOwnedTeardownNoFalseFailure(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	e.installFactory()
	restore := slowMonitor(t)
	defer restore()
	cancel, done := e.startRunReady(t)

	stopDone := make(chan error, 1)
	go func() { stopDone <- e.supervise(context.Background(), "stop") }()
	// The supervisor's own context is canceled while the owned stop of the
	// same generation is in flight: the monitor's teardown and the CLI
	// stop both confirm the SAME generation and both must complete.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the supervisor teardown must not fail: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the supervisor teardown must not hang")
	}
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("the owned stop must not fail: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the owned stop must not hang")
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the record must be stopped after the concurrent teardown, got %s", st)
	}
}

// TestNativeStopEntrySeesSameGenStoppedConfirmedNilNoExtraStops: the REAL
// entry race — the supervisor's own teardown completed the SAME generation
// (stopped) BEFORE the explicit stop's entry read. The stop must confirm
// FRESH physical proof and return nil with ZERO extra unit stop commands
// and ZERO state CAS — never "Cannot move glm from stopped to stopping".
func TestNativeStopEntrySeesSameGenStoppedConfirmedNilNoExtraStops(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	e.installFactory()
	cancel, done := e.startRunReady(t)
	if err := joinSupervisor(t, cancel, done); err != nil {
		t.Fatalf("the supervisor lifetime must end cleanly: %v", err)
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("fixture: the generation must be stopped, got %s", st)
	}
	before := e.stoppedList()
	if err := e.supervise(context.Background(), "stop"); err != nil {
		t.Fatalf("a stop whose entry sees the SAME generation already stopped must confirm and return nil: %v", err)
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the record must remain honestly stopped, got %s", st)
	}
	after := e.stoppedList()
	if len(after) != len(before) {
		t.Fatalf("the confirmed-nil entry must issue ZERO extra unit stop commands, before %v after %v", before, after)
	}
}

// TestNativeCompleteStoppedManyConcurrentSameGenAllSucceed: MANY concurrent
// nativeCompleteStopped calls for the SAME generation (the real race shape:
// the CLI stop and the supervisor's own teardown both completing) must ALL
// succeed and the record must end stopped on the SAME generation — no
// helper may fail with "Cannot move ... to stopped/failed" because of a
// lost TOCTOU race between the initial check and the CAS.
func TestNativeCompleteStoppedManyConcurrentSameGenAllSucceed(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	rt := e.centralRuntime()
	for _, st := range []string{StateStarting, StateReady, StateStopping} {
		if err := rt.TransitionState("glm", e.gen, st); err != nil {
			t.Fatalf("fixture transition to %s: %v", st, err)
		}
	}
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = nativeCompleteStopped(rt, "glm", e.gen)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent completion %d must succeed: %v", i, err)
		}
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the record must end stopped, got %s", st)
	}
	rec, err := rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if str(rec["generation"]) != e.gen {
		t.Fatalf("the generation must be unchanged, got %s", str(rec["generation"]))
	}
}

// TestNativeCompleteStoppedWrongGenConcurrentRefusedNoChange: while a
// completion of the current generation is in flight, a helper carrying a
// STALE generation must refuse (superseded) and must not touch the new
// record: the final record is stopped on the NEW generation, never
// rewritten by the stale writer.
func TestNativeCompleteStoppedWrongGenConcurrentRefusedNoChange(t *testing.T) {
	e := newSupEnv(t, "both")
	e.begin()
	rt := e.centralRuntime()
	for _, st := range []string{StateStarting, StateReady, StateStopping} {
		if err := rt.TransitionState("glm", e.gen, st); err != nil {
			t.Fatalf("fixture transition to %s: %v", st, err)
		}
	}
	const n = 8
	var wg sync.WaitGroup
	goodErrs := make([]error, n)
	staleErrs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			goodErrs[i] = nativeCompleteStopped(rt, "glm", e.gen)
		}(i)
		go func(i int) {
			defer wg.Done()
			staleErrs[i] = nativeCompleteStopped(rt, "glm", "stale-generation")
		}(i)
	}
	wg.Wait()
	for i, err := range goodErrs {
		if err != nil {
			t.Fatalf("current-generation completion %d must succeed: %v", i, err)
		}
	}
	for i, err := range staleErrs {
		if err == nil || !strings.Contains(err.Error(), "superseded") {
			t.Fatalf("stale completion %d must be refused with supersession: %v", i, err)
		}
	}
	if st := e.recordState(); st != StateStopped {
		t.Fatalf("the record must end stopped, got %s", st)
	}
	rec, err := rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if str(rec["generation"]) != e.gen {
		t.Fatalf("the current generation must be unchanged, got %s", str(rec["generation"]))
	}
}
