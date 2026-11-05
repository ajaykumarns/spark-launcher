package sparky

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type gate chan struct{}

func newGate() gate { return make(chan struct{}, 1) }
func (g gate) lock(ctx context.Context) error {
	select {
	case g <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fail("The operation was cancelled or timed out.")
	}
}
func (g gate) unlock() { <-g }

type lease struct {
	ID, Name, RequestID, Backend                string
	Nodes                                       []string
	Exclusive, Cancelled, Validating, Validated bool
	Started                                     float64
	Cancel                                      context.CancelFunc
}
type waiting struct {
	Name, RequestID, ForceID string
	Nodes                    []string
	Exclusive, Cancelled     bool
	Limit                    int
}
type Coordinator struct {
	mu                      sync.Mutex
	path                    string
	config                  Object
	runtime                 *Runtime
	leases                  map[string]*lease
	pending                 []*waiting
	wake                    chan struct{}
	reconfigure, transition gate
	forceMu                 sync.Mutex
	forcingNodes            []string
	forceID                 string
	generation              map[string]uint64
	fenced, reloading       bool
	imageGate               chan struct{}
	autoPool                Object
}

func newCoordinator(path string, rt *Runtime) (*Coordinator, error) {
	c, err := readCluster(path)
	if err != nil {
		return nil, err
	}
	if rt == nil {
		rt = newRuntime(c, path)
	}
	rt.DrainAdmission = func(ctx context.Context) (func(), error) { return rt.drainAdmission(ctx) }
	co := &Coordinator{path: path, config: c, runtime: rt, leases: map[string]*lease{}, wake: make(chan struct{}), reconfigure: newGate(), transition: newGate(), generation: map[string]uint64{}, imageGate: make(chan struct{}, 1)}
	rt.owner = co
	return co, nil
}

// imageLease reports whether a lease belongs to a ComfyUI engine job, which
// the legacy manager reload never disturbs.
func imageLease(name string) bool {
	return strings.HasPrefix(name, "comfyui@") || strings.HasPrefix(name, "cpu-probe@") || strings.HasPrefix(name, "adopted-")
}

// drainAdmission is the admission-drain callback used by the legacy manager
// sync: it sets the coordinator's own reloading flag (which blocks new
// reservations in acquire), waits for every direct Sparky lease that maps
// to a manager request to drain, and returns the restore function. The
// manual fenced flag is never read or written here, so a fence set before,
// during or after the reload is preserved; only the reload's own flag is
// cleared, even on error.
func (rt *Runtime) drainAdmission(ctx context.Context) (func(), error) {
	c := rt.owner
	if c == nil {
		return func() {}, nil
	}
	c.mu.Lock()
	c.reloading = true
	c.signal()
	c.mu.Unlock()
	restore := func() {
		c.mu.Lock()
		c.reloading = false
		c.signal()
		c.mu.Unlock()
	}
	for {
		c.mu.Lock()
		busy := false
		for _, l := range c.leases {
			if !imageLease(l.Name) {
				busy = true
			}
		}
		c.mu.Unlock()
		if !busy {
			return restore, nil
		}
		select {
		case <-ctx.Done():
			restore()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (c *Coordinator) signal()     { close(c.wake); c.wake = make(chan struct{}) }
func (c *Coordinator) cfg() Object { c.mu.Lock(); defer c.mu.Unlock(); return copyObject(c.config) }
func (c *Coordinator) refresh() error {
	v, e := readCluster(c.path)
	if e != nil {
		return e
	}
	c.mu.Lock()
	c.config = v
	c.mu.Unlock()
	c.runtime.configure(v)
	return nil
}

// effectiveNodes returns the node set a request for name must use: the
// ACTUAL frozen loaded placement when the model is loaded, otherwise the
// desired placement of the current config. A transient one-shot load (or a
// saved run_on change not yet applied) therefore keeps admission, readiness
// and inference routing on the nodes the model really runs on. The record is
// validated (enabled current model, llm kind, matching placement, units,
// recipe fingerprint and topology) before its nodes are trusted.
func (c *Coordinator) effectiveNodes(cfg Object, name string) ([]string, error) {
	sel, err := c.loadedSelection(name)
	if err != nil {
		return nil, err
	}
	return sel.Nodes, nil
}

// checkPlacement validates an explicit one-shot placement for name against
// the ACTUAL configured recipe (aliases resolve first). Only language
// models accept the override: audio and image workloads keep their own
// placement semantics and are refused BEFORE any side effect. Unit names are
// per-node identities: a configured unit for each selected node is required,
// and identical unit basenames on different nodes remain valid.
func checkPlacement(cfg Object, name, on string) error {
	if on == "" {
		return nil
	}
	if placements[on] == nil {
		return fail("--on must be spark01, spark02, or both.")
	}
	_, s, _, _, err := selectPlacement(cfg, name, on)
	if err != nil {
		return err
	}
	m := obj(obj(cfg["models"])[name])
	if str(m["kind"]) != "llm" {
		return fail("--on is supported only for language models; %s is %s.", name, orDefault(str(m["kind"]), "a service"))
	}
	for _, node := range placements[on] {
		unit := str(obj(s["units"])[node])
		if unit == "" || !serviceName.MatchString(unit) {
			return fail("%s has no complete recipe for %s.", name, on)
		}
	}
	return nil
}

// placementNodesFor reports the nodes of the placement a load will use.
func placementNodesFor(cfg Object, name, on string) []string {
	if on != "" {
		if nodes := placements[on]; nodes != nil {
			return append([]string{}, nodes...)
		}
	}
	return placements[str(obj(obj(cfg["models"])[name])["run_on"])]
}

// selectionFor resolves what a load of name with one-shot placement on would
// run at: the requested placement for an explicit override, the SAVED
// default for a no-flag load. It is a pure config query (no record read) so
// a load always targets the requested placement even while an old
// generation is still loaded elsewhere.
func selectionFor(cfg Object, name, on string) (*runtimeSelection, error) {
	m, s, nodes, placement, err := selectPlacement(cfg, name, on)
	if err != nil {
		return nil, err
	}
	return &runtimeSelection{Model: m, Setting: s, Nodes: nodes, Placement: placement}, nil
}

// targetSelection resolves the load plan: an explicit one-shot placement is
// validated first, then the target selection is resolved. Reservation nodes
// (the model's currently recorded nodes) are computed separately and are
// never mixed into the target recipe nodes.
func (c *Coordinator) targetSelection(cfg Object, name, on string) (*runtimeSelection, error) {
	if err := checkPlacement(cfg, name, on); err != nil {
		return nil, err
	}
	return selectionFor(cfg, name, on)
}

// loadedSelection resolves the ACTUAL loaded placement of name from its
// validated durable record (see Runtime.loadedSelection).
func (c *Coordinator) loadedSelection(name string) (*runtimeSelection, error) {
	return c.runtime.loadedSelection(name)
}

// reservationNodes returns the nodes a model currently owns (its validated
// loaded record), which a new load must additionally reserve/evict on. These
// are distinct from the target recipe nodes and never feed readiness.
func (c *Coordinator) reservationNodes(name string) []string {
	sel, err := c.loadedSelection(name)
	if err != nil || !sel.Actual {
		return nil
	}
	return sel.Nodes
}

func sharedComfy(cfg Object) bool {
	return !boolean(value(obj(cfg["policy"]), "comfyui_exclusive_with_llms", true))
}
func conflicts(cfg Object, aName string, aNodes []string, aExclusive bool, bName string, bNodes []string, bExclusive bool) bool {
	if !overlapNodes(aNodes, bNodes) {
		return false
	}
	if aExclusive || bExclusive {
		return true
	}
	if aName == bName {
		return strings.HasPrefix(aName, "comfyui@")
	}
	if sharedComfy(cfg) {
		allowed := stringsOf(obj(cfg["policy"])["comfyui_shared_llms"])
		if strings.HasPrefix(aName, "comfyui@") && contains(allowed, bName) || strings.HasPrefix(bName, "comfyui@") && contains(allowed, aName) {
			return false
		}
	}
	return true
}

// acquire admits a job. With wait=true it queues FIFO until the devices are
// free, the context is cancelled or a forced operation cancels the entry.
// With wait=false (ComfyUI retries) it returns "" promptly while keeping the
// pending entry queued exactly once, preserving FIFO order for later retries.
// A request ID that already holds a lease returns that lease instead of
// creating a duplicate; this is re-checked on every wake so two concurrent
// retries of the same prompt can never mint two leases.
func (c *Coordinator) acquire(ctx context.Context, name string, nodes []string, exclusive bool, limit int, requestID, forceID string, wait bool) (string, error) {
	if requestID == "" {
		requestID = token()
	}
	entry := &waiting{Name: name, Nodes: append([]string{}, nodes...), Exclusive: exclusive, Limit: limit, RequestID: requestID, ForceID: forceID}
	c.mu.Lock()
	if overlapNodes(nodes, c.forcingNodes) && forceID != c.forceID {
		c.mu.Unlock()
		return "", fail("A forced model operation is using the requested device. Try again after it finishes.")
	}
	for id, l := range c.leases {
		if l.RequestID == requestID {
			c.mu.Unlock()
			return id, nil
		}
	}
	found := false
	for _, p := range c.pending {
		if p.RequestID == requestID {
			entry = p
			found = true
			break
		}
	}
	if !found {
		c.pending = append(c.pending, entry)
	}
	remove := func() {
		for i, p := range c.pending {
			if p == entry {
				c.pending = append(c.pending[:i], c.pending[i+1:]...)
				break
			}
		}
	}
	for {
		if entry.Cancelled {
			remove()
			c.mu.Unlock()
			return "", fail("A forced model operation cancelled this waiting request.")
		}
		// A retry racing an admission must observe the new lease, not mint a second one.
		for id, l := range c.leases {
			if l.RequestID == requestID {
				remove()
				c.mu.Unlock()
				return id, nil
			}
		}
		blocked := c.fenced || c.reloading
		count := 0
		for _, l := range c.leases {
			if l.Name == name {
				count++
			}
			blocked = blocked || conflicts(c.config, name, nodes, exclusive, l.Name, l.Nodes, l.Exclusive)
		}
		if limit > 0 && count >= limit {
			blocked = true
		}
		for _, p := range c.pending {
			if p == entry {
				break
			}
			if !p.Cancelled && conflicts(c.config, name, nodes, exclusive, p.Name, p.Nodes, p.Exclusive) {
				blocked = true
			}
		}
		if !blocked {
			remove()
			id := token()
			c.leases[id] = &lease{ID: id, Name: name, Nodes: append([]string{}, nodes...), Exclusive: exclusive, RequestID: requestID, Started: float64(time.Now().UnixNano()) / 1e9}
			c.signal()
			c.mu.Unlock()
			return id, nil
		}
		if !wait {
			c.mu.Unlock()
			return "", nil
		}
		wake := c.wake
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			c.mu.Lock()
			remove()
			c.signal()
			c.mu.Unlock()
			return "", fail("The waiting request was cancelled.")
		case <-wake:
		}
		c.mu.Lock()
	}
}
func (c *Coordinator) release(id string) {
	c.mu.Lock()
	l := c.leases[id]
	delete(c.leases, id)
	c.signal()
	c.mu.Unlock()
	if l != nil {
		_ = c.runtime.event("released", Object{"workload": l.Name, "request_id": l.RequestID})
	}
}
func (c *Coordinator) registerCancel(id string, cancel context.CancelFunc) {
	c.mu.Lock()
	l := c.leases[id]
	cancelled := l == nil
	if l != nil {
		l.Cancel = cancel
		cancelled = l.Cancelled
	}
	c.mu.Unlock()
	if cancelled {
		cancel()
	}
}
func (c *Coordinator) leaseBackend(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l := c.leases[id]; l != nil {
		return l.Backend
	}
	return ""
}
func (c *Coordinator) admission() Object {
	c.mu.Lock()
	defer c.mu.Unlock()
	active := []any{}
	pending := []any{}
	for _, l := range c.leases {
		active = append(active, Object{"workload": l.Name, "nodes": l.Nodes, "started_at": l.Started})
	}
	for _, p := range c.pending {
		if !p.Cancelled {
			pending = append(pending, Object{"workload": p.Name, "nodes": p.Nodes})
		}
	}
	return Object{"active": active, "waiting": pending, "config_transition": c.fenced || c.reloading}
}
func (c *Coordinator) snapshot(ctx context.Context, fresh bool) (Object, error) {
	data, err := c.runtime.snapshot(ctx, fresh)
	if err != nil {
		return nil, err
	}
	legacy, e := c.runtime.legacyActivity(ctx)
	data["activity_known"] = e == nil
	c.mu.Lock()
	for name, v := range obj(data["models"]) {
		count := 0
		for _, l := range c.leases {
			if l.Name == name {
				count++
			}
		}
		obj(v)["active_requests"] = max(count, legacy[name])
	}
	c.mu.Unlock()
	// Status reports the ACTUAL validated loaded placement of every native
	// LLM, not just the nodes the physical probe saw: a one-shot load or a
	// pending run_on change must show where the model really runs. A model
	// with no valid loaded record keeps the probed row unchanged.
	if c.runtime.native() {
		for name, v := range obj(data["models"]) {
			row := obj(v)
			if str(row["kind"]) != "llm" {
				continue
			}
			sel, err := c.loadedSelection(name)
			if err != nil || !sel.Actual {
				continue
			}
			row["nodes"] = append([]string{}, sel.Nodes...)
			row["placement"] = sel.Placement
		}
	}
	data["admission"] = c.admission()
	if bifrostAuto(c.cfg()) {
		c.mu.Lock()
		data["auto_routing"] = copyObject(c.autoPool)
		c.mu.Unlock()
	}
	data["config_path"] = c.path
	return data, nil
}
func (c *Coordinator) plan(ctx context.Context, requested, action, on string) (Object, error) {
	if err := c.refresh(); err != nil {
		return nil, err
	}
	cfg := c.cfg()
	name, _, err := resolveWorkload(cfg, requested)
	if err != nil {
		return nil, err
	}
	// An explicit load placement is refused in a legacy deployment BEFORE
	// the snapshot is even taken, consistently with execution: the manager
	// sync path cannot honour a transient placement, so the plan never
	// previews one or advertises its capability.
	if on != "" && action == "load" {
		if err := checkPlacement(cfg, name, on); err != nil {
			return nil, err
		}
		if str(cfg["lifecycle"]) != "native" {
			return nil, fail("--on requires the native model lifecycle and cannot be used in legacy mode.")
		}
	}
	snapshot, err := c.snapshot(ctx, true)
	if err != nil {
		return nil, err
	}
	// A one-shot load placement is previewed exactly as it will run: the
	// actual recipe is validated and the conflict decisions use the actual
	// loaded nodes, while the saved run_on stays untouched.
	result, err := makePlan(cfg, name, snapshot, action, on)
	if err != nil {
		return nil, err
	}
	if on != "" && action == "load" {
		// Capability marker for the private node: an explicit placement
		// override is only sent to a controller that advertises it. An OLD
		// controller may already preview the placement but silently ignores
		// the "on" field on load, so the node refuses to proceed unless this
		// exact marker is present in the READ-ONLY plan response.
		result["placement_override_supported"] = true
	}
	return result, nil
}

// ReconcileJobs compares leases with the actual ComfyUI engine state. An
// unreachable engine means the state is unknown: every reservation is kept.
// Only a confirmed engine state may remove obsolete pending entries and
// leases, and an executing engine job is adopted after a controller restart
// under the same shared/exclusive rules.
func (c *Coordinator) ReconcileJobs() { c.reconcileJobs(context.Background()) }
func (c *Coordinator) reconcileJobs(ctx context.Context) {
	cfg := c.cfg()
	for node, m := range obj(cfg["nodes"]) {
		url := str(obj(m)["comfy_url"])
		if url == "" {
			continue
		}
		state, err := requestObject(ctx, "GET", url+"/sparky/state", nil, nil, 3*time.Second, false)
		if err != nil {
			continue
		}
		// A malformed or ambiguous state (wrong types, executing without a usable
		// prompt id) means the engine state is UNKNOWN: nothing may be deleted.
		exec, execOK := state["executing"].(bool)
		if !execOK {
			continue
		}
		if exec {
			if _, ok := state["prompt_id"].(string); !ok {
				continue
			}
		}
		prompt := state["prompt_id"]
		current := node + ":" + str(prompt)
		c.mu.Lock()
		imageNames := []string{"comfyui@" + node, "cpu-probe@" + node}
		for id, l := range c.leases {
			if contains(imageNames, l.Name) && l.RequestID != current {
				delete(c.leases, id)
			}
		}
		kept := c.pending[:0]
		for _, p := range c.pending {
			if contains(imageNames, p.Name) && p.RequestID != current {
				p.Cancelled = true
				continue
			}
			kept = append(kept, p)
		}
		c.pending = kept
		exists := false
		for _, l := range c.leases {
			if l.RequestID == current {
				exists = true
			}
		}
		if boolean(state["executing"]) && !exists && prompt != nil {
			c.leases["adopted-"+node] = &lease{ID: "adopted-" + node, Name: "comfyui@" + node, Nodes: []string{node}, Exclusive: !sharedComfy(cfg), RequestID: current, Started: float64(time.Now().Unix())}
		}
		c.signal()
		c.mu.Unlock()
	}
}
func (c *Coordinator) resolveAuto(ctx context.Context) (string, error) {
	cfg := c.cfg()
	if len(obj(cfg["auto"])) == 0 {
		return "", fail("Automatic model routing is not configured.")
	}
	for _, name := range stringsOf(obj(cfg["auto"])["targets"]) {
		// Auto admission probes the ACTUAL loaded selection of each target:
		// a one-shot placement or a pending run_on change must never be
		// reported ready on the desired nodes it does not run on.
		sel, err := c.loadedSelection(name)
		if err == nil && backendForRecipe(sel.Setting, sel.Record) != "" && c.runtime.ready(ctx, name, sel.Setting, sel.Nodes) {
			return name, nil
		}
	}
	return "", fail("No automatic routing target is ready. Ask the human to switch the model. Do not load or unload models automatically.")
}
func (c *Coordinator) requireReady(ctx context.Context, name string, sel *runtimeSelection) error {
	if sel == nil || !c.runtime.ready(ctx, name, sel.Setting, sel.Nodes) {
		return fail("%s is not ready at the selected placement. Ask the human to switch the model. Do not load or unload models automatically.", name)
	}
	return nil
}
func (c *Coordinator) generationCopy() map[string]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := map[string]uint64{}
	for n, v := range c.generation {
		m[n] = v
	}
	return m
}
func (c *Coordinator) unchanged(g map[string]uint64, nodes []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range nodes {
		if g[n] != c.generation[n] {
			return false
		}
	}
	return true
}
func (c *Coordinator) llmRequest(ctx context.Context, requested string) (string, string, error) {
	generation := c.generationCopy()
	if err := c.reconfigure.lock(ctx); err != nil {
		return "", "", err
	}
	defer c.reconfigure.unlock()
	if err := c.refresh(); err != nil {
		return "", "", err
	}
	cfg := c.cfg()
	if bifrostAuto(cfg) && (autoRequested(requested) || requested == "sparky-auto-unavailable") {
		return "", "", autoUnavailableError{"Automatic inference must use the Bifrost spark-auto route. No automatic target is available, or the caller used a provider-prefixed alias."}
	}
	var name string
	var err error
	if normalized := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(requested)), "garuda/"); normalized == "auto" || normalized == "spark-auto" {
		name, err = c.resolveAuto(ctx)
	} else {
		name, _, err = resolveWorkload(cfg, requested)
	}
	if err != nil {
		return "", "", err
	}
	// The ACTUAL loaded selection drives the whole request: nodes, frozen
	// recipe, backend identity and the readiness proof. A model loaded
	// through a one-shot placement (or a saved run_on change not yet
	// applied) is admitted, probed and routed on what really runs — never
	// the desired run_on that the backend no longer serves.
	sel, err := c.loadedSelection(name)
	if err != nil {
		return "", "", err
	}
	if str(sel.Model["kind"]) != "llm" {
		return "", "", fail("Select a language model for this endpoint.")
	}
	backend := backendForRecipe(sel.Setting, sel.Record)
	if backend == "" {
		return "", "", fail("%s is not ready at the selected placement. Ask the human to switch the model. Do not load or unload models automatically.", name)
	}
	if !c.unchanged(generation, sel.Nodes) {
		return "", "", fail("A forced model operation cancelled this waiting request.")
	}
	if err := c.requireReady(ctx, name, sel); err != nil {
		return "", "", err
	}
	c.reconcileJobs(ctx)
	id, err := c.acquire(ctx, name, sel.Nodes, false, int(num(sel.Setting["concurrency"])), "", "", true)
	if err != nil {
		return "", "", err
	}
	if err := c.requireReady(ctx, name, sel); err != nil {
		c.release(id)
		return "", "", err
	}
	c.mu.Lock()
	l := c.leases[id]
	if l == nil {
		// The lease was released between acquire and bind; never dereference nil.
		c.mu.Unlock()
		return "", "", fail("The admission lease vanished before the backend was bound.")
	}
	cancelled := l.Cancelled
	l.Backend = backend
	c.mu.Unlock()
	if cancelled {
		c.release(id)
		return "", "", fail("A forced model operation cancelled this request.")
	}
	return name, id, nil
}

// confirmedSelection returns the ACTUAL validated loaded selection of name
// as a result acknowledgment: the confirmed placement and nodes that the
// durable record proves, plus the backend identity that recipe binds. A
// successful operation must report only what really runs; there is no
// fallback to the requested placement.
func (c *Coordinator) confirmedSelection(name string) (Object, error) {
	sel, err := c.loadedSelection(name)
	if err != nil {
		return nil, err
	}
	if sel == nil || !sel.Actual {
		return nil, fail("The actual placement of %s is not confirmed. The controller must be upgraded and restarted to support one-shot placement overrides.", name)
	}
	// A queued or starting generation is not a confirmed success: status
	// ready may be acknowledged only from the validated READY record.
	if str(sel.Record["state"]) != StateReady {
		return nil, fail("%s is not ready at the selected placement. Ask the human to switch the model. Do not load or unload models automatically.", name)
	}
	backend := backendForRecipe(sel.Setting, sel.Record)
	if backend == "" {
		return nil, fail("%s is not ready at the selected placement. Ask the human to switch the model. Do not load or unload models automatically.", name)
	}
	result := Object{"status": "ready", "workload": name, "placement": sel.Placement,
		"nodes": append([]any{}, anyStrings(sel.Nodes)...), "backend": backend}
	return result, nil
}

func (c *Coordinator) load(ctx context.Context, requested string, force bool, on ...string) (Object, error) {
	override := ""
	if len(on) > 0 {
		override = on[0]
	}
	if force {
		return c.force(ctx, requested, "load", override)
	}
	return c.loadInternal(ctx, requested, "", override)
}
func (c *Coordinator) loadInternal(ctx context.Context, requested, forceID string, on ...string) (Object, error) {
	override := ""
	if len(on) > 0 {
		override = on[0]
	}
	generation := c.generationCopy()
	if err := c.reconfigure.lock(ctx); err != nil {
		return nil, err
	}
	defer c.reconfigure.unlock()
	if err := c.refresh(); err != nil {
		return nil, err
	}
	cfg := c.cfg()
	name, m, err := resolveWorkload(cfg, requested)
	if err != nil {
		return nil, err
	}
	// An explicit one-shot placement is validated against the model's
	// ACTUAL recipe before any stop, eviction or reservation.
	sel, err := c.targetSelection(cfg, name, override)
	if err != nil {
		return nil, err
	}
	if override != "" && str(cfg["lifecycle"]) != "native" {
		// Legacy deployments have no native lifecycle: the manager sync/HUP
		// path cannot honour a transient placement. Refuse BEFORE any sync,
		// lease cancellation or unit change, for normal, force and JSON.
		return nil, fail("--on requires the native model lifecycle and cannot be used in legacy mode.")
	}
	s, nodes, placement := sel.Setting, append([]string{}, sel.Nodes...), sel.Placement
	// The transient selection must be reflected in the operation's own
	// config snapshot so readiness, probes and the durable record all see
	// the frozen recipe (the saved run_on is never rewritten).
	if override != "" {
		cfg = copyObject(cfg)
		obj(obj(cfg["models"])[name])["run_on"] = override
	}
	// Reservation nodes are computed SEPARATELY from the target recipe
	// nodes: the model's current validated loaded placement must be freed
	// and reserved, but it is never fed into readiness, start or the
	// durable record as if it were part of the new recipe. A model moving
	// between placements therefore targets exactly the requested nodes.
	reserved := unionNodes(nodes, c.reservationNodes(name))
	if !c.unchanged(generation, reserved) {
		return nil, fail("A forced model operation cancelled this waiting request.")
	}
	kind := str(m["kind"])
	var id string
	if kind == "audio" {
		// An audio service's containers live on its saved node; a one-shot
		// placement override can never move it safely.
		if override != "" {
			return nil, fail("--on is supported only for language models; %s is %s.", name, kind)
		}
		if err := c.runtime.ensureAudio(ctx, name); err != nil {
			return nil, err
		}
		return Object{"status": "ready", "workload": name}, nil
	}
	if kind != "llm" {
		// ComfyUI weights are placed by the workflow request, not by a model
		// load; refuse the override before starting any unit.
		if override != "" {
			return nil, fail("--on is supported only for language models; %s is %s.", name, kind)
		}
		for _, n := range nodes {
			state, e := c.runtime.inspectNode(ctx, n)
			if e != nil {
				return nil, e
			}
			if !boolean(obj(state["comfyui"])["reachable"]) {
				if e := c.runtime.unit(ctx, n, "start", str(obj(obj(cfg["nodes"])[n])["comfy_unit"])); e != nil {
					return nil, e
				}
			}
		}
		return Object{"status": "ready", "workload": name, "note": "ComfyUI is available. Weights load when a workflow runs."}, nil
	}
	// Fastpath: a strongly ready native model needs no full hardware, disk
	// or GPU inventory. Admission still happens (a lease is acquired under
	// the normal rules) and readiness is rechecked on the frozen recipe
	// before returning; a lost race falls through to the full path below.
	if kind == "llm" && c.runtime.native() && c.runtime.nativeReady(ctx, name, m, s, nodes, placement) {
		fastID, err := c.acquire(ctx, name, nodes, false, int(num(s["concurrency"])), "", forceID, true)
		if err != nil {
			return nil, err
		}
		defer c.release(fastID)
		if c.runtime.nativeReady(ctx, name, m, s, nodes, placement) {
			if override != "" {
				// An explicit override acknowledges only the ACTUAL
				// validated record of the frozen transie selection.
				return c.confirmedSelection(name)
			}
			return Object{"status": "ready", "workload": name}, nil
		}
	}
	// A transient placement selection runs against its own frozen config
	// snapshot: the shared runtime config is never mutated for the whole
	// operation, so concurrent requests can never observe the override.
	rt := c.runtime
	if override != "" {
		rt = c.runtime.frozenClone(cfg)
		rt.owner = nil
	}
	if err := rt.syncLegacy(ctx); err != nil {
		return nil, err
	}
	c.reconcileJobs(ctx)
	ready := rt.ready(ctx, name, s, nodes)
	snapshot, err := rt.snapshot(ctx, true)
	if err != nil {
		return nil, err
	}
	// The admission lease covers BOTH the target recipe nodes and the nodes
	// the model actually occupies now, so nothing is double-booked while the
	// old placement drains; readiness below still only proves the target.
	reserved = unionNodes(nodes, c.reservationNodes(name))
	id, err = c.acquire(ctx, name, reserved, !ready, 0, "", forceID, true)
	if err != nil {
		return nil, err
	}
	defer c.release(id)
	operationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.registerCancel(id, cancel)
	operationLockErr := c.transition.lock(operationCtx)
	if operationLockErr != nil {
		return nil, operationLockErr
	}
	defer c.transition.unlock()
	if rt.ready(operationCtx, name, s, nodes) {
		if override != "" {
			return c.confirmedSelection(name)
		}
		return Object{"status": "ready", "workload": name}, nil
	}
	snapshot, err = rt.snapshot(operationCtx, true)
	if err != nil {
		return nil, err
	}
	for _, other := range keys(obj(snapshot["models"])) {
		row := obj(obj(snapshot["models"])[other])
		// Only the TARGET nodes drive which other models conflict. This
		// model's own old placement is released explicitly below.
		if str(row["kind"]) != "llm" || !boolean(row["loaded"]) || (!overlapNodes(nodes, stringsOf(row["nodes"])) && other != name) {
			continue
		}
		if other == name && rt.native() && rt.adoptSelf(operationCtx, name, m, s, nodes, placement) {
			// The model's own durable record already matches the selected
			// recipe, placement, nodes and topology: adopt the SAME
			// generation and wait instead of evicting itself. A changed
			// recipe/placement/host cleans the actual old recorded units
			// first below.
			continue
		}
		if effectiveProtected(row, obj(cfg["policy"]), other) {
			return nil, fail("%s must stay loaded on the requested device.", other)
		}
		if rt.native() {
			if other == name {
				// Re-placements must clean this model's OWN previous
				// generation first, even when the new placement is disjoint:
				// a stopped record never blocks the later adoption.
				samePlacement := sameStrings(stringsOf(row["nodes"]), nodes) && str(row["placement"]) == placement
				if !samePlacement || !rt.adoptSelf(operationCtx, name, m, s, nodes, placement) {
					if err := c.syncAutoRoutesLocked(operationCtx, []string{other}); err != nil {
						return nil, err
					}
					if err := rt.stopModel(operationCtx, other); err != nil {
						return nil, err
					}
				}
				continue
			}
			if err := c.syncAutoRoutesLocked(operationCtx, []string{other}); err != nil {
				return nil, err
			}
			if err := rt.stopModel(operationCtx, other); err != nil {
				return nil, err
			}
		}
	}
	_ = rt.event("load_requested", Object{"workload": name, "placement": placement})
	if !rt.native() {
		// Legacy deployments have no native lifecycle: the load goes
		// through the manager health path exactly like a direct start.
		if err := rt.startModel(operationCtx, name); err != nil {
			return nil, err
		}
	} else if err := rt.startModelSelected(operationCtx, name, cfg); err != nil {
		return nil, err
	}
	// Readiness proof reads the ACTUAL frozen record of a transient load,
	// never the shared runtime's saved placement.
	if !rt.ready(operationCtx, name, s, nodes) {
		return nil, fail("%s is not ready at the selected placement. Ask the human to switch the model. Do not load or unload models automatically.", name)
	}
	result := Object{"status": "ready", "workload": name}
	if override != "" {
		// A transient selection is acknowledged ONLY with the ACTUAL
		// confirmed placement and nodes; never the requested placement.
		result, err = c.confirmedSelection(name)
		if err != nil {
			return nil, err
		} else if result == nil {
			return nil, fail("%s placement was not confirmed by the controller.", name)
		}
	}
	return result, nil
}

// effectiveProtected reports whether a model must stay loaded under the
// EFFECTIVE policy: the snapshot row's keep_loaded (which already merges the
// current config keep_loaded with the frozen record policy for a deleted,
// disabled or renamed canonical name) or the current policy's
// protected_models allowlist. A deleted name must never read as unprotected
// just because its current config entry is gone.
func effectiveProtected(row, policy Object, name string) bool {
	if boolean(row["keep_loaded"]) {
		return true
	}
	return contains(stringsOf(policy["protected_models"]), name)
}

// nativeRecordUnload resolves an UNLOAD for a model the CURRENT config can
// no longer resolve (deleted, disabled or renamed) through its valid
// durable record ONLY: the record's canonical key, llm kind and RECORDED
// nodes are used — never an invented record, alias or desired placement.
// keep_loaded from the frozen record config AND the current config protects
// the model exactly like a current model.
func (c *Coordinator) nativeRecordUnload(cfg Object, requested string) (string, Object, []string, error) {
	if !c.runtime.native() {
		return "", nil, nil, fail("Unknown workload.")
	}
	name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(requested), "garuda/")))
	rec, err := c.runtime.readRecord(name)
	if err != nil {
		return "", nil, nil, fail("Cannot resolve %s for unload and no valid lifecycle record exists.", requested)
	}
	name = str(rec["canonicalkey"])
	if name == "" || !simpleName.MatchString(name) {
		name = str(rec["name"])
	}
	if boolean(obj(obj(obj(rec["config"])["models"])[name])["keep_loaded"]) ||
		boolean(obj(obj(cfg["models"])[name])["keep_loaded"]) {
		return "", nil, nil, fail("%s must stay loaded.", name)
	}
	return name, Object{"kind": "llm", "name": name}, stringsOf(rec["nodes"]), nil
}

func (c *Coordinator) unload(ctx context.Context, requested string, force bool) (Object, error) {
	if force {
		return c.force(ctx, requested, "unload")
	}
	if err := c.reconfigure.lock(ctx); err != nil {
		return nil, err
	}
	defer c.reconfigure.unlock()
	if err := c.refresh(); err != nil {
		return nil, err
	}
	cfg := c.cfg()
	name, m, err := resolveWorkload(cfg, requested)
	recordNodes := []string(nil)
	if err != nil {
		// A deleted, disabled or otherwise unresolvable model may still be
		// unloaded through its valid durable record (native only).
		if name, m, recordNodes, err = c.nativeRecordUnload(cfg, requested); err != nil {
			return nil, err
		}
	}
	if boolean(m["keep_loaded"]) {
		return nil, fail("%s must stay loaded.", name)
	}
	nodes := recordNodes
	if recordNodes == nil {
		_, _, nodes, _, err = selectPlacement(cfg, name, "")
		if err != nil {
			return nil, err
		}
	}
	snapshot, err := c.runtime.snapshot(ctx, true)
	if err != nil {
		return nil, err
	}
	nodes = unionNodes(nodes, stringsOf(obj(obj(snapshot["models"])[name])["nodes"]))
	if str(m["kind"]) == "llm" {
		if err := c.syncAutoRoutesLocked(ctx, []string{name}); err != nil {
			return nil, err
		}
	}
	id, err := c.acquire(ctx, name, nodes, true, 0, "", "", true)
	if err != nil {
		return nil, err
	}
	defer c.release(id)
	if err := c.transition.lock(ctx); err != nil {
		return nil, err
	}
	defer c.transition.unlock()
	switch str(m["kind"]) {
	case "llm":
		for {
			counts, e := c.runtime.legacyActivity(ctx)
			if e != nil {
				return nil, e
			}
			if counts[name] == 0 {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		err = c.runtime.stopModel(ctx, name)
	case "comfyui", "comfy_model":
		for _, node := range nodes {
			for {
				d, e := c.runtime.inspectNode(ctx, node)
				if e != nil {
					return nil, e
				}
				state := obj(d["comfyui"])
				if !boolean(state["executing"]) && (num(state["running"]) == 0 || str(state["phase"]) == "waiting") {
					break
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Second):
				}
			}
			if e := c.runtime.clearComfy(ctx, node); e != nil {
				return nil, e
			}
		}
	default:
		err = fail("No safe unload adapter is configured for this audio service.")
	}
	if err != nil {
		return nil, err
	}
	c.runtime.invalidate()
	_ = c.runtime.event("unloaded", Object{"workload": name})
	return Object{"status": "unloaded", "workload": name}, nil
}
func (c *Coordinator) force(ctx context.Context, requested, action string, on ...string) (Object, error) {
	override := ""
	if len(on) > 0 {
		override = on[0]
	}
	if !c.forceMu.TryLock() {
		return nil, fail("Another forced model operation is running.")
	}
	defer c.forceMu.Unlock()
	if err := c.refresh(); err != nil {
		return nil, err
	}
	cfg := c.cfg()
	name, m, err := resolveWorkload(cfg, requested)
	recordNodes := []string(nil)
	if err != nil {
		// A forced UNLOAD of a deleted/disabled model goes through its
		// valid durable record only; every other action needs a current
		// recipe. The record path refuses protected models itself.
		if action != "unload" {
			return nil, err
		}
		if name, m, recordNodes, err = c.nativeRecordUnload(cfg, requested); err != nil {
			return nil, err
		}
	}
	if str(m["kind"]) != "llm" {
		return nil, fail("--force currently supports language models only.")
	}
	if boolean(m["keep_loaded"]) {
		return nil, fail("%s must stay loaded.", name)
	}
	if override != "" && str(cfg["lifecycle"]) != "native" {
		// An explicit --on is refused BEFORE the forced generation bump,
		// lease cancellation, drain, stop or reload (JSON and force too).
		return nil, fail("--on requires the native model lifecycle and cannot be used in legacy mode.")
	}
	nodes := recordNodes
	if recordNodes == nil {
		// A forced one-shot load is validated against the model's actual
		// recipe before any cancel, drain or stop.
		if action == "load" {
			if err := checkPlacement(cfg, name, override); err != nil {
				return nil, err
			}
		}
		_, _, nodes, _, err = selectPlacement(cfg, name, override)
		if err != nil {
			return nil, err
		}
	}
	snapshot, err := c.runtime.snapshot(ctx, true)
	if err != nil {
		return nil, err
	}
	nodes = unionNodes(nodes, stringsOf(obj(obj(snapshot["models"])[name])["nodes"]))
	// A forced load must also cancel and drain work on the model's ACTUAL
	// nodes: the old placement holds the model and its requests.
	if action == "load" {
		if actual, e := c.effectiveNodes(cfg, name); e == nil {
			nodes = unionNodes(nodes, actual)
		}
	}
	affected := Object{name: true}
	for n, v := range obj(snapshot["models"]) {
		row := obj(v)
		if str(row["kind"]) == "llm" && boolean(row["loaded"]) && overlapNodes(nodes, stringsOf(row["nodes"])) {
			affected[n] = true
		}
	}
	for _, n := range nodes {
		state := obj(obj(snapshot["comfyui"])[n])
		if boolean(state["executing"]) || (num(state["running"]) > 0 && str(state["phase"]) != "waiting") {
			return nil, fail("An active image job uses the requested device. Stop that job before using --force.")
		}
	}
	c.mu.Lock()
	if c.fenced || c.reloading {
		c.mu.Unlock()
		return nil, fail("Configuration is changing. Try again after it finishes.")
	}
	for _, l := range c.leases {
		if overlapNodes(nodes, l.Nodes) {
			affected[l.Name] = true
		}
	}
	for _, n := range keys(affected) {
		if n == name && recordNodes != nil {
			// The record-unloaded name has no current config entry; its llm
			// kind and protection were validated from the durable record.
			continue
		}
		if effectiveProtected(obj(obj(snapshot["models"])[n]), obj(cfg["policy"]), n) {
			c.mu.Unlock()
			return nil, fail("A protected model uses the requested device.")
		}
		am := obj(obj(cfg["models"])[n])
		if str(am["kind"]) != "llm" {
			c.mu.Unlock()
			return nil, fail("An active image job uses the requested device. Stop that job before using --force.")
		}
	}
	if action == "unload" {
		affected = Object{name: true}
	}
	forceID := token()
	c.forceID = forceID
	c.forcingNodes = nodes
	for _, n := range nodes {
		c.generation[n]++
	}
	for _, p := range c.pending {
		if overlapNodes(nodes, p.Nodes) {
			p.Cancelled = true
		}
	}
	cancels := []context.CancelFunc{}
	for _, l := range c.leases {
		if boolean(affected[l.Name]) {
			l.Cancelled = true
			if l.Cancel != nil {
				cancels = append(cancels, l.Cancel)
			}
		}
	}
	c.signal()
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.forceID = ""; c.forcingNodes = nil; c.signal(); c.mu.Unlock() }()
	for _, cancel := range cancels {
		cancel()
	}
	drainCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if err := c.runtime.cancelLegacy(drainCtx, keys(affected)); err != nil {
			return nil, err
		}
		counts, err := c.runtime.legacyActivity(drainCtx)
		if err != nil {
			return nil, err
		}
		busy := false
		c.mu.Lock()
		for _, l := range c.leases {
			if overlapNodes(nodes, l.Nodes) {
				busy = true
			}
		}
		c.mu.Unlock()
		for _, n := range keys(affected) {
			busy = busy || counts[n] > 0
		}
		if !busy {
			break
		}
		select {
		case <-drainCtx.Done():
			return nil, fail("Conflicting requests did not stop within 30 seconds. No new model was started.")
		case <-time.After(100 * time.Millisecond):
		}
	}
	lockCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	if err := c.reconfigure.lock(lockCtx); err != nil {
		return nil, fail("An earlier configuration operation did not stop within 30 seconds.")
	}
	if err := c.transition.lock(lockCtx); err != nil {
		c.reconfigure.unlock()
		return nil, fail("An earlier model operation did not stop within 30 seconds.")
	}
	if syncErr := c.syncAutoRoutesLocked(ctx, keys(affected)); syncErr != nil {
		c.transition.unlock()
		c.reconfigure.unlock()
		return nil, syncErr
	}
	for _, other := range keys(affected) {
		if err = c.runtime.stopModel(ctx, other); err != nil {
			break
		}
	}
	c.transition.unlock()
	c.reconfigure.unlock()
	if err != nil {
		return nil, err
	}
	if action == "load" {
		result, err := c.loadInternal(ctx, name, forceID, override)
		if err != nil {
			return nil, err
		}
		result["forced"] = true
		return result, nil
	}
	return Object{"status": "unloaded", "workload": name, "forced": true}, nil
}
func (c *Coordinator) setPlacement(ctx context.Context, requested, on string) (Object, error) {
	if err := c.reconfigure.lock(ctx); err != nil {
		return nil, err
	}
	defer c.reconfigure.unlock()
	if err := c.refresh(); err != nil {
		return nil, err
	}
	cfg := c.cfg()
	name, _, err := resolveWorkload(cfg, requested)
	if err != nil {
		return nil, err
	}
	if _, _, _, _, err = selectPlacement(cfg, name, on); err != nil {
		return nil, err
	}
	c.mu.Lock()
	forcing := len(c.forcingNodes) > 0
	c.mu.Unlock()
	if forcing {
		return nil, fail("A forced model operation is running.")
	}
	obj(obj(cfg["models"])[name])["run_on"] = on
	if err = writeCluster(c.path, cfg); err != nil {
		return nil, err
	}
	_ = c.refresh()
	return Object{"workload": name, "run_on": on, "takes_effect": "on the next load request"}, nil
}

func (c *Coordinator) configPath() string { return filepath.Clean(c.path) }

// comfyReadinessWait bounds how long SubmitComfy waits for a freshly started
// ComfyUI unit to answer its HTTP queue. Tests shrink it.
var comfyReadinessWait = 180 * time.Second

// comfyAcquireBudget bounds one ComfyAcquire operation (queue fetch,
// transition gate, snapshot, cache release and re-probe). It is a seam so
// tests can shrink it; production never waits unbounded.
var comfyAcquireBudget = 30 * time.Second

// ComfyAcquire reserves the device for a ComfyUI job already present in the
// real engine queue. The caller's cpu_only flag is never trusted alone: the
// exact workflow at the engine is inspected, and only a nonempty graph made
// exclusively of SparkyProbe nodes runs without a GPU reservation. GPU jobs
// respect the shared-LLM allowlist and the minimum available memory; they
// never unload a model to make room.
func (c *Coordinator) ComfyAcquire(node, promptID string, cpuOnly bool) (Object, error) {
	cfg := c.cfg()
	if _, known := obj(cfg["nodes"])[node]; !known || node == "" {
		return nil, fail("Unknown device.")
	}
	meta := obj(obj(cfg["nodes"])[node])
	opCtx, opCancel := context.WithTimeout(context.Background(), comfyAcquireBudget)
	defer opCancel()
	queue, err := requestObject(opCtx, "GET", str(meta["comfy_url"])+"/queue", nil, nil, 5*time.Second, false)
	if err != nil {
		return nil, fail("The ComfyUI job is not running in its engine queue.")
	}
	if promptID == "" {
		return nil, fail("The ComfyUI job is not running in its engine queue.")
	}
	var graph Object
	for _, row := range arr(queue["queue_running"]) {
		items := arr(row)
		if len(items) > 2 && str(items[1]) == promptID {
			graph = obj(items[2])
			break
		}
	}
	if len(graph) == 0 {
		return nil, fail("The ComfyUI job is not running in its engine queue.")
	}
	actualCPU := true
	for _, v := range graph {
		if str(obj(v)["class_type"]) != "SparkyProbe" {
			actualCPU = false
			break
		}
	}
	if cpuOnly && !actualCPU {
		return nil, fail("This workflow requires a GPU reservation.")
	}
	name := "comfyui@" + node
	nodes := []string{node}
	if actualCPU {
		name = "cpu-probe@" + node
		nodes = nil
	}
	shared := !actualCPU && sharedComfy(cfg)
	id, err := c.acquire(opCtx, name, nodes, !shared, 0, node+":"+promptID, "", false)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return Object{"status": "waiting", "retry_after": 1}, nil
	}
	// Concurrent retries of the same prompt share one lease. Exactly the
	// first caller validates it; a duplicate of a validated lease reuses the
	// committed token without re-running checks, and a duplicate racing the
	// validation waits instead of releasing the provisional reservation.
	c.mu.Lock()
	l := c.leases[id]
	if l == nil {
		c.mu.Unlock()
		return nil, fail("The ComfyUI reservation disappeared.")
	}
	if l.Validated {
		// The committed lease, not the current policy, defines the grant.
		c.mu.Unlock()
		return comfyAdmitted(id, node, l.Name == "cpu-probe@"+node, !l.Exclusive), nil
	}
	owner := false
	if !l.Validating {
		l.Validating = true
		owner = true
	}
	if !owner {
		c.mu.Unlock()
		return Object{"status": "waiting", "retry_after": 1}, nil
	}
	c.mu.Unlock()
	// Only the validation owner may release its own provisional lease; a
	// replacement lease for the same request belongs to someone else.
	releaseOwned := func() {
		c.mu.Lock()
		mine := c.leases[id] == l
		c.mu.Unlock()
		if mine {
			c.release(id)
		}
	}
	// Commit succeeds only if the SAME lease is still present, not cancelled
	// and still owned by this validation. Otherwise the token must never be
	// reported as admitted.
	commit := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		cur := c.leases[id]
		if cur != l || l.Cancelled || !l.Validating {
			return false
		}
		l.Validated = true
		return true
	}
	_ = c.runtime.event("admitted", Object{"workload": name, "nodes": nodes, "request_id": node + ":" + promptID})
	if !actualCPU {
		if err := c.transition.lock(opCtx); err != nil {
			releaseOwned()
			return nil, err
		}
		defer c.transition.unlock()
		snapshot, err := c.runtime.snapshot(opCtx, true)
		if err != nil {
			releaseOwned()
			return nil, err
		}
		var conflicts []string
		for other, v := range obj(snapshot["models"]) {
			row := obj(v)
			if str(row["kind"]) == "llm" && boolean(row["loaded"]) && contains(stringsOf(row["nodes"]), node) {
				conflicts = append(conflicts, other)
			}
		}
		if shared {
			policy := obj(cfg["policy"])
			allowed := stringsOf(policy["comfyui_shared_llms"])
			unsupported := []string{}
			for _, n := range conflicts {
				if !contains(allowed, n) {
					unsupported = append(unsupported, n)
				}
			}
			available := num(obj(obj(obj(snapshot["nodes"])[node])["memory_bytes"])["MemAvailable"])
			minimum := num(policy["comfyui_min_available_gib"]) * 1024 * 1024 * 1024
			for _, n := range conflicts {
				if !contains(allowed, n) {
					unsupported = append(unsupported, n)
				}
			}
			if len(unsupported) > 0 {
				// The waiting response keeps the memory fields so the caller
				// can see why the GPU is unavailable, exactly like Python.
				releaseOwned()
				return Object{"status": "waiting", "retry_after": 2, "reason": "unapproved_loaded_llm",
					"available_bytes": available, "minimum_bytes": minimum}, nil
			}
			if available < minimum {
				// Only this engine's idle cache may be released, exactly
				// once per validation; an LLM is never stopped to make room
				// for a shared image job.
				if err := c.runtime.clearComfy(opCtx, node); err == nil {
					if probe, e := c.runtime.inspectNode(opCtx, node); e == nil {
						available = num(obj(probe["memory_bytes"])["MemAvailable"])
					}
				}
				if available < minimum {
					releaseOwned()
					return Object{"status": "waiting", "retry_after": 2, "reason": "low_available_memory", "available_bytes": available, "minimum_bytes": minimum}, nil
				}
			}
			_ = c.runtime.event("comfy_shared_admitted", Object{"node": node, "prompt_id": promptID, "llms": anyStrings(conflicts), "available_bytes": available, "minimum_bytes": minimum})
		} else if len(conflicts) > 0 {
			releaseOwned()
			return Object{"status": "waiting", "retry_after": 2, "reason": "loaded_llm_requires_explicit_unload"}, nil
		} else {
			// Audio stays loaded; the next free-memory check sees fresh state.
			c.runtime.invalidate()
		}
	}
	if opCtx.Err() != nil {
		releaseOwned()
		return nil, fail("The image job reservation expired during validation.")
	}
	if !commit() {
		// The lease was reconciled away, cancelled or replaced while the
		// validation ran: report an honest retry instead of an orphan token.
		return Object{"status": "waiting", "retry_after": 2, "reason": "reservation_superseded"}, nil
	}
	return comfyAdmitted(id, node, actualCPU, shared), nil
}
func comfyAdmitted(id, node string, actualCPU, shared bool) Object {
	out := Object{"token": id, "status": "admitted", "node": node, "cpu_only": actualCPU}
	if !actualCPU {
		out["shared"] = shared
	}
	return out
}

// released reports whether the lease token still exists.
func leaseExists(c *Coordinator, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.leases[id]
	return ok
}

// CancelJob removes the queued request of a node:prompt pair. Only entries
// that are still waiting are affected; an already admitted lease is managed
// through its own release path.
func (c *Coordinator) CancelJob(node, promptID string) {
	requestID := node + ":" + promptID
	c.mu.Lock()
	kept := c.pending[:0]
	for _, p := range c.pending {
		if p.RequestID == requestID {
			p.Cancelled = true
			continue
		}
		kept = append(kept, p)
	}
	c.pending = kept
	c.signal()
	c.mu.Unlock()
}

// CheckJobLease validates that a release belongs to the node's image or CPU
// job. A missing token is idempotent, matching the Python controller.
func (c *Coordinator) CheckJobLease(token, node string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.leases[token]; ok && l.Name != "comfyui@"+node && l.Name != "cpu-probe@"+node {
		return fail("Lease belongs to another workload.")
	}
	return nil
}

// SubmitComfy sends a workflow to exactly one device's ComfyUI engine,
// starting the unit first when it is not reachable and waiting a bounded
// time for readiness. It never starts or stops a language model. The public
// result keeps its existing shape: no backend URLs are ever returned.
func (c *Coordinator) SubmitComfy(workflow Object, on string) (Object, error) {
	_, out, err := c.submitComfyOrigin(workflow, on)
	return out, err
}

// submitComfyOrigin is the internal submission path behind SubmitComfy. It
// resolves the actual engine origin from a fresh config snapshot at submit
// time and returns that exact base URL alongside the result, so the caller
// can poll and view on the accepted engine even if the configuration later
// changes. Nothing else about the result shape differs.
func (c *Coordinator) submitComfyOrigin(workflow Object, on string) (string, Object, error) {
	if err := c.refresh(); err != nil {
		return "", nil, err
	}
	cfg := c.cfg()
	if on == "" {
		on = str(obj(obj(cfg["models"])["comfyui"])["run_on"])
	}
	return c.submitComfyPinned(cfg, on, workflow)
}

// submitComfyPinned submits the workflow using exactly the caller-supplied
// config snapshot and node: no second refresh happens, so validation and
// submission share one immutable snapshot. A trailing slash on the configured
// engine origin is normalized before /prompt and /history paths are appended.
func (c *Coordinator) submitComfyPinned(cfg Object, on string, workflow Object) (string, Object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), comfyReadinessWait+2*time.Minute)
	defer cancel()
	_, s, nodes, _, err := selectPlacement(cfg, "comfyui", on)
	if err != nil {
		return "", nil, err
	}
	if len(nodes) != 1 {
		return "", nil, fail("Submit each ComfyUI workflow to one device.")
	}
	node := nodes[0]
	meta := obj(obj(cfg["nodes"])[node])
	url := strings.TrimRight(str(obj(s)["backend_url"]), "/")
	if url == "" {
		url = strings.TrimRight(str(meta["comfy_url"]), "/")
	}
	state, e := c.runtime.inspectNode(ctx, node)
	if e != nil {
		// An unknown inventory or failed device query is an error, never a
		// confirmed unreachable engine: nothing may be started from it.
		return "", nil, e
	}
	if !boolean(obj(state["comfyui"])["reachable"]) {
		if e := c.runtime.unit(ctx, node, "start", str(meta["comfy_unit"])); e != nil {
			return "", nil, e
		}
		deadline := time.Now().Add(comfyReadinessWait)
		for {
			state, e := c.runtime.inspectNode(ctx, node)
			if e == nil && boolean(obj(state["comfyui"])["reachable"]) {
				break
			}
			if time.Now().After(deadline) {
				return "", nil, fail("ComfyUI did not become ready in time.")
			}
			select {
			case <-ctx.Done():
				return "", nil, fail("ComfyUI did not become ready in time.")
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	payload := workflow
	if _, hasPrompt := workflow["prompt"]; !hasPrompt {
		payload = Object{"prompt": workflow}
	}
	result, err := requestObject(ctx, "POST", url+"/prompt", payload, nil, 120*time.Second, false)
	if err != nil {
		return "", nil, err
	}
	if str(result["prompt_id"]) == "" {
		return "", nil, fail("The ComfyUI engine did not return a prompt_id.")
	}
	_ = c.runtime.event("workflow_submitted", Object{"node": node, "prompt_id": result["prompt_id"]})
	out := Object{"node": node}
	for k, v := range result {
		out[k] = v
	}
	return url, out, nil
}

// Exported thin wrappers required by the Controller interface (server.go).
// Features that are not implemented yet (image generation, wire listing)
// deliberately have no method here.

func (c *Coordinator) ConfigPath() string { return c.configPath() }
func (c *Coordinator) Config() Object     { return c.cfg() }
func (c *Coordinator) Refresh() (Object, error) {
	if err := c.refresh(); err != nil {
		return nil, err
	}
	return c.cfg(), nil
}
func (c *Coordinator) Snapshot(fresh bool) (Object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.snapshot(ctx, fresh)
}
func (c *Coordinator) Admission() Object { return c.admission() }
func (c *Coordinator) Fenced() bool      { c.mu.Lock(); defer c.mu.Unlock(); return c.fenced }
func (c *Coordinator) Fence(enabled bool) {
	c.mu.Lock()
	if c.fenced != enabled {
		c.fenced = enabled
		c.signal()
	}
	c.mu.Unlock()
}
func (c *Coordinator) Plan(model, action, on string) (Object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return c.plan(ctx, model, action, on)
}
func (c *Coordinator) Load(model string, force bool, on string) (Object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	result, err := c.load(ctx, model, force, on)
	return c.finishAutoOperation(result, err)
}
func (c *Coordinator) Unload(model string, force bool) (Object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	result, err := c.unload(ctx, model, force)
	return c.finishAutoOperation(result, err)
}
func (c *Coordinator) SetPlacement(model, on string) (Object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.setPlacement(ctx, model, on)
}
func (c *Coordinator) LLMRequest(ctx context.Context, model string) (string, string, error) {
	return c.llmRequest(ctx, model)
}
func (c *Coordinator) InferenceOrigin(token string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.leases[token]; ok && l.Backend != "" {
		return l.Backend, nil
	}
	return "", fail("The lease has no inference origin.")
}
func (c *Coordinator) Release(token string) { c.release(token) }
func (c *Coordinator) RegisterInferenceCancel(token string, cancel context.CancelFunc) {
	c.registerCancel(token, cancel)
}
