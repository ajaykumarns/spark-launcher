package sparky

// Wire-contract tests: the device self-update library must never print or
// exit; Main owns the single JSON document on stdout and the exit code.

import (
	"reflect"
	"strings"
	"testing"
)

func TestUpdateDeviceSelfUpdateWireContract(t *testing.T) {
	mainDirs(t)
	if NodeDispatch == nil {
		t.Fatal("NodeDispatch is not bound")
	}
	if reflect.ValueOf(NodeDispatch).Pointer() != reflect.ValueOf(NodeMain).Pointer() {
		t.Fatal("this test must drive the real NodeMain, not a faked dispatcher")
	}

	t.Run("runtime error returns one report on stdout and exits 1", func(t *testing.T) {
		w := newDeviceWorld(t)
		w.installerFail = "spark01" // the worker install landed, the head did not
		w.install()

		code, out, errout := runMain(
			[]string{"node", "self-update", "--root", w.root, "--payload", "-"},
			string(jsonBytes(w.spec)))
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 (stdout %q)", code, out)
		}
		if errout != "" {
			t.Fatalf("the library must never write to stderr: %q", errout)
		}
		body, err := decode([]byte(out))
		if err != nil {
			t.Fatalf("stdout must be exactly one JSON report: %q (%v)", out, err)
		}
		if str(body["status"]) != "error" || str(body["error"]) == "" {
			t.Fatalf("error report = %v", body)
		}
		// The partial report survives: both targets plus the backup that was
		// really taken are still reported so a caller can recover.
		targets := obj(body["targets"])
		if len(targets) != len(updateDeviceNodeNames) {
			t.Fatalf("targets lost from the error report: %v", body)
		}
		if len(obj(body["backups"])) == 0 {
			t.Fatalf("backups lost from the error report: %v", body)
		}
		if !strings.Contains(str(obj(targets["spark01"])["state"]), "fail") {
			t.Fatalf("the failed node is not reported: %v", targets)
		}
		if devInstalledDigest(t, w, "spark02") != devOld() {
			t.Fatalf("the node that had been replaced was not restored: %v", w.installs)
		}
		if out != string(jsonBytes(body)) {
			t.Fatalf("stdout carried more than the report: %q", out)
		}
	})

	t.Run("success keeps the ordinary node contract", func(t *testing.T) {
		w := newDeviceWorld(t)
		w.install()

		code, out, errout := runMain(
			[]string{"node", "self-update", "--root", w.root, "--payload", "-"},
			string(jsonBytes(w.spec)))
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stdout %q stderr %q)", code, out, errout)
		}
		if errout != "" {
			t.Fatalf("success must not write to stderr: %q", errout)
		}
		body, err := decode([]byte(out))
		if err != nil {
			t.Fatalf("stdout must be exactly one JSON report: %q (%v)", out, err)
		}
		if str(body["status"]) != "updated" {
			t.Fatalf("status = %q, report = %v", str(body["status"]), body)
		}
	})

	t.Run("payload grammar is an ordinary error, not a report", func(t *testing.T) {
		w := newDeviceWorld(t)
		w.install()

		bad := Object{}
		for key, value := range w.spec {
			if key != "commit" {
				bad[key] = value
			}
		}
		code, out, errout := runMain(
			[]string{"node", "self-update", "--root", w.root, "--payload", "-"},
			string(jsonBytes(bad)))
		if code == 0 {
			t.Fatalf("an invalid payload must not succeed: %q", out)
		}
		if errout != "" {
			t.Fatalf("a private node failure must not write to stderr: %q", errout)
		}
		body, err := decode([]byte(out))
		if err != nil {
			t.Fatalf("stdout must still be one JSON envelope: %q (%v)", out, err)
		}
		if str(body["error"]) == "" {
			t.Fatalf("grammar failure = %v", body)
		}
		if body["targets"] != nil || body["backups"] != nil {
			t.Fatalf("a grammar failure must not invent a report: %v", body)
		}
	})
}
