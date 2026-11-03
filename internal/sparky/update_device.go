// Head-side device self-update transaction.
//
// The Mac integration worker first pushes the VERIFIED release payload to both
// nodes through the existing node write-file protocol, then invokes the private
// action "node self-update --root ROOT --payload -" on Spark1 from the staged
// new Linux CLI. This file owns the head-side half of that transaction:
//
//   - strict payload grammar (tag, version, commit, staging dirs, digests);
//   - independent re-verification of every staged artifact on BOTH nodes
//     (digest, BUILDINFO identity, staged "sparky --version") before anything
//     installed is replaced;
//   - cluster quiescence guards read only from the existing controller,
//     runtime and durable native lifecycle helpers, refusing to touch anything
//     busy, unknown, or owned by somebody else;
//   - a nonblocking process lock, an owned admission fence, atomic installs
//     through the VERIFIED staged sparky-install, and one narrowly scoped
//     restart of the Spark1 head controller when and only when the running
//     executable actually differs;
//   - verified rollback of every binary this transaction changed, or an honest
//     partial report when a rollback cannot be proved.
//
// It never calls InstallHead, never installs guards, config, units or policy,
// never starts, stops, enables, disables, reloads or cancels a model, service,
// worker controller or ComfyUI job, and never runs Python, curl or any
// caller-supplied command: every command is assembled from pinned constants
// plus payload paths that were validated to live under <root>/.sparky-update.
package sparky

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// updateDeviceStageBase is the single private directory under the cluster
	// root that may hold staged release payloads.
	updateDeviceStageBase = ".sparky-update"
	// updateDeviceLockName is the nonblocking update lock inside the stage base.
	updateDeviceLockName = "update.lock"
	// updateDeviceControllerUnit is the head controller unit. Only this unit is
	// ever queried or restarted, and only on Spark1.
	updateDeviceControllerUnit = "sparky-controller.service"
	// updateDeviceConfigPath is the central config location under the root.
	updateDeviceConfigPath = "config/models.json"
	// updateDeviceHelperRelative is the fixed installed CLI target on every
	// node, matching journalHelper's account layout convention.
	updateDeviceHelperRelative = ".local/bin/sparky"
	// updateDeviceInstallerBound bounds one installer or version command.
	updateDeviceInstallerBound = 60 * time.Second
	// updateDeviceControlBound bounds one controller admission read or toggle.
	updateDeviceControlBound = 15 * time.Second
	// updateDeviceReadyBound bounds the head controller readiness wait.
	updateDeviceReadyBound = 60 * time.Second
	// updateDeviceRollbackBound bounds the whole best-effort rollback (restore
	// of every changed node plus the one allowed controller restart) after the
	// caller's context is no longer usable.
	updateDeviceRollbackBound = 3 * time.Minute
)

// updateDeviceSpecKeys is the exact accepted payload grammar: every field is
// required, nothing else is allowed, and no version field of any other shape
// is accepted (the CLI already owns a global --version flag).
var updateDeviceSpecKeys = map[string]bool{
	"tag":              true,
	"version":          true,
	"commit":           true,
	"head_stage":       true,
	"worker_stage":     true,
	"cli_sha256":       true,
	"installer_sha256": true,
}

// updateDeviceStagePath blocks anything a shell could reinterpret: staged
// paths are pinned to a conservative character set on top of the clean
// absolute and stage-base containment checks.
var updateDeviceStagePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// updateDeviceBusyStates are the durable lifecycle and manager states that
// mean work is in flight or about to be: an update is always refused. The
// native lifecycle states come from the existing constants so this gate can
// never drift from the durable state machine.
var updateDeviceBusyStates = map[string]bool{
	StateQueued: true, StateStarting: true, StateStopping: true,
	"draining": true,
}

// updateDeviceIdleStates are the only states an update may proceed through:
// a loaded but idle model is allowed, anything else is treated as unknown.
var updateDeviceIdleStates = map[string]bool{
	StateStopped: true, StateReady: true, StateFailed: true,
	"running": true, "idle": true,
}

// updateDeviceNodeNames is the fixed target set: the head and the worker.
var updateDeviceNodeNames = []string{"spark01", "spark02"}

// updateDeviceInstallOrder installs the worker before the head so the head
// controller keeps supervising while the worker binary is swapped.
var updateDeviceInstallOrder = []string{"spark02", "spark01"}

// updateFileSnapshot is one strict node file protocol observation: existence,
// permission bits, digest and exact bytes of a real regular file.
type updateFileSnapshot struct {
	exists bool
	mode   os.FileMode
	sha    string
	data   []byte
}

// updateFileAccess is the node file protocol surface this transaction needs:
// the strict readonly inspect and the own-payload-only remove. The default
// implementation is the existing transactional DeploymentJournal dispatch
// (in-process for Spark1, pinned non-interactive SSH for Spark02); tests
// inject a fake so no SSH or filesystem effect is ever required.
type updateFileAccess interface {
	inspect(ctx context.Context, node, path string) (updateFileSnapshot, error)
	remove(ctx context.Context, node, path, expected string) error
	backup(ctx context.Context, node, source, target, expected string) error
}

// updateRuntimeSurface is the read-only runtime surface used for quiescence
// and identity checks. *Runtime implements it; tests inject fakes so no
// manager, model, systemd or ComfyUI effect can happen offline.
type updateRuntimeSurface interface {
	legacyActivity(context.Context) (map[string]int, error)
	nativeOwnership(context.Context) (Object, error)
	snapshot(ctx context.Context, fresh bool) (Object, error)
}

// updateInstallResult is one sparky-install result JSON payload.
type updateInstallResult struct {
	installed string
	backup    string
	sha       string
	format    string
}

// Injectable effect seams. Every default calls the real implementation; tests
// substitute fakes. No test-only branch lives in the logic below.
var (
	// updateRun dispatches every pinned command: the staged CLI --version
	// probes, the staged installer, the head controller MainPID read and the
	// single head controller restart. It honors an inherited earlier deadline.
	updateRun = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		bounded, cancel := context.WithTimeout(ctx, updateDeviceInstallerBound)
		defer cancel()
		return deploymentRun(bounded, args, input)
	}
	// updateFiles builds the node file protocol access for one transaction.
	updateFiles = func(cfg Object, stage string) updateFileAccess {
		return &updateJournalFiles{j: NewDeploymentJournal(cfg, stage)}
	}
	// updateRuntime builds the read-only runtime surface.
	updateRuntime = func(cfg Object, path string) updateRuntimeSurface { return newRuntime(cfg, path) }
	// updateControl reads one loopback controller route.
	updateControl = func(ctx context.Context, root, operation string, payload any) (Object, error) {
		return controllerRequest(ctx, root, operation, payload)
	}
	// updateFence and updateFenceState own the admission fence through the
	// existing controller fence contract.
	updateFence = func(ctx context.Context, port int, enabled bool) error {
		return installFence(ctx, port, enabled)
	}
	updateFenceState = func(ctx context.Context, port int) (bool, error) {
		return installFenceState(ctx, port)
	}
	// updateUnitState reads the head controller unit's real systemd state.
	updateUnitState = func(ctx context.Context, unit string) (present, enabled, active bool, err error) {
		return installUnitState(ctx, unit)
	}
	// updateWaitHTTP waits for one readiness URL with the existing bounded poll.
	updateWaitHTTP = func(ctx context.Context, url string, timeout time.Duration) (Object, error) {
		return waitHTTP(ctx, url, timeout)
	}
	// updateLock takes the nonblocking update lock.
	updateLock = func(path string, fn func() error) error { return withFileLock(path, false, fn) }
	// updateProcessDigest hashes the executable image of one running process.
	updateProcessDigest = func(ctx context.Context, pid int) (string, error) {
		return updateDeviceReadExe(ctx, pid)
	}
	// updateNow is the report timestamp seam.
	updateNow = func() string { return now() }
)

// updateJournalFiles adapts the existing transactional journal dispatch to the
// narrow inspect/remove surface this transaction needs. Root mode is never
// used: every target here is an owned file in the account's own bin directory.
type updateJournalFiles struct{ j *DeploymentJournal }

func (f *updateJournalFiles) inspect(ctx context.Context, node, path string) (updateFileSnapshot, error) {
	return f.inspectMetadata(ctx, node, path)
}

func (f *updateJournalFiles) remove(ctx context.Context, node, path, expected string) error {
	if !isHexDigest(expected) {
		return fail("A recorded digest is required for removal.")
	}
	if _, err := f.j.call(ctx, node, Object{"operation": "remove", "path": path, "expected_sha256": expected}, false); err != nil {
		return err
	}
	return nil
}

// updateDeviceTarget is the validated payload plus the fixed device layout
// derived from the cluster root. Nothing downstream re-parses the payload.
type updateDeviceTarget struct {
	tag          string
	version      string
	commit       string
	headStage    string
	workerStage  string
	cliSHA       string
	installerSHA string
	root         string
	base         string
	configPath   string
	targetPath   string
}

// parseUpdateDeviceSpec validates the payload grammar and the fixed device
// layout. It is pure: no file, network or service effect happens here.
func parseUpdateDeviceSpec(root string, spec Object) (*updateDeviceTarget, error) {
	if spec == nil {
		return nil, fail("The self-update payload must be one JSON object.")
	}
	for _, key := range []string{"tag", "version", "commit", "head_stage", "worker_stage", "cli_sha256", "installer_sha256"} {
		if _, ok := spec[key]; !ok {
			return nil, fail("The self-update payload needs the %s field.", key)
		}
	}
	for key := range spec {
		if !updateDeviceSpecKeys[key] {
			return nil, fail("The self-update payload field %s is not supported.", key)
		}
	}
	text := func(key string) (string, error) {
		value, ok := spec[key].(string)
		if !ok {
			return "", fail("The self-update payload field %s must be a string.", key)
		}
		return value, nil
	}
	target := &updateDeviceTarget{}
	for _, field := range []struct {
		key  string
		dest *string
	}{
		{"tag", &target.tag}, {"version", &target.version}, {"commit", &target.commit},
		{"head_stage", &target.headStage}, {"worker_stage", &target.workerStage},
		{"cli_sha256", &target.cliSHA}, {"installer_sha256", &target.installerSHA},
	} {
		value, err := text(field.key)
		if err != nil {
			return nil, err
		}
		*field.dest = value
	}
	if !isReleaseVersion(target.version) {
		return nil, fail("The self-update version must be a plain numeric X.Y.Z release version.")
	}
	if target.tag != "v"+target.version {
		return nil, fail("The self-update tag must be the version with a leading v.")
	}
	if !isFullCommit(target.commit) {
		return nil, fail("The self-update commit must be a full lowercase hex commit hash.")
	}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"cli_sha256", &target.cliSHA}, {"installer_sha256", &target.installerSHA},
	} {
		if !isHexDigest(*field.value) {
			return nil, fail("The self-update payload field %s must be a lowercase sha256 hex digest.", field.name)
		}
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fail("The self-update root must be a clean absolute path.")
	}
	target.root = root
	target.base = filepath.Join(root, updateDeviceStageBase)
	target.configPath = filepath.Join(root, updateDeviceConfigPath)
	for _, stage := range []struct {
		name  string
		value *string
	}{
		{"head_stage", &target.headStage}, {"worker_stage", &target.workerStage},
	} {
		if err := updateDeviceCheckStage(stage.name, *stage.value, target.base); err != nil {
			return nil, err
		}
	}
	if target.headStage == target.workerStage {
		return nil, fail("The head and worker staging directories must be different.")
	}
	homedir := home()
	if homedir == "" || !filepath.IsAbs(homedir) {
		return nil, fail("The account home directory for the installed CLI is unknown.")
	}
	target.targetPath = filepath.Join(homedir, filepath.FromSlash(updateDeviceHelperRelative))
	if !updateDeviceStagePath.MatchString(target.targetPath) {
		return nil, fail("The installed CLI target path is not a safe clean absolute path.")
	}
	if updateDeviceWithin(target.targetPath, target.base) {
		return nil, fail("The installed CLI target must not live inside the staging directory.")
	}
	return target, nil
}

// updateDeviceCheckStage pins one staging directory to a real subdirectory of
// the private stage base, with no traversal and no shell-reinterpretable text.
func updateDeviceCheckStage(name, dir, base string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fail("The self-update %s must be a clean absolute directory.", name)
	}
	if !updateDeviceStagePath.MatchString(dir) {
		return fail("The self-update %s contains unsupported characters.", name)
	}
	if !updateDeviceWithin(dir, base) {
		return fail("The self-update %s must live under %s.", name, base)
	}
	return nil
}

// updateDeviceWithin reports whether path is strictly inside base.
func updateDeviceWithin(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// updateDeviceStageFiles returns the three pinned artifact paths of one stage.
func updateDeviceStageFiles(stage string) (cli, installer, buildinfo string) {
	return filepath.Join(stage, "bin", "sparky"),
		filepath.Join(stage, "bin", "sparky-install"),
		filepath.Join(stage, "BUILDINFO.json")
}

// updateDeviceNumber accepts only a real whole JSON number. A string, bool or
// fractional count is never mistaken for activity.
func updateDeviceNumber(v any) (float64, bool) {
	if !integer(v) {
		return 0, false
	}
	return num(v), true
}

// updateDeviceReadExe hashes the executable image behind /proc/<pid>/exe. The
// link is opened, never resolved, so a replaced or deleted binary is hashed as
// the bytes that are actually running.
func updateDeviceReadExe(ctx context.Context, pid int) (string, error) {
	if pid <= 0 {
		return "", fail("The running controller process id is unknown.")
	}
	if err := ctx.Err(); err != nil {
		return "", fail("The device update was cancelled.")
	}
	path := filepath.Join("/proc", strconv.Itoa(pid), "exe")
	file, err := os.Open(path)
	if err != nil {
		return "", fail("The running controller executable could not be read.")
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, deploymentMaxPayload+1)); err != nil {
		return "", fail("The running controller executable could not be read.")
	}
	info, err := file.Stat()
	if err != nil || info.Size() > deploymentMaxPayload {
		return "", fail("The running controller executable is too large.")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// updateDeviceOwned reports whether the head path is a regular file owned by
// the account running the transaction. The worker side of this guarantee comes
// from the write-file protocol, which only ever creates owned regular files.
func updateDeviceOwned(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fail("The staged artifact %s could not be inspected.", filepath.Base(path))
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fail("The staged artifact %s is not a regular file.", filepath.Base(path))
	}
	if os.Geteuid() != 0 {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
			return fail("The staged artifact %s is not owned by this account.", filepath.Base(path))
		}
	}
	return nil
}

// updateDeviceCommand dispatches one pinned command and returns trimmed
// stdout. Diagnostics never carry a host, URL, argv or command output.
func updateDeviceCommand(ctx context.Context, args []string, input []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fail("The device update was cancelled.")
	}
	result, err := updateRun(ctx, args, input)
	if err != nil || result.Code != 0 {
		return "", fail("A device update command did not succeed.")
	}
	return strings.TrimSpace(result.Out), nil
}

// updateDeviceSSH pins the worker transport for one fixed argv: the remote
// command is the single shell-quoted token, exactly as the deployment journal
// composes it, so no staged or payload text can ever be reinterpreted.
func updateDeviceSSH(host string, argv []string) []string {
	return append(deploymentSSHArgs(host), shellJoin(argv))
}

// updateDeviceVersionLine asks one exact binary for its version line.
func updateDeviceVersion(ctx context.Context, host, binary string) (string, error) {
	if host == "" {
		return updateDeviceCommand(ctx, []string{binary, "--version"}, nil)
	}
	return updateDeviceCommand(ctx, updateDeviceSSH(host, []string{binary, "--version"}), nil)
}

// updateDeviceCheckVersion proves the version line names the exact release.
func updateDeviceCheckVersion(line string, target *updateDeviceTarget) error {
	version, commit, ok := updateParseVersionLine(line)
	if !ok || version != target.version || !strings.EqualFold(commit, target.commit) {
		return fail("A Sparky executable did not report the expected version and commit.")
	}
	return nil
}

// updateDeviceInstaller runs one verified staged sparky-install for one
// target path and parses its result JSON strictly.
func updateDeviceInstaller(ctx context.Context, host, installer, source, target string) (updateInstallResult, error) {
	argv := []string{installer, "--source", source, "--target", target}
	var out string
	var err error
	if host == "" {
		out, err = updateDeviceCommand(ctx, argv, nil)
	} else {
		out, err = updateDeviceCommand(ctx, updateDeviceSSH(host, argv), nil)
	}
	if err != nil {
		return updateInstallResult{}, err
	}
	result, derr := decode([]byte(out))
	if derr != nil {
		return updateInstallResult{}, fail("The installer did not return a valid JSON result.")
	}
	install := updateInstallResult{
		installed: str(result["installed"]),
		backup:    str(result["backup"]),
		sha:       strings.ToLower(str(result["sha256"])),
		format:    str(result["format"]),
	}
	if install.installed == "" || install.sha == "" {
		return updateInstallResult{}, fail("The installer result is incomplete.")
	}
	if !isHexDigest(install.sha) {
		return updateInstallResult{}, fail("The installer result digest is not valid.")
	}
	return install, nil
}

// updateDeviceMainPID reads the head controller's current main pid.
func updateDeviceMainPID(ctx context.Context, unit string) (int, error) {
	if !serviceName.MatchString(unit) {
		return 0, fail("The controller unit name is not valid.")
	}
	out, err := updateDeviceCommand(ctx, []string{"systemctl", "show", unit, "-p", "MainPID", "--value"}, nil)
	if err != nil {
		return 0, err
	}
	pid, cerr := strconv.Atoi(strings.TrimSpace(out))
	if cerr != nil || pid <= 0 {
		return 0, fail("The running controller process id is unknown.")
	}
	return pid, nil
}

// updateDeviceRestart restarts the head controller unit and nothing else.
func updateDeviceRestart(ctx context.Context, unit string) error {
	if !serviceName.MatchString(unit) {
		return fail("The controller unit name is not valid.")
	}
	_, err := updateDeviceCommand(ctx, []string{"sudo", "-n", "systemctl", "restart", unit}, nil)
	return err
}
