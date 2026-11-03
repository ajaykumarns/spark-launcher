package sparky

// Mac-side orchestration for "sparky update" (CLI-only updater). This file
// resolves and verifies one release, discovers the Mac CLI plus both Sparks
// read-only, stages verified payloads through the existing node file
// protocol, invokes the STAGED new Linux CLI's private device transaction on
// Spark1, installs the Mac CLI last through the verified bundled
// sparky-install, and reports one per-component result. Fencing, the safe
// controller restart, rollback, and model/config preservation belong to the
// device transaction: nothing here restarts a service, duplicates
// transaction logic, or touches models, inference, or the catalogue.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// updatePlatformClient is the only client platform the updater ships for.
	updatePlatformClient = "darwin/arm64"
	// updatePlatformNode is the platform of both Sparks.
	updatePlatformNode = "linux/arm64"
	// updateFirstUpdaterVersion is the first release whose staged payload
	// carries the private self-update transaction. The Mac CLI is never
	// self-updated from anything older: that machine is bootstrapped
	// manually once by installing this release.
	updateFirstUpdaterVersion = "0.6.4"
	// updateNodeBootstrapVersion is the oldest Spark CLI that "sparky update"
	// can still carry to the current release. Such a node has no updater
	// command of its own, but it does speak the node write-file protocol, so
	// the verified new CLI is staged through it and the STAGED CLI then runs
	// the device transaction. Only the target release metadata (a release
	// publishing no binary digests) is "predates the updater".
	updateNodeBootstrapVersion = "0.6.3"
	// updateNodeCLIRemotePath is the fixed installed CLI location on every
	// node, matching the device transaction's helper path. Tilde expansion
	// happens in the remote shell, exactly like the existing remote prefix.
	updateNodeCLIRemotePath = "~/.local/bin/sparky"
	// updateLocalFileBound bounds one local digest read; every payload push
	// is additionally bound by deploymentMaxPayload from the node protocol.
	updateLocalFileBound = 256 << 20
	// Timeouts for each remote interaction. None of these bound model work.
	updateProbeTimeout      = 60 * time.Second
	updatePushTimeout       = 300 * time.Second
	updateSelfUpdateTimeout = 1800 * time.Second
)

// updateVersionLinePattern matches exactly the "sparky --version" contract
// rendered by cliVersionLine (with or without the release commit suffix).
var updateVersionLinePattern = regexp.MustCompile(`^sparky ([0-9]+\.[0-9]+\.[0-9]+)(?: \(commit ([0-9a-fA-F]{40})\))?[ \t]*$`)

// Injectable effect seams. Every default calls the real implementation;
// tests substitute fakes. No test-only branch lives in the logic below.
var (
	updateResolveSeam   = updateResolveRelease
	updatePrepareSeam   = updatePrepareAsset
	updateExecSeam      = runCommand
	updateMacTargetSeam = func() (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fail("Could not locate the home directory for the Mac CLI path.")
		}
		return filepath.Join(home, ".local", "bin", "sparky"), nil
	}
	updateClientPlatformSeam = func() (string, string) { return runtime.GOOS, runtime.GOARCH }
)

// updateTargetState is one component's discovered state. Component labels
// are fixed: mac, spark01, spark02.
type updateTargetState struct {
	component         string
	version           string
	commit            string
	sha256            string
	controllerSHA     string
	status            string // current|stale_controller|pending|missing|predates|busy|unreachable|unknown_controller|updated|failed
	error             string
	backup            string
	controllerRestart *bool
}

// updateResult is one finished update attempt.
type updateResult struct {
	report    Object
	hasReport bool
	err       error // failure before any per-target state existed
	canceled  bool  // the caller's context was canceled mid-flight
}

// updateIsSelfUpdateArgs reports whether raw private node words begin with
// the self-update action. Main uses this to apply the self-update-only
// error-report exit rule without touching any other node command.
func updateIsSelfUpdateArgs(args []string) bool {
	return len(args) > 0 && args[0] == "self-update"
}

// updateParseVersionLine parses one "sparky --version" line.
func updateParseVersionLine(line string) (version, commit string, ok bool) {
	m := updateVersionLinePattern.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// updateVersionLess compares two numeric X.Y.Z versions. The caller has
// already accepted both with isReleaseVersion.
func updateVersionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range as {
		an, bn := strings.TrimLeft(as[i], "0"), strings.TrimLeft(bs[i], "0")
		if len(an) != len(bn) {
			return len(an) < len(bn)
		}
		if an != bn {
			return an < bn
		}
	}
	return false
}

// updateEnsureClientPlatform rejects clients other than the released Mac
// target with a clear message before any effect.
func updateEnsureClientPlatform() error {
	goos, goarch := updateClientPlatformSeam()
	if goos+"/"+goarch != updatePlatformClient {
		return fail("sparky update supports the macOS (Apple Silicon) CLI today; this client is %s/%s. Update this machine manually.", goos, goarch)
	}
	return nil
}

// updateValidateTag validates an explicit --tag before any I/O.
func updateValidateTag(tag string) error {
	if tag == "" {
		return nil
	}
	if !strings.HasPrefix(tag, "v") || !isReleaseVersion(strings.TrimPrefix(tag, "v")) {
		return usagef("Option --tag needs a release tag like vX.Y.Z%s", cliUsageSuffix)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Remote helpers. Every remote word is built from fixed constants, a
// config-validated host alias, or a config-validated absolute root, and goes
// through shellQuote/shellJoin: no caller-supplied path ever reaches a shell.

// updateHostsAndRoot reads the SSH aliases and root from the client config.
func updateHostsAndRoot(cfg clientConfig) (string, string, string, error) {
	head, err := remoteSafeHost(remoteConfigString(cfg, "admin_ssh_host"))
	if err != nil {
		return "", "", "", err
	}
	worker, err := remoteSafeHost(remoteConfigString(cfg, "worker_ssh_host"))
	if err != nil {
		return "", "", "", err
	}
	root, err := remoteRoot(cfg, "Set an absolute remote_root in the site file.")
	if err != nil {
		return "", "", "", err
	}
	return head, worker, root, nil
}

// updateRemoteNode runs one command string on a component ("spark01" direct,
// "spark02" nested through the head, mirroring cliDevices) with stdin and a
// bounded timeout. The command string is already quoted by its builder.
func updateRemoteNode(ctx context.Context, head, worker, component, command string, stdin []byte, timeout time.Duration) (remoteRunResult, error) {
	remote := command
	if component == "spark02" {
		remote = "ssh " + shellQuote(worker) + " " + shellQuote(command)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return remoteRun(runCtx, []string{"ssh", "-o", remoteConnectTimeoutArg, head, remote}, stdin, func([]byte) {})
}

// updateInstalledCLI invokes a node CLI's private protocol with the given
// action words and one JSON stdin object (nil for none). The executable part
// lives inside commandBase so the staged CLI can be addressed too.
func updateInstalledCLI(ctx context.Context, head, worker, component, commandBase string, words []string, payload Object, timeout time.Duration) (remoteRunResult, error) {
	var stdin []byte
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return remoteRunResult{}, fail("The transfer metadata could not be encoded.")
		}
		stdin = body
	}
	return updateRemoteNode(ctx, head, worker, component, commandBase+shellJoin(words), stdin, timeout)
}

// updateParseDigest extracts the leading lowercase hex sha256 from
// sha256sum output.
func updateParseDigest(out []byte) (string, bool) {
	field := strings.Fields(string(out))
	if len(field) == 0 || !isHexDigest(strings.ToLower(field[0])) {
		return "", false
	}
	return strings.ToLower(field[0]), true
}

// updateFileBytes reads one local file within the update bound, refusing
// symlinks, non-regular files, and oversized files before opening them.
func updateFileBytes(p string, bound int64) ([]byte, error) {
	info, err := os.Lstat(p)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fail("Could not read %s.", filepath.Base(p))
	}
	if info.Size() > bound {
		return nil, fail("%s exceeds the %d MiB update bound.", filepath.Base(p), bound>>20)
	}
	return os.ReadFile(p)
}

// updateBusyModelStates are the in-flight model states. updateIdleModelStates
// are the ACTUAL steady-state strings the head snapshot emits: a loaded but
// idle audio container or Comfy engine reports "running", and a model may be
// reported "idle", so neither is in-flight work. Anything outside both sets
// is unknown, and an unknown state is never treated as idle.
var (
	updateBusyModelStates = map[string]bool{StateQueued: true, StateStarting: true, StateStopping: true, "draining": true}
	updateIdleModelStates = map[string]bool{StateStopped: true, StateReady: true, StateFailed: true, "running": true, "idle": true}
)

// The three quiescence verdicts of one read-only head status snapshot.
const (
	updateActivityIdle    = "idle"
	updateActivityBusy    = "busy"
	updateActivityUnknown = "unknown"
)

// updateActivity is the quiescence verdict of one head status snapshot plus a
// human-readable reason. Unknown is never collapsed into idle: it blocks an
// apply and makes a check incomplete, exactly like an unreadable snapshot, and
// the authoritative device transaction still has to prove every unit inactive
// before it replaces any installed binary.
type updateActivity struct {
	state  string
	detail string
}

// updateUnprovenComfy lists nodes whose ComfyUI engine reported clean queues
// but no reachable engine. The public snapshot cannot show such an engine
// itself idle, so the updater reports the engine as unproven rather than
// claiming it idle: the device transaction's unit check is what proves the
// service inactive before any installed binary is replaced.
func updateUnprovenComfy(status Object) []string {
	comfy := obj(status["comfyui"])
	var unproven []string
	for _, node := range []string{"spark01", "spark02"} {
		state, present := comfy[node]
		if !present {
			continue
		}
		row := obj(state)
		if boolean(row["reachable"]) {
			continue
		}
		running, okRunning := updateActivityCount(row["running"])
		pending, okPending := updateActivityCount(row["pending"])
		if !okRunning || !okPending || running > 0 || pending > 0 {
			continue
		}
		if boolean(row["executing"]) || strings.EqualFold(strings.TrimSpace(str(row["phase"])), "executing") {
			continue
		}
		unproven = append(unproven, node)
	}
	return unproven
}

// updateQuiescenceReport describes the pre-check verdict for the user. It
// never claims more than the public snapshot proved: an engine that was clean
// but unreachable is named as unproven here, because the authoritative proof
// of its inactivity belongs to the device transaction.
func updateQuiescenceReport(statusKnown bool, activity updateActivity, status Object) Object {
	state := activity.state
	if !statusKnown {
		state, activity.detail = updateActivityUnknown, "the head controller state could not be read"
	}
	out := Object{"state": state, "detail": activity.detail}
	if unproven := updateUnprovenComfy(status); len(unproven) > 0 {
		out["unproven_comfyui"] = unproven
		if state == updateActivityIdle {
			out["note"] = "unreachable ComfyUI engines are proven inactive by the device transaction before any installed binary is replaced"
		}
	}
	return out
}

// updateActivityCount reads one non-negative integer count out of a snapshot
// field. A missing, fractional, negative, or otherwise unreadable value is
// not a count: the caller treats it as unknown rather than as zero.
func updateActivityCount(v any) (int, bool) {
	if v == nil || !integer(v) {
		return 0, false
	}
	n := num(v)
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, false
	}
	return int(n), true
}

// updateStatusActivity inspects the read-only head status snapshot and
// reports the cluster's quiescence. It reads the activity signals the head
// actually publishes — per-model lifecycle state, per-model active_requests,
// the controller admission queue, and the per-node ComfyUI queue and guard
// state — and never infers idleness from their absence.
//
// Two deliberate limits keep this a pre-check instead of a second head gate:
// a positively idle Comfy engine may be HTTP unreachable (the probe reports
// zero queues, and the device transaction proves the systemd unit inactive
// before any installed binary is replaced), and this helper never starts a
// probe, drains a queue, or takes a fence of its own.
func updateStatusActivity(status Object) updateActivity {
	if raw, present := status["admission"]; present {
		admission := obj(raw)
		if len(arr(admission["active"])) > 0 {
			return updateActivity{updateActivityBusy, "work is admitted on this cluster"}
		}
		if len(arr(admission["waiting"])) > 0 {
			return updateActivity{updateActivityBusy, "work is queued on the head controller"}
		}
		if transition, has := admission["config_transition"]; has {
			if _, ok := transition.(bool); !ok {
				return updateActivity{updateActivityUnknown, "the head controller transition state is unreadable"}
			}
			if boolean(transition) {
				return updateActivity{updateActivityBusy, "a controller transition is in progress"}
			}
		}
	}
	if raw, present := status["activity_known"]; present && !boolean(raw) {
		return updateActivity{updateActivityUnknown, "the head controller cannot read its own request activity"}
	}

	comfy := obj(status["comfyui"])
	comfyReachable := false
	for _, node := range []string{"spark01", "spark02"} {
		if boolean(obj(comfy[node])["reachable"]) {
			comfyReachable = true
		}
	}
	comfyLoaded := false
	models := obj(status["models"])
	for _, name := range keys(models) {
		state := obj(models[name])
		raw := str(state["state"])
		switch {
		case updateBusyModelStates[raw]:
			return updateActivity{updateActivityBusy, name + " is " + raw}
		case raw == "":
			if loaded, ok := state["loaded"].(bool); ok && loaded {
				return updateActivity{updateActivityUnknown, name + " reports an unknown state"}
			}
		case !updateIdleModelStates[raw]:
			return updateActivity{updateActivityUnknown, name + " reports an unknown state: " + raw}
		}
		if str(state["kind"]) == "comfyui" {
			if loaded, ok := state["loaded"].(bool); ok && loaded {
				comfyLoaded = true
			}
		}
		// A loaded but idle model is allowed; a model serving or queueing a
		// request is not. A model row without a readable request count cannot
		// prove its own idleness.
		count, ok := updateActivityCount(state["active_requests"])
		if !ok {
			return updateActivity{updateActivityUnknown, name + " does not report a readable request count"}
		}
		if count > 0 {
			return updateActivity{updateActivityBusy, name + " has " + strconv.Itoa(count) + " active request(s)"}
		}
	}
	if comfyLoaded && !comfyReachable {
		return updateActivity{updateActivityUnknown, "the ComfyUI model is loaded but no engine reports itself reachable"}
	}

	for _, node := range []string{"spark01", "spark02"} {
		row, present := comfy[node]
		if !present {
			// The head always probes both Sparks; a snapshot that says nothing
			// about a node's engine cannot prove that node idle either.
			return updateActivity{updateActivityUnknown, "the ComfyUI state on " + node + " is not reported"}
		}
		state := obj(row)
		if _, ok := state["reachable"].(bool); !ok {
			return updateActivity{updateActivityUnknown, "the ComfyUI reachability on " + node + " is unreadable"}
		}
		running, okRunning := updateActivityCount(state["running"])
		pending, okPending := updateActivityCount(state["pending"])
		if !okRunning || !okPending {
			return updateActivity{updateActivityUnknown, "the ComfyUI queue on " + node + " is unreadable"}
		}
		if running > 0 {
			return updateActivity{updateActivityBusy, "ComfyUI on " + node + " is running " + strconv.Itoa(running) + " job(s)"}
		}
		if pending > 0 {
			return updateActivity{updateActivityBusy, "ComfyUI on " + node + " has " + strconv.Itoa(pending) + " queued job(s)"}
		}
		if guarded, ok := state["guarded"].(bool); ok && guarded {
			if _, ok := state["executing"].(bool); !ok {
				return updateActivity{updateActivityUnknown, "the ComfyUI guard state on " + node + " is unreadable"}
			}
		}
		if raw, has := state["executing"]; has {
			executing, ok := raw.(bool)
			if !ok {
				return updateActivity{updateActivityUnknown, "the ComfyUI guard state on " + node + " is unreadable"}
			}
			if executing {
				return updateActivity{updateActivityBusy, "ComfyUI on " + node + " is executing a job"}
			}
		}
		if raw, has := state["phase"]; has {
			phase, ok := raw.(string)
			if !ok {
				return updateActivity{updateActivityUnknown, "the ComfyUI guard state on " + node + " is unreadable"}
			}
			if strings.EqualFold(strings.TrimSpace(phase), "executing") {
				return updateActivity{updateActivityBusy, "ComfyUI on " + node + " is executing a job"}
			}
		}
	}
	return updateActivity{updateActivityIdle, ""}
}

// firstVersionLine extracts the first plausible version line from a
// --version stream.
func firstVersionLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[0])
}

// ---------------------------------------------------------------------------
// Discovery (read-only): versions, disk digests, and the running head
// controller digest. A current CLI version never hides a stale node or a
// stale running controller.

func updateDiscoverMac(ctx context.Context, t *updateTargetState, expectedSHA string, progress func(string)) {
	progress("Checking the Mac CLI...")
	target, err := updateMacTargetSeam()
	if err != nil {
		t.status, t.error = "unknown", err.Error()
		return
	}
	if _, err := os.Lstat(target); err != nil {
		t.status = "missing"
		t.error = "No Sparky CLI is installed at " + target + ". Install this machine manually once before using sparky update."
		return
	}
	res, err := updateExecSeam(ctx, []string{target, "--version"}, nil)
	if err != nil {
		t.status, t.error = "unknown", "The installed Mac CLI could not be checked."
		return
	}
	version, commit, ok := updateParseVersionLine(firstVersionLine(res.Out))
	if res.Code != 0 || !ok {
		t.status = "predates"
		t.error = "The Mac CLI at " + target + " predates the sparky updater and must be updated manually once before sparky update can be used."
		return
	}
	t.version, t.commit = version, commit
	raw, err := updateFileBytes(target, updateLocalFileBound)
	if err != nil {
		t.status, t.error = "unknown", "The installed Mac CLI could not be digested."
		return
	}
	t.sha256 = sha256Hex(raw)
	switch {
	case updateVersionLess(version, updateFirstUpdaterVersion):
		t.status = "predates"
		t.error = fmt.Sprintf("The Mac CLI version %s predates the sparky updater (v%s). Update this machine manually once.", version, updateFirstUpdaterVersion)
	case t.sha256 == expectedSHA:
		t.status = "current"
	default:
		t.status = "pending"
	}
}

// updateDiscoverNode reads the installed CLI version and its disk digest,
// and for the head also the running controller executable digest. The four
// ways a node can fail to answer are reported apart, because each needs a
// different operator action: no binary at all, an unreachable device, a
// version this updater cannot read, and a version it cannot carry forward.
func updateDiscoverNode(ctx context.Context, head, worker string, t *updateTargetState, expectedSHA string, progress func(string)) {
	progress("Checking " + t.component + "...")
	ver, err := updateRemoteNode(ctx, head, worker, t.component, updateNodeCLIRemotePath+" --version", nil, updateProbeTimeout)
	switch {
	case ver.code == 255:
		t.status, t.error = "unreachable", "The device could not be reached."
		return
	case ver.code == 127:
		t.status, t.error = "missing", t.component+" has no Sparky CLI at "+updateNodeCLIRemotePath+". Install this release there manually once before sparky update can carry it forward."
		return
	case err != nil:
		t.status, t.error = "unknown", "The installed CLI on "+t.component+" could not be checked."
		return
	}
	version, commit, ok := updateParseVersionLine(firstVersionLine(string(ver.stdout)))
	if ver.code != 0 || !ok {
		// An unreadable version line is the pre-updater zipapp output. It is
		// never assumed current: the operator installs once by hand.
		t.status = "predates"
		t.error = t.component + " did not report a readable sparky version, so it predates the sparky updater and must be updated manually once (see README)."
		return
	}
	t.version, t.commit = version, commit
	dig, err := updateRemoteNode(ctx, head, worker, t.component, "sha256sum "+updateNodeCLIRemotePath, nil, updateProbeTimeout)
	sha, ok := updateParseDigest(dig.stdout)
	if err != nil || dig.code != 0 || !ok {
		t.status, t.error = "unknown", "The installed CLI on "+t.component+" could not be digested."
		return
	}
	t.sha256 = sha
	switch {
	case updateVersionLess(version, updateNodeBootstrapVersion):
		t.status = "predates"
		t.error = fmt.Sprintf("%s version %s is older than v%s, the oldest Spark CLI sparky update can stage forward. Update that device manually once.", t.component, version, updateNodeBootstrapVersion)
	case t.sha256 == expectedSHA:
		t.status = "current"
	default:
		t.status = "pending"
	}
	if t.status != "current" {
		return
	}
	// Head only: a same-version disk CLI must not hide a stale running
	// controller process.
	if t.component == "spark01" {
		pid, err := updateControllerMainPID(ctx, head, worker, t.component)
		if err != nil {
			t.status, t.error = "unknown_controller", "The head controller state could not be determined."
			return
		}
		if pid > 0 {
			exe, err := updateRemoteNode(ctx, head, worker, t.component, "sha256sum /proc/"+strconv.Itoa(pid)+"/exe", nil, updateProbeTimeout)
			digest, ok := updateParseDigest(exe.stdout)
			if err != nil || exe.code != 0 || !ok {
				t.status, t.error = "unknown_controller", "The running head controller executable could not be digested."
				return
			}
			t.controllerSHA = digest
			if digest != t.sha256 {
				t.status = "stale_controller"
			}
		}
	}
}

// updateControllerMainPID reads the head controller unit's MainPID through
// the safe read-only systemctl surface. An inactive unit reports pid 0.
func updateControllerMainPID(ctx context.Context, head, worker, component string) (int, error) {
	res, err := updateRemoteNode(ctx, head, worker, component, shellJoin([]string{"systemctl", "show", updateDeviceControllerUnit, "--property", "MainPID", "--value"}), nil, updateProbeTimeout)
	if err != nil || res.code != 0 {
		return 0, fail("systemctl MainPID read failed")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(res.stdout)))
	if err != nil || pid < 0 {
		return 0, fail("systemctl MainPID is not a number")
	}
	return pid, nil
}

// ---------------------------------------------------------------------------
// Push: three own-payload files per node through the existing node file
// protocol (write-file creates the private parent tree itself), each
// confirmed independently on disk with sha256sum.

func updatePushStagedFile(ctx context.Context, head, worker, component, installedCLI, pathName, mode, wantSHA string, data []byte) error {
	if int64(len(data)) > deploymentMaxPayload {
		return fail("%s exceeds the %d MiB transfer limit of the node file protocol.", filepath.Base(pathName), deploymentMaxPayload>>20)
	}
	meta := Object{"path": pathName, "mode": mode, "data_base64": base64.StdEncoding.EncodeToString(data), "expected_missing": true}
	res, err := updateInstalledCLI(ctx, head, worker, component, installedCLI, []string{"write-file"}, meta, updatePushTimeout)
	if err != nil {
		return fail("The staged payload for %s could not be transferred to %s.", filepath.Base(pathName), component)
	}
	if res.code != 0 {
		return fail("The staged payload for %s was refused on %s.", filepath.Base(pathName), component)
	}
	// The receiver echoes the digest it wrote. It is a cheap first check, and
	// never the proof: the file is hashed again below.
	if reply, derr := decode(res.stdout); derr == nil {
		if got := str(reply["sha256"]); got != "" && !strings.EqualFold(got, wantSHA) {
			return fail("The staged payload digest on %s does not match the verified release.", component)
		}
	}
	// Independent confirmation of what is actually on disk. The path is
	// shell-quoted so it can never be reinterpreted as shell words, and the
	// digest parser accepts only a leading hex sha256. "inspect" is
	// deliberately NOT used here: it returns the whole file base64-encoded,
	// which would move every megabyte of the payload a second time.
	sum, err := updateRemoteNode(ctx, head, worker, component, "sha256sum -- "+shellQuote(pathName), nil, updatePushTimeout)
	if err != nil || sum.code != 0 {
		return fail("The staged payload on %s could not be confirmed.", component)
	}
	got, ok := updateParseDigest(sum.stdout)
	if !ok || got != strings.ToLower(wantSHA) {
		return fail("The staged payload digest on %s does not match the verified release.", component)
	}
	return nil
}

// updatePushNode stages the three verified Linux files under one node's
// private stage directory and confirms every digest on disk.
func updatePushNode(ctx context.Context, head, worker, component, installedCLI, stageDir string, linux *updatePrepared) error {
	buildinfo := filepath.Join(linux.Dir, "BUILDINFO.json")
	rawBuild, err := os.ReadFile(buildinfo)
	if err != nil {
		return fail("The verified release is missing its build identity.")
	}
	files := []struct {
		name string
		mode string
		sha  string
		data []byte
	}{
		{"bin/sparky", "755", linux.CLISHA, nil},
		{"bin/sparky-install", "755", linux.InstallerSHA, nil},
		{"BUILDINFO.json", "644", sha256Hex(rawBuild), rawBuild},
	}
	for i := range files {
		if files[i].data == nil {
			var src string
			switch files[i].name {
			case "bin/sparky":
				src = linux.CLI
			default:
				src = linux.Installer
			}
			data, err := updateFileBytes(src, updateLocalFileBound)
			if err != nil {
				return err
			}
			if sha256Hex(data) != files[i].sha {
				return fail("The staged %s does not match the verified release digest.", filepath.Base(files[i].name))
			}
			files[i].data = data
		}
		target := path.Join(stageDir, filepath.ToSlash(files[i].name))
		if err := updatePushStagedFile(ctx, head, worker, component, installedCLI, target, files[i].mode, files[i].sha, files[i].data); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Engine.

// updateExecute runs the full check or apply flow. It never mutates anything
// unless every gate has passed: platform, reachability, known states,
// version ordering, quiescence, and digest availability.
func updateExecute(ctx context.Context, cfg clientConfig, check bool, tag string, progress func(string)) updateResult {
	var res updateResult
	canceled := func() bool {
		if ctx.Err() != nil {
			res.canceled = true
			return true
		}
		return false
	}
	action := "update"
	if check {
		action = "check"
	}
	if progress == nil {
		progress = func(string) {}
	}

	head, worker, root, err := updateHostsAndRoot(cfg)
	if err != nil {
		res.err = err
		return res
	}

	progress("Checking the verified release...")
	rel, err := updateResolveSeam(ctx, tag)
	if err != nil {
		res.err = err
		return res
	}
	// The resolver only ever returns releases whose per-binary digests are
	// complete; an older release fails there with the "predates the
	// updater" guidance, so both expected digests are present here.
	macExpected := rel.BinarySHA256[updatePlatformClient]["sparky"]
	nodeExpected := rel.BinarySHA256[updatePlatformNode]["sparky"]
	if macExpected == "" || nodeExpected == "" {
		res.err = fail("Release %s predates the sparky updater: it carries no binary digests. Update manually once; the first updater release is v%s.", rel.Tag, updateFirstUpdaterVersion)
		return res
	}

	targets := map[string]*updateTargetState{
		"mac":     {component: "mac"},
		"spark01": {component: "spark01"},
		"spark02": {component: "spark02"},
	}

	updateDiscoverMac(ctx, targets["mac"], macExpected, progress)
	if canceled() {
		return finishUpdate(res, action, rel, targets, nil)
	}
	updateDiscoverNode(ctx, head, worker, targets["spark01"], nodeExpected, progress)
	if canceled() {
		return finishUpdate(res, action, rel, targets, nil)
	}
	updateDiscoverNode(ctx, head, worker, targets["spark02"], nodeExpected, progress)
	if canceled() {
		return finishUpdate(res, action, rel, targets, nil)
	}

	// Head controller quiescence (read-only). An unreadable snapshot, or one
	// whose activity cannot be understood, is an unknown state: apply blocks
	// and a check reports itself incomplete. Unknown is never reported as
	// idle; the device transaction stays the authoritative idle proof.
	activity := updateActivity{updateActivityUnknown, "the head controller state could not be read"}
	statusKnown := false
	var snapshot Object
	if st, err := cliRemoteStatus(ctx, cfg); err == nil {
		snapshot, statusKnown = st, true
		activity = updateStatusActivity(st)
	}
	if canceled() {
		return finishUpdate(res, action, rel, targets, nil)
	}
	busy := statusKnown && activity.state == updateActivityBusy
	if !statusKnown && targets["spark01"].status == "current" {
		targets["spark01"].status = "unknown_controller"
		targets["spark01"].error = "The head controller state could not be read."
	}
	if busy && (targets["spark01"].status == "current" || targets["spark01"].status == "stale_controller") {
		targets["spark01"].status = "busy"
		targets["spark01"].error = "Active work is running: " + activity.detail + ". Nothing was installed."
	}
	if busy && targets["spark02"].status == "current" {
		targets["spark02"].status = "busy"
		targets["spark02"].error = "Active work is running: " + activity.detail + ". Nothing was installed."
	}
	// withQuiescence attaches the pre-check verdict to a report so the user
	// sees what the public snapshot actually proved, including engines whose
	// inactivity only the device transaction can prove.
	withQuiescence := func(report updateResult) updateResult {
		report.report["quiescence"] = updateQuiescenceReport(statusKnown, activity, snapshot)
		return report
	}

	// Version gate: refuse to select a release older than any known target,
	// explicit --tag included, before any write.
	for _, t := range []*updateTargetState{targets["mac"], targets["spark01"], targets["spark02"]} {
		if t.version != "" && isReleaseVersion(t.version) && updateVersionLess(rel.Version, t.version) {
			res.err = fail("Refusing %s: %s already runs %s, which is newer than the selected release %s.", action, t.component, t.version, rel.Version)
			return res
		}
	}

	blockers := []string{}
	if !statusKnown {
		blockers = append(blockers, "The head controller state could not be read, so the update is blocked and the check is incomplete.")
	} else if activity.state == updateActivityUnknown {
		blockers = append(blockers, "The head controller state is not understood: "+activity.detail+". Nothing was installed, and the check is incomplete.")
	}
	for _, name := range []string{"mac", "spark01", "spark02"} {
		switch targets[name].status {
		case "unreachable", "missing", "predates", "unknown", "unknown_controller":
			blockers = append(blockers, targets[name].component+": "+targets[name].error)
		case "busy":
			if !check {
				blockers = append(blockers, targets[name].component+": "+targets[name].error)
			}
		}
	}
	if busy && !check && len(blockers) == 0 {
		blockers = append(blockers, "Active work is running: "+activity.detail+". Nothing was installed.")
	}
	if len(blockers) > 0 {
		report := finishUpdate(res, action, rel, targets, nil)
		report.report["ok"] = false
		report.report["blocked"] = strings.Join(blockers, "; ")
		return withQuiescence(report)
	}

	if check {
		report := finishUpdate(res, action, rel, targets, nil)
		report.report["ok"] = true
		return withQuiescence(report)
	}

	// Nothing to do unless some component differs from the verified release
	// (or the head runs a stale controller process).
	nodesNeedWork := targets["spark01"].status == "pending" || targets["spark01"].status == "stale_controller" ||
		targets["spark02"].status == "pending" || targets["spark02"].status == "stale_controller"
	macNeedsWork := targets["mac"].status == "pending"
	if !nodesNeedWork && !macNeedsWork {
		report := finishUpdate(res, action, rel, targets, nil)
		report.report["ok"] = true
		return withQuiescence(report)
	}

	// Private local staging (0700) holding the verified platform trees.
	base, err := os.MkdirTemp("", "sparky-update-")
	if err != nil {
		res.err = fail("Could not create a private staging directory.")
		return res
	}
	defer os.RemoveAll(base)
	if err := os.Chmod(base, 0o700); err != nil {
		res.err = fail("Could not protect the staging directory.")
		return res
	}

	var deviceReport Object
	if nodesNeedWork {
		progress("Staging verified binaries...")
		linux, err := updatePrepareSeam(ctx, rel, updatePlatformNode, base)
		if err != nil {
			res.err = err
			return res
		}
		if canceled() {
			return finishUpdate(res, action, rel, targets, nil)
		}
		suffix, err := updateStageSuffix()
		if err != nil {
			res.err = err
			return res
		}
		// The device transaction requires distinct head and worker stage
		// directories, both under the private stage base.
		headStage := path.Join(root, updateDeviceStageBase, rel.Tag+"-"+suffix+"-head")
		workerStage := path.Join(root, updateDeviceStageBase, rel.Tag+"-"+suffix+"-worker")
		installedCLI := remoteSparkyPrefix

		progress("Transferring the verified payload to both Sparks...")
		for _, push := range []struct {
			component string
			stage     string
		}{
			{"spark01", headStage},
			{"spark02", workerStage},
		} {
			if err := updatePushNode(ctx, head, worker, push.component, installedCLI, push.stage, linux); err != nil {
				targets[push.component].status, targets[push.component].error = "failed", err.Error()
				markPushedFailed(targets, push.component)
				// Remove whatever this task wrote (idempotent, own-payload
				// only) before returning the partial failure report.
				updateCleanupStaged(head, worker, linux, headStage, workerStage)
				report := finishUpdate(res, action, rel, targets, nil)
				report.report["ok"] = false
				return report
			}
			if canceled() {
				// Cancelled after some pushes but BEFORE the device
				// transaction started: nothing remote is mid-transaction, so
				// the only bytes that exist are the ones this task wrote.
				// Remove exactly those (own-payload digests only, on the
				// detached cleanup context) instead of leaving them behind.
				// Stage directories themselves belong to the device-side
				// transaction and are left for it to reap.
				updateCleanupStaged(head, worker, linux, headStage, workerStage)
				return finishUpdate(res, action, rel, targets, nil)
			}
		}

		progress("Updating spark01 and spark02...")
		spec := Object{
			"tag":              rel.Tag,
			"version":          rel.Version,
			"commit":           rel.Commit,
			"head_stage":       headStage,
			"worker_stage":     workerStage,
			"cli_sha256":       linux.CLISHA,
			"installer_sha256": linux.InstallerSHA,
		}
		// The OLD installed CLIs never receive this spec: only the STAGED
		// verified new CLI on the head runs the device transaction. The
		// spec travels on stdin only, never on argv.
		stagedBase := shellQuote(path.Join(headStage, "bin", "sparky")) + " --json node "
		run, runErr := updateInstalledCLI(ctx, head, worker, "spark01", stagedBase, []string{"self-update", "--root", root, "--payload", "-"}, spec, updateSelfUpdateTimeout)
		deviceReport = nil
		if len(run.stdout) > 0 {
			if parsed, derr := decode(run.stdout); derr == nil {
				deviceReport = parsed
			}
		}
		if updateDeviceSucceeded(deviceReport, run.code, runErr) && !updateDeviceConfirmsRelease(deviceReport, rel, linux.CLISHA) {
			deviceReport["outcome"] = deviceReport["status"]
			deviceReport["status"] = "error"
			deviceReport["error"] = "The device transaction did not confirm the selected release on both nodes and the running controller."
		}
		applyDeviceReport(targets, deviceReport, run.code, runErr)

		// Whether the remote transaction finished is UNKNOWN when the session
		// dropped, when no report came back, or when the caller was cancelled
		// while it ran. Then the staged payload and whatever rollback helpers
		// the device recorded must survive: deleting them would destroy the
		// only way back, so cleanup is skipped and the recovery paths are
		// reported instead.
		completionUnknown := runErr != nil || deviceReport == nil || ctx.Err() != nil
		safeToClean := !completionUnknown && (updateDeviceSucceeded(deviceReport, run.code, runErr) ||
			(boolean(obj(deviceReport["rollback"])["complete"]) && boolean(obj(deviceReport["fence"])["restored"])))
		if safeToClean {
			// Best-effort removal of only the files this task wrote; a missing
			// file is an idempotent success and a changed file is refused by
			// the receiver. Backups recorded by the device are never touched.
			// Cancellation must not skip cleanup, so this uses a fresh context.
			updateCleanupStaged(head, worker, linux, headStage, workerStage)
		}
		if canceled() {
			report := finishUpdate(res, action, rel, targets, deviceReport)
			if !safeToClean {
				report.report["ok"] = false
				updateReportStagedRecovery(report.report, root, headStage, workerStage)
			}
			return report
		}
		if !updateDeviceSucceeded(deviceReport, run.code, runErr) {
			report := finishUpdate(res, action, rel, targets, deviceReport)
			report.report["ok"] = false
			if !safeToClean {
				updateReportStagedRecovery(report.report, root, headStage, workerStage)
			}
			return report
		}
	}

	if macNeedsWork {
		progress("Installing the Mac CLI...")
		macPrepared, err := updatePrepareSeam(ctx, rel, updatePlatformClient, base)
		if err != nil {
			res.err = err
			return res
		}
		if canceled() {
			return finishUpdate(res, action, rel, targets, deviceReport)
		}
		target, err := updateMacTargetSeam()
		if err != nil {
			res.err = err
			return res
		}
		if err := updateInstallMac(ctx, targets["mac"], macPrepared, target, rel.Version, rel.Commit); err != nil && ctx.Err() != nil {
			res.canceled = true
			return finishUpdate(res, action, rel, targets, deviceReport)
		}
	}

	report := withQuiescence(finishUpdate(res, action, rel, targets, deviceReport))
	report.report["ok"] = updateReportOK(targets)
	return report
}

// markPushedFailed marks the peer component failed too when one push cannot
// complete: the transaction needs both stages and must not run half-staged.
func markPushedFailed(targets map[string]*updateTargetState, failed string) {
	peer := "spark02"
	if failed == "spark02" {
		peer = "spark01"
	}
	if targets[peer].status == "pending" || targets[peer].status == "stale_controller" {
		targets[peer].status = "failed"
		targets[peer].error = "The paired transfer did not complete, so the device update did not run."
	}
}

// updateStageSuffix builds a per-task random suffix for the stage
// directories using the system CSPRNG.
func updateStageSuffix() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", fail("Could not generate a staging directory name.")
	}
	return fmt.Sprintf("%x", buf), nil
}

// updateDeviceSucceeded reports whether the staged device transaction
// completed. A report without a terminal success status or a nonzero remote
// exit blocks the Mac install.
func updateDeviceSucceeded(report Object, code int, runErr error) bool {
	if runErr != nil {
		return false
	}
	if code != 0 {
		return false
	}
	if report == nil {
		return false
	}
	switch str(report["status"]) {
	case "updated", "unchanged":
		return true
	}
	return false
}

// A success label alone is not proof of a completed cluster update.
func updateDeviceConfirmsRelease(report Object, rel *updateRelease, digest string) bool {
	identity := obj(report["release"])
	if str(identity["version"]) != rel.Version || str(identity["commit"]) != rel.Commit {
		return false
	}
	for _, name := range []string{"spark01", "spark02"} {
		target := obj(obj(report["targets"])[name])
		if str(target["version"]) != rel.Version || str(target["sha256"]) != digest {
			return false
		}
	}
	return str(obj(report["controller"])["running_sha256"]) == digest
}

// applyDeviceReport folds the device transaction report into the two node
// components, preserving per-target digests, backups, and controller
// restart status for the partial report.
func applyDeviceReport(targets map[string]*updateTargetState, report Object, code int, runErr error) {
	if !updateDeviceSucceeded(report, code, runErr) {
		for _, name := range []string{"spark01", "spark02"} {
			t := targets[name]
			node := obj(obj(report["targets"])[name])
			t.backup = str(node["backup"])
			t.sha256 = str(node["sha256"])
			if version := str(node["version"]); version != "" {
				t.version = version
				t.commit = str(obj(report["release"])["commit"])
			}
			t.status = "failed"
			switch {
			case runErr != nil:
				t.error = "The device self-update could not be run."
			case report == nil:
				t.error = "The device self-update returned no report."
			default:
				t.error = "The device self-update reported " + str(report["status"]) + "."
				if e := str(report["error"]); e != "" {
					t.error += ": " + e
				}
			}
		}
		return
	}
	restart := obj(report["controller"])
	for _, name := range []string{"spark01", "spark02"} {
		t := targets[name]
		if _, has := restart["restarted"]; has && name == "spark01" {
			value := boolean(restart["restarted"])
			t.controllerRestart = &value
		}
		node := obj(obj(report["targets"])[name])
		if boolean(node["changed"]) {
			t.status = "updated"
		} else {
			t.status = "current"
		}
		if s := str(node["sha256"]); s != "" {
			t.sha256 = s
		}
		if v := str(node["version"]); v != "" {
			t.version = v
			t.commit = str(obj(report["release"])["commit"])
		}
		if b := str(node["backup"]); b != "" {
			t.backup = b
		}
	}
}

// updateReportStagedRecovery names the preserved task-owned stages when a
// device transaction's completion could not be observed. An interrupted
// transaction may still need those bytes and the rollback helpers the device
// recorded, so nothing here deletes them: the report says where they are and
// how the next run picks them up.
func updateReportStagedRecovery(report Object, root, headStage, workerStage string) {
	report["staged_recovery"] = Object{
		"head_stage":   headStage,
		"worker_stage": workerStage,
		"staged_cli":   path.Join(headStage, "bin", "sparky"),
		"stage_base":   path.Join(root, updateDeviceStageBase),
		"next": "Re-run \"sparky update\" once both Sparks are reachable: it re-verifies the same release " +
			"digests and re-stages safely. The preserved stages and any rollback helpers the device recorded " +
			"are left for that run, and installed-binary backups are never deleted from here.",
	}
}

// updateCleanupStaged removes exactly the task-owned staged files (never a
// backup) through the own-payload-only remove. Cancellation must not skip
// cleanup, so the calls run on a short detached background context.
func updateCleanupStaged(head, worker string, linux *updatePrepared, headStage, workerStage string) {
	cleanCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	buildinfo := filepath.Join(linux.Dir, "BUILDINFO.json")
	buildSHA := ""
	if raw, err := os.ReadFile(buildinfo); err == nil {
		buildSHA = sha256Hex(raw)
	}
	items := []struct {
		name string
		src  string
		sha  string
	}{
		{"bin/sparky", linux.CLI, linux.CLISHA},
		{"bin/sparky-install", linux.Installer, linux.InstallerSHA},
		{"BUILDINFO.json", buildinfo, buildSHA},
	}
	stages := map[string]string{"spark01": headStage, "spark02": workerStage}
	for _, component := range []string{"spark01", "spark02"} {
		stage := stages[component]
		for _, it := range items {
			if it.sha == "" {
				continue
			}
			target := path.Join(stage, filepath.ToSlash(it.name))
			_, _ = updateInstalledCLI(cleanCtx, head, worker, component, remoteSparkyPrefix, []string{"write-file"},
				Object{"operation": "remove", "path": target, "expected_sha256": it.sha}, 30*time.Second)
		}
	}
}

// updateInstallerError pulls the actionable error text out of one Mac
// installer run. The installer prints its success object on stdout and its
// error envelope {"error":"..."} on stderr, so a failed run usually has an
// empty stdout and the guidance - including the --legacy-processes-stopped
// instruction - lives only in that stderr line. Both streams are already
// size-bounded by the exec seam; anything that is not a JSON envelope yields
// no message rather than leaking raw output into the report.
func updateInstallerError(res CommandResult) string {
	stderr := strings.TrimSpace(res.Err)
	candidates := []string{strings.TrimSpace(res.Out), stderr}
	if line := lastNonEmptyLine(stderr); line != "" {
		candidates = append(candidates, line)
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if body, derr := decode([]byte(candidate)); derr == nil && body != nil {
			if message := strings.TrimSpace(str(body["error"])); message != "" {
				return message
			}
		}
	}
	return ""
}

// lastNonEmptyLine is the final line of s that carries any text.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// updateInstallMac installs the Mac CLI last through the verified bundled
// installer and confirms the returned hash, the backup, the installed
// digest, and the installed version. The legacy-processes override is a human
// decision about draining the old Python controller, so it is never passed
// automatically: the installer's own guidance is reported instead.
func updateInstallMac(ctx context.Context, t *updateTargetState, prepared *updatePrepared, target, expectedVersion, expectedCommit string) error {
	res, err := updateExecSeam(ctx, []string{prepared.Installer, "--source", prepared.CLI, "--target", target}, nil)
	var result Object
	if res.Out != "" {
		result, _ = decode([]byte(res.Out))
	}
	message := updateInstallerError(res)
	if err != nil || res.Code != 0 {
		if message == "" {
			message = "the Mac installer reported a failure"
		}
		t.status, t.error = "failed", "The Mac CLI could not be installed: "+message
		return fail("Mac install failed.")
	}
	if result == nil || str(result["sha256"]) != prepared.CLISHA || str(result["installed"]) != target {
		t.status, t.error = "failed", "The Mac installer result did not match the verified payload."
		return fail("Mac install verification failed.")
	}
	if backup := str(result["backup"]); backup != "" {
		t.backup = backup
	}
	raw, derr := updateFileBytes(target, updateLocalFileBound)
	if derr != nil || sha256Hex(raw) != prepared.CLISHA {
		t.status, t.error = "failed", "The installed Mac CLI digest does not match the verified payload."
		return fail("Mac install digest failed.")
	}
	check, cerr := updateExecSeam(ctx, []string{target, "--version"}, nil)
	version, commit, ok := updateParseVersionLine(firstVersionLine(check.Out))
	if cerr != nil || check.Code != 0 || !ok || version != expectedVersion || commit != expectedCommit {
		t.status, t.error = "failed", "The installed Mac CLI did not report the expected version."
		return fail("Mac install version check failed.")
	}
	t.status, t.version, t.commit = "updated", version, commit
	return nil
}

// updateReportOK is true when every component ended current or updated.
func updateReportOK(targets map[string]*updateTargetState) bool {
	for _, name := range []string{"mac", "spark01", "spark02"} {
		switch targets[name].status {
		case "current", "updated":
		default:
			return false
		}
	}
	return true
}

// finishUpdate renders the per-component report.
func finishUpdate(res updateResult, action string, rel *updateRelease, targets map[string]*updateTargetState, deviceReport Object) updateResult {
	components := []any{}
	for _, name := range []string{"mac", "spark01", "spark02"} {
		t := targets[name]
		o := Object{"component": t.component, "status": t.status}
		if t.version != "" {
			o["version"] = t.version
		}
		if t.sha256 != "" {
			o["sha256"] = t.sha256
		}
		if t.controllerSHA != "" {
			o["controller_sha256"] = t.controllerSHA
		}
		if t.controllerRestart != nil {
			o["controller_restarted"] = *t.controllerRestart
		}
		if t.backup != "" {
			o["backup"] = t.backup
		}
		if t.error != "" {
			o["error"] = t.error
		}
		components = append(components, o)
	}
	report := Object{
		"action":     action,
		"tag":        rel.Tag,
		"version":    rel.Version,
		"commit":     rel.Commit,
		"components": components,
	}
	if deviceReport != nil {
		report["device_report"] = deviceReport
	}
	res.report = report
	res.hasReport = true
	return res
}
