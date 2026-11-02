package sparky

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// runtimeSelection is the COMPLETE placement a model really runs at: the
// model entry, the frozen recipe, the exact nodes and placement plus the
// backend identity that recipe binds. It is resolved from the durable
// lifecycle record under strong policy (validated record, enabled current
// model, matching llm kind and matching topology) — never trusted verbatim
// from a raw record and never read from the saved run_on alone.
type runtimeSelection struct {
	Model     Object
	Setting   Object
	Nodes     []string
	Placement string
	Record    Object // validated durable record for an Actual selection
	Actual    bool   // true when resolved from a loaded durable record
}

// loadedSelection resolves the ACTUAL loaded placement of name from its
// durable record. Only a READY (or in-progress queued/starting) record is
// considered, and the record's own placement, nodes, units and frozen
// recipe must all agree with the current config before it is used: a raw
// record is never trusted. The current model must be enabled and a
// language model under the current policy, with an unchanged node topology.
// When no valid loaded record exists the desired (saved run_on) selection
// is returned with Actual=false.
func (r *Runtime) loadedSelection(name string) (*runtimeSelection, error) {
	return r.loadedSelectionWith(r.config(), name)
}

func (r *Runtime) loadedSelectionWith(cfg Object, name string) (*runtimeSelection, error) {
	m, s, nodes, placement, err := selectPlacement(cfg, name, "")
	if err != nil {
		return nil, err
	}
	target := &runtimeSelection{Model: m, Setting: s, Nodes: nodes, Placement: placement}
	if !r.native() {
		return target, nil
	}
	if m == nil || str(m["kind"]) != "llm" || !boolean(value(m, "enabled", true)) {
		return target, nil
	}
	rec, err := r.readRecord(name)
	if err != nil {
		return target, nil
	}
	switch str(rec["state"]) {
	case StateReady, StateStarting, StateQueued:
	default:
		return target, nil
	}
	placement = str(rec["placement"])
	settings := obj(m["settings"])
	frozen, found := settings[placement]
	if !found || placements[placement] == nil {
		return target, nil
	}
	s = obj(frozen)
	if !boolean(value(s, "enabled", true)) {
		return target, nil
	}
	units := Object{}
	for _, node := range placements[placement] {
		unit := str(obj(s["units"])[node])
		if unit == "" || !serviceName.MatchString(unit) {
			return target, nil
		}
		units[node] = unit
	}
	recorded := stringsOf(rec["nodes"])
	if !sameStrings(recorded, placements[placement]) {
		return target, nil
	}
	recordUnits := obj(rec["units"])
	for node, unit := range units {
		if str(recordUnits[node]) != str(unit) {
			return target, nil
		}
	}
	// The record must be the very recipe and topology this config selects:
	// a matching fingerprint and a matching recorded node topology (ssh host
	// and direct IP per selected node, controller root).
	if str(rec["fingerprint"]) != lifecycleFingerprint(name, m, s, placements[placement], placement) {
		return target, nil
	}
	if !topologyMatches(obj(rec["config"]), cfg, placements[placement]) {
		return target, nil
	}
	return &runtimeSelection{Model: target.Model, Setting: s, Nodes: placements[placement], Placement: placement,
		Record: rec, Actual: true}, nil
}

// ready resolves the placement and dispatches to the mode-specific check.
// The native branch is the strong stage-1 readiness; the legacy branch keeps
// its historical health-200 shortcut unchanged.
func (r *Runtime) ready(ctx context.Context, name string, setting Object, nodes []string) bool {
	if r.native() {
		// The loaded placement is the ACTUAL validated selection of the
		// durable record, which may differ from the saved run_on after a
		// one-shot load or a pending run_on change. The nodes argument must
		// still be exactly the nodes this selection resolved: readiness is
		// never granted for a caller's invented node set.
		sel, err := r.loadedSelection(name)
		if err != nil || !sameStrings(sel.Nodes, nodes) {
			return false
		}
		return r.nativeReady(ctx, name, sel.Model, sel.Setting, sel.Nodes, sel.Placement)
	}
	path := filepath.Join(r.root(), "controller", "running-"+name+".json")
	record, err := readObject(path)
	if err != nil || !sameStrings(stringsOf(record["nodes"]), nodes) || !reflect.DeepEqual(obj(record["units"]), obj(setting["units"])) {
		return false
	}
	states, err := r.modelStates(ctx)
	return err == nil && states[name] == "ready"
}

// syncEqual reports whether the existing manager configuration already
// matches the desired sync semantically: proxy, concurrency, TTL zero and
// context per enabled model (compared as numbers, so 262144 and 262144.0
// are equal) plus the placement matrix sets (compared as membership sets,
// so routing labels and group ordering never trigger a HUP). All other
// manager/model fields are ignored: the sync never rewrites them.
func syncEqual(config, updated, cfg Object) bool {
	if num(config["globalTTL"]) != num(updated["globalTTL"]) {
		return false
	}
	if !sameSetMembership(matrixStrings(obj(value(config, "routing", Object{}))), matrixStrings(obj(value(updated, "routing", Object{})))) {
		return false
	}
	for _, name := range enabledLLMs(cfg) {
		want, ok := obj(updated["models"])[name]
		if !ok {
			return false
		}
		have, ok := obj(config["models"])[name]
		if !ok {
			return false
		}
		_, s, _, _, err := selectPlacement(cfg, name, "")
		if err != nil {
			return false
		}
		if str(obj(have)["proxy"]) != str(obj(want)["proxy"]) {
			return false
		}
		if num(obj(have)["concurrencyLimit"]) != num(obj(want)["concurrencyLimit"]) || num(obj(want)["concurrencyLimit"]) != num(s["concurrency"]) {
			return false
		}
		if num(obj(have)["ttl"]) != 0 {
			return false
		}
		if num(obj(obj(have)["capabilities"])["context"]) != num(s["context_tokens"]) {
			return false
		}
	}
	return true
}

// matrixStrings collects the canonical membership of every matrix set:
// model names sorted within a set, the set strings sorted overall.
func matrixStrings(routing Object) []string {
	out := []string{}
	for _, v := range obj(obj(obj(routing["router"])["settings"])["matrix"]) {
		parts := strings.Split(str(v), "&")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		sort.Strings(parts)
		out = append(out, strings.Join(parts, "&"))
	}
	sort.Strings(out)
	return out
}
func sameSetMembership(a, b []string) bool { return sameStrings(a, b) }

func (r *Runtime) syncLegacy(ctx context.Context) error {
	if r.native() {
		return nil
	}
	path := filepath.Join(r.root(), "manager/config.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var config Object
	if err = yaml.Unmarshal(before, &config); err != nil {
		return fail("Invalid manager configuration.")
	}
	updated := copyObject(config)
	cfg := r.config()
	models := obj(updated["models"])
	sets := Object{}
	group := 0
	names := enabledLLMs(cfg)
	paired := map[string]bool{}
	for i, a := range names {
		for _, b := range names[i+1:] {
			_, _, an, _, _ := selectPlacement(cfg, a, "")
			_, _, bn, _, _ := selectPlacement(cfg, b, "")
			if !overlapNodes(an, bn) {
				group++
				sets["placement_"+intText(group)] = a + " & " + b
				paired[a] = true
				paired[b] = true
			}
		}
	}
	for _, name := range names {
		if !paired[name] {
			group++
			sets["placement_"+intText(group)] = name
		}
		_, s, _, _, _ := selectPlacement(cfg, name, "")
		entry := obj(models[name])
		if len(entry) == 0 {
			return fail("Missing manager model configuration: %s.", name)
		}
		entry["proxy"] = s["backend_url"]
		entry["concurrencyLimit"] = s["concurrency"]
		entry["ttl"] = 0
		caps := obj(entry["capabilities"])
		caps["context"] = s["context_tokens"]
		entry["capabilities"] = caps
	}
	updated["globalTTL"] = 0
	updated["routing"] = Object{"router": Object{"use": "matrix", "settings": Object{"matrix": Object{"sets": sets}}}}
	// A semantic no-op (unchanged placement, TTL, concurrency or context)
	// must never restart the manager; label or ordering differences alone
	// do not count as change.
	if syncEqual(config, updated, cfg) {
		return nil
	}
	candidate, err := yaml.Marshal(updated)
	if err != nil {
		return err
	}
	// Fence admission and drain direct Sparky leases before the HUP: the
	// coordinator-owned callback blocks new reservations, waits for the
	// existing leases and always restores admission (the manual fenced
	// flag is never touched, only the reload's own flag is cleared).
	if r.DrainAdmission != nil {
		restore, err := r.DrainAdmission(ctx)
		if err != nil {
			return err
		}
		defer restore()
	}
	for {
		counts, err := r.legacyActivity(ctx)
		if err != nil {
			return err
		}
		busy := false
		for _, v := range counts {
			busy = busy || v > 0
		}
		if !busy {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	staged := filepath.Join(r.root(), "controller/manager-candidate.yaml")
	if err := atomicWrite(staged, candidate, 0600); err != nil {
		return err
	}
	// The original live file's mode is captured so the rollback restores
	// exactly what was there, not a default.
	originalMode := os.FileMode(0600)
	if info, statErr := os.Stat(path); statErr == nil {
		originalMode = info.Mode().Perm()
	}
	if _, err = r.runChecked(ctx, []string{filepath.Join(home(), ".local/bin/llama-swap"), "--config", staged, "--validate"}, nil, 30*time.Second); err != nil {
		return fail("Generated manager config failed validation. No live config was changed.")
	}
	backup := filepath.Join(r.root(), "controller/backups/manager-"+token()+".yaml")
	if err := atomicWrite(backup, before, originalMode); err != nil {
		return err
	}
	if err := atomicWrite(path, candidate, originalMode); err != nil {
		return err
	}
	if _, err = r.runChecked(ctx, []string{"sudo", "-n", "systemctl", "kill", "--kill-whom=main", "--signal=HUP", "garuda-models.service"}, nil, 30*time.Second); err != nil {
		// The rollback is an unconditional local file restore: it must not
		// depend on the (possibly cancelled or timed out) request context,
		// and a failed restore is reported loudly instead of swallowed.
		if restoreErr := atomicWrite(path, before, originalMode); restoreErr != nil {
			return fail("The manager reload failed (%v) and the live config could not be restored: %v. Manual recovery is required.", err, restoreErr)
		}
		return err
	}
	r.invalidate()
	return nil
}
func (r *Runtime) startModel(ctx context.Context, name string) error {
	if !r.native() {
		_, err := r.legacyManager(ctx, "/upstream/"+name+"/health", "GET")
		r.invalidate()
		return err
	}
	// Callers without an explicit reservation freeze the recipe from the
	// current runtime config once; reservation-aware callers (the
	// coordinator) pass their frozen config to startModelSelected directly.
	return r.startModelSelected(ctx, name, r.config())
}

// startModelSelected is the native parent start path. Everything is frozen
// from the caller's immutable config snapshot: the selected cfg/model/
// setting/nodes/placement at reservation time drive the whole operation and
// a later Refresh or LLM wire change can never redirect the recipe.
//
// Flow: a matching READY record with strong readiness returns without any
// inventory probe, start or restart; a matching queued/starting record with
// an active or activating supervisor is adopted under the same generation
// and waited (never a duplicate start); a new generation is queued under
// the record lock BEFORE reset-failed/start --no-block. Start errors and
// failures write failed only under a generation check, and a superseded
// generation fails promptly instead of waiting out a stale health timeout.
func (r *Runtime) startModelSelected(ctx context.Context, name string, cfg Object) error {
	if !simpleName.MatchString(name) {
		return fail("Invalid model name.")
	}
	m, s, nodes, placement, err := selectPlacement(cfg, name, "")
	if err != nil {
		return err
	}
	// One operation cannot select a different controller root: the frozen
	// record paths and probes are rooted at the runtime's own controller
	// root, so a selected cfg with a different root is refused BEFORE any
	// clone, record read, probe or write (it would otherwise be treated as
	// a fresh topology and try to create records on the unowned path).
	if str(cfg["root"]) != r.root() {
		return fail("%s's operation was written for a different controller root; clean it up first.", name)
	}
	// A frozen runtime clone carries the deep-copied selected cfg through
	// the whole operation: Begin/wait/probes can never be redirected by a
	// later Refresh or LLM wire change to the base runtime's config, while
	// the Run/Probe seams and coordinator wiring stay intact.
	fr := r.frozenClone(cfg)
	// Fastpath: a matching READY record with strong readiness needs no
	// start, restart or full hardware inventory.
	if rec, e := fr.readRecord(name); e == nil && str(rec["state"]) == StateReady &&
		str(rec["fingerprint"]) == lifecycleFingerprint(name, m, s, nodes, placement) &&
		fr.nativeReady(ctx, name, m, s, nodes, placement) {
		fr.invalidate()
		r.invalidate()
		return nil
	}
	// Adopt a matching in-progress operation when its supervisor is alive:
	// the actual unit start belongs to that supervisor, never a duplicate.
	// The frozen record topology must also match the caller's frozen
	// config: a recipe fingerprint alone omits the ssh host, direct IP and
	// controller root, so an edited host must never be adopted and waited
	// out to the health timeout.
	if rec, e := fr.readRecord(name); e == nil &&
		(str(rec["state"]) == StateQueued || str(rec["state"]) == StateStarting) &&
		str(rec["fingerprint"]) == lifecycleFingerprint(name, m, s, nodes, placement) {
		if !topologyMatches(obj(rec["config"]), cfg, nodes) {
			return fail("%s's operation was written for a different host topology; clean it up first.", name)
		}
		if active, aerr := fr.supervisorActive(ctx, name); aerr == nil && active {
			err := fr.waitNativeReady(ctx, name, m, s, nodes, placement, str(rec["generation"]))
			r.invalidate()
			return err
		}
		// The previous owner is not running: its ownership is released and
		// the same recipe may proceed with a fresh (or adopted) generation.
	}
	rec, err := fr.BeginOperation(name, m, s, nodes, placement)
	if err != nil {
		return err
	}
	generation := str(rec["generation"])
	// The durable queued generation exists before any systemctl invocation.
	x, resetErr := fr.execute(ctx, []string{"sudo", "-n", "systemctl", "reset-failed", "sparky-model@" + name + ".service"}, nil, 15*time.Second)
	if resetErr != nil || x.Code != 0 {
		// A never-started inactive instance is garbage collected: reset-failed
		// exits 1 with "not loaded". That must not block the first start when
		// a FRESH KEYED show of the supervisor proves the installed template
		// is loaded and the instance inactive (no failed state to clear). Any
		// other reset failure, an unknown unit or a non-inactive state stays
		// fatal — English stderr is never parsed, only the keyed properties.
		proceed := false
		if resetErr == nil && x.Code == 1 {
			load, active, perr := nativeRankStates(ctx, fr, "spark01", "sparky-model@"+name+".service")
			proceed = perr == nil && load == "loaded" && active == "inactive"
		}
		if !proceed {
			fr.markNativeFailed(name, generation)
			if resetErr != nil {
				return resetErr
			}
			return fail("Cannot clear the failed state of sparky-model@%s.service (exit %d); refusing to start.", name, x.Code)
		}
	}
	if _, err := fr.runChecked(ctx, []string{"sudo", "-n", "systemctl", "start", "--no-block", "sparky-model@" + name + ".service"}, nil, 15*time.Second); err != nil {
		fr.markNativeFailed(name, generation)
		return err
	}
	err = fr.waitNativeReady(ctx, name, m, s, nodes, placement, generation)
	// The base runtime's snapshot cache must not outlive the operation:
	// the frozen clone only invalidates its own cache.
	r.invalidate()
	return err
}

// frozenClone returns a new runtime whose Config is a deep copy of cfg and
// which preserves the original runtime's Run/Probe seams and coordinator
// wiring. The base runtime is never mutated.
func (r *Runtime) frozenClone(cfg Object) *Runtime {
	fr := newRuntime(copyObject(cfg), r.Path)
	fr.Run = r.Run
	fr.ProbeNode = r.ProbeNode
	fr.DrainAdmission = r.DrainAdmission
	fr.owner = r.owner
	return fr
}

// stopOwnGeneration stops the supervisor of exactly this captured
// generation with a fresh bounded context, then its recorded rank units.
// The generation is CASed to stopping FIRST: a superseded generation owns
// nothing and must never stop a newer supervisor or its new units.
func (r *Runtime) stopOwnGeneration(ctx context.Context, name, generation string) error {
	if err := r.TransitionState(name, generation, StateStopping); err != nil {
		return err
	}
	if _, err := r.runChecked(ctx, []string{"sudo", "-n", "systemctl", "stop", "sparky-model@" + name + ".service"}, nil, 4*time.Minute); err != nil {
		return err
	}
	return r.stopNativeUnits(ctx, name, generation)
}

// markNativeFailed records a failed verdict for exactly this generation; a
// superseding operation is never overwritten.
func (r *Runtime) markNativeFailed(name, generation string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = r.TransitionState(name, generation, StateFailed)
	_ = r.stopNativeUnits(cleanupCtx, name, generation)
}

// stopNativeUnits stops only the units OWNED by a generation — the ranks
// whose start was actually attempted (or all recorded units once the record
// reached ready) — generation-checked: the actual stop of a newer generation
// belongs to its own owner, and a preflight failure never stops unowned
// units. Effects run through the record's frozen config clone so edited,
// disabled or deleted recipes cannot redirect the stop.
func (r *Runtime) stopNativeUnits(ctx context.Context, name, generation string) error {
	rec, err := r.readRecord(name)
	if err != nil {
		return fail("A newer lifecycle operation owns the model; its owner will stop it.")
	}
	if str(rec["generation"]) != generation {
		return fail("A newer lifecycle operation owns the model; its owner will stop it.")
	}
	owned := ownedUnits(rec)
	if len(owned) == 0 {
		// A preflight failure owns nothing: zero stop commands.
		return nil
	}
	frozen := obj(rec["config"])
	if frozen == nil {
		return fail("The lifecycle record has no frozen config snapshot; refusing to stop.")
	}
	return nativeStopRanks(r.frozenClone(frozen), owned)
}

// waitNativeReady waits for THIS generation to become strongly ready. A
// superseded generation, a failed verdict or a dead supervisor fails
// promptly; context cancellation owns only its own generation's cleanup.
func (r *Runtime) waitNativeReady(ctx context.Context, name string, m, s Object, nodes []string, placement, generation string) error {
	deadline := time.Now().Add(durationSeconds(value(s, "health_timeout_seconds", r.config()["health_timeout_seconds"]), 1800))
	started := time.Now()
	// A systemd start is asynchronous: a supervisor that has not yet entered
	// activating is not dead. Only an inactive supervisor AFTER this grace
	// period is a prompt verdict; a failed supervisor always is.
	grace := 10 * time.Second
	for {
		if rec, e := r.readRecord(name); e == nil {
			state := str(rec["state"])
			if str(rec["generation"]) != generation {
				return fail("A newer lifecycle operation superseded this model start.")
			}
			if state == StateFailed {
				return fail("The model failed to start. Check its supervisor log.")
			}
			if state == StateReady && r.nativeReady(ctx, name, m, s, nodes, placement) {
				r.invalidate()
				return nil
			}
		}
		// A dead or failed supervisor is a prompt verdict, never a stale
		// health wait to the full timeout.
		if state, e := r.supervisorState(ctx, name); e == nil && (state == "failed" || (state == "inactive" && time.Since(started) > grace)) {
			r.markNativeFailed(name, generation)
			return fail("The model supervisor is %s. Check its log.", state)
		}
		if time.Now().After(deadline) {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			stopErr := r.stopOwnGeneration(cleanupCtx, name, generation)
			r.markNativeFailed(name, generation)
			cancel()
			if stopErr != nil {
				return fail("Model readiness timed out and cleanup needs attention.")
			}
			return fail("Model readiness timed out; started services were stopped.")
		}
		select {
		case <-ctx.Done():
			// Cancellation owns this generation's cleanup only: the captured
			// generation is CASed to stopping (a superseded generation owns
			// nothing and never stops a newer supervisor), then the OWN
			// supervisor is stopped with a fresh bounded context before the
			// recorded rank units; a later supervisor handles any further
			// actual stops and a newer generation is never blindly
			// overwritten.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			stopErr := r.stopOwnGeneration(cleanupCtx, name, generation)
			r.markNativeFailed(name, generation)
			cancel()
			if stopErr != nil {
				return fail("Model startup was cancelled and cleanup needs attention.")
			}
			return fail("Model startup was cancelled; started services were stopped.")
		case <-time.After(500 * time.Millisecond):
		}
	}
}
func (r *Runtime) actualModel(name string) (Object, error) {
	if !simpleName.MatchString(name) {
		return nil, fail("Invalid model name.")
	}
	if r.native() {
		// The native branch MUST read a valid durable record. A missing or
		// corrupt record is an error with zero stops: the desired config is
		// never a fallback, because only the record knows which units
		// actually own devices and which hosts they live on.
		rec, err := r.readRecord(name)
		if err != nil {
			return nil, fail("No valid lifecycle record for %s exists; nothing was stopped. (%v)", name, err)
		}
		return rec, nil
	}
	path := filepath.Join(r.root(), "controller", "running-"+name+".json")
	info, err := readObject(path)
	if err == nil {
		return info, nil
	}
	if !os.IsNotExist(err) {
		return nil, fail("Cannot read the model's actual placement. No service was stopped.")
	}
	_, s, nodes, _, err := selectPlacement(r.config(), name, "")
	if err != nil {
		return nil, err
	}
	return Object{"nodes": nodes, "units": s["units"]}, nil
}
func (r *Runtime) stopUnits(ctx context.Context, info Object) error {
	failed := []string{}
	for _, node := range stringsOf(info["nodes"]) {
		unit := str(obj(info["units"])[node])
		if !serviceName.MatchString(unit) {
			failed = append(failed, node)
			continue
		}
		if err := r.unit(ctx, node, "stop", unit); err != nil {
			failed = append(failed, node)
		}
	}
	if len(failed) > 0 {
		return fail("Could not confirm model shutdown on: %s.", strings.Join(failed, ", "))
	}
	return nil
}

// stopModel stops a native model from the PARENT (controller) side. It acts
// only on a valid durable record: the record's frozen config, units, nodes
// and generation drive every effect, so a recipe edited, disabled or deleted
// after the start can never redirect the stop. A missing or corrupt record
// is an error with zero stops. The generation is CASed to stopping BEFORE
// any physical stop; a newer generation is refused before any command. The
// supervisor is stopped first with a fresh bounded context that survives
// the caller's cancellation, so an independent supervisor can never continue
// (or warm-restart) after the caller leaves.
func (r *Runtime) stopModel(ctx context.Context, name string) error {
	if !r.native() {
		_, err := r.legacyManager(ctx, "/api/models/unload/"+name, "POST")
		r.invalidate()
		return err
	}
	info, err := r.actualModel(name)
	if err != nil {
		return err
	}
	gen := str(info["generation"])
	owned := ownedUnits(info)
	// Release PROOF is read-only and must cover EVERY recorded unit — not
	// just the attempted subset — using the frozen hosts: an unattempted
	// worker that is still active can never yield a confirmed release. The
	// physical stops below remain the attempted subset ONLY; this proof
	// never stops a preexisting unowned process, it refuses a false
	// "stopped".
	proof := obj(info["units"])
	frozen := obj(info["config"])
	if frozen == nil {
		return fail("The lifecycle record has no frozen config snapshot; refusing to stop.")
	}
	// The frozen clone preserves the Run/Probe seams and coordinator wiring
	// while pinning every later probe and ssh host to the record's immutable
	// config snapshot; the base runtime is never mutated.
	fr := r.frozenClone(frozen)
	switch str(info["state"]) {
	case StateReady, StateStarting, StateQueued, StateFailed:
		// CAS first: a stale generation is refused before any effect.
		if err := fr.TransitionState(name, gen, StateStopping); err != nil {
			return err
		}
	case StateStopping:
		// Idempotent re-stop of the same generation.
	case StateStopped:
		// Idempotent repeat unload of the SAME generation: success only
		// with PROOF the recorded generation is actually released — every
		// owned rank inactive/loaded, the frozen backend port refused and
		// the supervisor gone. No stop command is ever issued and an
		// unknown or unconfirmed generation is never a blind success.
		confirmCtx, confirmCancel := context.WithTimeout(context.WithoutCancel(context.Background()), 2*time.Minute)
		defer confirmCancel()
		if err := fr.confirmNativeRelease(confirmCtx, name, gen, proof); err != nil {
			return fail("%s's recorded generation is not confirmed released; refusing blind success: %v", name, err)
		}
		fr.invalidate()
		r.invalidate()
		return nil
	default:
		return fail("%s has no operation to stop.", name)
	}
	// A superseded generation must never command the NEW generation's units:
	// the record is re-checked after the CAS and before each stop stage.
	current, err := fr.readRecord(name)
	if err != nil {
		return err
	}
	if str(current["generation"]) != gen {
		return fail("A newer lifecycle operation superseded this stop.")
	}
	// The supervisor stop uses a fresh bounded context even when the caller
	// is gone or canceled: the physical stop must finish regardless.
	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(context.Background()), 4*time.Minute)
	defer stopCancel()
	_, supErr := fr.runChecked(stopCtx, []string{"sudo", "-n", "systemctl", "stop", "sparky-model@" + name + ".service"}, nil, 4*time.Minute)
	// Every recorded rank attempt is stopped INDEPENDENTLY with its own
	// fresh bounded context, so one hung head can never skip a worker.
	var rankErr error
	if len(owned) > 0 {
		if current, err = fr.readRecord(name); err != nil {
			return err
		}
		if str(current["generation"]) != gen {
			return fail("A newer lifecycle operation superseded this stop.")
		}
		rankErr = nativeStopRanks(fr, owned)
	}
	confirm := func() error {
		confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 2*time.Minute)
		defer cancel()
		// The release proof is read-only and covers EVERY recorded unit:
		// only the supervisor and the frozen backend port are additionally
		// confirmed. No probe ever stops anything.
		return fr.confirmNativeRelease(confirmCtx, name, gen, proof)
	}
	if supErr != nil || rankErr != nil {
		// The plain stop did not finish cleanly: invoke the REAL owned-
		// generation cleanup through the actual native stop helper child.
		// The generation is re-checked before the fallback so a superseded
		// stop never runs the helper against a newer operation.
		current, err = fr.readRecord(name)
		if err != nil {
			return err
		}
		if str(current["generation"]) != gen {
			return fail("A newer lifecycle operation superseded this stop.")
		}
		fallbackErr := fr.nativeGenerationFallback(name)
		if fallbackErr != nil {
			supErr = fail("%v; the owned-generation cleanup also failed: %v", supErr, fallbackErr)
		}
	}
	if err := confirm(); err != nil {
		// A failed cleanup is owned and blocks reuse until the release is
		// verified; the failed verdict is CASed so a newer generation is
		// never overwritten. Remaining ownership is reported honestly and
		// the model is never claimed stopped or reusable.
		reason := err.Error()
		if supErr != nil {
			reason = supErr.Error() + "; " + reason
		}
		if casErr := fr.TransitionState(name, gen, StateFailed); casErr != nil {
			return fail("%v The failed state could not be recorded: %v.", reason, casErr)
		}
		return fail("The model stop is incomplete and ownership is retained: %s.", reason)
	}
	// No blind record rewrite: only a record that is still ours and still
	// stopping is CASed to stopped (the supervisor's own ExecStop or the
	// fallback helper may already have completed it).
	current, err = fr.readRecord(name)
	if err != nil {
		return err
	}
	if str(current["generation"]) == gen && str(current["state"]) == StateStopping {
		if err := fr.TransitionState(name, gen, StateStopped); err != nil {
			return err
		}
	}
	fr.invalidate()
	r.invalidate()
	return nil
}

// confirmNativeRelease refuses to report released until every GIVEN rank
// unit is a LOADED unit with an inactive/failed ActiveState, the exact
// frozen backend port answers ECONNREFUSED and the model supervisor is
// inactive or failed. Unknown probes never count as released. The physical
// stop callers pass the attempted subset; the parent stop proof passes ALL
// recorded units — proof is read-only and never stops anything.
func (r *Runtime) confirmNativeRelease(ctx context.Context, name, gen string, units Object) error {
	if err := nativeConfirmStopped(ctx, r, name, gen, keys(units), units); err != nil {
		return err
	}
	state, err := r.supervisorState(ctx, name)
	if err != nil {
		return fail("Cannot confirm the model supervisor state.")
	}
	if !contains([]string{"inactive", "failed"}, state) {
		return fail("The model supervisor is still %s.", state)
	}
	return nil
}

// nativeGenerationFallback invokes the REAL native stop helper as a child
// process for exactly this generation: `env -u INVOCATION_ID sparky --json
// node lifecycle --file CONFIG --model NAME --action stop`. The controller
// process itself has a different INVOCATION_ID than the model unit, so the
// helper's stale-invocation guard must not inherit it; the helper re-checks
// the durable generation itself, so a superseded operation is refused. The
// helper runs through the Run seam: tests fake it and no untracked live
// child is ever spawned.
func (r *Runtime) nativeGenerationFallback(name string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	fallbackCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 4*time.Minute)
	defer cancel()
	_, err = r.runChecked(fallbackCtx, []string{
		"env", "-u", "INVOCATION_ID", exe,
		"--json", "node", "lifecycle", "--file", r.Path, "--model", name, "--action", "stop",
	}, nil, 4*time.Minute)
	return err
}

// superviseModel holds OS locks for the whole model lifetime, independently of HTTP requests.
func superviseModel(ctx context.Context, path, action, name string) error {
	cfg, err := readCluster(path)
	if err != nil {
		// A stop or cleanup may still be served when the central cluster
		// file is unreadable: superviseNative's STRICT root fallback
		// locates the root from ROOT/config/models.json and acts only on
		// an existing validated durable record (nothing is ever guessed).
		// run has no record to serve and fails closed here.
		if action == "stop" || action == "cleanup" {
			return superviseNative(ctx, path, action, name)
		}
		return err
	}
	r := newRuntime(cfg, path)
	// Every native action (run, stop, cleanup) belongs to the real
	// supervisor: it binds the record's immutable config snapshot, claims
	// the queued generation under lock and starts the real units with
	// lifetime OS locks. This happens BEFORE any recipe selection or
	// legacy stop/cleanup branch so native effects are never half-served
	// by the legacy path.
	if r.native() {
		return superviseNative(ctx, path, action, name)
	}
	m, s, nodes, placement, err := selectPlacement(cfg, name, "")
	if err != nil && action == "run" {
		return err
	}
	if str(m["kind"]) != "llm" && action == "run" {
		return fail("This lifecycle entry is for language models.")
	}
	stateDir := filepath.Join(r.root(), "manager")
	if r.native() {
		stateDir = filepath.Join(r.root(), "controller/lifecycle")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	marker := filepath.Join(stateDir, "stopping."+name)
	if action == "stop" || action == "cleanup" {
		info, e := r.actualModel(name)
		if e != nil {
			return e
		}
		info["state"] = "stopping"
		if r.native() {
			if err := writeObject(r.recordPath(name), info); err != nil {
				return err
			}
		}
		if err := atomicWrite(marker, nil, 0600); err != nil {
			return err
		}
		if err := r.stopUnits(ctx, info); err != nil {
			return err
		}
		if r.native() {
			info["state"] = "stopped"
			info["updated_at"] = now()
			return writeObject(r.recordPath(name), info)
		}
		return nil
	}
	if action != "run" {
		return fail("Use run, stop, or cleanup.")
	}
	files := []*os.File{}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for _, node := range nodes {
		f, e := os.OpenFile(filepath.Join(stateDir, "owner."+node+".lock"), os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return e
		}
		files = append(files, f)
		if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
			return fail("A model still owns the required device.")
		}
	}
	info := Object{"nodes": nodes, "units": s["units"], "setting": s, "placement": placement, "generation": token(), "state": "starting", "updated_at": now()}
	started := false
	success := false
	defer func() {
		if started {
			cleanup, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			stopErr := r.stopUnits(cleanup, info)
			cancel()
			if r.native() {
				info["state"] = "stopped"
				if !success || stopErr != nil {
					info["state"] = "failed"
				}
				info["updated_at"] = now()
				_ = writeObject(r.recordPath(name), info)
			}
		}
	}()
	err = withFileLock(filepath.Join(stateDir, "operation.lock"), true, func() error {
		snapshot, e := r.snapshot(ctx, true)
		if e != nil {
			return e
		}
		for other, v := range obj(snapshot["models"]) {
			row := obj(v)
			if str(row["kind"]) == "llm" && boolean(row["loaded"]) && overlapNodes(nodes, stringsOf(row["nodes"])) {
				return fail("%s still occupies a required device.", other)
			}
		}
		for _, node := range nodes {
			comfy := obj(obj(snapshot["comfyui"])[node])
			if boolean(comfy["executing"]) || (num(comfy["running"]) > 0 && str(comfy["phase"]) != "waiting") {
				return fail("ComfyUI still has active GPU work. No job was stopped.")
			}
			if e := r.clearComfy(ctx, node); e != nil {
				return e
			}
		}
		snapshot, e = r.snapshot(ctx, true)
		if e != nil {
			return e
		}
		for node, v := range obj(s["minimum_available_gib"]) {
			available := num(obj(obj(obj(snapshot["nodes"])[node])["memory_bytes"])["MemAvailable"]) / (1 << 30)
			if available < num(v) {
				return fail("%s has %.1f GiB available; this recipe needs %.1f GiB at startup. Parakeet was kept loaded.", node, available, num(v))
			}
		}
		for _, node := range nodes {
			params := merge(obj(s["parameters"]), Object{"context_tokens": s["context_tokens"]})
			spec := Object{"parameters": params, "bindings": value(obj(s["parameter_bindings"]), node, []any{})}
			if node == "spark01" {
				if _, e := RenderParameters(spec); e != nil {
					return e
				}
			} else {
				host := str(obj(obj(cfg["nodes"])[node])["ssh"])
				if _, e := r.runChecked(ctx, []string{"ssh", "-o", "BatchMode=yes", host, "~/.local/bin/sparky --json node render --payload -"}, jsonBytes(spec), 30*time.Second); e != nil {
					return fail("Worker recipe rendering failed. No model was started.")
				}
			}
		}
		if e := writeObject(filepath.Join(r.root(), "controller", "running-"+name+".json"), info); e != nil {
			return e
		}
		if r.native() {
			if e := writeObject(r.recordPath(name), info); e != nil {
				return e
			}
		}
		_ = os.Remove(marker)
		started = true
		order := stringsOf(value(s, "startup_order", anyStrings(reverse(nodes))))
		for _, node := range order {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e := r.unit(ctx, node, "start", str(obj(s["units"])[node])); e != nil {
				return e
			}
			rv := obj(s["rendezvous"])
			if str(rv["node"]) == node {
				args := []string{"ss", "-H", "-ltn", "sport = :" + intText(rv["port"])}
				if node != "spark01" {
					args = []string{"ssh", str(obj(obj(cfg["nodes"])[node])["ssh"]), shellJoin(args)}
				}
				deadline := time.Now().Add(100 * time.Second)
				found := false
				for time.Now().Before(deadline) {
					out, e := r.execute(ctx, args, nil, 8*time.Second)
					if e == nil && strings.TrimSpace(out.Out) != "" {
						found = true
						break
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(time.Second):
					}
				}
				if !found {
					return fail("The recipe rendezvous did not start.")
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if r.native() {
		deadline := time.Now().Add(durationSeconds(value(s, "health_timeout_seconds", cfg["health_timeout_seconds"]), 1800))
		ready := false
		for time.Now().Before(deadline) {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, _, e := request(ctx, "GET", str(s["backend_url"])+"/health", nil, r.backendHeaders(), 3*time.Second)
			if e == nil {
				ready = true
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		if !ready {
			return fail("Model readiness timed out.")
		}
		info["state"] = "ready"
		info["updated_at"] = now()
		if err := writeObject(r.recordPath(name), info); err != nil {
			return err
		}
	}
	for {
		if _, e := os.Stat(marker); e == nil {
			success = true
			return nil
		}
		select {
		case <-ctx.Done():
			success = true
			return nil
		case <-time.After(3 * time.Second):
		}
		for _, node := range nodes {
			args := []string{"systemctl", "is-active", "--quiet", str(obj(s["units"])[node])}
			if node != "spark01" {
				args = []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", str(obj(obj(cfg["nodes"])[node])["ssh"]), shellJoin(args)}
			}
			out, e := r.execute(ctx, args, nil, 12*time.Second)
			if e != nil || out.Code != 0 {
				return fail("A model node stopped. The remaining nodes will be released.")
			}
		}
	}
}
func reverse(xs []string) []string {
	out := append([]string{}, xs...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

var _ = json.Valid
