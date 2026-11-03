package sparky

// Offline tests for the Mac-side "sparky update" orchestration. Nothing
// here opens a socket, spawns a child, or touches a real device: SSH, local
// exec, release resolution, asset preparation, the client platform, and the
// Mac target path are all substituted with recorded fakes.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	updateTestVersion = "0.6.5"
	updateTestTag     = "v0.6.5"
	updateTestOld     = "0.6.4" // first updater-capable release
)

// updateHarness is the recorded fake world for one engine run.
type updateHarness struct {
	t *testing.T

	// payloads (verified release contents the prepare seam writes out)
	linuxCLIBytes      []byte
	linuxInstallerByte []byte
	buildinfoBytes     []byte
	macCLIBytes        []byte
	nodeCLISHA         string // = sha256 of linuxCLIBytes (the release digest)
	macCLISHA          string

	// discovered world state
	nodeVersion     string // "sparky --version" line on both nodes
	nodeVersionArg  string // non-empty overrides the version output verbatim
	nodeVersionCode int    // non-zero exit for "sparky --version"
	nodeInstalled   string // disk sha256 reported for ~/.local/bin/sparky
	controllerSHA   string // disk sha256 reported for /proc/<pid>/exe
	nodeReachable   bool
	statusJSON      string
	macFileExists   bool
	macFileBytes    []byte
	macVersionLine  string // rendered from macFileBytes

	// behavior overrides
	readbackWrongSHA bool
	readbackFails    bool
	writeRefuses     bool
	macInstallFails  bool
	// macInstallErrJSON is what the fake bundled installer prints on STDERR
	// when it fails, matching the real sparky-install error envelope.
	macInstallErrJSON string
	deviceStatus      string // device transaction report status
	deviceRunErr      error
	deviceReport      *Object // nil = default built from deviceStatus
	cancelAfterCall   int     // >0: cancel the context after N remote calls
	// cancelAfterWrites cancels the caller's context the instant the Nth
	// staged file has been served, so the updater's next cancellation check
	// (between two pushes) finds a dead context.
	cancelAfterWrites int
	cancelFn          context.CancelFunc
	lastRemoteStderr  string

	// records
	order    []string
	calls    []string // one line per remote call: "<host> <command>"
	writes   []string // "<component> <path>" written
	removes  []string // "<component> <path>" removed with digest
	digests  []string // "<component> <path>" confirmed on disk with sha256sum
	inspects []string // "<component> <path>" read back with node inspect
	selfs    []string // full self-update command strings
	// macInstallArgs records each sparky-install invocation, so a test can
	// prove the legacy-processes override is never passed automatically.
	macInstallArgs []string
}

// updateTestComfyIdle is the ComfyUI half of a real head snapshot: the head
// probes both Sparks, an enabled idle engine reports itself reachable with
// clean queues, and a disabled engine is simply unreachable with zero queues.
const updateTestComfyIdle = `"spark01":{"reachable":true,"running":0,"pending":0,"guarded":true,"executing":false,"phase":"idle"},"spark02":{"reachable":false,"running":0,"pending":0,"guarded":false,"executing":false,"phase":""}`

// updateTestStatusFull builds one head status snapshot from its parts. Empty
// parts fall back to the shapes the head really publishes.
func updateTestStatusFull(models, comfy, extra string) string {
	if strings.TrimSpace(models) == "" {
		models = "{}"
	}
	if strings.TrimSpace(comfy) == "" {
		comfy = updateTestComfyIdle
	}
	return `{"models":` + models + `,"comfyui":{` + comfy + `}` + extra + `}`
}

// updateTestStatus builds a head status snapshot shaped like the one
// runtime.snapshot actually emits: model rows carrying a lifecycle state and
// an active_requests count, plus the ComfyUI rows the head always probes.
func updateTestStatus(models string) string {
	return updateTestStatusFull(models, "", "")
}

func newUpdateHarness(t *testing.T) *updateHarness {
	t.Helper()
	h := &updateHarness{
		t:                  t,
		linuxCLIBytes:      []byte("LINUX-STAGED-CLI-BYTES"),
		linuxInstallerByte: []byte("LINUX-STAGED-INSTALLER-BYTES"),
		buildinfoBytes:     []byte(`{"version":"` + updateTestVersion + `","commit":"` + updateTestCommit + `"}`),
		macCLIBytes:        []byte("MAC-STAGED-CLI-BYTES"),
		nodeVersion:        "sparky " + updateTestOld + " (commit " + updateTestCommit + ")",
		nodeReachable:      true,
		statusJSON:         updateTestStatus(""),
		macFileExists:      true,
		deviceStatus:       "updated",
	}
	h.nodeCLISHA = sha256Hex(h.linuxCLIBytes)
	h.macCLISHA = sha256Hex(h.macCLIBytes)
	// Default: both nodes and the head controller already run the exact
	// verified bytes at the release version (all current).
	h.nodeInstalled = h.nodeCLISHA
	h.controllerSHA = h.nodeCLISHA
	// Default Mac file: the OLD release CLI (pending).
	h.macFileBytes = []byte("MAC-OLD-CLI-BYTES")
	h.refreshMacVersionLine()
	return h
}

func (h *updateHarness) refreshMacVersionLine() {
	if bytes.Equal(h.macFileBytes, h.macCLIBytes) {
		h.macVersionLine = "sparky " + updateTestVersion + " (commit " + updateTestCommit + ")"
		return
	}
	h.macVersionLine = "sparky " + updateTestOld + " (commit " + updateTestCommit + ")"
}

// makeCurrent marks every component as already on the verified release.
func (h *updateHarness) makeCurrent() {
	h.nodeInstalled = h.nodeCLISHA
	h.controllerSHA = h.nodeCLISHA
	h.nodeVersion = "sparky " + updateTestVersion + " (commit " + updateTestCommit + ")"
	h.macFileBytes = append([]byte(nil), h.macCLIBytes...)
	h.refreshMacVersionLine()
}

// setNodeStale makes both node CLIs differ from the verified bytes.
func (h *updateHarness) setNodeStale() {
	h.nodeInstalled = sha256Hex([]byte("stale-node-cli"))
	h.controllerSHA = h.nodeInstalled
	h.nodeVersion = "sparky " + updateTestOld + " (commit " + updateTestCommit + ")"
}

func (h *updateHarness) setMacCurrent() {
	h.macFileBytes = append([]byte(nil), h.macCLIBytes...)
	h.refreshMacVersionLine()
}

func (h *updateHarness) setNodeCurrent() {
	h.nodeInstalled = h.nodeCLISHA
	h.controllerSHA = h.nodeCLISHA
	h.nodeVersion = "sparky " + updateTestVersion + " (commit " + updateTestCommit + ")"
}

// install swaps every effect seam for the duration of the test.
func (h *updateHarness) install() {
	h.t.Helper()
	dir := h.t.TempDir()
	macTarget := filepath.Join(dir, "bin", "sparky")
	if err := os.MkdirAll(filepath.Dir(macTarget), 0o755); err != nil {
		h.t.Fatal(err)
	}
	writeMac := func() {
		if h.macFileExists {
			if err := os.WriteFile(macTarget, h.macFileBytes, 0o755); err != nil {
				h.t.Fatal(err)
			}
		} else {
			_ = os.Remove(macTarget)
		}
	}
	writeMac()

	rel := &updateRelease{
		Tag: updateTestTag, Version: updateTestVersion, Commit: updateTestCommit,
		Assets: map[string]updateAsset{},
		BinarySHA256: map[string]map[string]string{
			updatePlatformClient: {"sparky": h.macCLISHA, "sparky-install": sha256Hex([]byte("mac-installer"))},
			updatePlatformNode:   {"sparky": h.nodeCLISHA, "sparky-install": sha256Hex(h.linuxInstallerByte)},
		},
	}

	orig := struct {
		resolve  func(context.Context, string) (*updateRelease, error)
		prepare  func(context.Context, *updateRelease, string, string) (*updatePrepared, error)
		exec     func(context.Context, []string, []byte) (CommandResult, error)
		mac      func() (string, error)
		platform func() (string, string)
	}{updateResolveSeam, updatePrepareSeam, updateExecSeam, updateMacTargetSeam, updateClientPlatformSeam}
	h.t.Cleanup(func() {
		updateResolveSeam, updatePrepareSeam, updateExecSeam = orig.resolve, orig.prepare, orig.exec
		updateMacTargetSeam, updateClientPlatformSeam = orig.mac, orig.platform
	})

	updateResolveSeam = func(_ context.Context, tag string) (*updateRelease, error) {
		h.order = append(h.order, "resolve")
		if tag != "" && tag != updateTestTag {
			return nil, fail("Release %s was not found.", tag)
		}
		return rel, nil
	}
	updatePrepareSeam = func(_ context.Context, _ *updateRelease, platform, dest string) (*updatePrepared, error) {
		h.order = append(h.order, "prepare-"+platform)
		dir, err := os.MkdirTemp(dest, "stage")
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
			return nil, err
		}
		prepared := &updatePrepared{Dir: dir}
		if platform == updatePlatformNode {
			prepared.CLI = filepath.Join(dir, "bin", "sparky")
			prepared.Installer = filepath.Join(dir, "bin", "sparky-install")
			if err := os.WriteFile(prepared.CLI, h.linuxCLIBytes, 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(prepared.Installer, h.linuxInstallerByte, 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, "BUILDINFO.json"), h.buildinfoBytes, 0o600); err != nil {
				return nil, err
			}
			prepared.CLISHA, prepared.InstallerSHA = h.nodeCLISHA, sha256Hex(h.linuxInstallerByte)
		} else {
			prepared.CLI = filepath.Join(dir, "bin", "sparky")
			prepared.Installer = filepath.Join(dir, "bin", "sparky-install")
			if err := os.WriteFile(prepared.CLI, h.macCLIBytes, 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(prepared.Installer, []byte("mac-installer"), 0o700); err != nil {
				return nil, err
			}
			prepared.CLISHA, prepared.InstallerSHA = h.macCLISHA, sha256Hex([]byte("mac-installer"))
		}
		return prepared, nil
	}
	updateMacTargetSeam = func() (string, error) { return macTarget, nil }
	updateClientPlatformSeam = func() (string, string) { return "darwin", "arm64" }

	updateExecSeam = func(_ context.Context, args []string, _ []byte) (CommandResult, error) {
		joined := strings.Join(args, " ")
		switch {
		case len(args) == 2 && args[1] == "--version":
			h.order = append(h.order, "exec:mac-version")
			if !h.macFileExists {
				return CommandResult{Code: 127}, nil
			}
			return CommandResult{Out: h.macVersionLine + "\n", Code: 0}, nil
		case strings.Contains(joined, "--source") && strings.Contains(joined, "--target"):
			h.order = append(h.order, "exec:mac-install")
			h.macInstallArgs = append(h.macInstallArgs, strings.Join(args, " "))
			if h.macInstallFails {
				// The real installer prints its error envelope on stderr and
				// leaves stdout empty.
				body := h.macInstallErrJSON
				if body == "" {
					body = `{"error":"a running process holds the file"}`
				}
				return CommandResult{Err: body + "\n", Code: 1}, nil
			}
			h.macFileBytes = append([]byte(nil), h.macCLIBytes...)
			h.macFileExists = true
			h.refreshMacVersionLine()
			writeMac()
			out := fmt.Sprintf(`{"installed":%q,"backup":%q,"sha256":%q,"format":"binary"}`, macTarget, macTarget+".bak", h.macCLISHA)
			return CommandResult{Out: out, Code: 0}, nil
		}
		return CommandResult{Code: 127}, nil
	}

	restoreRemote := remoteUseFakeRunner(h.fakeRemote)
	h.t.Cleanup(restoreRemote)
	origProgress := cliProgressOut
	cliProgressOut = io.Discard
	h.t.Cleanup(func() { cliProgressOut = origProgress })
}

func (h *updateHarness) fakeRemote(ctx context.Context, argv []string, stdin []byte, _ func([]byte)) (remoteRunResult, error) {
	if h.cancelAfterCall > 0 && len(h.calls) >= h.cancelAfterCall {
		if h.cancelFn != nil {
			h.cancelFn()
		}
		return remoteRunResult{}, context.Canceled
	}
	joined := strings.Join(argv, " ")
	// shellJoin quotes each word and shellQuote's inner escapes contain double
	// quotes; match on the fully unquoted form.
	match := strings.NewReplacer("'", "", `"`, "").Replace(joined)
	component := "spark01"
	if strings.Contains(match, "ssh spark02 ") {
		component = "spark02"
	}
	h.calls = append(h.calls, component+" "+joined)
	host := argv[len(argv)-2]
	resp := func(kind string, out string, code int) remoteRunResult {
		h.order = append(h.order, "remote:"+kind)
		_ = host
		return remoteRunResult{stdout: []byte(out), code: code}
	}
	switch {
	case strings.Contains(match, "self-update"):
		h.selfs = append(h.selfs, joined)
		h.order = append(h.order, "remote:self-update")
		if h.deviceRunErr != nil {
			return remoteRunResult{}, h.deviceRunErr
		}
		if h.deviceReport != nil {
			b, _ := json.Marshal(*h.deviceReport)
			code := 0
			if str((*h.deviceReport)["status"]) != "updated" && str((*h.deviceReport)["status"]) != "unchanged" {
				code = 1
			}
			return remoteRunResult{stdout: b, code: code}, nil
		}
		report := updateTestDeviceReport(h.deviceStatus, h.nodeCLISHA)
		b, _ := json.Marshal(report)
		code := 0
		if h.deviceStatus != "updated" && h.deviceStatus != "unchanged" {
			code = 1
		}
		return remoteRunResult{stdout: b, code: code}, nil

	case strings.Contains(match, "node write-file"):
		var meta Object
		if err := json.Unmarshal(stdin, &meta); err != nil {
			return remoteRunResult{stdout: []byte(`{"error":"bad metadata"}`), code: 1}, nil
		}
		pathName := str(meta["path"])
		switch str(meta["operation"]) {
		case "", "write":
			h.writes = append(h.writes, component+" "+pathName)
			if h.cancelAfterWrites > 0 && len(h.writes) == h.cancelAfterWrites && h.cancelFn != nil {
				// The operator walks away the instant this file lands: the
				// call is served, and the updater's next cancellation check
				// finds a dead context.
				h.cancelFn()
			}
			if h.writeRefuses {
				return resp("write", `{"error":"refused"}`, 1), nil
			}
			// The real receiver echoes the digest of what it wrote.
			raw, derr := base64.StdEncoding.DecodeString(str(meta["data_base64"]))
			if derr != nil {
				return resp("write", `{"error":"bad base64"}`, 1), nil
			}
			return resp("write", fmt.Sprintf(`{"status":"written","path":%q,"sha256":%q}`, pathName, sha256Hex(raw)), 0), nil
		case "inspect":
			// The updater must NOT use inspect for payload readback (it would
			// return the whole file base64-encoded); recording it lets a test
			// fail if that ever regresses.
			h.inspects = append(h.inspects, component+" "+pathName)
			want := h.nodeCLISHA
			switch {
			case strings.Contains(pathName, "sparky-install"):
				want = sha256Hex(h.linuxInstallerByte)
			case strings.HasSuffix(pathName, "BUILDINFO.json"):
				want = sha256Hex(h.buildinfoBytes)
			}
			return resp("inspect", fmt.Sprintf(`{"exists":true,"sha256":%q,"mode":"755","data_base64":""}`, want), 0), nil
		case "remove":
			h.removes = append(h.removes, component+" "+pathName)
			return resp("remove", `{"status":"removed"}`, 0), nil
		}
		return remoteRunResult{code: 1}, nil

	case strings.Contains(match, "node status --root"):
		return resp("status", h.statusJSON, 0), nil

	case strings.Contains(match, "sha256sum -- "):
		// On-disk readback of one staged file. The updater shell-quotes the
		// path (and the nested spark02 command is quoted again), so the
		// quote-stripped "match" form yields the clean path.
		pathName := strings.Trim(match[strings.Index(match, "sha256sum -- ")+len("sha256sum -- "):], "\\'")
		h.digests = append(h.digests, component+" "+pathName)
		if h.readbackFails {
			return resp("digest", "", 1), nil
		}
		want := h.nodeCLISHA
		switch {
		case strings.Contains(pathName, "sparky-install"):
			want = sha256Hex(h.linuxInstallerByte)
		case strings.HasSuffix(pathName, "BUILDINFO.json"):
			want = sha256Hex(h.buildinfoBytes)
		}
		if h.readbackWrongSHA {
			want = sha256Hex([]byte("tampered"))
		}
		return resp("digest", want+"  "+pathName+"\n", 0), nil

	case strings.Contains(match, "~/.local/bin/sparky --version"):
		if !h.nodeReachable {
			return resp("version", "", 255), nil
		}
		if h.nodeVersionCode != 0 {
			return resp("version", "", h.nodeVersionCode), nil
		}
		line := h.nodeVersion
		if h.nodeVersionArg != "" {
			line = h.nodeVersionArg
		}
		return resp("version", line+"\n", 0), nil

	case strings.Contains(match, "sha256sum ~/.local/bin/sparky"):
		if !h.nodeReachable {
			return resp("digest", "", 255), nil
		}
		return resp("digest", h.nodeInstalled+"  /home/spark/.local/bin/sparky\n", 0), nil

	case strings.Contains(match, "systemctl show"):
		return resp("mainpid", "1234\n", 0), nil

	case strings.Contains(match, "sha256sum /proc/"):
		return resp("exe", h.controllerSHA+"  /proc/1234/exe\n", 0), nil
	}
	return remoteRunResult{code: 1}, nil
}

// updateTestDeviceReport builds one staged device transaction report.
func updateTestDeviceReport(status, cliSHA string) Object {
	changed := status == "updated"
	node := func(name string) Object {
		o := Object{"node": name, "changed": changed, "state": "ready", "sha256": cliSHA, "version": updateTestVersion}
		if changed {
			o["sha256"] = cliSHA
			o["version"] = updateTestVersion
			o["backup"] = "/srv/backups/" + name + ".sparky.065"
		}
		return o
	}
	return Object{
		"status":     status,
		"release":    Object{"tag": updateTestTag, "version": updateTestVersion, "commit": updateTestCommit},
		"targets":    Object{"spark01": node("spark01"), "spark02": node("spark02")},
		"controller": Object{"unit": updateDeviceControllerUnit, "restarted": changed, "restart": "restarted", "running_sha256": cliSHA},
		"rollback":   Object{"complete": status == "rolled_back"},
		"fence":      Object{"restored": status == "rolled_back" || changed || status == "unchanged"},
	}
}

func updateHarnessConfig() clientConfig {
	return clientConfig{
		Version: 1,
		Extra: Object{
			"admin_ssh_host":  "spark01",
			"worker_ssh_host": "spark02",
			"remote_root":     "/srv",
		},
	}
}

func (h *updateHarness) run(ctx context.Context, check bool, tag string) updateResult {
	h.t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	h.cancelFn = cancel
	defer cancel()
	return updateExecute(runCtx, updateHarnessConfig(), check, tag, nil)
}

func (h *updateHarness) components(report Object) map[string]Object {
	out := map[string]Object{}
	for _, raw := range arr(report["components"]) {
		c := obj(raw)
		out[str(c["component"])] = c
	}
	return out
}

func (h *updateHarness) hasEvent(prefix string) bool {
	for _, e := range h.order {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func (h *updateHarness) indexOf(event string) int {
	for i, e := range h.order {
		if e == event {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Engine tests.

func TestUpdateAllCurrentAppliesNoEffects(t *testing.T) {
	h := newUpdateHarness(t)
	h.makeCurrent()
	h.install()
	res := h.run(context.Background(), false, "")
	if res.err != nil || res.canceled || !res.hasReport {
		t.Fatalf("res %+v", res)
	}
	if !boolean(res.report["ok"]) {
		t.Fatalf("ok: %+v", res.report)
	}
	if len(h.writes) != 0 || len(h.selfs) != 0 {
		t.Fatalf("all-current must not write or fence: %+v %+v", h.writes, h.selfs)
	}
	if h.hasEvent("exec:mac-install") || h.hasEvent("prepare-") {
		t.Fatalf("all-current must not stage or install: %v", h.order)
	}
	for _, name := range []string{"mac", "spark01", "spark02"} {
		if s := str(h.components(res.report)[name]["status"]); s != "current" {
			t.Fatalf("%s status %q", name, s)
		}
	}
}

func TestUpdateCheckAvailabilityAndNoDiskEffects(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale() // mac and nodes pending
	h.install()
	res := h.run(context.Background(), true, "")
	if res.err != nil || !boolean(res.report["ok"]) {
		t.Fatalf("check with update available must succeed: %+v", res)
	}
	c := h.components(res.report)
	if str(c["mac"]["status"]) != "pending" || str(c["spark01"]["status"]) != "pending" {
		t.Fatalf("statuses: %+v", c)
	}
	if len(h.writes) != 0 || len(h.selfs) != 0 || h.hasEvent("prepare-") || h.hasEvent("exec:mac-install") {
		t.Fatalf("check must not stage, push, install, or restart: %v %v", h.order, h.writes)
	}
}

func TestUpdateCheckIncompleteIsNonZeroReport(t *testing.T) {
	h := newUpdateHarness(t)
	h.nodeReachable = false
	h.install()
	res := h.run(context.Background(), true, "")
	if boolean(res.report["ok"]) {
		t.Fatalf("unreachable target must make the check incomplete")
	}
	if str(h.components(res.report)["spark01"]["status"]) != "unreachable" {
		t.Fatalf("component: %+v", res.report)
	}
	if len(h.writes) != 0 || len(h.selfs) != 0 {
		t.Fatalf("no writes allowed: %v", h.writes)
	}
}

func TestUpdateBusyRefusesApply(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.statusJSON = `{"models":{"glm":{"state":"queued","loaded":false}}}`
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) {
		t.Fatalf("busy must refuse apply")
	}
	if len(h.writes) != 0 || len(h.selfs) != 0 || h.hasEvent("exec:mac-install") {
		t.Fatalf("busy must not write anything: %v", h.order)
	}
	// A check with a busy controller is still informational (known state).
	h2 := newUpdateHarness(t)
	h2.setNodeStale()
	h2.statusJSON = `{"models":{"glm":{"state":"queued","loaded":false}}}`
	h2.install()
	if res2 := h2.run(context.Background(), true, ""); !boolean(res2.report["ok"]) {
		t.Fatalf("busy check should report ok")
	}
}

func TestUpdateUnknownStatusBlocksApply(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.statusJSON = `{"models":{"glm":{"state":"warp-speed","loaded":true}}}`
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) || len(h.writes) != 0 {
		t.Fatalf("unknown model state must block: %v", h.order)
	}
}

func TestUpdateControllerStaleTriggersDeviceTransaction(t *testing.T) {
	h := newUpdateHarness(t)
	h.setMacCurrent()
	h.nodeInstalled = h.nodeCLISHA
	h.nodeVersion = "sparky " + updateTestVersion + " (commit " + updateTestCommit + ")"
	h.controllerSHA = sha256Hex([]byte("old-running-controller"))
	h.install()
	res := h.run(context.Background(), false, "")
	if !boolean(res.report["ok"]) {
		t.Fatalf("stale controller must converge: %+v", res.report)
	}
	if len(h.selfs) != 1 {
		t.Fatalf("device transaction must run once: %v", h.selfs)
	}
	if h.hasEvent("exec:mac-install") {
		t.Fatalf("mac was current; it must not reinstall")
	}
	c := h.components(res.report)
	if restarted, _ := c["spark01"]["controller_restarted"].(bool); !restarted {
		t.Fatalf("controller restart must be reported: %+v", c["spark01"])
	}
}

func TestUpdateStaleNodeUpdatesViaStagedCLIOnly(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.setMacCurrent()
	h.install()
	res := h.run(context.Background(), false, "")
	if !boolean(res.report["ok"]) {
		t.Fatalf("ok: %+v", res.report)
	}
	if len(h.selfs) != 1 {
		t.Fatalf("one transaction: %v", h.selfs)
	}
	// Bootstrap rule: the spec goes ONLY to the staged verified CLI.
	if !strings.Contains(h.selfs[0], ".sparky-update") || !strings.Contains(h.selfs[0], "self-update") {
		t.Fatalf("transaction must run from the staged CLI: %s", h.selfs[0])
	}
	for _, line := range h.selfs {
		if strings.Contains(strings.NewReplacer("'", "", `"`, "").Replace(line), "~/.local/bin/sparky --json node self-update") {
			t.Fatalf("installed CLI must never receive self-update: %s", line)
		}
	}
	// Head and worker stages must be distinct directories under the base.
	if len(h.writes) != 6 {
		t.Fatalf("expected 3 files x 2 nodes pushes: %v", h.writes)
	}
	// F5: each staged file is confirmed on disk with sha256sum, never with
	// node inspect (which returns the whole file base64-encoded and would
	// move every megabyte of the payload a second time).
	if len(h.digests) != 6 {
		t.Fatalf("every staged file must be hashed back: %v", h.digests)
	}
	if len(h.inspects) != 0 {
		t.Fatalf("payload readback must not use inspect: %v", h.inspects)
	}
	for _, d := range h.digests {
		if !strings.HasSuffix(d, "bin/sparky") && !strings.HasSuffix(d, "bin/sparky-install") && !strings.HasSuffix(d, "BUILDINFO.json") {
			t.Fatalf("unexpected digest readback target: %s", d)
		}
	}
	// The path must reach the shell quoted, so it cannot be re-split.
	quoted := false
	for _, line := range h.calls {
		if strings.Contains(line, "sha256sum -- '") {
			quoted = true
		}
	}
	if !quoted {
		t.Fatalf("the readback path must be shell-quoted: %v", h.calls)
	}
	headSeen, workerSeen := false, false
	for _, w := range h.writes {
		if strings.Contains(w, "-head/bin/sparky") {
			headSeen = true
		}
		if strings.Contains(w, "-worker/bin/sparky") {
			workerSeen = true
		}
	}
	if !headSeen || !workerSeen {
		t.Fatalf("distinct head/worker stages required: %v", h.writes)
	}
	// Cleanup removed exactly the task-owned files, never a backup.
	if len(h.removes) != 6 {
		t.Fatalf("cleanup must remove the 6 staged files: %v", h.removes)
	}
	for _, r := range h.removes {
		if strings.Contains(r, "backups") {
			t.Fatalf("cleanup must never touch backups: %s", r)
		}
	}
}

func TestUpdatePushDigestMismatchBlocksAll(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.readbackWrongSHA = true
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) {
		t.Fatalf("digest mismatch must fail closed")
	}
	if len(h.selfs) != 0 {
		t.Fatalf("a mismatched stage must never run: %v", h.selfs)
	}
	if h.hasEvent("exec:mac-install") {
		t.Fatalf("mac install must be blocked by push mismatch")
	}
	c := h.components(res.report)
	if str(c["spark01"]["status"]) != "failed" || str(c["spark02"]["status"]) != "failed" {
		t.Fatalf("both nodes reported failed: %+v", c)
	}
}

func TestUpdateRemoteFailureBlocksMacInstall(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.deviceStatus = "failed"
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) {
		t.Fatalf("device failure must surface")
	}
	if h.hasEvent("exec:mac-install") {
		t.Fatalf("Mac install is blocked while the device transaction failed")
	}
	c := h.components(res.report)
	if str(c["mac"]["status"]) != "pending" || str(c["spark01"]["status"]) != "failed" {
		t.Fatalf("partial report preserved: %+v", c)
	}
	if !strings.Contains(str(c["spark01"]["error"]), "failed") {
		t.Fatalf("error text preserved: %+v", c["spark01"])
	}
}

func TestUpdateDeviceErrorStatusBlocksMac(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	rep := Object{"status": "error", "error": "controller did not come back"}
	h.deviceReport = &rep
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) || h.hasEvent("exec:mac-install") {
		t.Fatalf("status:error must block the Mac: %+v %v", res.report, h.order)
	}
}

func TestUpdateInstallsMacLast(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.install()
	res := h.run(context.Background(), false, "")
	if !boolean(res.report["ok"]) {
		t.Fatalf("ok: %+v", res.report)
	}
	selfIdx, installIdx := h.indexOf("remote:self-update"), h.indexOf("exec:mac-install")
	if selfIdx < 0 || installIdx < 0 || selfIdx > installIdx {
		t.Fatalf("Mac must be installed last: %v", h.order)
	}
	c := h.components(res.report)
	if str(c["mac"]["status"]) != "updated" || str(c["mac"]["version"]) != updateTestVersion {
		t.Fatalf("mac component: %+v", c["mac"])
	}
	if str(c["mac"]["backup"]) == "" {
		t.Fatalf("backup path must be reported")
	}
	if !h.hasEvent("exec:mac-version") {
		t.Fatalf("installed digest AND version must be confirmed")
	}
}

func TestUpdateMacFailureKeepsRemoteSuccess(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.macInstallFails = true
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) {
		t.Fatalf("mac failure must not report success")
	}
	c := h.components(res.report)
	if str(c["spark01"]["status"]) != "updated" || str(c["spark02"]["status"]) != "updated" {
		t.Fatalf("remote updates preserved: %+v", c)
	}
	if str(c["mac"]["status"]) != "failed" {
		t.Fatalf("mac failure preserved: %+v", c["mac"])
	}
}

func TestUpdateRefusesOlderVersionEvenWithExplicitTag(t *testing.T) {
	h := newUpdateHarness(t)
	h.nodeVersion = "sparky 0.7.0 (commit " + updateTestCommit + ")"
	h.install()
	res := h.run(context.Background(), false, updateTestTag)
	if res.err == nil || !strings.Contains(res.err.Error(), "newer") {
		t.Fatalf("must refuse older selection: %+v", res)
	}
	if len(h.writes) != 0 || h.hasEvent("prepare-") {
		t.Fatalf("no effects before the gate passes: %v", h.order)
	}
}

func TestUpdateOldTargetRequiresManualBootstrap(t *testing.T) {
	h := newUpdateHarness(t)
	h.nodeVersion = "Sparky zipapp 0.5.9" // unparseable pre-updater output
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) {
		t.Fatalf("old target must block")
	}
	c := h.components(res.report)
	if str(c["spark01"]["status"]) != "predates" || !strings.Contains(str(c["spark01"]["error"]), "manually") {
		t.Fatalf("bootstrap guidance: %+v", c["spark01"])
	}
	for _, w := range h.writes {
		if strings.Contains(w, ".sparky-update") {
			t.Fatalf("old target must never be pushed to: %s", w)
		}
	}
	if len(h.selfs) != 0 {
		t.Fatalf("old target must never receive self-update: %v", h.selfs)
	}
}

func TestUpdateMissingMacCLIBlocks(t *testing.T) {
	h := newUpdateHarness(t)
	h.macFileExists = false
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) {
		t.Fatalf("missing mac CLI must block")
	}
	if s := str(h.components(res.report)["mac"]["status"]); s != "missing" {
		t.Fatalf("missing status: %q", s)
	}
}

func TestUpdateUnsupportedClientPlatform(t *testing.T) {
	h := newUpdateHarness(t)
	h.install()
	updateClientPlatformSeam = func() (string, string) { return "linux", "amd64" }
	inv, err := cliParse([]string{"update"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	code := cliUpdateCommand(context.Background(), updateHarnessConfig(), inv, &out, &errout)
	if code != 1 || !strings.Contains(errout.String(), "macOS") {
		t.Fatalf("platform gate: %d %q", code, errout.String())
	}
	if len(h.calls) != 0 {
		t.Fatalf("platform gate must precede all I/O: %v", h.calls)
	}
}

func TestUpdateCancellationKeepsPartialReport(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.cancelAfterCall = 1
	h.install()
	res := h.run(context.Background(), false, "")
	if !res.canceled {
		t.Fatalf("cancellation must be reported")
	}
	if !res.hasReport {
		t.Fatalf("partial report must survive cancellation")
	}
	// CLI maps cancellation to 130 while still emitting the report.
	inv, err := cliParse([]string{"update", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	cliCtx, cliCancel := context.WithCancel(context.Background())
	defer cliCancel()
	h.cancelFn = cliCancel
	var out, errout bytes.Buffer
	code := cliUpdateCommand(cliCtx, updateHarnessConfig(), inv, &out, &errout)
	if code != cliInterruptExit {
		t.Fatalf("interrupt exit: %d %s", code, errout.String())
	}
	if !strings.Contains(out.String(), "components") {
		t.Fatalf("partial report emitted before interrupt notice: %q", out.String())
	}
}

func TestUpdateEngineCleanupRunsEvenOnDeviceFailure(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.deviceStatus = "rolled_back"
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) {
		t.Fatalf("rollback must fail the run")
	}
	if len(h.removes) != 6 {
		t.Fatalf("task-owned staging must be cleaned: %v", h.removes)
	}
}

func TestUpdateVersionGateAgainstNewerMacCLI(t *testing.T) {
	h := newUpdateHarness(t)
	h.macFileBytes = append([]byte(nil), h.macCLIBytes...)
	h.macVersionLine = "sparky 9.9.9 (commit " + updateTestCommit + ")"
	h.install()
	res := h.run(context.Background(), true, "")
	if res.err == nil || !strings.Contains(res.err.Error(), "newer") {
		t.Fatalf("a newer Mac CLI must refuse an older selection: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// Quiescence pre-check. The helper must read the activity signals the head
// actually publishes, and must never turn an unreadable signal into "idle".

const (
	updateTestIdleAudio  = `{"audio":{"kind":"audio","loaded":true,"state":"running","active_requests":0}}`
	updateTestIdleComfy  = `"spark01":{"reachable":false,"running":0,"pending":0,"guarded":false},"spark02":{"reachable":false,"running":0,"pending":0,"guarded":false}`
	updateTestBusyComfy0 = `"spark01":{"reachable":true,"running":0,"pending":0,"guarded":true,"executing":true},"spark02":{"reachable":false,"running":0,"pending":0,"guarded":false}`
)

func TestUpdateStatusActivityVerdicts(t *testing.T) {
	cases := []struct {
		name   string
		status string
		want   string
	}{
		{"production idle", updateTestStatusFull(`{"audio":{"kind":"audio","loaded":true,"state":"running","active_requests":0},"glm":{"kind":"llm","loaded":false,"state":"stopped","active_requests":0}}`, updateTestComfyIdle, ""), updateActivityIdle},
		{"loaded audio reports running", updateTestStatus(updateTestIdleAudio), updateActivityIdle},
		{"legacy idle spelling", updateTestStatus(`{"audio":{"kind":"audio","loaded":true,"state":"idle","active_requests":0}}`), updateActivityIdle},
		{"ready llm with a live request", updateTestStatus(`{"glm":{"kind":"llm","loaded":true,"state":"ready","active_requests":1}}`), updateActivityBusy},
		{"queued model", updateTestStatus(`{"glm":{"kind":"llm","loaded":false,"state":"queued","active_requests":0}}`), updateActivityBusy},
		{"draining model", updateTestStatus(`{"glm":{"kind":"llm","loaded":true,"state":"draining","active_requests":0}}`), updateActivityBusy},
		{"unrecognised model state", updateTestStatus(`{"glm":{"kind":"llm","loaded":true,"state":"warp-speed","active_requests":0}}`), updateActivityUnknown},
		{"missing request count", updateTestStatus(`{"glm":{"kind":"llm","loaded":true,"state":"ready"}}`), updateActivityUnknown},
		{"non-numeric request count", updateTestStatus(`{"glm":{"kind":"llm","loaded":true,"state":"ready","active_requests":"1"}}`), updateActivityUnknown},
		{"negative request count", updateTestStatus(`{"glm":{"kind":"llm","loaded":true,"state":"ready","active_requests":-2}}`), updateActivityUnknown},
		{"admission active", updateTestStatusFull("", "", `,"admission":{"active":[{"model":"glm"}]}`), updateActivityBusy},
		{"admission waiting", updateTestStatusFull("", "", `,"admission":{"waiting":["job-1"]}`), updateActivityBusy},
		{"controller transition in progress", updateTestStatusFull("", "", `,"admission":{"config_transition":true}`), updateActivityBusy},
		{"garbled transition flag", updateTestStatusFull("", "", `,"admission":{"config_transition":"yes"}`), updateActivityUnknown},
		{"controller cannot read its activity", updateTestStatusFull("", "", `,"activity_known":false`), updateActivityUnknown},
		{"comfy executing with an empty queue", updateTestStatusFull("", updateTestBusyComfy0, ""), updateActivityBusy},
		{"comfy phase executing", updateTestStatusFull("", `"spark01":{"reachable":true,"running":0,"pending":0,"phase":"executing"},"spark02":{"reachable":false,"running":0,"pending":0}`, ""), updateActivityBusy},
		{"comfy queued work", updateTestStatusFull("", `"spark01":{"reachable":true,"running":0,"pending":3},"spark02":{"reachable":false,"running":0,"pending":0}`, ""), updateActivityBusy},
		{"comfy running while unreachable", updateTestStatusFull("", `"spark01":{"reachable":false,"running":1,"pending":0},"spark02":{"reachable":false,"running":0,"pending":0}`, ""), updateActivityBusy},
		{"comfy node not reported", updateTestStatusFull("", `"spark01":{"reachable":true,"running":0,"pending":0}`, ""), updateActivityUnknown},
		{"comfy queue unreadable", updateTestStatusFull("", `"spark01":{"reachable":true,"running":null,"pending":0},"spark02":{"reachable":false,"running":0,"pending":0}`, ""), updateActivityUnknown},
		{"comfy reachability unreadable", updateTestStatusFull("", `"spark01":{"running":0,"pending":0},"spark02":{"reachable":false,"running":0,"pending":0}`, ""), updateActivityUnknown},
		{"guarded comfy without its guard state", updateTestStatusFull("", `"spark01":{"reachable":true,"running":0,"pending":0,"guarded":true},"spark02":{"reachable":false,"running":0,"pending":0}`, ""), updateActivityUnknown},
		{"both engines idle and unreachable", updateTestStatusFull("", updateTestIdleComfy, ""), updateActivityIdle},
		{"loaded comfy with no reachable engine", updateTestStatusFull(`{"flux":{"kind":"comfyui","loaded":true,"state":"running","active_requests":0}}`, updateTestIdleComfy, ""), updateActivityUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, derr := decode([]byte(tc.status))
			if derr != nil {
				t.Fatalf("fixture is not valid JSON: %v", derr)
			}
			got := updateStatusActivity(status)
			if got.state != tc.want {
				t.Fatalf("verdict %q (%s), want %q", got.state, got.detail, tc.want)
			}
			if got.state != updateActivityIdle && got.detail == "" {
				t.Fatalf("a blocking verdict must name its signal: %+v", got)
			}
		})
	}
}

func TestUpdateIdleProductionSnapshotApplies(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	// A loaded-but-idle audio container, a loaded ready LLM with no requests,
	// and an idle Comfy engine: the ordinary production shape must apply.
	h.statusJSON = updateTestStatusFull(`{"audio":{"kind":"audio","loaded":true,"state":"running","active_requests":0},"glm":{"kind":"llm","loaded":true,"state":"ready","active_requests":0}}`, updateTestComfyIdle, "")
	h.install()
	res := h.run(context.Background(), false, "")
	if !boolean(res.report["ok"]) {
		t.Fatalf("an idle cluster must apply: %+v", res.report)
	}
	if len(h.selfs) != 1 {
		t.Fatalf("transaction must run once: %v", h.selfs)
	}
	if str(obj(res.report["quiescence"])["state"]) != updateActivityIdle {
		t.Fatalf("quiescence must be idle: %+v", res.report["quiescence"])
	}
}

func TestUpdateActiveRequestsBlocksApply(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	// state "ready" is steady, but a live request is not: pending==0 says
	// nothing about a model serving work right now.
	h.statusJSON = updateTestStatus(`{"glm":{"kind":"llm","loaded":true,"state":"ready","active_requests":1}}`)
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) || len(h.writes) != 0 || len(h.selfs) != 0 {
		t.Fatalf("a live request must block apply: %+v", res.report)
	}
	if !strings.Contains(str(res.report["blocked"]), "active request") {
		t.Fatalf("the blocker must name the request count: %+v", res.report)
	}
	// The same cluster checked without --apply stays informational.
	h2 := newUpdateHarness(t)
	h2.setNodeStale()
	h2.statusJSON = updateTestStatus(`{"glm":{"kind":"llm","loaded":true,"state":"ready","active_requests":1}}`)
	h2.install()
	res2 := h2.run(context.Background(), true, "")
	if !boolean(res2.report["ok"]) {
		t.Fatalf("a busy check is still a valid report: %+v", res2.report)
	}
	if str(obj(res2.report["quiescence"])["state"]) != updateActivityBusy {
		t.Fatalf("the check must say busy: %+v", res2.report["quiescence"])
	}
}

func TestUpdateExecutingComfyBlocksApply(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	// A guarded engine executing one job while its queue reads 0.
	h.statusJSON = updateTestStatusFull("", updateTestBusyComfy0, "")
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) || len(h.writes) != 0 {
		t.Fatalf("an executing engine must block apply: %+v", res.report)
	}
	if !strings.Contains(str(res.report["blocked"]), "executing") {
		t.Fatalf("the blocker must name the guard state: %+v", res.report)
	}
}

func TestUpdateMalformedActivityIsNotIdle(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.statusJSON = updateTestStatus(`{"glm":{"kind":"llm","loaded":true,"state":"ready","active_requests":"later"}}`)
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) || len(h.writes) != 0 {
		t.Fatalf("unreadable activity must block apply: %+v", res.report)
	}
	if !strings.Contains(str(res.report["blocked"]), "not understood") {
		t.Fatalf("unreadable activity must be reported as unknown: %+v", res.report)
	}
	if str(obj(res.report["quiescence"])["state"]) != updateActivityUnknown {
		t.Fatalf("quiescence must be unknown: %+v", res.report["quiescence"])
	}
	// An unknown state also makes the CHECK incomplete, unlike a busy one.
	h2 := newUpdateHarness(t)
	h2.setNodeStale()
	h2.statusJSON = updateTestStatusFull("", `"spark01":{"reachable":true,"running":0,"pending":0,"guarded":true},"spark02":{"reachable":false,"running":0,"pending":0}`, "")
	h2.install()
	if res2 := h2.run(context.Background(), true, ""); boolean(res2.report["ok"]) {
		t.Fatalf("a check that cannot prove idleness must be incomplete: %+v", res2.report)
	}
}

func TestUpdateUnreachableIdleComfyIsNotBusy(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.statusJSON = updateTestStatusFull(updateTestIdleAudio, updateTestIdleComfy, "")
	h.install()
	res := h.run(context.Background(), false, "")
	if !boolean(res.report["ok"]) {
		t.Fatalf("an inactive unreachable engine must not block: %+v", res.report)
	}
	q := obj(res.report["quiescence"])
	if str(q["state"]) != updateActivityIdle {
		t.Fatalf("clean zero queues are idle: %+v", q)
	}
	// But the public snapshot did not prove those engines idle, so the report
	// says so instead of claiming more than it knows.
	if len(arr(q["unproven_comfyui"])) != 2 {
		t.Fatalf("unreachable engines must be reported unproven: %+v", q)
	}
	if str(q["note"]) == "" {
		t.Fatalf("the report must point at the authoritative proof: %+v", q)
	}
}

func TestUpdateActiveUnreachableComfyBlocks(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.statusJSON = updateTestStatusFull("", `"spark01":{"reachable":false,"running":1,"pending":0},"spark02":{"reachable":false,"running":0,"pending":0}`, "")
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) || len(h.writes) != 0 {
		t.Fatalf("a running engine must block even when unreachable: %+v", res.report)
	}
}

// ---------------------------------------------------------------------------
// Staged-bytes ownership around cancellation and interrupted transactions.

func TestUpdateCancelBetweenPushesCleansOwnedStaging(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.setMacCurrent()
	// Walk away as the last spark01 file lands, before the device transaction
	// has been started.
	h.cancelAfterWrites = 3
	h.install()
	res := h.run(context.Background(), false, "")
	if !res.canceled {
		t.Fatalf("cancellation must be reported")
	}
	if len(h.selfs) != 0 {
		t.Fatalf("nothing may start remotely: %v", h.selfs)
	}
	// The bytes this task already wrote must not be left behind.
	if len(h.removes) == 0 {
		t.Fatalf("owned staging must be cleaned after a cancelled push: %v", h.writes)
	}
	for _, r := range h.removes {
		if strings.Contains(r, "backups") {
			t.Fatalf("cleanup must never touch backups: %s", r)
		}
	}
	// The transaction never started, so the second node was never touched.
	for _, w := range h.writes {
		if strings.HasPrefix(w, "spark02 ") {
			t.Fatalf("spark02 must not be written after cancellation: %s", w)
		}
	}
}

func TestUpdateUnobservedTransactionKeepsStages(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.setMacCurrent()
	// The session drops while the transaction runs: whether the device
	// finished is unknown, so the staged payload and the rollback helpers the
	// device recorded must survive.
	h.deviceRunErr = fail("ssh: connection reset by peer")
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) {
		t.Fatalf("an unobserved transaction must not report success")
	}
	if len(h.removes) != 0 {
		t.Fatalf("stages must survive an unobserved transaction: %v", h.removes)
	}
	recovery := obj(res.report["staged_recovery"])
	if !strings.Contains(str(recovery["head_stage"]), updateDeviceStageBase) || str(recovery["staged_cli"]) == "" || str(recovery["next"]) == "" {
		t.Fatalf("the report must give recovery paths: %+v", res.report)
	}
}

// ---------------------------------------------------------------------------
// Mac installer failure reporting.

func TestUpdateInstallerErrorReadsBothStreams(t *testing.T) {
	guidance := "target is an old Python zipapp; rerun with --legacy-processes-stopped to confirm"
	body, _ := json.Marshal(Object{"error": guidance})
	if got := updateInstallerError(CommandResult{Err: string(body) + "\n"}); got != guidance {
		t.Fatalf("stderr envelope must be parsed: %q", got)
	}
	if got := updateInstallerError(CommandResult{Out: string(body) + "\n"}); got != guidance {
		t.Fatalf("stdout envelope must still be parsed: %q", got)
	}
	// A trailing child line must not hide the envelope.
	if got := updateInstallerError(CommandResult{Err: "warning: something\n" + string(body) + "\n"}); got != guidance {
		t.Fatalf("the last JSON line must be parsed: %q", got)
	}
	if got := updateInstallerError(CommandResult{Err: "not json at all"}); got != "" {
		t.Fatalf("raw stderr must not leak into the report: %q", got)
	}
}

func TestUpdateMacInstallGuidanceSurvivesStderr(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeCurrent()
	h.macInstallFails = true
	h.macInstallErrJSON = `{"error":"target is an old Python zipapp or script. Drain and stop the legacy Python controller first, then rerun with --legacy-processes-stopped to confirm."}`
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) {
		t.Fatalf("a failed Mac install must not report success")
	}
	message := str(h.components(res.report)["mac"]["error"])
	if !strings.Contains(message, "--legacy-processes-stopped") {
		t.Fatalf("the installer's actionable guidance must survive: %q", message)
	}
	if len(h.macInstallArgs) != 1 {
		t.Fatalf("the installer must be invoked once: %v", h.macInstallArgs)
	}
	if strings.Contains(h.macInstallArgs[0], "--legacy-processes-stopped") {
		t.Fatalf("the override is a human decision and must never be auto-passed: %s", h.macInstallArgs[0])
	}
}

// ---------------------------------------------------------------------------
// Node bootstrap floor: v0.6.3 has no updater command of its own, but it does
// speak the node file protocol, so the verified new CLI carries it forward.

func TestUpdateNode063BootstrapsThroughStagedCLI(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.setMacCurrent()
	h.nodeVersion = "sparky 0.6.3 (commit " + updateTestCommit + ")"
	h.install()
	res := h.run(context.Background(), false, "")
	if !boolean(res.report["ok"]) {
		t.Fatalf("a v0.6.3 node must bootstrap, not be refused: %+v", res.report)
	}
	if len(h.writes) != 6 || len(h.digests) != 6 {
		t.Fatalf("both nodes must be staged: %v / %v", h.writes, h.digests)
	}
	if len(h.selfs) != 1 || !strings.Contains(h.selfs[0], updateDeviceStageBase) {
		t.Fatalf("only the staged CLI may run the transaction: %v", h.selfs)
	}
	// The installed v0.6.3 CLI is only ever asked for commands it actually
	// has: the node file protocol and the read-only status snapshot. Never
	// the updater command, which only the staged new CLI owns.
	for _, c := range h.calls {
		if !strings.Contains(c, "~/.local/bin/sparky --json node") {
			continue
		}
		if strings.Contains(c, "self-update") {
			t.Fatalf("the installed v0.6.3 CLI has no updater command: %s", c)
		}
		if !strings.Contains(c, "write-file") && !strings.Contains(c, "status") {
			t.Fatalf("only the old file protocol and status may reach the installed CLI: %s", c)
		}
	}
}

func TestUpdateNodeBelowBootstrapFloorIsRefused(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.nodeVersion = "sparky 0.6.2 (commit " + updateTestCommit + ")"
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) {
		t.Fatalf("a v0.6.2 node cannot be carried forward")
	}
	c := h.components(res.report)["spark01"]
	if str(c["status"]) != "predates" || !strings.Contains(str(c["error"]), updateNodeBootstrapVersion) {
		t.Fatalf("the refusal must name the bootstrap floor: %+v", c)
	}
	if len(h.writes) != 0 || len(h.selfs) != 0 {
		t.Fatalf("a refused node must never be pushed to: %v %v", h.writes, h.selfs)
	}
}

func TestUpdateNodeMissingBinaryIsMissing(t *testing.T) {
	h := newUpdateHarness(t)
	h.setNodeStale()
	h.nodeVersionCode = 127 // the shell found no CLI at that path
	h.install()
	res := h.run(context.Background(), false, "")
	if boolean(res.report["ok"]) {
		t.Fatalf("a node with no CLI must block")
	}
	c := h.components(res.report)["spark01"]
	if str(c["status"]) != "missing" || !strings.Contains(str(c["error"]), updateNodeCLIRemotePath) {
		t.Fatalf("a missing binary is its own state: %+v", c)
	}
	if len(h.writes) != 0 {
		t.Fatalf("no writes before the gate clears: %v", h.writes)
	}
}
