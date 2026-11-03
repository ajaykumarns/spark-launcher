package sparky

// Rollback and fence-ownership tests for the head-side device self-update
// transaction (second half: install/restart faults and cluster identity).

import (
	"context"
	"strings"
	"testing"
)

// devOld is the digest both nodes' installed CLIs carry before any install.
func devOld() string { return sha256Hex([]byte("OLD-INSTALLED-CLI-" + updateTestOld)) }

// devRequireRolledBack asserts both nodes are back exactly as found, with the
// backups still named in the report and the fence handed back.
func devRequireRolledBack(t *testing.T, w *deviceWorld) {
	t.Helper()
	devRequireClean(t, w)
	for _, node := range updateDeviceNodeNames {
		if devInstalledDigest(t, w, node) != devOld() {
			t.Fatalf("%s still carries the new CLI: %v", node, w.installs)
		}
	}
	if len(w.fences) == 0 || w.fences[len(w.fences)-1] {
		t.Fatalf("fence was not handed back: %v", w.fences)
	}
	if !boolean(obj(w.report["fence"])["restored"]) {
		t.Fatalf("fence report = %v", w.report["fence"])
	}
	if len(obj(w.report["backups"])) == 0 {
		t.Fatalf("rollback must keep naming the backups: %v", w.report["backups"])
	}
}

func TestUpdateDeviceTransactionRollsBackWorkerWhenHeadInstallFails(t *testing.T) {
	w := newDeviceWorld(t)
	w.installerFail = "spark01"
	_, err := w.run(context.Background())
	if err == nil {
		t.Fatalf("expected a failed install, report %v", w.report)
	}
	devRequireRolledBack(t, w)
	if w.restarts != 0 || w.hasCommand("restart") {
		t.Fatalf("a failed install must never restart the controller: %v", w.commands())
	}
	if str(w.report["status"]) != "rolled_back" {
		t.Fatalf("status = %q, want rolled_back", str(w.report["status"]))
	}
	targets := devTargets(t, w.report)
	if str(targets["spark02"]["state"]) != "rolled_back" || !boolean(targets["spark02"]["rolled_back"]) {
		t.Fatalf("worker target = %v", targets["spark02"])
	}
	if str(targets["spark01"]["state"]) != "failed" || boolean(targets["spark01"]["changed"]) {
		t.Fatalf("head target = %v", targets["spark01"])
	}
	// The worker restore went through the same verified staged installer, from
	// the exact backup the installer recorded for it.
	backup := str(targets["spark02"]["backup"])
	if backup == "" || !w.hasCommand("restore spark02 "+backup) {
		t.Fatalf("worker restore did not use the recorded backup: %q calls=%v", backup, w.commands())
	}
	if !boolean(obj(w.report["rollback"])["complete"]) || obj(w.report["rollback"])["attempted"] != true {
		t.Fatalf("rollback = %v", w.report["rollback"])
	}
}

func TestUpdateDeviceTransactionRollsBackBothWhenRestartFails(t *testing.T) {
	w := newDeviceWorld(t)
	w.restartFail = true
	_, err := w.run(context.Background())
	if err == nil {
		t.Fatalf("expected a failed restart, report %v", w.report)
	}
	devRequireRolledBack(t, w)
	if len(w.installs) != 2 {
		t.Fatalf("both installs should have landed first: %v", w.installs)
	}
	if w.restarts != 0 {
		t.Fatalf("a failed restart must not be counted as restarted: %v", w.commands())
	}
	if w.countCalls("restart") != 1 {
		t.Fatalf("the controller must not be restarted repeatedly: %v", w.commands())
	}
	if str(w.report["status"]) != "rolled_back" {
		t.Fatalf("status = %q", str(w.report["status"]))
	}
	for _, node := range updateDeviceNodeNames {
		row := devTargets(t, w.report)[node]
		if str(row["state"]) != "rolled_back" || str(row["version"]) != "" {
			t.Fatalf("%s target after rollback = %v", node, row)
		}
	}
}

func TestUpdateDeviceTransactionRollsBackAfterRestartAndRecoversWithOldBytes(t *testing.T) {
	w := newDeviceWorld(t)
	w.waitFailFor = 1 // the restarted controller never answers the readiness probe
	_, err := w.run(context.Background())
	if err == nil {
		t.Fatalf("expected a readiness failure, report %v", w.report)
	}
	devRequireRolledBack(t, w)
	// Rollback restarts the head only after re-proving the cluster is idle, and
	// the restarted controller must run the restored bytes.
	if w.restarts != 2 {
		t.Fatalf("expected one restart and one rollback restart: %v", w.commands())
	}
	if !boolean(obj(w.report["controller"])["restarted"]) {
		t.Fatalf("controller = %v", w.report["controller"])
	}
	if str(obj(w.report["controller"])["running_sha256"]) != devOld() {
		t.Fatalf("rollback restart did not return the old controller: %v", w.report["controller"])
	}
	if obj(w.report["rollback"])["attempted"] != true || !boolean(obj(w.report["rollback"])["complete"]) {
		t.Fatalf("rollback = %v", w.report["rollback"])
	}
}

func TestUpdateDeviceTransactionNeverRestartsDuringRollbackWhenWorkAppears(t *testing.T) {
	w := newDeviceWorld(t)
	w.waitFailFor = 1
	w.afterRestart = func() { w.admit() } // work is admitted behind the fence
	_, err := w.run(context.Background())
	if err == nil {
		t.Fatalf("expected failure, report %v", w.report)
	}
	devRequireClean(t, w)
	if !strings.Contains(err.Error(), "could not be fully rolled back") {
		t.Fatalf("error = %v", err)
	}
	if str(w.report["status"]) != "rollback_incomplete" {
		t.Fatalf("status = %q, want rollback_incomplete", str(w.report["status"]))
	}
	if w.restarts != 1 {
		t.Fatalf("a busy controller must never be restarted during rollback: %v", w.commands())
	}
	problems := obj(w.report["rollback"])["problems"]
	list, ok := problems.([]any)
	if !ok || len(list) != 1 || !strings.Contains(str(list[0]), "controller") {
		t.Fatalf("rollback problems = %v", problems)
	}
	// The binaries were still restored, and the report still names the backups
	// so the operator can recover by hand.
	for _, node := range updateDeviceNodeNames {
		if devInstalledDigest(t, w, node) != devOld() {
			t.Fatalf("%s was not restored before the refusal: %v", node, w.installs)
		}
	}
	if len(obj(w.report["backups"])) != 2 {
		t.Fatalf("backups must survive an incomplete rollback: %v", w.report["backups"])
	}
	if !boolean(obj(w.report["fence"])["restored"]) {
		t.Fatalf("fence report = %v", w.report["fence"])
	}
}

func TestUpdateDeviceTransactionReportsIncompleteRollbackWhenRestoreFails(t *testing.T) {
	w := newDeviceWorld(t)
	w.installerFail = "spark01"
	w.restoreFail = "spark02"
	_, err := w.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not be fully rolled back") {
		t.Fatalf("error = %v", err)
	}
	devRequireClean(t, w)
	if str(w.report["status"]) != "rollback_incomplete" {
		t.Fatalf("status = %q", str(w.report["status"]))
	}
	targets := devTargets(t, w.report)
	if str(targets["spark02"]["state"]) != "rollback_failed" || !boolean(targets["spark02"]["changed"]) {
		t.Fatalf("worker target = %v", targets["spark02"])
	}
	if str(targets["spark01"]["state"]) != "failed" {
		t.Fatalf("head target = %v", targets["spark01"])
	}
	problems, ok := obj(w.report["rollback"])["problems"].([]any)
	if !ok || len(problems) != 1 || !strings.Contains(str(problems[0]), "spark02") {
		t.Fatalf("problems = %v", obj(w.report["rollback"])["problems"])
	}
	if boolean(obj(w.report["rollback"])["complete"]) {
		t.Fatalf("rollback must not claim completeness: %v", w.report["rollback"])
	}
	if str(targets["spark02"]["backup"]) == "" || len(obj(w.report["backups"])) == 0 {
		t.Fatalf("partial rollback must still report backups: %v", w.report["backups"])
	}
	if !boolean(obj(w.report["fence"])["restored"]) {
		t.Fatalf("the fence is still handed back: %v", w.report["fence"])
	}
}
