package sparky

// Offline fixture for the head-side device self-update transaction. Every
// effect seam of update_device.go is substituted: no SSH, no systemctl, no
// installer process, no controller HTTP, no model/ComfyUI/lifecycle call and
// no filesystem effect outside the per-test temporary root. Anything the
// transaction attempts that the fixture does not model is recorded as an
// UNEXPECTED command and fails closed with a non-zero result, so a test that
// quietly grows a new effect fails instead of passing.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	devVersion = updateTestVersion
	devTag     = updateTestTag
	devCommit  = updateTestCommit
	devPID     = 4242
)

// devFile is one node file as the strict file protocol reports it.
type devFile struct {
	data []byte
	mode os.FileMode
}

type devCall struct {
	kind string // version | install | restore | pid | restart | unexpected
	node string
	text string
}

// deviceWorld is the whole fake world for one device transaction.
type deviceWorld struct {
	t    *testing.T
	root string
	spec Object

	cliBytes, installerBytes, buildinfo []byte
	cliSHA, installerSHA                string

	files map[string]*devFile // node + "\x00" + path
	calls []devCall

	installs                       []string // "<node> <source> -> <target>"
	removes                        []string // "<node> <path>@<digest>"
	fences                         []bool   // updateFence enable values, in order
	clearCtx                       []bool   // ctx.Err() seen at each fence clear
	fenceReads, controls, restarts int
	waits, digests                 int
	lockPath                       string
	priorFenced                    bool

	unitPresent, unitEnabled, unitActive bool
	runningSHA                           string
	stagedVersion                        string
	installedOld                         string
	leases                               Object
	activity                             map[string]int
	records                              Object
	snapshot                             Object

	// installerFail / restoreFail name the node whose installer or backup
	// restore is made to fail; the rest are one-shot fault injections.
	installerFail, restoreFail                  string
	restartFail, fenceFail, clearFail, lockBusy bool
	// refenceFailAt fails the Nth fence enable (1 is the transaction's own);
	// refenceFailAll fails every enable after the first.
	refenceFailAt  int
	refenceFailAll bool
	// waitFailFor makes the first N controller readiness waits fail.
	waitFailFor int

	seamed bool
	report Object
	err    error

	// afterInstall runs right after one node's real install succeeds, so a
	// test can mutate the world exactly when the transaction has committed.
	afterInstall func(node string)
	// afterRestart runs right after the one allowed controller restart.
	afterWrite   func(node string, result *CommandResult)
	inspectFault func(node, path string) error
	afterRestart func()
	enables      int
}

func devKey(node, path string) string { return node + "\x00" + path }

func newDeviceWorld(t *testing.T) *deviceWorld {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	w := &deviceWorld{
		t: t, root: root,
		unitPresent: true, unitEnabled: true, unitActive: true,
		stagedVersion: "sparky " + devVersion + " (commit " + devCommit + ")",
		installedOld:  "sparky " + updateTestOld + " (commit " + devCommit + ")",
		activity:      map[string]int{},
		records:       Object{},
	}
	w.cliBytes = []byte("STAGED-LINUX-CLI-BYTES")
	w.installerBytes = []byte("STAGED-LINUX-INSTALLER-BYTES")
	w.cliSHA = sha256Hex(w.cliBytes)
	w.installerSHA = sha256Hex(w.installerBytes)
	w.buildinfo = []byte(`{"name":"sparky","version":"` + devVersion + `","tag":"` + devTag +
		`","commit":"` + devCommit + `","platform":"` + updateDevicePlatform +
		`","binaries":[{"path":"bin/sparky","sha256":"` + w.cliSHA +
		`"},{"path":"bin/sparky-install","sha256":"` + w.installerSHA + `"}]}`)
	w.runningSHA = sha256Hex([]byte("running-controller-0.6.4"))
	w.leases = Object{"active": []any{}, "waiting": []any{}, "config_transition": false, "fenced": false}
	w.snapshot = Object{
		"nodes": Object{
			"spark01": Object{"units": Object{"sparky-controller.service": "active"}},
			"spark02": Object{"units": Object{"sparky-comfyui.service": "inactive"}},
		},
		"models":  Object{},
		"comfyui": Object{"spark01": devComfyRow(), "spark02": devComfyRow()},
	}
	headStage := filepath.Join(root, updateDeviceStageBase, "task-head")
	workerStage := filepath.Join(root, updateDeviceStageBase, "task-worker")
	w.spec = Object{"tag": devTag, "version": devVersion, "commit": devCommit,
		"head_stage": headStage, "worker_stage": workerStage,
		"cli_sha256": w.cliSHA, "installer_sha256": w.installerSHA}
	w.stage("spark01", headStage)
	w.stage("spark02", workerStage)
	// updateDeviceOwned runs against the real filesystem on the head only.
	for _, path := range []string{"bin/sparky", "bin/sparky-install", "BUILDINFO.json"} {
		writeOut := filepath.Join(headStage, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(writeOut), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(writeOut, w.stagedBytes(filepath.Base(path)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Both nodes start on the OLD installed CLI, so installs are exercised.
	w.setInstalled("spark01", []byte("OLD-INSTALLED-CLI-"+updateTestOld))
	w.setInstalled("spark02", []byte("OLD-INSTALLED-CLI-"+updateTestOld))
	// The central config is a real file (readCluster) AND a node-protocol file
	// (the identity digest captured before and after the transaction).
	cfg := nativeTestConfig(root, "http://127.0.0.1:8080")
	obj(obj(cfg["nodes"])["spark02"])["ssh"] = "spark02"
	for _, node := range updateDeviceNodeNames {
		obj(obj(cfg["nodes"])[node])["comfy_unit"] = "sparky-comfyui.service"
		obj(obj(obj(w.snapshot["nodes"])[node])["units"])["sparky-comfyui.service"] = "inactive"
	}

	cfgPath := filepath.Join(root, filepath.FromSlash(updateDeviceConfigPath))
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCluster(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	w.files2("spark01", cfgPath, raw, 0o600)
	return w
}

func devComfyRow() Object {
	return Object{"reachable": false, "running": float64(0), "pending": float64(0), "guarded": false}
}

func (w *deviceWorld) stagedBytes(base string) []byte {
	switch base {
	case "sparky":
		return w.cliBytes
	case "sparky-install":
		return w.installerBytes
	}
	return w.buildinfo
}

func (w *deviceWorld) stage(node, dir string) {
	for _, item := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{"bin/sparky", w.cliBytes, 0o755},
		{"bin/sparky-install", w.installerBytes, 0o755},
		{"BUILDINFO.json", w.buildinfo, 0o600},
	} {
		w.files2(node, filepath.Join(dir, filepath.FromSlash(item.path)), item.data, item.mode)
	}
}

// files2 seeds one node file (the fake protocol reads this map).
func (w *deviceWorld) files2(node, path string, data []byte, mode os.FileMode) {
	if w.files == nil {
		w.files = map[string]*devFile{}
	}
	w.files[devKey(node, path)] = &devFile{data: append([]byte(nil), data...), mode: mode}
}

func (w *deviceWorld) lookup(node, path string) *devFile {
	return w.files[devKey(node, path)]
}

func (w *deviceWorld) setInstalled(node string, data []byte) {
	w.files2(node, devTargetPath(), data, 0o755)
}

func devTargetPath() string {
	return filepath.Join(devHome(), filepath.FromSlash(updateDeviceHelperRelative))
}

func devHome() string { return home() }

// run executes one transaction against this world.
func (w *deviceWorld) run(ctx context.Context) (Object, error) {
	w.install()
	report, err := updateDeviceTransaction(ctx, w.root, w.spec)
	w.report, w.err = report, err
	return report, err
}

// record appends one modelled call.
func (w *deviceWorld) record(kind, node, text string) {
	w.calls = append(w.calls, devCall{kind: kind, node: node, text: text})
}

// --- scenario knobs -------------------------------------------------------

func (w *deviceWorld) stagePath(node, rel string) string {
	return filepath.Join(w.stageOf(node), filepath.FromSlash(rel))
}

// corruptStaged changes one staged artifact's bytes so its digest stops
// matching the release without touching the real head-side file.
func (w *deviceWorld) corruptStaged(node, rel string) {
	w.files2(node, w.stagePath(node, rel), []byte("CORRUPTED-PAYLOAD"), 0o755)
}

func (w *deviceWorld) dropStaged(node, rel string) {
	delete(w.files, devKey(node, w.stagePath(node, rel)))
}

func (w *deviceWorld) setBuildinfo(raw []byte) {
	for _, node := range updateDeviceNodeNames {
		w.files2(node, w.stagePath(node, "BUILDINFO.json"), raw, 0o600)
	}
}

// bothCurrent puts the release bytes on both installed targets.
func (w *deviceWorld) bothCurrent() {
	w.setInstalled("spark01", w.cliBytes)
	w.setInstalled("spark02", w.cliBytes)
}

func (w *deviceWorld) setUnit(present, enabled, active bool) {
	w.unitPresent, w.unitEnabled, w.unitActive = present, enabled, active
}

func (w *deviceWorld) setUnitState(node, unit, state string) {
	units := obj(obj(w.snapshot["nodes"])[node])
	obj(units["units"])[unit] = state
}

func (w *deviceWorld) setComfy(node string, row Object) {
	obj(w.snapshot["comfyui"])[node] = row
}

func (w *deviceWorld) setModel(name string, row Object) {
	if _, ok := row["active_requests"]; !ok {
		row["active_requests"] = 0
	}
	obj(w.snapshot["models"])[name] = row
}

func (w *deviceWorld) setRecord(name, state string) {
	w.records[name] = Object{"state": state, "nodes": []any{"spark01"}, "reserved": false}
}

func (w *deviceWorld) admit() {
	w.leases["active"] = []any{Object{"model": "glm"}}
}

func (w *deviceWorld) queue() {
	w.leases["waiting"] = []any{Object{"model": "glm"}}
}

func (w *deviceWorld) transition() { w.leases["config_transition"] = true }
func (w *deviceWorld) fenced()     { w.leases["fenced"] = true }

// rewriteConfig mutates and rewrites the real central config plus its
// node-protocol copy, so the captured identity digest follows the file.
func (w *deviceWorld) rewriteConfig(mutate func(Object)) {
	cfg, err := readCluster(filepath.Join(w.root, filepath.FromSlash(updateDeviceConfigPath)))
	if err != nil {
		w.t.Fatal(err)
	}
	mutate(cfg)
	path := filepath.Join(w.root, filepath.FromSlash(updateDeviceConfigPath))
	if err := writeCluster(path, cfg); err != nil {
		w.t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		w.t.Fatal(err)
	}
	w.files2("spark01", path, raw, 0o600)
}

// commands returns every pinned command the transaction dispatched.
func (w *deviceWorld) commands() []string {
	out := []string{}
	for _, c := range w.calls {
		out = append(out, c.kind+" "+c.node+" "+c.text)
	}
	return out
}

func (w *deviceWorld) hasCommand(needle string) bool {
	for _, line := range w.commands() {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func (w *deviceWorld) countCalls(kind string) int {
	n := 0
	for _, c := range w.calls {
		if c.kind == kind {
			n++
		}
	}
	return n
}

// install swaps every seam for the duration of the calling test.
func (w *deviceWorld) install() {
	if w.seamed {
		return
	}
	w.seamed = true
	orig := struct {
		run     func(context.Context, []string, []byte) (CommandResult, error)
		files   func(Object, string) updateFileAccess
		rt      func(Object, string) updateRuntimeSurface
		control func(context.Context, string, string, any) (Object, error)
		fence   func(context.Context, int, bool) error
		state   func(context.Context, int) (bool, error)
		unit    func(context.Context, string) (bool, bool, bool, error)
		wait    func(context.Context, string, time.Duration) (Object, error)
		lock    func(string, func() error) error
		exe     func(context.Context, int) (string, error)
		now     func() string
	}{updateRun, updateFiles, updateRuntime, updateControl, updateFence, updateFenceState,
		updateUnitState, updateWaitHTTP, updateLock, updateProcessDigest, updateNow}
	w.t.Cleanup(func() {
		updateRun, updateFiles, updateRuntime, updateControl = orig.run, orig.files, orig.rt, orig.control
		updateFence, updateFenceState, updateUnitState = orig.fence, orig.state, orig.unit
		updateWaitHTTP, updateLock, updateProcessDigest, updateNow = orig.wait, orig.lock, orig.exe, orig.now
	})
	updateRun = w.command
	updateFiles = func(Object, string) updateFileAccess { return devAccess{w} }
	updateRuntime = func(Object, string) updateRuntimeSurface { return devRuntime{w} }
	updateControl = func(_ context.Context, _, operation string, _ any) (Object, error) {
		w.controls++
		if operation == "leases" {
			return copyObject(w.leases), nil
		}
		if operation == "status" {
			return Object{"ok": true}, nil
		}
		return nil, fail("unexpected controller operation %s", operation)
	}
	updateFence = func(ctx context.Context, _ int, enabled bool) error {
		if !enabled {
			w.clearCtx = append(w.clearCtx, ctx.Err() != nil)
		} else {
			w.enables++
		}
		w.fences = append(w.fences, enabled)
		blocked := (!enabled && w.clearFail) ||
			(enabled && w.fenceFail) ||
			(enabled && w.refenceFailAt > 0 && w.enables == w.refenceFailAt) ||
			(enabled && w.refenceFailAll && w.enables > 1)
		if blocked {
			return fail("the fence transport failed")
		}
		return nil
	}
	updateFenceState = func(context.Context, int) (bool, error) {
		w.fenceReads++
		return w.priorFenced, nil
	}
	updateUnitState = func(_ context.Context, unit string) (bool, bool, bool, error) {
		if unit != updateDeviceControllerUnit {
			w.record("unexpected", "-", "unit-state "+unit)
			return false, false, false, fail("unexpected unit queried: %s", unit)
		}
		return w.unitPresent, w.unitEnabled, w.unitActive, nil
	}
	updateWaitHTTP = func(_ context.Context, url string, _ time.Duration) (Object, error) {
		w.waits++
		if w.waits <= w.waitFailFor {
			return nil, fail("the controller never became ready")
		}
		return Object{"status": "ok"}, nil
	}
	updateLock = func(path string, fn func() error) error {
		w.lockPath = path
		if w.lockBusy {
			return fail("another device update holds the lock")
		}
		return fn()
	}
	updateProcessDigest = func(_ context.Context, pid int) (string, error) {
		w.digests++
		if pid != devPID {
			return "", fail("unexpected pid %d", pid)
		}
		return w.runningSHA, nil
	}
	updateNow = func() string { return "2026-11-03T00:00:00Z" }
}
