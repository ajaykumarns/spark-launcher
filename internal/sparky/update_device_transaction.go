package sparky

// Transaction phases of the head-side device self-update. The orchestration
// here is deliberately linear: verify, guard, fence, install, restart once,
// verify the cluster, and always restore the owned fence. Every effect is
// performed through the seams declared in update_device.go so the whole
// transaction is testable offline.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// updateDeviceNodeState is one node's install and rollback bookkeeping.
type updateDeviceNodeState struct {
	node          string
	stage         string
	cliPath       string
	installerPath string
	buildinfoPath string
	targetPath    string
	wantSHA       string
	priorSHA      string
	observedSHA   string
	changed       bool
	rolledBack    bool
	backup        string
	version       string
	status        string
}

// updateDeviceBaseline is the captured cluster identity that must be
// byte-identical when the transaction claims success.
type updateDeviceBaseline struct {
	models  Object
	records Object
	units   Object
	config  string
}

// updateDeviceRun carries one transaction.
type updateDeviceRun struct {
	target   *updateDeviceTarget
	nodes    map[string]*updateDeviceNodeState
	report   Object
	cfg      Object
	port     int
	worker   string
	files    updateFileAccess
	rt       updateRuntimeSurface
	baseline *updateDeviceBaseline

	fenceOwned bool
	restarted  bool
	fenceError string
}

// updateDeviceTransaction runs the whole head-side self-update. The returned
// report always describes the partial state that was reached (target digests,
// backups, controller and rollback outcome), so an integration can surface it
// even when the error is non-nil.
func updateDeviceTransaction(ctx context.Context, root string, spec Object) (Object, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	target, err := parseUpdateDeviceSpec(root, spec)
	if err != nil {
		return nil, err
	}
	run := newUpdateDeviceRun(target)
	var transaction error
	if lockErr := updateLock(filepath.Join(target.base, updateDeviceLockName), func() error {
		transaction = run.execute(ctx)
		return nil
	}); lockErr != nil {
		// The lock was never held: no effect of any kind happened.
		return nil, lockErr
	}
	run.sync()
	return run.report, transaction
}

// newUpdateDeviceRun prepares the report skeleton and the per-node state.
func newUpdateDeviceRun(target *updateDeviceTarget) *updateDeviceRun {
	nodes := map[string]*updateDeviceNodeState{}
	for _, node := range updateDeviceNodeNames {
		stage := target.headStage
		if node == "spark02" {
			stage = target.workerStage
		}
		cli, installer, buildinfo := updateDeviceStageFiles(stage)
		nodes[node] = &updateDeviceNodeState{
			node: node, stage: stage, cliPath: cli, installerPath: installer,
			buildinfoPath: buildinfo, targetPath: target.targetPath,
			wantSHA: target.cliSHA, status: "pending",
		}
	}
	report := Object{
		"action":   "self-update",
		"status":   "failed",
		"at":       updateNow(),
		"release":  Object{"tag": target.tag, "version": target.version, "commit": target.commit},
		"rollback": Object{"attempted": false, "complete": false, "problems": []any{}},
	}
	return &updateDeviceRun{target: target, nodes: nodes, report: report}
}

// hostFor returns the worker transport host, or "" for the head.
func (u *updateDeviceRun) hostFor(node string) string {
	if node == "spark02" {
		return u.worker
	}
	return ""
}

// setNodeStatus records one node's terminal state for the report.
func (u *updateDeviceRun) setStatus(status string) { u.report["status"] = status }

// execute runs the transaction and guarantees the owned fence is restored.
func (u *updateDeviceRun) execute(ctx context.Context) (result error) {
	defer func() {
		u.restoreFence()
		if u.fenceError != "" {
			if result == nil {
				u.setStatus("fence_restore_failed")
				result = fail("%s The verified binaries were retained.", u.fenceError)
			} else {
				result = fail("%v %s", result, u.fenceError)
			}
		}
	}()
	for _, phase := range []func(context.Context) error{
		u.loadConfig,
		u.verifyStaged,
		u.preflight,
		u.takeFence,
		u.install,
		u.restart,
		u.verifyCluster,
	} {
		if err := phase(ctx); err != nil {
			if u.fenceOwned || len(u.changedNodes()) > 0 || u.restarted {
				return u.rollback(err)
			}
			u.setStatus("failed")
			u.sync()
			return err
		}
	}
	if len(u.changedNodes()) == 0 && !u.restarted {
		u.setStatus("unchanged")
	} else {
		u.setStatus("updated")
	}
	u.sync()
	return nil
}

// loadConfig reads the central cluster configuration once and pins every
// later effect to that deep copy.
func (u *updateDeviceRun) loadConfig(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fail("The device update was cancelled.")
	}
	cfg, err := readCluster(u.target.configPath)
	if err != nil {
		return err
	}
	if str(cfg["lifecycle"]) != "native" {
		return fail("The device update requires the native model lifecycle.")
	}
	port := controllerPort(cfg)
	if port == 0 {
		return fail("The cluster file has no usable controller port.")
	}
	host := strings.TrimSpace(str(obj(obj(cfg["nodes"])["spark02"])["ssh"]))
	if err := safeHost(host); err != nil {
		return fail("The worker SSH target is not usable for the device update.")
	}
	u.cfg = cfg
	u.port = port
	u.worker = host
	u.files = updateFiles(cfg, u.target.base)
	u.rt = updateRuntime(cfg, u.target.configPath)
	u.report["cluster"] = Object{"root": u.target.root, "config": u.target.configPath}
	return nil
}

// verifyStaged independently re-proves every staged artifact on BOTH nodes:
// the two digests, the BUILDINFO identity, and the staged CLI's own version
// line. Nothing installed is touched before all of this passes.
func (u *updateDeviceRun) verifyStaged(ctx context.Context) error {
	for _, node := range updateDeviceNodeNames {
		if err := ctx.Err(); err != nil {
			return fail("The device update was cancelled.")
		}
		state := u.nodes[node]
		host := u.hostFor(node)
		for _, artifact := range []struct {
			path string
			want string
			exec bool
		}{
			{state.cliPath, u.target.cliSHA, true},
			{state.installerPath, u.target.installerSHA, true},
			{state.buildinfoPath, "", false},
		} {
			if host == "" {
				if err := updateDeviceOwned(artifact.path); err != nil {
					return err
				}
			}
			snap, err := u.files.inspect(ctx, node, artifact.path)
			if err != nil {
				return err
			}
			if !snap.exists {
				return fail("The staged release is incomplete on %s.", node)
			}
			if artifact.want != "" {
				if snap.sha != artifact.want {
					return fail("A staged artifact on %s does not match the release digest.", node)
				}
				if artifact.exec && snap.mode&0o111 == 0 {
					return fail("A staged artifact on %s is not executable.", node)
				}
			}
			if artifact.path == state.buildinfoPath {
				if err := updateDeviceCheckBuildInfo(snap.data, u.target); err != nil {
					return err
				}
			}
		}
		line, err := updateDeviceVersion(ctx, host, state.cliPath)
		if err != nil {
			return err
		}
		if err := updateDeviceCheckVersion(line, u.target); err != nil {
			return err
		}
	}
	return nil
}

// updateDeviceBuildInfo mirrors the BUILDINFO.json contract written by
// tools/release-bundle.sh for one platform archive.
type updateDeviceBuildInfo struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Tag      string `json:"tag"`
	Commit   string `json:"commit"`
	Platform string `json:"platform"`
	Binaries []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"binaries"`
}

// updateDeviceCheckBuildInfo proves the staged BUILDINFO.json names exactly
// this release, this platform, and these two binaries.
func updateDeviceCheckBuildInfo(raw []byte, target *updateDeviceTarget) error {
	if len(raw) > updateDeviceBuildInfoBound {
		return fail("The staged build info is too large.")
	}
	var info updateDeviceBuildInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return fail("The staged build info is not valid JSON.")
	}
	if info.Name != "" && info.Name != "sparky" {
		return fail("The staged build info names an unexpected product.")
	}
	if info.Version != target.version || info.Tag != target.tag {
		return fail("The staged build info does not name this release.")
	}
	if !strings.EqualFold(info.Commit, target.commit) {
		return fail("The staged build info commit does not match this release.")
	}
	if info.Platform != updateDevicePlatform {
		return fail("The staged build info is not for this platform.")
	}
	digests := map[string]string{}
	for _, binary := range info.Binaries {
		if filepath.Base(binary.Path) != "sparky" && filepath.Base(binary.Path) != "sparky-install" {
			return fail("The staged build info lists an unexpected binary.")
		}
		sha := strings.ToLower(binary.SHA256)
		if !isHexDigest(sha) {
			return fail("The staged build info lists an invalid binary digest.")
		}
		digests[filepath.Base(binary.Path)] = sha
	}
	if len(digests) != 2 {
		return fail("The staged build info does not cover both release binaries.")
	}
	if digests["sparky"] != target.cliSHA || digests["sparky-install"] != target.installerSHA {
		return fail("The staged build info digests do not match the release payload.")
	}
	return nil
}

// preflight reads the quiescence and identity evidence once, before any fence
// or effect. A busy, transitioning or unknown cluster is refused here.
func (u *updateDeviceRun) preflight(ctx context.Context) error {
	fenced, err := updateFenceState(ctx, u.port)
	if err != nil {
		return err
	}
	if fenced {
		return fail("A manual admission fence is active. Lift it before the device update.")
	}
	if _, _, active, err := updateUnitState(ctx, updateDeviceControllerUnit); err != nil {
		return err
	} else if !active {
		// The update never enables or starts a controller: an inactive head
		// controller means this cluster is not in the state the command
		// requires, so nothing is replaced.
		return fail("The head controller service is not running. The device update was refused.")
	}
	if err := u.quiesce(ctx, false); err != nil {
		return err
	}
	baseline, err := u.capture(ctx)
	if err != nil {
		return err
	}
	u.baseline = baseline
	return nil
}

// takeFence records the prior fence state and takes the admission fence BEFORE
// any installed binary is replaced.
func (u *updateDeviceRun) takeFence(ctx context.Context) error {
	prior, err := updateFenceState(ctx, u.port)
	if err != nil {
		return err
	}
	if prior {
		return fail("A manual admission fence is active. Lift it before the device update.")
	}
	u.report["fence"] = Object{"prior": false, "owned": false, "restored": false}
	if err := updateFence(ctx, u.port, true); err != nil {
		// The enable may have landed before the transport failed, so the fence
		// is handed straight back: this command never leaks an operator fence.
		// The original failure is still what stops the transaction.
		u.fenceOwned = true
		u.restoreFence()
		return err
	}
	u.fenceOwned = true
	u.report["fence"] = Object{"prior": false, "owned": true, "restored": false}
	// Fresh quiescence under the fence: work that started between the
	// preflight read and the fence must still stop this transaction.
	return u.quiesce(ctx, true)
}

// restoreFence always gives the fence back, on a context that cannot be
// cancelled by the caller or by the failure that triggered the exit.
func (u *updateDeviceRun) restoreFence() {
	if !u.fenceOwned {
		return
	}
	u.fenceOwned = false
	bounded, cancel := context.WithTimeout(context.Background(), updateDeviceControlBound)
	defer cancel()
	if err := updateFence(bounded, u.port, false); err != nil {
		u.fenceError = "The update admission fence could not be cleared; check /control/fence."
	}
	fence := Object{"prior": false, "owned": false, "restored": u.fenceError == ""}
	if u.fenceError != "" {
		fence["error"] = u.fenceError
	}
	u.report["fence"] = fence
}

// quiesce refuses to proceed while anything is admitted, queued, transitioning,
// draining or otherwise not provably idle. When ownFence is false a foreign
// fence or transition is refused too.
func (u *updateDeviceRun) quiesce(ctx context.Context, ownFence bool) error {
	leases, err := updateControl(ctx, u.target.root, "leases", nil)
	if err != nil {
		return err
	}
	active, waiting, transition, fenced, err := imageLeases(leases)
	if err != nil {
		return err
	}
	if len(active) > 0 {
		return fail("Work is admitted on this cluster. Wait for it to finish and try again.")
	}
	if len(waiting) > 0 {
		return fail("Work is queued on this controller. Clear the queue and try again.")
	}
	if !ownFence {
		if transition {
			return fail("A controller transition is in progress. Wait for it to finish and try again.")
		}
		if fenced {
			return fail("A manual admission fence is active. Lift it before the device update.")
		}
	}
	activity, err := u.rt.legacyActivity(ctx)
	if err != nil {
		return err
	}
	for _, name := range updateDeviceSorted(activity) {
		if activity[name] > 0 {
			return fail("The %s service is busy. The device update was refused.", name)
		}
	}
	records, err := u.rt.nativeOwnership(ctx)
	if err != nil {
		return err
	}
	for _, name := range keys(records) {
		state := str(obj(records[name])["state"])
		if updateDeviceBusyStates[state] {
			return fail("%s is %s. Wait for it to settle and try again.", name, state)
		}
		if !updateDeviceIdleStates[state] {
			return fail("%s reported the lifecycle state %s, which cannot be treated as idle.", name, state)
		}
	}
	snapshot, err := u.rt.snapshot(ctx, true)
	if err != nil {
		return err
	}
	if err := updateDeviceCheckUnits(snapshot); err != nil {
		return err
	}
	return updateDeviceCheckComfy(snapshot, u.cfg)
}

// updateDeviceSorted returns the names of an activity map in stable order so a
// busy-service refusal never depends on Go map iteration order.
func updateDeviceSorted(activity map[string]int) []string {
	names := make([]string, 0, len(activity))
	for name := range activity {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// updateDeviceCheckUnitStates allows a loaded but idle model and refuses any
// unit that is mid-transition or in a state this command cannot interpret.
func updateDeviceCheckUnits(snapshot Object) error {
	for _, node := range updateDeviceNodeNames {
		units := obj(obj(snapshot["nodes"])[node])["units"]
		for _, unit := range keys(obj(units)) {
			state := str(obj(units)[unit])
			switch state {
			case "active", "inactive", "failed", "":
				if state == "" {
					return fail("The %s service state on %s is unknown. The device update was refused.", unit, node)
				}
			default:
				return fail("The %s service on %s is %s. The device update was refused.", unit, node, state)
			}
		}
	}
	return nil
}

// updateDeviceCheckComfy refuses any active, queued or ambiguous ComfyUI job
// on either node. Reachable-but-unreadable guard state is unknown, not idle.
func updateDeviceCheckComfy(snapshot, cfg Object) error {
	comfy := obj(snapshot["comfyui"])
	for _, node := range updateDeviceNodeNames {
		row, present := comfy[node]
		if !present {
			return fail("The ComfyUI state on %s is unknown. The device update was refused.", node)
		}
		state := obj(row)
		if _, ok := state["reachable"].(bool); !ok {
			return fail("The ComfyUI state on %s is unknown. The device update was refused.", node)
		}
		if !boolean(state["reachable"]) {
			unit := str(obj(obj(cfg["nodes"])[node])["comfy_unit"])
			unitState := str(obj(obj(obj(snapshot["nodes"])[node])["units"])[unit])
			if unit == "" || (unitState != "inactive" && unitState != "failed") {
				return fail("ComfyUI on %s is unreachable and is not confirmed stopped.", node)
			}
		}
		if raw, exists := state["executing"]; exists {
			executing, valid := raw.(bool)
			if !valid {
				return fail("ComfyUI activity on %s is unknown.", node)
			}
			if executing {
				return fail("ComfyUI on %s is executing a guarded job. The device update was refused.", node)
			}
		}
		running, okRunning := updateDeviceNumber(state["running"])
		pending, okPending := updateDeviceNumber(state["pending"])
		if !okRunning || !okPending || running < 0 || pending < 0 {
			return fail("The ComfyUI queue on %s is unreadable. The device update was refused.", node)
		}
		if running > 0 {
			return fail("ComfyUI on %s is running a job. The device update was refused.", node)
		}
		if pending > 0 {
			return fail("ComfyUI on %s has a queued job. The device update was refused.", node)
		}
		if guarded, ok := state["guarded"].(bool); ok && guarded {
			executing, okExec := state["executing"].(bool)
			if !okExec {
				return fail("The ComfyUI guard state on %s is unknown. The device update was refused.", node)
			}
			if executing {
				return fail("ComfyUI on %s is executing a guarded job. The device update was refused.", node)
			}
		} else if raw, present := state["executing"]; present {
			if _, ok := raw.(bool); !ok {
				return fail("The ComfyUI guard state on %s is unknown. The device update was refused.", node)
			}
		}
		if raw, present := state["phase"]; present {
			phase, ok := raw.(string)
			if !ok {
				return fail("The ComfyUI guard state on %s is unknown. The device update was refused.", node)
			}
			if phase == "executing" {
				return fail("ComfyUI on %s is executing a guarded job. The device update was refused.", node)
			}
		}
	}
	for _, name := range keys(obj(snapshot["models"])) {
		count, known := updateActivityCount(obj(obj(snapshot["models"])[name])["active_requests"])
		if !known {
			return fail("%s request activity is unknown. The device update was refused.", name)
		}
		if count > 0 {
			return fail("%s has an active request. The device update was refused.", name)
		}
	}
	return nil
}

// capture records the cluster identity that must not change: the model set and
// placement, the actual durable lifecycle rows, the actual supervisor and rank
// unit states, and the central config digest.
func (u *updateDeviceRun) capture(ctx context.Context) (*updateDeviceBaseline, error) {
	records, err := u.rt.nativeOwnership(ctx)
	if err != nil {
		return nil, err
	}
	snapshot, err := u.rt.snapshot(ctx, true)
	if err != nil {
		return nil, err
	}
	digest, err := u.configDigest(ctx)
	if err != nil {
		return nil, err
	}
	models := Object{}
	for _, name := range keys(obj(u.cfg["models"])) {
		m := obj(obj(u.cfg["models"])[name])
		models[name] = Object{
			"kind":        str(m["kind"]),
			"desired":     m["run_on"],
			"enabled":     value(obj(obj(m["settings"])[str(m["run_on"])]), "enabled", true),
			"keep_loaded": m["keep_loaded"],
		}
	}
	units := Object{}
	for _, node := range updateDeviceNodeNames {
		observed := Object{}
		observedUnits := obj(obj(snapshot["nodes"])[node])["units"]
		for _, unit := range keys(obj(observedUnits)) {
			observed[unit] = str(obj(observedUnits)[unit])
		}
		units[node] = observed
	}
	live := Object{}
	for _, name := range keys(obj(snapshot["models"])) {
		m := obj(obj(snapshot["models"])[name])
		live[name] = Object{"loaded": m["loaded"], "nodes": stringsOf(m["nodes"]), "state": str(m["state"]), "reserved": m["reserved"]}
	}
	return &updateDeviceBaseline{
		models:  Object{"config": models, "observed": live},
		records: copyObject(records),
		units:   units,
		config:  digest,
	}, nil
}

// configDigest hashes the central config file as the node sees it.
func (u *updateDeviceRun) configDigest(ctx context.Context) (string, error) {
	snap, err := u.files.inspect(ctx, "spark01", u.target.configPath)
	if err != nil {
		return "", err
	}
	if !snap.exists {
		return "", fail("The central cluster configuration is missing.")
	}
	return snap.sha, nil
}

// changedNodes lists the nodes whose installed binary this transaction wrote.
func (u *updateDeviceRun) changedNodes() []*updateDeviceNodeState {
	out := []*updateDeviceNodeState{}
	for _, node := range updateDeviceInstallOrder {
		if state := u.nodes[node]; state.changed {
			out = append(out, state)
		}
	}
	return out
}

// install replaces the installed CLI on the worker first and then the head,
// always through the verified staged installer, and proves every write.
func (u *updateDeviceRun) install(ctx context.Context) error {
	for _, node := range updateDeviceInstallOrder {
		if err := ctx.Err(); err != nil {
			return fail("The device update was cancelled.")
		}
		state := u.nodes[node]
		host := u.hostFor(node)
		current, err := u.files.inspect(ctx, node, state.targetPath)
		if err != nil {
			return err
		}
		if current.exists {
			state.priorSHA = current.sha
			state.observedSHA = current.sha
		}
		if current.exists && current.sha == state.wantSHA {
			// Already the target bytes: nothing is written and no backup is
			// needed, but the version still has to answer on this node.
			state.status = "unchanged"
			if err := u.checkInstalledVersion(ctx, node, state); err != nil {
				state.status = "failed"
				u.sync()
				return err
			}
			u.sync()
			continue
		}
		// failNode records the honest per-node state before the transaction
		// unwinds, so a partial report always names the node that stopped.
		failNode := func(err error) error {
			state.status = "failed"
			u.sync()
			return err
		}
		if current.exists {
			state.backup = filepath.Join(filepath.Dir(state.targetPath), ".sparky-backups", "sparky.bak-"+sha256Hex([]byte(state.stage))[:16])
			if err := u.files.backup(ctx, node, state.targetPath, state.backup, state.priorSHA); err != nil {
				return failNode(err)
			}
		}
		// Dispatch may commit before its response is lost. Rollback must include
		// this node even when subsequent receipt/verification fails.
		state.changed = true
		state.observedSHA = ""
		u.sync()
		result, err := updateDeviceInstaller(ctx, host, state.installerPath, state.cliPath, state.targetPath)
		if err != nil {
			return failNode(err)
		}
		if result.sha != state.wantSHA {
			return failNode(fail("The installer on %s did not produce the release digest.", node))
		}
		if result.installed != state.targetPath {
			return failNode(fail("The installer on %s wrote an unexpected target path.", node))
		}
		if result.format != "elf" && result.format != "macho" {
			return failNode(fail("The installer on %s did not install a native executable.", node))
		}
		if current.exists {
			if result.backup == "" {
				return failNode(fail("The installer on %s did not record a backup of the previous CLI.", node))
			}
			backup, err := u.files.inspect(ctx, node, result.backup)
			if err != nil {
				return failNode(err)
			}
			if !backup.exists || backup.sha != state.priorSHA {
				return failNode(fail("The backup of the previous CLI on %s does not match the installed bytes.", node))
			}
		}
		installed, err := u.files.inspect(ctx, node, state.targetPath)
		if err != nil {
			return failNode(err)
		}
		if !installed.exists || installed.sha != state.wantSHA || installed.mode&0o111 == 0 {
			return failNode(fail("The installed CLI on %s could not be verified.", node))
		}
		state.changed = true
		state.observedSHA = installed.sha
		state.status = "updated"
		if err := u.checkInstalledVersion(ctx, node, state); err != nil {
			return failNode(err)
		}
		u.sync()
	}
	return nil
}

// checkInstalledVersion proves the installed CLI reports this release.
func (u *updateDeviceRun) checkInstalledVersion(ctx context.Context, node string, state *updateDeviceNodeState) error {
	line, err := updateDeviceVersion(ctx, u.hostFor(node), state.targetPath)
	if err != nil {
		return err
	}
	if err := updateDeviceCheckVersion(line, u.target); err != nil {
		return err
	}
	state.version = u.target.version
	return nil
}

// restart restarts the head controller only when it is running, provably idle,
// and actually running bytes that differ from what was just installed. The
// worker is never restarted, and an inactive controller is never started.
func (u *updateDeviceRun) restart(ctx context.Context) error {
	head := u.nodes["spark01"]
	controller := Object{"unit": updateDeviceControllerUnit, "restarted": false, "restart": "skipped"}
	defer func() { u.report["controller"] = controller }()

	present, enabled, active, err := updateUnitState(ctx, updateDeviceControllerUnit)
	if err != nil {
		return err
	}
	controller["present"], controller["enabled"], controller["active"] = present, enabled, active
	if !present || !active {
		return fail("The head controller stopped responding during the device update. Nothing further was changed.")
	}
	pid, err := updateDeviceMainPID(ctx, updateDeviceControllerUnit)
	if err != nil {
		return err
	}
	controller["main_pid"] = pid
	running, err := updateProcessDigest(ctx, pid)
	if err != nil {
		return err
	}
	controller["running_sha256"] = running
	if running == head.wantSHA {
		// The running image already matches the target bytes on disk.
		return nil
	}
	// A differing running image is only chased when the bytes waiting on disk
	// have just been proven to be this release. Without that proof the running
	// process belongs to somebody else's change and is left exactly as found.
	ondisk, err := u.files.inspect(ctx, "spark01", head.targetPath)
	if err != nil {
		return err
	}
	if head.status == "failed" || !ondisk.exists || ondisk.sha != head.wantSHA || ondisk.mode&0o111 == 0 {
		controller["restart"] = "skipped-unverified-target"
		return nil
	}
	// Re-prove quiescence immediately before the restart: the fence blocks new
	// admission, not work that a caller already handed to a loaded model.
	if err := u.quiesce(ctx, true); err != nil {
		return err
	}
	u.restarted = true // The command can commit before its response is lost.
	if err := updateDeviceRestart(ctx, updateDeviceControllerUnit); err != nil {
		if actualPID, probeErr := updateDeviceMainPID(ctx, updateDeviceControllerUnit); probeErr == nil && actualPID == pid {
			if digest, probeErr := updateProcessDigest(ctx, actualPID); probeErr == nil && digest == running {
				u.restarted = false
			}
		}
		return err
	}
	controller["restarted"] = true
	controller["restart"] = "restarted"
	if _, err := updateWaitHTTP(ctx, "http://127.0.0.1:"+itoa(u.port)+"/health", updateDeviceReadyBound); err != nil {
		return err
	}
	// The replacement process must be fenced before it can admit anything.
	if err := updateFence(ctx, u.port, true); err != nil {
		return err
	}
	pid, err = updateDeviceMainPID(ctx, updateDeviceControllerUnit)
	if err != nil {
		return err
	}
	controller["main_pid"] = pid
	running, err = updateProcessDigest(ctx, pid)
	if err != nil {
		return err
	}
	controller["running_sha256"] = running
	if running != head.wantSHA {
		return fail("The restarted head controller is not running the installed release.")
	}
	return nil
}

// verifyCluster proves the cluster came back exactly as it was left.
func (u *updateDeviceRun) verifyCluster(ctx context.Context) error {
	if _, err := updateControl(ctx, u.target.root, "status", nil); err != nil {
		return err
	}
	head := u.nodes["spark01"]
	worker := u.nodes["spark02"]
	for _, node := range updateDeviceNodeNames {
		state := u.nodes[node]
		if state.version != u.target.version {
			if err := u.checkInstalledVersion(ctx, node, state); err != nil {
				return err
			}
		}
	}
	if _, err := updateWaitHTTP(ctx, "http://127.0.0.1:"+itoa(u.port)+"/health", updateDeviceReadyBound); err != nil {
		return err
	}
	if head.status == "failed" || worker.status == "failed" {
		return fail("A device target is not in a verified state.")
	}
	after, err := u.capture(ctx)
	if err != nil {
		return err
	}
	if u.baseline == nil {
		return fail("The pre-update cluster identity was not captured.")
	}
	cluster := Object{"models_unchanged": false, "supervisors_unchanged": false, "config_unchanged": false, "models": len(u.baseline.models)}
	defer func() { u.report["cluster"] = merge(obj(u.report["cluster"]), cluster) }()
	if !equalJSON(u.baseline.models, after.models) {
		return fail("The model set or placement changed during the device update.")
	}
	cluster["models_unchanged"] = true
	if !equalJSON(u.baseline.records, after.records) {
		return fail("The durable model lifecycle records changed during the device update.")
	}
	if !equalJSON(u.baseline.units, after.units) {
		return fail("The running supervisor and rank identities changed during the device update.")
	}
	cluster["supervisors_unchanged"] = true
	digest, err := u.configDigest(ctx)
	if err != nil {
		return err
	}
	if digest != u.baseline.config {
		return fail("The central cluster configuration changed during the device update.")
	}
	cluster["config_unchanged"] = true
	return nil
}

// rollback restores every binary this transaction changed, or reports honestly
// when a restore cannot be proved. It never touches a service it cannot prove
// idle, and it never starts a stopped controller. It runs on its own bounded,
// uncancellable context: a caller who has given up must still not be left with
// binaries half-installed across the two nodes, and the admission fence is
// still held while the restore happens.
func (u *updateDeviceRun) rollback(cause error) error {
	changed := u.changedNodes()
	ctx, cancel := context.WithTimeout(context.Background(), updateDeviceRollbackBound)
	defer cancel()
	problems := []any{}
	rollback := Object{"attempted": len(changed) > 0 || u.restarted, "complete": false, "problems": problems}
	u.report["rollback"] = rollback
	for i := len(changed) - 1; i >= 0; i-- {
		state := changed[i]
		if err := u.restore(ctx, state); err != nil {
			state.status = "rollback_failed"
			problems = append(problems, state.node+": "+err.Error())
			continue
		}
		state.changed = false
		if state.rolledBack {
			state.status = "rolled_back"
			state.version = ""
		} else if state.status != "failed" {
			state.status = "unchanged"
		}
	}
	if u.restarted && len(problems) == 0 {
		if err := u.restartAfterRollback(ctx); err != nil {
			problems = append(problems, "controller: "+err.Error())
		}
	}
	rollback["problems"] = problems
	rollback["complete"] = len(problems) == 0
	if len(problems) > 0 {
		u.setStatus("rollback_incomplete")
		u.sync()
		return fail("The device update failed and could not be fully rolled back. Backups and digests are in the report. Cause: %v", cause)
	}
	if len(changed) > 0 || u.restarted {
		u.setStatus("rolled_back")
	} else {
		u.setStatus("failed")
	}
	u.sync()
	return cause
}

// restore puts one node back exactly as this transaction found it.
func (u *updateDeviceRun) restore(ctx context.Context, state *updateDeviceNodeState) error {
	host := u.hostFor(state.node)
	current, err := u.files.inspect(ctx, state.node, state.targetPath)
	if err != nil {
		state.observedSHA = ""
		return err
	}
	state.observedSHA = current.sha
	if current.exists && state.priorSHA != "" && current.sha == state.priorSHA {
		return nil
	}
	if !current.exists && state.priorSHA == "" {
		return nil
	}
	if current.exists && current.sha != state.wantSHA {
		return fail("The target on %s changed unexpectedly; its backup was retained for recovery.", state.node)
	}
	if state.priorSHA == "" {
		// This transaction created the file, so removing exactly what it wrote
		// (proved by the digest it installed) is the restore.
		if err := u.files.remove(ctx, state.node, state.targetPath, state.wantSHA); err != nil {
			return err
		}
		after, err := u.files.inspect(ctx, state.node, state.targetPath)
		if err != nil {
			return err
		}
		if after.exists {
			return fail("The installed CLI on %s is still present.", state.node)
		}
		state.rolledBack = true
		return nil
	}
	if state.backup == "" {
		return fail("No verified backup is recorded for %s.", state.node)
	}
	backup, err := u.files.inspect(ctx, state.node, state.backup)
	if err != nil {
		return err
	}
	if !backup.exists || backup.sha != state.priorSHA {
		return fail("The recorded backup for %s does not match the previous installation.", state.node)
	}
	result, err := updateDeviceInstaller(ctx, host, state.installerPath, state.backup, state.targetPath)
	if err != nil {
		return err
	}
	if result.sha != state.priorSHA || result.installed != state.targetPath {
		return fail("Restoring the backup on %s did not reproduce the previous installation.", state.node)
	}
	after, err := u.files.inspect(ctx, state.node, state.targetPath)
	if err != nil {
		return err
	}
	if !after.exists || after.sha != state.priorSHA {
		return fail("The restored CLI on %s could not be verified.", state.node)
	}
	state.observedSHA = after.sha
	state.rolledBack = true
	return nil
}

// restartAfterRollback brings the previous controller image back only when the
// unit is provably running and idle. A busy, unknown or inactive controller is
// never restarted or started; that is reported as an incomplete rollback.
func (u *updateDeviceRun) restartAfterRollback(ctx context.Context) error {
	head := u.nodes["spark01"]
	present, _, active, err := updateUnitState(ctx, updateDeviceControllerUnit)
	if err != nil {
		return err
	}
	if !present || !active {
		return fail("The head controller is not running, so it was not restarted during rollback.")
	}
	if err := u.quiesce(ctx, true); err != nil {
		return fail("The head controller became busy, so it was not restarted during rollback.")
	}
	if err := updateDeviceRestart(ctx, updateDeviceControllerUnit); err != nil {
		return err
	}
	if _, err := updateWaitHTTP(ctx, "http://127.0.0.1:"+itoa(u.port)+"/health", updateDeviceReadyBound); err != nil {
		return err
	}
	if err := updateFence(ctx, u.port, true); err != nil {
		return err
	}
	pid, err := updateDeviceMainPID(ctx, updateDeviceControllerUnit)
	if err != nil {
		return err
	}
	running, err := updateProcessDigest(ctx, pid)
	if err != nil {
		return err
	}
	controller := obj(u.report["controller"])
	controller["main_pid"] = pid
	controller["running_sha256"] = running
	if head.priorSHA != "" && running != head.priorSHA {
		return fail("The restarted head controller is not running the restored CLI.")
	}
	return nil
}

// sync renders the live per-node and fence state into the report so a partial
// transaction is always fully described.
func (u *updateDeviceRun) sync() {
	targets := Object{}
	backups := Object{}
	for _, node := range updateDeviceNodeNames {
		state := u.nodes[node]
		targets[node] = Object{
			"node":         state.node,
			"path":         state.targetPath,
			"stage":        state.stage,
			"sha256":       state.observedSHA,
			"want_sha256":  state.wantSHA,
			"prior_sha256": state.priorSHA,
			"state":        state.status,
			"changed":      state.changed,
			"backup":       state.backup,
			"version":      state.version,
			"rolled_back":  state.rolledBack,
		}
		if state.backup != "" {
			backups[node] = state.backup
		}
	}
	u.report["targets"] = targets
	u.report["backups"] = backups
	if _, ok := u.report["fence"]; !ok {
		u.report["fence"] = Object{"prior": false, "owned": false, "restored": false}
	}
}

// equalJSON compares two captured values by their canonical encoding.
func equalJSON(a, b any) bool {
	return string(jsonBytes(a)) == string(jsonBytes(b))
}

// updateDeviceBuildInfoBound bounds the staged BUILDINFO.json bytes.
const updateDeviceBuildInfoBound = 64 << 10

// updateDevicePlatform is the only platform this device transaction supports.
const updateDevicePlatform = "linux/arm64"
