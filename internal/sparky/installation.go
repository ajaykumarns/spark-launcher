// Real device installation: the Go-native port of the source install_head.
// It consumes the pure installation plan, writes every file, config, guard,
// unit and hook through the device transaction journal, preserves the actual
// runtime baseline, and confirms each effect before reporting success. The
// legacy manager and parity path stay authoritative; nothing is autoloaded
// and no native generation state is manufactured.
package sparky

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// installationJournal is the stable device transaction contract. The device
// agent's *DeploymentJournal implements it; tests inject fakes.
type installationJournal interface {
	WriteFile(ctx context.Context, node, path string, data []byte, mode os.FileMode) error
	WriteUnit(ctx context.Context, node, path, text string) error
	Rollback() error
}

// installationRuntime is the runtime surface the installation needs.
type installationRuntime interface {
	snapshot(ctx context.Context, fresh bool) (Object, error)
	unit(ctx context.Context, node, action, unit string) error
}

// Journal node selectors: the head's own files are written as "spark01"
// (local protocol), worker files as "spark02" (private SSH transfer).

// Injectable effect seams. Every default calls the real implementation;
// tests substitute fakes. No test-only branches live in the logic below.
var (
	installNewJournal = func(cfg Object, stage string) installationJournal {
		return NewDeploymentJournal(cfg, stage)
	}
	installPrepareQwen = func(ctx context.Context, cfg Object, stage string, j installationJournal) error {
		real, ok := j.(*DeploymentJournal)
		if !ok {
			return fail("The installation journal does not support the Qwen preparation.")
		}
		return prepareQwenDualWithJournal(ctx, cfg, stage, real)
	}
	// installGateway connects the existing Bifrost garuda provider to the
	// newly installed controller and returns the public result plus an undo
	// that restores the previous provider, files and nginx state
	// independently when a later installation step fails.
	installGateway = func(cfg Object, port int, backup string) (Object, func() error, error) {
		return installGatewayConnect(cfg, port, backup)
	}
	installRuntime = func(cfg Object, configPath string) installationRuntime {
		return newRuntime(cfg, configPath)
	}
	// installRun bounds every command to at most sixty seconds while still
	// honoring an inherited earlier deadline, and always dispatches through
	// the current deploymentRun seam.
	installRun = func(ctx context.Context, args []string, input []byte) (CommandResult, error) {
		bounded, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		return deploymentRun(bounded, args, input)
	}
	// installFence toggles controller admission fencing.
	installFence = func(ctx context.Context, port int, enabled bool) error {
		_, err := requestObject(ctx, "POST",
			"http://127.0.0.1:"+strconv.Itoa(port)+"/control/fence",
			Object{"enabled": enabled}, nil, 5*time.Second, false)
		return err
	}
	// installFenceState reads the current admission fence so the installer
	// restores the prior state — including a pre-existing manual fence —
	// instead of blindly unfencing.
	installFenceState = func(ctx context.Context, port int) (bool, error) {
		o, err := requestObject(ctx, "GET",
			"http://127.0.0.1:"+strconv.Itoa(port)+"/control/fence", nil, nil, 5*time.Second, false)
		if err != nil {
			return false, err
		}
		enabled, ok := o["fenced"].(bool)
		if !ok {
			return false, fail("The controller fence state is unreadable.")
		}
		return enabled, nil
	}
	// installUnitState reads the controller unit's presence, enabled and
	// active state on the head. is-enabled exit code 1 means disabled and 4
	// means absent; is-active exit code 3 means inactive and 4 means
	// unknown to systemd. Anything else is an error, never a guess.
	installUnitState = func(ctx context.Context, unit string) (present, enabled, active bool, err error) {
		r, runErr := installRun(ctx, []string{"sudo", "-n", "systemctl", "is-enabled", unit}, nil)
		if runErr != nil {
			return false, false, false, runErr
		}
		switch r.Code {
		case 0:
			present, enabled = true, true
		case 1:
			present, enabled = true, false
		case 4:
		default:
			return false, false, false, fail("The controller unit state is unknown.")
		}
		r, runErr = installRun(ctx, []string{"sudo", "-n", "systemctl", "is-active", unit}, nil)
		if runErr != nil {
			return false, false, false, runErr
		}
		switch r.Code {
		case 0:
			active = true
		case 3, 4:
		default:
			return false, false, false, fail("The controller unit state is unknown.")
		}
		return present, enabled, active, nil
	}
	installWaitHTTP = waitHTTP
	installIdle     = func(ctx context.Context, url string) error {
		return waitComfyIdle(ctx, url, 0)
	}
	installGuardState = func(ctx context.Context, url string) (bool, error) {
		o, err := requestObject(ctx, "GET", url+"/sparky/state", nil, nil, 3*time.Second, false)
		if err != nil {
			return false, err
		}
		return num(o["version"]) == 1, nil
	}
	installDrainActivity = func(ctx context.Context, rt installationRuntime) error {
		return runtimeActivityIdle(ctx, rt.(*Runtime))
	}
)

// installBaselineUnitState reads one unit's exact state from the device
// baseline. A missing unit, an empty state, or a non-string entry is
// unknown and is an error — never silently treated as idle or inactive.
func installBaselineUnitState(before Object, node, unit string) (string, error) {
	if unit == "" {
		return "", fail("The cluster recipe has no unit for %s.", node)
	}
	state := str(obj(obj(obj(before["nodes"])[node])["units"])[unit])
	if state == "" {
		return "", fail("The baseline state of %s on %s is unknown.", unit, node)
	}
	return state, nil
}

// installAdoptPlacement derives the actual running placement of one loaded
// model from the baseline's active units across every configured recipe. A
// recipe matches only when each of its placement nodes is active exactly
// when the baseline reports the model loaded there. Zero or multiple
// matches are ambiguous and abort — no placement is ever invented from
// desired state.
func installAdoptPlacement(before Object, m Object, actualNodes []string) (string, Object, error) {
	if len(actualNodes) == 0 {
		return "", nil, fail("The loaded model has no actual nodes in the baseline.")
	}
	matches := []string{}
	var units Object
	for key, sv := range obj(m["settings"]) {
		placementNodes, known := placements[key]
		if !known {
			continue
		}
		setUnits := obj(obj(sv)["units"])
		// The placement must cover every actually loaded node, otherwise
		// it cannot be the recipe the engine was started from.
		match := true
		for _, node := range actualNodes {
			if !contains(placementNodes, node) {
				match = false
				break
			}
		}
		for _, node := range placementNodes {
			if !contains(actualNodes, node) {
				// A placement node outside the actual nodes must not be
				// running the model's unit either.
				state, err := installBaselineUnitState(before, node, str(setUnits[node]))
				if err != nil {
					return "", nil, err
				}
				if state == "active" {
					match = false
					break
				}
				continue
			}
			state, err := installBaselineUnitState(before, node, str(setUnits[node]))
			if err != nil {
				return "", nil, err
			}
			if state != "active" {
				match = false
				break
			}
		}
		if match {
			matches = append(matches, key)
			units = setUnits
		}
	}
	if len(matches) != 1 {
		return "", nil, fail("The actual placement of the loaded model is ambiguous. No running record was written.")
	}
	return matches[0], units, nil
}

// installProtected extracts the protected Parakeet identity from a device
// snapshot. An incomplete baseline is an error: the installer never guesses
// what is running before its first mutation.
func installProtected(before Object) (map[string]Object, error) {
	containers := obj(obj(obj(obj(before["nodes"])["spark02"])["containers"]))
	out := map[string]Object{}
	for _, name := range []string{"parakeet-engine", "parakeet-gateway"} {
		c := obj(containers[name])
		if str(c["id"]) == "" || str(c["started_at"]) == "" {
			return nil, fail("Protected Parakeet identity is unknown in the device baseline.")
		}
		out[name] = copyObject(c)
	}
	return out, nil
}

// installProtectedChanged reports whether any protected container identity
// (running state, container ID, or start time) moved between snapshots.
func installProtectedChanged(before map[string]Object, after Object) bool {
	containers := obj(obj(obj(obj(after["nodes"])["spark02"])["containers"]))
	for _, name := range []string{"parakeet-engine", "parakeet-gateway"} {
		was := before[name]
		now := obj(containers[name])
		if !boolean(now["running"]) ||
			str(now["id"]) != str(was["id"]) ||
			str(now["started_at"]) != str(was["started_at"]) {
			return true
		}
	}
	return false
}

// installDaemonReload runs systemctl daemon-reload on the head and worker.
func installDaemonReload(ctx context.Context, cfg Object) error {
	if r, err := installRun(ctx, []string{"sudo", "-n", "systemctl", "daemon-reload"}, nil); err != nil || r.Code != 0 {
		return fail("The local systemd reload did not succeed.")
	}
	host := str(obj(obj(cfg["nodes"])["spark02"])["ssh"])
	if err := safeHost(host); err != nil {
		return err
	}
	if r, err := installRun(ctx, append(deploymentSSHArgs(host), "sudo -n systemctl daemon-reload"), nil); err != nil || r.Code != 0 {
		return fail("The worker systemd reload did not succeed.")
	}
	return nil
}

// guardRestart records one guard activation whose prior engine state was
// active, so a rollback can restore the engine exactly after the old files
// are back and confirmed idle.
type guardRestart struct {
	node, unit, url string
}

// InstallHead ports the source install_head: it installs the Go-native
// controller, inactive model template, ComfyUI guard and drop-ins, the
// manager compatibility hook and the inactive Qwen dual recipe, then
// connects the gateway route. The original runtime baseline is snapshotted
// before the first mutation, the protected Parakeet identity is
// re-confirmed afterwards, and the prior admission fence state — including
// a pre-existing manual fence — is restored on success and on every
// failure. Inactive ComfyUI engines are never waited on, restarted or
// enabled: their files and drop-ins are installed for a future start. No
// engine is enabled or autoloaded, the lifecycle mode is never switched,
// and queued ComfyUI work on an active engine always completes before any
// guard restart. On failure the journal restores every prior file state,
// the controller and guard service states are restored, the gateway undo
// runs, and the fence returns to its prior state — every failure reported.
func InstallHead(stageDir string) (Object, error) {
	// One bounded context for the whole installation.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if err := validateCleanAbs("stage directory", stageDir); err != nil {
		return nil, err
	}
	staged, err := readCluster(filepath.Join(stageDir, "models.json"))
	if err != nil {
		return nil, err
	}
	root := str(staged["root"])
	if !filepath.IsAbs(root) {
		return nil, fail("The staged cluster root must be an absolute path.")
	}
	configPath := filepath.Join(root, "config", "models.json")
	// The installed central configuration, when present, is the authority;
	// a root disagreement is aborted exactly like the source.
	var central Object
	if _, statErr := os.Stat(configPath); statErr == nil {
		if central, err = readCluster(configPath); err != nil {
			return nil, err
		}
		if str(central["root"]) != root {
			return nil, fail("The installed cluster root differs from the staged root. No services were changed.")
		}
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	}
	user := filepath.Base(home())
	helper := filepath.Join(home(), ".local", "bin", "sparky")
	plan, err := BuildInstallationPlan(stageDir, central, user, helper)
	if err != nil {
		return nil, err
	}
	cfg := plan.Config
	port := plan.ControllerPort
	controllerUnit := filepath.Base(plan.ControllerUnit.Dest)
	controllerDir := filepath.Join(root, "controller")

	// The original containers and unit states are the preservation
	// baseline; they must be known before the first mutation. An unknown
	// controller or ComfyUI unit state aborts before any write.
	rt := installRuntime(cfg, configPath)
	before, err := rt.snapshot(ctx, true)
	if err != nil {
		return nil, fail("The device baseline could not be read.")
	}
	protected, err := installProtected(before)
	if err != nil {
		return nil, err
	}
	_, controllerEnabled, controllerActive, err := installUnitState(ctx, controllerUnit)
	if err != nil {
		return nil, fail("The controller unit state could not be read before the installation.")
	}
	for _, n := range plan.Nodes {
		meta := obj(obj(cfg["nodes"])[n.Node])
		comfyUnit := str(meta["comfy_unit"])
		// The head unit state is probed locally; a worker unit is never
		// probed through the head's own systemctl — that would query the
		// wrong host. Every node's proof of its actual ComfyUI state comes
		// from the per-node baseline instead: an unknown baseline state
		// aborts before any mutation, and an inactive engine is preserved.
		if n.Node == "spark01" {
			if _, _, _, probeErr := installUnitState(ctx, comfyUnit); probeErr != nil {
				return nil, fail("The %s ComfyUI unit state on %s is unknown.", comfyUnit, n.Node)
			}
		}
		if _, err := installBaselineUnitState(before, n.Node, comfyUnit); err != nil {
			return nil, err
		}
	}

	journal := installNewJournal(cfg, stageDir)
	backup := filepath.Join(controllerDir, "backups", "install-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	fenced := false
	fenceKnown := false
	priorFenced := false
	controllerActivated := false
	var gatewayUndo func() error
	var guardRestarts []guardRestart

	// restoreServices brings the controller and any restarted ComfyUI
	// engine back to their exact prior states with fresh bounded contexts.
	// Every attempt runs even when an earlier one fails; the combined
	// problems are reported, never swallowed.
	restoreServices := func() []string {
		var problems []string
		svcCtx, svcCancel := context.WithTimeout(context.Background(), time.Minute)
		defer svcCancel()
		if controllerActivated {
			if !controllerActive {
				if r, err := installRun(svcCtx, []string{"sudo", "-n", "systemctl", "stop", controllerUnit}, nil); err != nil || r.Code != 0 {
					problems = append(problems, "the controller service could not be stopped")
				}
			}
			if !controllerEnabled {
				if r, err := installRun(svcCtx, []string{"sudo", "-n", "systemctl", "disable", controllerUnit}, nil); err != nil || r.Code != 0 {
					problems = append(problems, "the controller service could not be disabled")
				}
			}
		}
		if controllerActivated || len(guardRestarts) > 0 {
			if err := installDaemonReload(svcCtx, cfg); err != nil {
				problems = append(problems, "the systemd reload after restore did not succeed")
			}
		}
		// Only an engine that was active before its guard activation is
		// restarted, and only after the old files are confirmed drained.
		for _, g := range guardRestarts {
			gCtx, gCancel := context.WithTimeout(context.Background(), 5*time.Minute)
			if err := installIdle(gCtx, g.url); err != nil {
				problems = append(problems, "the "+g.node+" ComfyUI queue did not confirm idle during restore")
				gCancel()
				continue
			}
			if err := rt.unit(gCtx, g.node, "restart", g.unit); err != nil {
				problems = append(problems, "the "+g.node+" ComfyUI engine could not be restarted")
			}
			gCancel()
		}
		return problems
	}

	// restore rolls the journal back, restores service states, undoes the
	// gateway connection, and returns the fence to its exact prior state.
	// The fence is held through the whole rollback; the prior-state restore
	// happens last with a fresh bounded context.
	restore := func(cause error) error {
		var problems []string
		if rberr := journal.Rollback(); rberr != nil {
			problems = append(problems, "rollback: "+rberr.Error())
		}
		problems = append(problems, restoreServices()...)
		if gatewayUndo != nil {
			if uerr := gatewayUndo(); uerr != nil {
				problems = append(problems, uerr.Error())
			}
		}
		if fenced || fenceKnown {
			unfenceCtx, unfenceCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer unfenceCancel()
			if ferr := installFence(unfenceCtx, port, priorFenced); ferr != nil {
				problems = append(problems, "the controller admission fence could not be restored")
			}
		}
		if len(problems) > 0 {
			return fail("%v Restoration needs attention: %s.", cause, strings.Join(problems, "; "))
		}
		return cause
	}
	write := func(node, path string, data []byte, mode os.FileMode) {
		if err == nil {
			err = journal.WriteFile(ctx, node, path, data, mode)
		}
	}
	writeUnit := func(node, path, text string) {
		if err == nil {
			err = journal.WriteUnit(ctx, node, path, text)
		}
	}

	// Durable private backups: the exact baseline snapshot.
	write("spark01", filepath.Join(backup, "services-before.json"), jsonBytes(before), 0o600)
	// The central config is written only on a fresh install; the existing
	// file stays byte-for-byte untouched.
	if central == nil {
		write("spark01", configPath, jsonBytes(cfg), 0o600)
	}

	for _, n := range plan.Nodes {
		write(n.Node, n.NodeFileDest, n.NodeFileData, 0o600)
		write(n.Node, n.PluginDest, jsonBytes(n.PluginConfig), 0o600)
		write(n.Node, n.GuardDest, n.GuardData, 0o644)
		writeUnit(n.Node, n.DropinDest, n.DropinText)
	}
	for _, node := range []string{"spark01", "spark02"} {
		writeUnit(node, plan.ControllerUnit.Dest, plan.ControllerUnit.Text)
		writeUnit(node, plan.ModelUnitTemplate.Dest, plan.ModelUnitTemplate.Text)
	}
	// The manager hook is replaced with the compatibility shim; the original
	// bytes are kept in the backup.
	hook := filepath.Join(root, "manager", "cluster-control.sh")
	if previous, readErr := os.ReadFile(hook); readErr == nil {
		write("spark01", filepath.Join(backup, "cluster-control-before"), previous, 0o600)
	} else if !os.IsNotExist(readErr) {
		err = readErr
	}
	write("spark01", hook, []byte(plan.LegacyShim.Text), 0o755)
	if err == nil {
		err = installPrepareQwen(ctx, cfg, stageDir, journal)
	}
	if err == nil {
		err = installDaemonReload(ctx, cfg)
	}
	if err == nil {
		controllerActivated = true
		r, runErr := installRun(ctx, []string{"sudo", "-n", "systemctl", "enable", "--now", controllerUnit}, nil)
		if runErr != nil || r.Code != 0 {
			err = fail("The controller service could not be enabled.")
		}
	}
	if err == nil {
		if _, err = installWaitHTTP(ctx, "http://127.0.0.1:"+strconv.Itoa(port)+"/health", 60*time.Second); err != nil {
			err = fail("The controller service did not become ready.")
		}
	}
	if err != nil {
		return nil, restore(err)
	}

	// The prior fence state is read before the fence is raised, so a
	// pre-existing manual fence survives the installation unchanged.
	prior, err := installFenceState(ctx, port)
	if err != nil {
		return nil, restore(fail("The controller fence state could not be read."))
	}
	priorFenced, fenceKnown = prior, true
	// Hold new admissions while the guarded changes land; every failure
	// below restores the prior fence with a fresh context. The flag is set
	// before the attempt so a partially engaged fence is always restored.
	fenced = true
	if err = installFence(ctx, port, true); err != nil {
		return nil, restore(fail("The controller admission fence could not be engaged."))
	}

	// The guard is installed on an active engine only after the queue
	// confirms idle three consecutive times, so queued work always
	// completes; nothing is forcibly cancelled. An engine that is inactive
	// in the baseline keeps running nothing: its files and drop-in are
	// already installed for its future start, and it is never waited on,
	// restarted or enabled.
	for _, n := range plan.Nodes {
		meta := obj(obj(cfg["nodes"])[n.Node])
		comfyURL := strings.TrimSuffix(str(meta["comfy_url"]), "/")
		state, _ := installBaselineUnitState(before, n.Node, str(meta["comfy_unit"]))
		if state != "active" {
			continue
		}
		guarded, _ := installGuardState(ctx, comfyURL)
		if !guarded {
			if err = installIdle(ctx, comfyURL); err != nil {
				return nil, restore(fail("ComfyUI did not confirm idle in time."))
			}
			// The restart intent is recorded before the attempt so a
			// partial activation is always restored.
			guardRestarts = append(guardRestarts, guardRestart{node: n.Node, unit: str(meta["comfy_unit"]), url: comfyURL})
			if err = rt.unit(ctx, n.Node, "restart", str(meta["comfy_unit"])); err != nil {
				return nil, restore(fail("The ComfyUI guard could not be activated."))
			}
			if _, err = installWaitHTTP(ctx, comfyURL+"/sparky/state", 180*time.Second); err != nil {
				return nil, restore(fail("The ComfyUI guard did not become ready."))
			}
			// The guard is only counted as active once its state endpoint
			// reports the exact accepted version.
			confirmed, err := installGuardState(ctx, comfyURL)
			if err != nil || !confirmed {
				return nil, restore(fail("The ComfyUI guard did not report version 1."))
			}
		}
	}
	if err != nil {
		return nil, restore(err)
	}

	// Parity: legacy running placements are recorded from the actual
	// baseline, never manufactured from desired placement. An existing
	// record is preserved exactly: it is never overwritten. A new record is
	// adopted only for a model that is actually loaded right now, with an
	// unambiguous mapping from the baseline's active units; anything else
	// aborts. A model that is not loaded gets no record at all.
	beforeModels := obj(before["models"])
	for name, v := range obj(cfg["models"]) {
		m := obj(v)
		if str(m["kind"]) != "llm" {
			continue
		}
		row := obj(beforeModels[name])
		if !boolean(row["loaded"]) {
			continue
		}
		recordPath := filepath.Join(controllerDir, "running-"+name+".json")
		if _, statErr := os.Stat(recordPath); statErr == nil {
			continue
		} else if !os.IsNotExist(statErr) {
			return nil, restore(statErr)
		}
		actualNodes := stringsOf(row["nodes"])
		placement, units, adoptErr := installAdoptPlacement(before, m, actualNodes)
		if adoptErr != nil {
			return nil, restore(adoptErr)
		}
		write("spark01", recordPath, jsonBytes(Object{
			"nodes":     copyValue(row["nodes"]),
			"units":     copyValue(units),
			"placement": placement,
		}), 0o600)
		if err != nil {
			return nil, restore(err)
		}
	}

	// The real gateway connection: the garuda provider points at the new
	// controller and the read-only catalog route is added. The undo is kept
	// for any later failure in this installation.
	var gateway Object
	gateway, gatewayUndo, err = installGateway(cfg, port, backup)
	if err != nil {
		return nil, restore(fail("The gateway route could not be connected."))
	}
	// Old requests that were already running keep their normal completion.
	if err = installDrainActivity(ctx, rt); err != nil {
		return nil, restore(err)
	}

	after, err := rt.snapshot(ctx, true)
	if err != nil {
		return nil, restore(fail("The device baseline could not be re-read."))
	}
	if installProtectedChanged(protected, after) {
		// The protected identity must be reported, never hidden: the best
		// effort restoration still runs, and every restore failure and the
		// unfence are reported alongside the original cause.
		return nil, restore(fail("Protected Parakeet identity changed unexpectedly."))
	}

	// The installation actually succeeded: the fence returns to its exact
	// prior state with a fresh bounded context. A failure here is fatal —
	// no installed result is persisted or returned.
	unfenceCtx, unfenceCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer unfenceCancel()
	if err = installFence(unfenceCtx, port, priorFenced); err != nil {
		return nil, fail("The installation completed but the controller admission fence could not be restored to its prior state.")
	}

	result := Object{
		"status":                 "installed",
		"config_path":            configPath,
		"backup":                 backup,
		"parakeet_preserved":     true,
		"manager_not_restarted":  true,
		"comfy_guards_installed": true,
		"gateway":                gateway,
	}
	write("spark01", filepath.Join(controllerDir, "installation-result.json"), jsonBytes(result), 0o600)
	if err != nil {
		return nil, restore(err)
	}
	return result, nil
}
