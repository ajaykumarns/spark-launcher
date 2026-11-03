package sparky

// Fence-ownership and cluster-identity tests for the head-side device
// self-update transaction.

import (
	"context"
	"strings"
	"testing"
)

func TestUpdateDeviceTransactionFenceFailuresNeverLeakAFence(t *testing.T) {
	tests := []struct {
		name         string
		setup        func(*deviceWorld)
		wantRestored bool
	}{
		{"enable fails", func(w *deviceWorld) { w.fenceFail = true }, true},
		{"enable and clear fail", func(w *deviceWorld) { w.fenceFail = true; w.clearFail = true }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newDeviceWorld(t)
			tc.setup(w)
			_, err := w.run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "fence transport failed") {
				t.Fatalf("error = %v", err)
			}
			devRequireClean(t, w)
			if len(w.installs) != 0 || w.restarts != 0 {
				t.Fatalf("a fence that never took must not install anything: %v", w.commands())
			}
			// The enable may have landed before the transport failed, so the
			// fence is always handed straight back and never left owned.
			if len(w.fences) != 2 || w.fences[0] != true || w.fences[1] != false {
				t.Fatalf("fence transitions = %v", w.fences)
			}
			if len(w.clearCtx) == 0 || w.clearCtx[len(w.clearCtx)-1] {
				t.Fatalf("fence clear must not use a cancellable context: %v", w.clearCtx)
			}
			fence := obj(w.report["fence"])
			if boolean(fence["owned"]) {
				t.Fatalf("fence must never stay owned: %v", fence)
			}
			if boolean(fence["restored"]) != tc.wantRestored {
				t.Fatalf("fence = %v, want restored %v", fence, tc.wantRestored)
			}
			if tc.wantRestored == false && str(fence["error"]) == "" {
				t.Fatalf("an unrestored fence must be reported: %v", fence)
			}
			if str(w.report["status"]) != "failed" {
				t.Fatalf("status = %q", str(w.report["status"]))
			}
		})
	}
}

func TestUpdateDeviceTransactionRefenceFailureRollsBackAndClearsFence(t *testing.T) {
	w := newDeviceWorld(t)
	// The second enable is the re-fence of the just-restarted controller.
	w.refenceFailAt = 2
	_, err := w.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "fence transport failed") {
		t.Fatalf("error = %v", err)
	}
	devRequireRolledBack(t, w)
	if len(w.installs) != 2 {
		t.Fatalf("installs = %v", w.installs)
	}
	// The replacement process is fenced before it can admit anything, and the
	// rollback restart re-fences too.
	if !w.hasCommand("restart spark01 " + updateDeviceControllerUnit) {
		t.Fatalf("restart calls = %v", w.commands())
	}
	enabled := 0
	for _, on := range w.fences {
		if on {
			enabled++
		}
	}
	if enabled < 2 || w.fences[len(w.fences)-1] {
		t.Fatalf("fence sequence = %v", w.fences)
	}
	if str(w.report["status"]) != "rolled_back" {
		t.Fatalf("status = %q", str(w.report["status"]))
	}
}

func TestUpdateDeviceTransactionReportsAControllerItCouldNotFence(t *testing.T) {
	w := newDeviceWorld(t)
	w.refenceFailAll = true // no restart ever comes back fenced
	_, err := w.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not be fully rolled back") {
		t.Fatalf("error = %v", err)
	}
	devRequireClean(t, w)
	if str(w.report["status"]) != "rollback_incomplete" {
		t.Fatalf("status = %q", str(w.report["status"]))
	}
	if boolean(obj(w.report["rollback"])["complete"]) {
		t.Fatalf("an unfenceable controller must not report a complete rollback: %v", w.report["rollback"])
	}
	problems, ok := obj(w.report["rollback"])["problems"].([]any)
	if !ok || len(problems) == 0 || !strings.Contains(str(problems[0]), "controller") {
		t.Fatalf("problems = %v", obj(w.report["rollback"])["problems"])
	}
	// The binaries are still restored and the fence is still handed back.
	for _, node := range updateDeviceNodeNames {
		if devInstalledDigest(t, w, node) != devOld() {
			t.Fatalf("%s was not restored: %v", node, w.installs)
		}
	}
	if w.fences[len(w.fences)-1] {
		t.Fatalf("fence left engaged: %v", w.fences)
	}
}

func TestUpdateDeviceTransactionRollsBackWhenClusterIdentityChanges(t *testing.T) {
	tests := []struct {
		name  string
		want  string
		twist func(*deviceWorld)
	}{
		{"model placement", "model set or placement changed", func(w *deviceWorld) {
			w.setModel("glm", Object{"loaded": true, "state": "running", "nodes": []any{"spark02"},
				"active_requests": float64(0)})
		}},
		{"durable records", "durable model lifecycle records changed", func(w *deviceWorld) {
			w.setRecord("glm", StateReady)
		}},
		{"supervisor units", "supervisor and rank identities changed", func(w *deviceWorld) {
			w.setUnitState("spark02", "sparky-comfyui.service", "failed")
		}},
		{"central config", "central cluster configuration changed", func(w *deviceWorld) {
			w.rewriteConfig(func(c Object) { c["controller_port"] = 8123 })
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newDeviceWorld(t)
			w.afterInstall = func(node string) {
				if node == "spark01" {
					tc.twist(w)
				}
			}
			_, err := w.run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q (commands %v)", err, tc.want, w.commands())
			}
			devRequireRolledBack(t, w)
			// Any restart that happened was the head controller only, and the
			// rollback restart brought the previously installed bytes back.
			if w.restarts > 2 || w.countCalls("restart") > 2 {
				t.Fatalf("too many restarts: %v", w.commands())
			}
			if w.restarts > 0 && str(obj(w.report["controller"])["running_sha256"]) != devOld() {
				t.Fatalf("rollback left the new controller running: %v", w.report["controller"])
			}
			if str(w.report["status"]) != "rolled_back" {
				t.Fatalf("status = %q", str(w.report["status"]))
			}
			for _, node := range updateDeviceNodeNames {
				row := devTargets(t, w.report)[node]
				if str(row["state"]) != "rolled_back" {
					t.Fatalf("%s target = %v", node, row)
				}
			}
			cluster := obj(w.report["cluster"])
			for _, key := range []string{"models_unchanged", "supervisors_unchanged", "config_unchanged"} {
				if boolean(cluster[key]) && strings.Contains(err.Error(), key) {
					t.Fatalf("inconsistent cluster report: %v", cluster)
				}
			}
		})
	}
}
