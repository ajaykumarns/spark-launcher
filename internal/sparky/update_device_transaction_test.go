package sparky

// Tests for the head-side device self-update transaction. They drive the real
// updateDeviceTransaction through the offline deviceWorld fixture, so every
// guard, ordering and reporting rule is proven against the shipped code paths.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// devTargets digs the per-node report rows out of one report.
func devTargets(t *testing.T, report Object) map[string]Object {
	t.Helper()
	targets := obj(report["targets"])
	if len(targets) != 2 {
		t.Fatalf("report must describe both nodes, got %v", targets)
	}
	out := map[string]Object{}
	for _, node := range updateDeviceNodeNames {
		row, ok := targets[node].(Object)
		if !ok {
			t.Fatalf("report target for %s is missing: %v", node, targets)
		}
		out[node] = row
	}
	return out
}

// devCommandShape reduces one command to its effect shape: every path token
// collapses to its base name so a temporary-directory name (which contains the
// test name) can never satisfy a banned-word match.
func devCommandShape(text string) string {
	fields := strings.Fields(text)
	for i, field := range fields {
		fields[i] = filepath.Base(strings.Trim(field, "'\""))
	}
	return strings.ToLower(strings.Join(fields, " "))
}

// devRequireClean asserts the transaction attempted nothing the fixture does
// not model: no stray command, no unit other than the head controller, no
// model, ComfyUI, worker-controller or Mac-side effect.
func devRequireClean(t *testing.T, w *deviceWorld) {
	t.Helper()
	for _, call := range w.calls {
		if call.kind == "unexpected" {
			t.Fatalf("transaction attempted an unmodelled effect: %s on %s: %s", call.kind, call.node, call.text)
		}
	}
	for _, call := range w.calls {
		shape := call.kind + " " + call.node + " " + devCommandShape(call.text)
		for _, banned := range []string{"launchctl", "brew", "curl", "python", "pip", "docker", "kubectl",
			"systemctl enable", "systemctl disable", "systemctl start", "systemctl stop",
			"systemctl daemon-reload", "systemctl reset-failed", "systemctl kill", "shutdown", "reboot",
			"rm ", "scp ", "rsync ", "model", "comfy", "worker", "install.sh", "sudo -n systemctl stop"} {
			if strings.Contains(shape, banned) {
				t.Fatalf("transaction ran a forbidden effect %q: %s", banned, shape)
			}
		}
		if strings.Contains(shape, "systemctl") && !strings.Contains(shape, "mainpid") &&
			!strings.Contains(shape, "restart "+updateDeviceControllerUnit) {
			t.Fatalf("transaction ran an unexpected systemctl command: %s", shape)
		}
	}
	for _, line := range w.removes {
		if strings.Contains(filepath.Base(line), "models.json") || strings.Contains(line, "config/") {
			t.Fatalf("transaction removed a config file: %s", line)
		}
	}
}

func TestUpdateDeviceTransactionUpdatesBothNodesAndRestartsHeadOnly(t *testing.T) {
	w := newDeviceWorld(t)
	report, err := w.run(context.Background())
	if err != nil {
		t.Fatalf("transaction failed: %v\ncommands: %v\nreport: %v", err, w.commands(), w.report)
	}
	devRequireClean(t, w)
	if str(report["status"]) != "updated" {
		t.Fatalf("status = %q, want updated (report %v)", str(report["status"]), report)
	}
	if str(report["action"]) != "self-update" {
		t.Fatalf("action = %q", str(report["action"]))
	}
	if str(obj(report["release"])["tag"]) != devTag || str(obj(report["release"])["commit"]) != devCommit {
		t.Fatalf("release identity = %v", report["release"])
	}
	if w.lockPath != filepath.Join(w.root, updateDeviceStageBase, updateDeviceLockName) {
		t.Fatalf("update lock path = %q", w.lockPath)
	}

	// The worker binary is swapped before the head so the running head
	// controller keeps supervising, and each node uses its own staged copy.
	if len(w.installs) != 2 {
		t.Fatalf("installs = %v, want exactly two", w.installs)
	}
	if !strings.HasPrefix(w.installs[0], "spark02 "+str(w.spec["worker_stage"])) ||
		!strings.HasPrefix(w.installs[1], "spark01 "+str(w.spec["head_stage"])) {
		t.Fatalf("install order/paths wrong: %v", w.installs)
	}
	if w.countCalls("install") != 2 {
		t.Fatalf("install calls = %v", w.commands())
	}

	// Exactly one restart, of the head controller unit, on the head only.
	if w.restarts != 1 || w.countCalls("restart") != 1 {
		t.Fatalf("restarts = %d, calls = %v", w.restarts, w.commands())
	}
	for _, call := range w.calls {
		if call.kind == "restart" && (call.node != "spark01" || call.text != updateDeviceControllerUnit) {
			t.Fatalf("restarted %s on %s", call.text, call.node)
		}
		if call.kind == "restart" && strings.Contains(call.text, "comfy") {
			t.Fatalf("ComfyUI unit restarted: %s", call.text)
		}
	}

	// Both targets ended verified at the release digest, with prior backups.
	prior := map[string]string{
		"spark01": sha256Hex([]byte("OLD-INSTALLED-CLI-" + updateTestOld)),
		"spark02": sha256Hex([]byte("OLD-INSTALLED-CLI-" + updateTestOld)),
	}
	targets := devTargets(t, w.report)
	for _, node := range updateDeviceNodeNames {
		row := targets[node]
		if str(row["state"]) != "updated" || !boolean(row["changed"]) {
			t.Fatalf("%s target = %v", node, row)
		}
		if str(row["sha256"]) != w.cliSHA || str(row["prior_sha256"]) != prior[node] {
			t.Fatalf("%s digests = %v", node, row)
		}
		if str(row["path"]) != devTargetPath() || str(row["version"]) != devVersion {
			t.Fatalf("%s target identity = %v", node, row)
		}
		if str(row["backup"]) == "" {
			t.Fatalf("%s recorded no backup: %v", node, row)
		}
		if sha256Hex(w.lookup(node, devTargetPath()).data) != w.cliSHA {
			t.Fatalf("%s installed bytes are not the release", node)
		}
	}
	if len(obj(w.report["backups"])) != 2 {
		t.Fatalf("backups = %v", w.report["backups"])
	}

	// The fence was owned by this transaction and handed back, and the last
	// clear ran on a context the caller could not cancel.
	fence := obj(w.report["fence"])
	if !boolean(fence["restored"]) || boolean(fence["prior"]) || boolean(fence["owned"]) {
		t.Fatalf("fence = %v", fence)
	}
	if len(w.fences) < 2 || !w.fences[0] || w.fences[len(w.fences)-1] {
		t.Fatalf("fence transitions = %v", w.fences)
	}
	if len(w.clearCtx) == 0 || w.clearCtx[len(w.clearCtx)-1] {
		t.Fatalf("fence clear used a cancelled context: %v", w.clearCtx)
	}

	// The head controller came back on the installed bytes; the readiness poll
	// and the cluster-identity checks all happened.
	controller := obj(w.report["controller"])
	if !boolean(controller["restarted"]) || str(controller["restart"]) != "restarted" {
		t.Fatalf("controller = %v", controller)
	}
	if str(controller["unit"]) != updateDeviceControllerUnit || num(controller["main_pid"]) != devPID {
		t.Fatalf("controller identity = %v", controller)
	}
	if str(controller["running_sha256"]) != w.cliSHA {
		t.Fatalf("controller running digest = %v", controller)
	}
	if boolean(controller["enabled"]) != true || !boolean(controller["active"]) || !boolean(controller["present"]) {
		t.Fatalf("controller unit state not reported: %v", controller)
	}
	if w.waits == 0 || w.digests == 0 {
		t.Fatalf("readiness/digest checks missing: waits=%d digests=%d", w.waits, w.digests)
	}
	cluster := obj(w.report["cluster"])
	for _, key := range []string{"models_unchanged", "supervisors_unchanged", "config_unchanged"} {
		if !boolean(cluster[key]) {
			t.Fatalf("cluster identity check %s did not pass: %v", key, cluster)
		}
	}
	if obj(w.report["rollback"])["attempted"] != false {
		t.Fatalf("rollback should not be attempted on success: %v", w.report["rollback"])
	}
}

// devRequireUntouched asserts a refusal happened strictly before any effect:
// no install, no restore, no removal, no fence write and no restart.
func devRequireUntouched(t *testing.T, w *deviceWorld) {
	t.Helper()
	devRequireClean(t, w)
	if len(w.installs) != 0 || len(w.removes) != 0 || w.restarts != 0 || len(w.fences) != 0 {
		t.Fatalf("refusal changed state: installs=%v removes=%v restarts=%d fences=%v",
			w.installs, w.removes, w.restarts, w.fences)
	}
	if w.report == nil {
		return
	}
	if str(w.report["status"]) != "failed" {
		t.Fatalf("refusal status = %q, want failed", str(w.report["status"]))
	}
	for _, row := range devTargets(t, w.report) {
		if str(row["state"]) != "pending" || boolean(row["changed"]) {
			t.Fatalf("refusal left a target touched: %v", row)
		}
	}
	if _, ok := w.report["controller"]; ok {
		t.Fatalf("refusal must not reach the restart phase: %v", w.report["controller"])
	}
}

func TestUpdateDeviceTransactionRefusesBeforeAnyEffect(t *testing.T) {
	tests := []struct {
		name  string
		want  string
		setup func(*deviceWorld)
	}{
		{"worker cli digest", "release digest", func(w *deviceWorld) { w.corruptStaged("spark02", "bin/sparky") }},
		{"worker installer digest", "release digest", func(w *deviceWorld) { w.corruptStaged("spark02", "bin/sparky-install") }},
		{"head installer digest", "release digest", func(w *deviceWorld) { w.corruptStaged("spark01", "bin/sparky-install") }},
		{"missing artifact", "incomplete on spark02", func(w *deviceWorld) { w.dropStaged("spark02", "BUILDINFO.json") }},
		{"buildinfo release", "does not name this release", func(w *deviceWorld) {
			w.setBuildinfo([]byte(`{"version":"9.9.9","tag":"v9.9.9","commit":"` + devCommit +
				`","platform":"` + updateDevicePlatform + `","binaries":[{"path":"bin/sparky","sha256":"` + w.cliSHA +
				`"},{"path":"bin/sparky-install","sha256":"` + w.installerSHA + `"}]}`))
		}},
		{"buildinfo platform", "not for this platform", func(w *deviceWorld) {
			w.setBuildinfo([]byte(`{"version":"` + devVersion + `","tag":"` + devTag + `","commit":"` + devCommit +
				`","platform":"linux/amd64","binaries":[{"path":"bin/sparky","sha256":"` + w.cliSHA +
				`"},{"path":"bin/sparky-install","sha256":"` + w.installerSHA + `"}]}`))
		}},
		{"buildinfo digests", "do not match the release payload", func(w *deviceWorld) {
			w.setBuildinfo([]byte(`{"version":"` + devVersion + `","tag":"` + devTag + `","commit":"` + devCommit +
				`","platform":"` + updateDevicePlatform + `","binaries":[{"path":"bin/sparky","sha256":"` +
				sha256Hex([]byte("other")) + `"},{"path":"bin/sparky-install","sha256":"` + w.installerSHA + `"}]}`))
		}},
		{"staged version", "did not report the expected version", func(w *deviceWorld) {
			w.stagedVersion = "sparky 9.9.9 (commit " + devCommit + ")"
		}},
		{"legacy lifecycle", "native model lifecycle", func(w *deviceWorld) {
			w.rewriteConfig(func(c Object) { c["lifecycle"] = "legacy" })
		}},
		{"busy controller", "Work is admitted", (*deviceWorld).admit},
		{"queued work", "Work is queued", (*deviceWorld).queue},
		{"controller transition", "transition is in progress", (*deviceWorld).transition},
		{"leases fenced", "admission fence is active", (*deviceWorld).fenced},
		{"pre-existing fence", "admission fence is active", func(w *deviceWorld) { w.priorFenced = true }},
		{"inactive head controller", "head controller service is not running", func(w *deviceWorld) {
			w.setUnit(true, true, false)
		}},
		{"legacy service busy", "service is busy", func(w *deviceWorld) { w.activity["comfyui"] = 1 }},
		{"durable starting row", "is starting", func(w *deviceWorld) {
			// The manager snapshot claims idle; the durable row says otherwise
			// and the real record is what must stop the update.
			w.setModel("glm", Object{"loaded": false, "state": "stopped", "active_requests": float64(0)})
			w.setRecord("glm", StateStarting)
		}},
		{"durable queued row", "is queued", func(w *deviceWorld) { w.setRecord("glm", StateQueued) }},
		{"durable draining row", "is draining", func(w *deviceWorld) { w.setRecord("glm", "draining") }},
		{"unknown lifecycle state", "cannot be treated as idle", func(w *deviceWorld) { w.setRecord("glm", "wobbling") }},
		{"unit mid transition", "is activating", func(w *deviceWorld) {
			w.setUnitState("spark01", updateDeviceControllerUnit, "activating")
		}},
		{"unit state unknown", "state on spark02 is unknown", func(w *deviceWorld) {
			w.setUnitState("spark02", "sparky-comfyui.service", "")
		}},
		{"comfyui running", "running a job", func(w *deviceWorld) {
			w.setComfy("spark02", Object{"reachable": true, "running": float64(1), "pending": float64(0)})
		}},
		{"comfyui queued", "has a queued job", func(w *deviceWorld) {
			w.setComfy("spark02", Object{"reachable": true, "running": float64(0), "pending": float64(2)})
		}},
		{"comfyui executing", "executing a guarded job", func(w *deviceWorld) {
			w.setComfy("spark02", Object{"reachable": true, "running": float64(0), "pending": float64(0),
				"guarded": true, "executing": true})
		}},
		{"comfyui guarded unknown", "guard state on spark02 is unknown", func(w *deviceWorld) {
			w.setComfy("spark02", Object{"reachable": true, "running": float64(0), "pending": float64(0), "guarded": true})
		}},
		{"comfyui state unknown", "ComfyUI state on spark02 is unknown", func(w *deviceWorld) {
			obj(w.snapshot["comfyui"])["spark02"] = nil
		}},
		{"comfyui queue unreadable", "queue on spark02 is unreadable", func(w *deviceWorld) {
			w.setComfy("spark02", Object{"reachable": true, "running": "busy", "pending": float64(0)})
		}},
		{"active model request", "has an active request", func(w *deviceWorld) {
			w.setModel("glm", Object{"loaded": true, "state": "running", "active_requests": float64(1)})
		}},
		{"update lock busy", "another device update", func(w *deviceWorld) { w.lockBusy = true }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newDeviceWorld(t)
			tc.setup(w)
			_, err := w.run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q (commands %v)", err, tc.want, w.commands())
			}
			devRequireUntouched(t, w)
		})
	}
}
