package sparky

// Ordering, restart-policy, cancellation and rollback tests for the head-side
// device self-update transaction.

import (
	"context"
	"strings"
	"testing"
)

// devInstalledDigest reports what a node's installed CLI actually contains.
func devInstalledDigest(t *testing.T, w *deviceWorld, node string) string {
	t.Helper()
	file := w.lookup(node, devTargetPath())
	if file == nil {
		return ""
	}
	return sha256Hex(file.data)
}

func TestUpdateDeviceTransactionAllCurrentChangesNothing(t *testing.T) {
	w := newDeviceWorld(t)
	w.bothCurrent()
	w.runningSHA = w.cliSHA // the head already runs the installed bytes
	if _, err := w.run(context.Background()); err != nil {
		t.Fatalf("transaction failed: %v (%v)", err, w.commands())
	}
	devRequireClean(t, w)
	if str(w.report["status"]) != "unchanged" {
		t.Fatalf("status = %q, want unchanged", str(w.report["status"]))
	}
	if len(w.installs) != 0 || w.countCalls("install") != 0 || w.restarts != 0 || len(w.removes) != 0 {
		t.Fatalf("an up-to-date cluster was touched: installs=%v restarts=%d removes=%v", w.installs, w.restarts, w.removes)
	}
	if !w.hasCommand("version spark01 "+devTargetPath()) || !w.hasCommand("version spark02 "+devTargetPath()) {
		t.Fatalf("installed CLI was never verified: %v", w.commands())
	}
	for _, node := range updateDeviceNodeNames {
		row := devTargets(t, w.report)[node]
		if str(row["state"]) != "unchanged" || boolean(row["changed"]) || str(row["backup"]) != "" {
			t.Fatalf("%s target = %v", node, row)
		}
		if devInstalledDigest(t, w, node) != w.cliSHA {
			t.Fatalf("%s bytes changed without an install", node)
		}
	}
	if len(obj(w.report["backups"])) != 0 {
		t.Fatalf("unexpected backups: %v", w.report["backups"])
	}
	controller := obj(w.report["controller"])
	if boolean(controller["restarted"]) || str(controller["restart"]) != "skipped" {
		t.Fatalf("controller = %v", controller)
	}
	// The fence is still owned and handed back, and the cluster identity is
	// still re-proved, even when nothing is installed.
	if len(w.fences) < 2 || !w.fences[0] || w.fences[len(w.fences)-1] {
		t.Fatalf("fence transitions = %v", w.fences)
	}
	for _, key := range []string{"models_unchanged", "supervisors_unchanged", "config_unchanged"} {
		if !boolean(obj(w.report["cluster"])[key]) {
			t.Fatalf("unchanged run did not prove %s: %v", key, w.report["cluster"])
		}
	}
}

func TestUpdateDeviceTransactionRestartsStaleControllerWithoutInstalling(t *testing.T) {
	w := newDeviceWorld(t)
	w.bothCurrent() // installed bytes are already this release on both nodes
	w.runningSHA = sha256Hex([]byte("stale-running-controller"))
	if _, err := w.run(context.Background()); err != nil {
		t.Fatalf("transaction failed: %v (%v)", err, w.commands())
	}
	devRequireClean(t, w)
	if len(w.installs) != 0 {
		t.Fatalf("nothing should be installed: %v", w.installs)
	}
	if w.restarts != 1 {
		t.Fatalf("a controller behind the verified on-disk release must be restarted once: %v", w.commands())
	}
	if !w.hasCommand("restart spark01 " + updateDeviceControllerUnit) {
		t.Fatalf("wrong restart target: %v", w.commands())
	}
	if str(w.report["status"]) != "updated" {
		t.Fatalf("status = %q, want updated", str(w.report["status"]))
	}
	controller := obj(w.report["controller"])
	if !boolean(controller["restarted"]) || str(controller["running_sha256"]) != w.cliSHA {
		t.Fatalf("controller = %v", controller)
	}
	for _, node := range updateDeviceNodeNames {
		if devInstalledDigest(t, w, node) != w.cliSHA {
			t.Fatalf("%s bytes were rewritten", node)
		}
	}
}

func TestUpdateDeviceTransactionNeverRestartsAControllerThatStopped(t *testing.T) {
	w := newDeviceWorld(t)
	w.afterInstall = func(node string) {
		if node == "spark01" {
			w.setUnit(true, true, false) // the controller died after the head install
		}
	}
	_, err := w.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "head controller stopped responding") {
		t.Fatalf("error = %v", err)
	}
	devRequireClean(t, w)
	if w.restarts != 0 || w.hasCommand("restart") {
		t.Fatalf("a stopped controller must never be restarted or started: %v", w.commands())
	}
	if w.hasCommand("systemctl start") || w.hasCommand("systemctl enable") {
		t.Fatalf("transaction started or enabled a unit: %v", w.commands())
	}
	// Both installs are unwound even though the controller is gone.
	for _, node := range updateDeviceInstallOrder {
		if str(devTargets(t, w.report)[node]["state"]) != "rolled_back" {
			t.Fatalf("%s target = %v", node, devTargets(t, w.report)[node])
		}
	}
	if devInstalledDigest(t, w, "spark01") == w.cliSHA || devInstalledDigest(t, w, "spark02") == w.cliSHA {
		t.Fatalf("installs were not rolled back: %v", w.installs)
	}
	if str(w.report["status"]) != "rolled_back" {
		t.Fatalf("status = %q, want rolled_back", str(w.report["status"]))
	}
	if len(w.fences) == 0 || w.fences[len(w.fences)-1] {
		t.Fatalf("fence was not handed back: %v", w.fences)
	}
}

func TestUpdateDeviceTransactionCancellationRollsBackAndRestoresFence(t *testing.T) {
	w := newDeviceWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.afterInstall = func(node string) {
		if node == "spark02" {
			cancel() // the caller gives up while the head install is next
		}
	}
	_, err := w.run(ctx)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("error = %v (%v)", err, w.commands())
	}
	devRequireClean(t, w)
	if len(w.installs) != 1 || !strings.HasPrefix(w.installs[0], "spark02 ") {
		t.Fatalf("only the worker install should have landed: %v", w.installs)
	}
	if w.restarts != 0 || w.hasCommand("restart") {
		t.Fatalf("a cancelled transaction must not restart anything: %v", w.commands())
	}
	// The worker is put back exactly as it was found, on a context the caller
	// already cancelled, while the fence is still held.
	if devInstalledDigest(t, w, "spark02") != sha256Hex([]byte("OLD-INSTALLED-CLI-"+updateTestOld)) {
		t.Fatalf("worker was not restored: %v", w.installs)
	}
	if devInstalledDigest(t, w, "spark01") == w.cliSHA {
		t.Fatalf("head must never have been written")
	}
	if !w.hasCommand("restore spark02 ") {
		t.Fatalf("no restore was attempted: %v", w.commands())
	}
	if str(w.report["status"]) != "rolled_back" {
		t.Fatalf("status = %q, want rolled_back (rollback %v)", str(w.report["status"]), w.report["rollback"])
	}
	if str(obj(obj(w.report["targets"])["spark02"])["state"]) != "rolled_back" {
		t.Fatalf("worker target = %v", obj(obj(w.report["targets"])["spark02"]))
	}
	if len(w.fences) == 0 || w.fences[len(w.fences)-1] {
		t.Fatalf("fence not restored after cancellation: %v", w.fences)
	}
	if len(w.clearCtx) == 0 || w.clearCtx[len(w.clearCtx)-1] {
		t.Fatalf("fence restore used the cancelled context: %v", w.clearCtx)
	}
	if !boolean(obj(w.report["fence"])["restored"]) {
		t.Fatalf("fence report = %v", w.report["fence"])
	}
}
