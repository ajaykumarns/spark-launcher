package sparky

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateDeviceDirtyInstallerReceiptsRollback(t *testing.T) {
	for _, kind := range []string{"digest", "path", "format", "json", "lost", "backup-inspect", "target-inspect"} {
		t.Run(kind, func(t *testing.T) {
			w := newDeviceWorld(t)
			w.afterWrite = func(node string, res *CommandResult) {
				if node != "spark01" {
					return
				}
				switch kind {
				case "json":
					res.Out = "bad json"
				case "lost":
					res.Code = 1
					res.Out = ""
				case "backup-inspect", "target-inspect":
					armed := true
					w.inspectFault = func(n, p string) error {
						match := p == devTargetPath()
						if kind == "backup-inspect" {
							match = strings.HasPrefix(p, devTargetPath()+".bak-")
						}
						if armed && n == node && match {
							armed = false
							return fail("inspection transport failed")
						}
						return nil
					}
				default:
					obj, _ := decode([]byte(res.Out))
					if kind == "digest" {
						obj["sha256"] = strings.Repeat("0", 64)
					}
					if kind == "path" {
						obj["installed"] = "/unexpected"
					}
					if kind == "format" {
						obj["format"] = "object"
					}
					res.Out = string(jsonBytes(obj))
				}
			}
			_, err := w.run(context.Background())
			if err == nil {
				t.Fatal("bad receipt succeeded")
			}
			for _, node := range updateDeviceNodeNames {
				if devInstalledDigest(t, w, node) != devOld() {
					t.Fatalf("%s was not restored: %v", node, w.report)
				}
			}
			if !boolean(obj(w.report["rollback"])["complete"]) {
				t.Fatalf("rollback incomplete: %v", w.report)
			}
			devRequireClean(t, w)
		})
	}
}

func TestUpdateDeviceSuccessfulInstallFenceClearFailureIsError(t *testing.T) {
	w := newDeviceWorld(t)
	w.clearFail = true
	w.install()
	code, out, _ := runMain([]string{"node", "self-update", "--root", w.root, "--payload", "-"}, string(jsonBytes(w.spec)))
	body, err := decode([]byte(out))
	if err != nil || code != 1 || str(body["status"]) != "error" {
		t.Fatalf("code=%d output=%s error=%v", code, out, err)
	}
	if boolean(obj(body["fence"])["restored"]) {
		t.Fatal("false fence restoration success")
	}
	for _, node := range updateDeviceNodeNames {
		if devInstalledDigest(t, w, node) != w.cliSHA {
			t.Fatal("verified install rolled back just for fence failure")
		}
	}
}

func TestUpdateDeviceUnknownActivityRefused(t *testing.T) {
	for _, value := range []any{nil, "0", true, -1, 0.5} {
		t.Run(strings.TrimSpace(string(jsonBytes(value))), func(t *testing.T) {
			w := newDeviceWorld(t)
			w.setModel("model", Object{"state": "ready", "active_requests": value})
			_, err := w.run(context.Background())
			if err == nil || len(w.installs) > 0 {
				t.Fatalf("unknown activity accepted: %v", w.report)
			}
		})
	}
}

func TestUpdateDeviceUnreachableComfyRequiresStoppedUnit(t *testing.T) {
	for _, state := range []string{"inactive", "failed", "active", ""} {
		t.Run(state, func(t *testing.T) {
			w := newDeviceWorld(t)
			w.setUnitState("spark01", "sparky-comfyui.service", state)
			_, err := w.run(context.Background())
			allowed := state == "inactive" || state == "failed"
			if (err == nil) != allowed {
				t.Fatalf("state=%s error=%v", state, err)
			}
			if !allowed && len(w.installs) > 0 {
				t.Fatal("unknown engine permitted installation")
			}
		})
	}
}

func TestUpdateDeviceExactVersion(t *testing.T) {
	target := &updateDeviceTarget{version: "0.6.4", commit: devCommit}
	if updateDeviceCheckVersion("sparky 0.6.40 (commit "+devCommit+")", target) == nil {
		t.Fatal("prefix accepted")
	}
}

func TestUpdateDeviceLargeLocalInspectionIsMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sparky")
	raw := make([]byte, 9<<20)
	copy(raw, "large executable fixture")
	if err := os.WriteFile(p, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &updateJournalFiles{}
	snap, err := f.inspect(context.Background(), "spark01", p)
	if err != nil || snap.sha != sha256Hex(raw) || len(snap.data) != 0 {
		t.Fatalf("metadata=%+v error=%v", snap, err)
	}
}

func TestUpdateDeviceLargeWorkerInspectionHasNoBinaryPayload(t *testing.T) {
	old := updateRun
	t.Cleanup(func() { updateRun = old })
	digest := strings.Repeat("a", 64)
	updateRun = func(_ context.Context, args []string, _ []byte) (CommandResult, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "write-file") || !strings.Contains(joined, "sha256sum") {
			t.Fatalf("not metadata-only: %v", args)
		}
		return CommandResult{Out: "755 1000 9437184\n1000\n" + digest + "  /cluster/sparky"}, nil
	}
	f := &updateJournalFiles{j: NewDeploymentJournal(Object{"nodes": Object{"spark02": Object{"ssh": "spark02"}}}, "")}
	snap, err := f.inspect(context.Background(), "spark02", "/cluster/sparky")
	if err != nil || snap.sha != digest || len(snap.data) != 0 {
		t.Fatalf("metadata=%+v error=%v", snap, err)
	}
}

func TestUpdateClientRejectsUnconfirmedRemoteSuccess(t *testing.T) {
	for _, kind := range []string{"target", "digest", "version", "commit", "controller"} {
		t.Run(kind, func(t *testing.T) {
			h := newUpdateHarness(t)
			h.setNodeStale()
			report := updateTestDeviceReport("updated", h.nodeCLISHA)
			switch kind {
			case "target":
				delete(obj(report["targets"]), "spark02")
			case "digest":
				obj(obj(report["targets"])["spark02"])["sha256"] = "wrong"
			case "version":
				obj(obj(report["targets"])["spark02"])["version"] = "0.0.0"
			case "commit":
				obj(report["release"])["commit"] = "wrong"
			case "controller":
				obj(report["controller"])["running_sha256"] = "wrong"
			}
			h.deviceReport = &report
			h.install()
			result := h.run(context.Background(), false, "")
			if boolean(result.report["ok"]) {
				t.Fatal("unconfirmed success accepted")
			}
			if h.indexOf("exec:mac-install") >= 0 {
				t.Fatal("Mac installed after unconfirmed cluster success")
			}
			if len(h.removes) != 0 {
				t.Fatal("recovery helpers removed after unconfirmed success")
			}
		})
	}
}
