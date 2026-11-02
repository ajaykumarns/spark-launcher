package sparky

// Stage 2 of the native lifecycle: the independent model supervisor. It runs
// as `sparky --json node supervise` (sparky-model@.service) and owns its
// devices for the ENTIRE model lifetime, independently of the HTTP
// controller. Everything the supervisor does comes from the durable record's
// FROZEN recipe snapshot: the central cluster file is read only to locate
// the root and the ssh hosts. A later edit, disable or delete of the current
// recipe never changes a running or stopping operation.

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Seams with the same precedent as runCommandWaitDelay: real defaults that
// tests may shrink so bounded waits stay fast. nativeRuntimeFactory is the
// injectable real factory; it defaults to newRuntime. nativeRankStopTimeout
// is per rank but every rank is stopped IN PARALLEL, so the whole teardown
// stays under the systemd 300s stop budget.
var (
	nativeRuntimeFactory   = newRuntime
	nativeRankStopTimeout  = 180 * time.Second
	nativePortProbeTimeout = 2 * time.Second
	nativeMonitorInterval  = 3 * time.Second
	nativeRetryInterval    = 100 * time.Millisecond
)

// nativeLockPaths acquires every listed lock file in order with a
// context-aware non-blocking retry loop. It NEVER blocks in a
// context-insensitive Flock wait: cancellation between attempts is honored
// promptly. On failure every already-held lock is released. The caller is
// responsible for closing the returned files (releasing the locks) only
// after its cleanup attempts have finished.
func nativeLockPaths(ctx context.Context, paths []string) ([]*os.File, error) {
	var held []*os.File
	closeHeld := func() {
		for _, f := range held {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		}
	}
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			closeHeld()
			return nil, err
		}
		for {
			f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				closeHeld()
				return nil, err
			}
			e := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if e == nil {
				held = append(held, f)
				break
			}
			_ = f.Close()
			select {
			case <-ctx.Done():
				closeHeld()
				return nil, fail("A model still owns the required device.")
			case <-time.After(nativeRetryInterval):
			}
		}
	}
	return held, nil
}

func nativeCloseLocks(locks []*os.File) {
	for _, f := range locks {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

// nativeBackendClosed refuses unless the shared backend TCP port answers
// ECONNREFUSED. A live listener, a stale same-id backend or any unknown
// dial error (timeout, unreachable, permission) can never mean free.
func nativeBackendClosed(backend string) error {
	u, err := url.Parse(backend)
	if err != nil || u.Hostname() == "" {
		return fail("The recipe backend address is not usable.")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(u.Hostname(), port), nativePortProbeTimeout)
	if err == nil {
		_ = conn.Close()
		return fail("The shared backend port is still in use. No rank was started.")
	}
	var oe *net.OpError
	if errors.As(err, &oe) && errors.Is(err, syscall.ECONNREFUSED) {
		return nil
	}
	return fail("Cannot confirm that the shared backend port is free.")
}

// nativeStaleHealthRefused blocks a start when something on the recipe
// backend URL already answers /health with 200: that is a stale same-id
// backend, never a reason to start a second one.
func nativeStaleHealthRefused(ctx context.Context, backend string, headers map[string]string) error {
	if _, _, err := request(ctx, "GET", backend+"/health", nil, headers, 3*time.Second); err == nil {
		return fail("A stale backend already answers on the recipe URL. No rank was started.")
	}
	return nil
}

// nativeRankStates queries the KEYED LoadState= and ActiveState= properties
// of one rank unit. The property names are parsed from the output, never
// the unstable position of --value lines. Any error, exit code or missing
// property is unknown and can never mean free: the caller must refuse.
func nativeRankStates(ctx context.Context, r *Runtime, node, unit string) (string, string, error) {
	if !serviceName.MatchString(unit) {
		return "", "", fail("Invalid service operation.")
	}
	args := []string{"systemctl", "show", unit, "-p", "LoadState", "-p", "ActiveState"}
	if node != "spark01" {
		h := str(obj(obj(r.config()["nodes"])[node])["ssh"])
		if err := safeHost(h); err != nil {
			return "", "", err
		}
		args = []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", h, shellJoin(args)}
	}
	x, err := r.execute(ctx, args, nil, 12*time.Second)
	if err != nil || x.Code != 0 {
		return "", "", fail("Cannot confirm the state of %s.", unit)
	}
	load, active := "", ""
	for _, line := range strings.Split(x.Out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "LoadState="):
			load = strings.TrimPrefix(line, "LoadState=")
		case strings.HasPrefix(line, "ActiveState="):
			active = strings.TrimPrefix(line, "ActiveState=")
		}
	}
	if load == "" || active == "" {
		return "", "", fail("Cannot confirm the state of %s.", unit)
	}
	return load, active, nil
}

// nativeNoOwnedOverlap refuses the start while any other lifecycle record
// holds a RESERVATION on one of the nodes this operation owns. The strict
// nativeOwnership scanner is the single validation authority: every record
// is validated with the real record shape rules, and a corrupt, malformed,
// symlinked, oversized or otherwise invalid record is an ERROR that refuses
// the start — never a free device. ANY non-stopped validated record on an
// overlapping node refuses the start regardless of its units' activity or
// unit names: a reservation is ownership, not resident weights, and
// disabled or deleted models keep holding their recorded nodes. Only a
// validated stopped record releases its devices.
func nativeNoOwnedOverlap(ctx context.Context, r *Runtime, name string, nodes []string) error {
	own, err := r.nativeOwnership(ctx)
	if err != nil {
		return fail("Cannot confirm ownership of a required device: %v.", err)
	}
	for other, v := range own {
		if other == name {
			continue
		}
		if !boolean(obj(v)["reserved"]) {
			continue
		}
		for _, node := range stringsOf(obj(v)["nodes"]) {
			if contains(nodes, node) {
				return fail("%s still occupies a required device.", other)
			}
		}
	}
	return nil
}

// nativeRenderFrozen renders the FROZEN recipe parameters: the head node is
// rendered locally, a worker through its configured ssh with the exact same
// payload. The current cluster config is never consulted for parameters.
func nativeRenderFrozen(ctx context.Context, r *Runtime, s Object, nodes []string) error {
	for _, node := range nodes {
		params := merge(obj(s["parameters"]), Object{"context_tokens": s["context_tokens"]})
		spec := Object{"parameters": params, "bindings": value(obj(s["parameter_bindings"]), node, []any{})}
		if node == "spark01" {
			if _, e := RenderParameters(spec); e != nil {
				return e
			}
		} else {
			host := str(obj(obj(r.config()["nodes"])[node])["ssh"])
			if _, e := r.runChecked(ctx, []string{"ssh", "-o", "BatchMode=yes", host, "~/.local/bin/sparky --json node render --payload -"}, jsonBytes(spec), 30*time.Second); e != nil {
				return fail("Worker recipe rendering failed. No model was started.")
			}
		}
	}
	return nil
}

// nativeRootFallback is the STRICT root-location fallback for a stop or
// cleanup while the central cluster file is unreadable: when the supervisor
// was started with ROOT/config/models.json, the root is located from that
// path alone. NO desired config, units or models are ever guessed: the
// runtime only knows the root, so every effect must come from an existing
// validated durable record or is refused.
func nativeRootFallback(path string) (Object, bool) {
	clean := filepath.Clean(path)
	dir := filepath.Dir(clean)
	if filepath.Base(dir) != "config" || filepath.Base(clean) != "models.json" {
		return nil, false
	}
	return Object{"root": filepath.Dir(dir), "lifecycle": "native"}, true
}

// superviseNative is the whole independent native supervisor: run, stop and
// cleanup. It configures the ACTUAL runtime from the validated immutable
// RECORD config snapshot and setting, never from the current mutable
// central model metadata. The central file is read only to locate the
// root; every effect (units, ssh hosts, parameters, backend) comes from the
// record's validated WHOLE frozen cluster config. If the central file is
// fallback may still serve an EXISTING validated record. run claims the
// durable queued/starting generation, holds its device locks for its entire
// lifetime (through any cleanup) and starts the frozen ranks; stop/cleanup
// act only on the actual durable recorded units of the record's current
// generation, even if the central recipe was edited, disabled or deleted.
func superviseNative(ctx context.Context, path, action, name string) error {
	if !simpleName.MatchString(name) {
		return fail("Invalid model name.")
	}
	switch action {
	case "run", "stop", "cleanup":
	default:
		return fail("Use run, stop, or cleanup.")
	}
	cfg, err := readCluster(path)
	if err != nil {
		// Strict root-location fallback only: an existing validated record
		// may still be served; nothing is ever guessed.
		fb, ok := nativeRootFallback(path)
		if !ok {
			return err
		}
		cfg = fb
	}
	r := nativeRuntimeFactory(cfg, path)
	stateDir := filepath.Join(r.root(), "controller/lifecycle")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	rec, err := r.readRecord(name)
	if err != nil {
		return err
	}
	// The frozen config snapshot and setting are mandatory: a record
	// without them is refused BEFORE any effect.
	frozen := obj(rec["config"])
	if frozen == nil {
		return fail("The lifecycle record has no frozen config snapshot; refusing to act.")
	}
	gen := str(rec["generation"])
	placement := str(rec["placement"])
	s := copyObject(obj(rec["setting"]))
	nodes := stringsOf(rec["nodes"])
	units := copyObject(obj(rec["units"]))
	// Root, units, nodes and setting consistency: the record's units must
	// equal the frozen setting's units on exactly the recorded nodes, and
	// the recorded node set must be exactly the placement's node set (the
	// placement is a GROUP name like "both", not a node). Anything else is
	// refused.
	pnodes, known := placements[placement]
	if !known || !sameStrings(nodes, pnodes) {
		return fail("The lifecycle record's placement does not match its nodes; refusing to act.")
	}
	for _, node := range nodes {
		if !simpleName.MatchString(node) || str(units[node]) == "" ||
			str(units[node]) != str(obj(s["units"])[node]) {
			return fail("The lifecycle record's frozen units and setting disagree; refusing to act.")
		}
	}
	if !sameStrings(keys(units), nodes) {
		return fail("The lifecycle record's frozen units and setting disagree; refusing to act.")
	}
	// The frozen snapshot is the WHOLE cluster config written by the Core
	// producer. Validate it for root and node consistency BEFORE any
	// effect: a snapshot written for another root or missing a recorded
	// node can never drive this operation.
	if str(frozen["root"]) != r.root() {
		return fail("The lifecycle record's frozen config was written for a different root; refusing to act.")
	}
	if !isObject(obj(frozen["models"])[str(rec["canonicalkey"])]) {
		return fail("The lifecycle record's frozen config is missing its canonical model; refusing to act.")
	}
	frozenNodes := obj(frozen["nodes"])
	if frozenNodes == nil {
		return fail("The lifecycle record's frozen config has no node map; refusing to act.")
	}
	for _, node := range nodes {
		if !isObject(frozenNodes[node]) {
			return fail("The lifecycle record's frozen config is missing a recorded node; refusing to act.")
		}
	}
	// Configure the ACTUAL runtime as a copy of the WHOLE frozen config
	// with the native lifecycle: the model catalog, node ssh hosts and
	// every other setting come from the immutable snapshot, so no current
	// central metadata (edited, disabled or deleted recipe, changed ssh
	// hosts) can leak into the operation. In fallback mode remote effects
	// are refused safely by the missing frozen ssh hosts themselves.
	actual := copyObject(frozen)
	actual["lifecycle"] = "native"
	r.configure(actual)
	if action == "run" {
		return nativeRun(ctx, r, name, gen, s, nodes, units, stateDir)
	}
	return nativeStop(ctx, r, name, gen, s, nodes, units, stateDir, action)
}

// nativeRun performs the run side. Locks are released only after the final
// cleanup attempts have finished (see the deferred order).
func nativeRun(ctx context.Context, r *Runtime, name, gen string, s Object, nodes []string, units Object, stateDir string) error {
	first, err := r.readRecord(name)
	if err != nil {
		return err
	}
	switch str(first["state"]) {
	case StateQueued, StateStarting:
	default:
		return fail("%s has no queued operation to supervise.", name)
	}
	// Per-device OS locks in sorted order, held for the ENTIRE supervisor
	// lifetime through cleanup. Context-aware retry; never a blocking wait.
	sorted := append([]string{}, nodes...)
	sort.Strings(sorted)
	lockPaths := make([]string, 0, len(sorted))
	for _, node := range sorted {
		lockPaths = append(lockPaths, filepath.Join(stateDir, "owner."+node+".lock"))
	}
	locks, err := nativeLockPaths(ctx, lockPaths)
	if err != nil {
		return err
	}
	// started tracks whether ANY rank of THIS generation was attempted;
	// attempted records exactly which ranks own effects. A preflight
	// failure must cause ZERO start AND ZERO stop commands.
	started := false
	attempted := Object{}
	runErr := nativeStartup(ctx, r, name, gen, s, nodes, units, stateDir, &started, attempted)
	if runErr == nil {
		runErr = nativeAwaitReady(ctx, r, name, gen, s, nodes, units, stateDir)
	}
	if runErr != nil {
		// Partial startup: ONLY the ranks owned/attempted by THIS
		// generation are cleaned up, each with a fresh bounded context,
		// and the record is failed via CAS so a superseded generation is
		// never overwritten. Locks are released only after these attempts
		// finished. A preflight failure (started == false) never touches
		// any existing process.
		var stopErr error
		if started {
			stopErr = nativeStopRanks(r, attempted)
			if cerr := nativeConfirmStoppedContext(r, name, gen, attempted); cerr != nil && stopErr == nil {
				stopErr = cerr
			}
		}
		casErr := r.TransitionState(name, gen, StateFailed)
		nativeCloseLocks(locks)
		superseded := false
		if casErr != nil {
			if cur, e := r.readRecord(name); e != nil || str(cur["generation"]) != gen {
				superseded = true // a newer owner owns the record now
			}
		}
		if runErr == context.Canceled || runErr == context.DeadlineExceeded {
			if stopErr != nil && !superseded {
				return fail("Startup was cancelled and cleanup needs attention.")
			}
			return fail("Model startup was cancelled; started services were stopped.")
		}
		if stopErr != nil && !superseded {
			return fail("The model failed to start and cleanup needs attention.")
		}
		if casErr != nil && !superseded {
			return fail("The failed startup could not be recorded: %v.", casErr)
		}
		return runErr
	}
	// The device locks stay held for the whole model lifetime: the monitor
	// releases them on every exit path, after any cleanup finished.
	return nativeMonitor(ctx, r, name, gen, s, nodes, units, stateDir, locks, started, attempted)
}

// nativeStartup claims the generation (queued -> starting), stores the
// systemd invocation id, verifies the world is actually free and starts the
// frozen ranks in the recipe order with the recipe rendezvous. It records
// every rank it ATTEMPTED to start so cleanup can be bounded to this
// generation's own effects.
func nativeStartup(ctx context.Context, r *Runtime, name, gen string, s Object, nodes []string, units Object, stateDir string, started *bool, attempted Object) error {
	// Store the systemd invocation id so a stale ExecStopPost from an old
	// supervisor can never stop a newer operation.
	if inv := os.Getenv("INVOCATION_ID"); inv != "" {
		if err := withFileLock(r.recordPath(name)+".lock", true, func() error {
			rec, e := r.readRecord(name)
			if e != nil {
				return e
			}
			if str(rec["generation"]) != gen {
				return fail("A newer lifecycle operation superseded this one.")
			}
			rec["invocation_id"] = inv
			rec["updated_at"] = now()
			if e := validateRecord(rec, name); e != nil {
				return e
			}
			return writeObject(r.recordPath(name), rec)
		}); err != nil {
			return err
		}
	}
	// Mark starting BEFORE any preflight effect or rank start.
	if err := r.TransitionState(name, gen, StateStarting); err != nil {
		return err
	}
	// The startup operation lock is context-aware as well.
	opLocks, err := nativeLockPaths(ctx, []string{filepath.Join(stateDir, "operation.lock")})
	if err != nil {
		return err
	}
	defer nativeCloseLocks(opLocks)

	backend := strings.TrimRight(str(s["backend_url"]), "/")
	if backend == "" {
		return fail("The frozen recipe has no backend URL.")
	}
	if err := nativeBackendClosed(backend); err != nil {
		return err
	}
	if err := nativeStaleHealthRefused(ctx, backend, r.backendHeaders()); err != nil {
		return err
	}
	// Every recorded rank must be provably free: LoadState loaded and
	// ActiveState inactive. Unknown units or probe errors refuse the start.
	for _, node := range nodes {
		load, active, err := nativeRankStates(ctx, r, node, str(units[node]))
		if err != nil {
			return err
		}
		if load != "loaded" || (active != "inactive" && active != "failed") {
			return fail("%s is not free on %s. No rank was started.", str(units[node]), node)
		}
	}
	if err := nativeNoOwnedOverlap(ctx, r, name, nodes); err != nil {
		return err
	}
	snapshot, err := r.snapshot(ctx, true)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		comfy := obj(obj(snapshot["comfyui"])[node])
		if boolean(comfy["executing"]) || (num(comfy["running"]) > 0 && str(comfy["phase"]) != "waiting") {
			return fail("ComfyUI still has active GPU work. No job was stopped.")
		}
		// clearComfy implements the existing guard policy: only a
		// CONFIRMED idle cache is cleared; active or unknown image work
		// blocks the start. Protected audio is never touched.
		if e := r.clearComfy(ctx, node); e != nil {
			return e
		}
	}
	snapshot, err = r.snapshot(ctx, true)
	if err != nil {
		return err
	}
	for node, v := range obj(s["minimum_available_gib"]) {
		available := num(obj(obj(obj(snapshot["nodes"])[node])["memory_bytes"])["MemAvailable"]) / (1 << 30)
		if available < num(v) {
			return fail("%s has %.1f GiB available; this recipe needs %.1f GiB at startup.", node, available, num(v))
		}
	}
	if err := nativeRenderFrozen(ctx, r, s, nodes); err != nil {
		return err
	}
	// The partial-startup flag goes up BEFORE the first rank start so a
	// failed command's effects are still cleaned up. The durable attempt
	// marker is persisted BEFORE the first start command too: a failed
	// preflight record without the marker can never later stop pre-existing
	// units through stop/cleanup/ExecStop/parent fallback.
	*started = true
	if err := nativeMarkAttemptedStart(r, name, gen); err != nil {
		return err
	}
	// The rank is recorded as attempted — in memory AND durably — BEFORE
	// the start command: a command that fails after applying its side
	// effects (or a process that dies mid-start) must still be stopped by
	// the cleanup, while an unattempted rank is never stopped.
	order := stringsOf(value(s, "startup_order", anyStrings(reverse(nodes))))
	for _, node := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		if node == "" {
			continue
		}
		attempted[node] = str(units[node])
		if err := nativePersistAttempted(r, name, gen, attempted); err != nil {
			return err
		}
		if err := r.unit(ctx, node, "start", str(units[node])); err != nil {
			return err
		}
		if err := nativeRendezvous(ctx, r, node, s); err != nil {
			return err
		}
	}
	return nil
}

// nativeMarkAttemptedStart durably records, BEFORE the first start command,
// that THIS generation attempted a start: a later stop/cleanup of a failed
// record may then act on the generation's own units only.
func nativeMarkAttemptedStart(r *Runtime, name, gen string) error {
	return withFileLock(r.recordPath(name)+".lock", true, func() error {
		rec, e := r.readRecord(name)
		if e != nil {
			return e
		}
		if str(rec["generation"]) != gen {
			return fail("A newer lifecycle operation superseded this one.")
		}
		rec["attempted_start"] = true
		rec["updated_at"] = now()
		if e := validateRecord(rec, name); e != nil {
			return e
		}
		return writeObject(r.recordPath(name), rec)
	})
}

// nativePersistAttempted durably records exactly which ranks THIS
// generation started, so a later stop targets only owned units.
func nativePersistAttempted(r *Runtime, name, gen string, attempted Object) error {
	return withFileLock(r.recordPath(name)+".lock", true, func() error {
		rec, e := r.readRecord(name)
		if e != nil {
			return e
		}
		if str(rec["generation"]) != gen {
			return fail("A newer lifecycle operation superseded this one.")
		}
		rec["attempted"] = attempted
		rec["updated_at"] = now()
		if e := validateRecord(rec, name); e != nil {
			return e
		}
		return writeObject(r.recordPath(name), rec)
	})
}

// nativeRendezvous waits for the recipe rendezvous listener after the
// rendezvous node's rank started.
func nativeRendezvous(ctx context.Context, r *Runtime, node string, s Object) error {
	rv := obj(s["rendezvous"])
	if str(rv["node"]) != node {
		return nil
	}
	args := []string{"ss", "-H", "-ltn", "sport = :" + intText(rv["port"])}
	if node != "spark01" {
		args = []string{"ssh", str(obj(obj(r.config()["nodes"])[node])["ssh"]), shellJoin(args)}
	}
	deadline := time.Now().Add(100 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, e := r.execute(ctx, args, nil, 8*time.Second)
		if e == nil && strings.TrimSpace(out.Out) != "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fail("The recipe rendezvous did not start.")
}

// nativeAwaitReady waits until the record still carries the exact
// generation, every recorded rank is ACTIVE, the backend answers /health
// AND serves the canonical model id. Only then the record becomes ready.
func nativeAwaitReady(ctx context.Context, r *Runtime, name, gen string, s Object, nodes []string, units Object, stateDir string) error {
	// The canonical served id comes from the frozen record, never from the
	// current config.
	current, err := r.readRecord(name)
	if err != nil {
		return err
	}
	if str(current["generation"]) != gen {
		return fail("%s was superseded while starting.", name)
	}
	backend := strings.TrimRight(str(s["backend_url"]), "/")
	want := normalizedModelID(str(current["model_id"]))
	deadline := time.Now().Add(durationSeconds(value(s, "health_timeout_seconds", r.config()["health_timeout_seconds"]), 1800))
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := nativeRanksActive(ctx, r, nodes, units); err != nil {
			return err
		}
		if ids, err := backendModelIDs(ctx, backend, r.backendHeaders()); err == nil {
			if _, _, he := request(ctx, "GET", backend+"/health", nil, r.backendHeaders(), 3*time.Second); he == nil {
				found := false
				for _, id := range ids {
					if normalizedModelID(id) == want {
						found = true
						break
					}
				}
				if found {
					// CAS: only the exact current generation becomes ready.
					return r.TransitionState(name, gen, StateReady)
				}
			}
		}
		// A superseded generation or an external stop marker ends the wait
		// immediately: this supervisor no longer owns the operation.
		if superseded, err := nativeSuperseded(r, name, gen); superseded || err != nil {
			if err != nil {
				return err
			}
			return fail("%s was superseded while starting.", name)
		}
		if markerExists(filepath.Join(stateDir, "stopping."+name+"."+gen)) {
			return fail("%s was superseded while starting.", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fail("Model readiness timed out.")
}

// nativeRanksActive fails as soon as any recorded rank is not exactly
// active. Unknown probe results count as dead: never as alive.
func nativeRanksActive(ctx context.Context, r *Runtime, nodes []string, units Object) error {
	for _, node := range nodes {
		active, err := r.unitActive(ctx, node, str(units[node]))
		if err != nil {
			return err
		}
		if !active {
			return fail("A model rank stopped.")
		}
	}
	return nil
}

func markerExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// nativeSuperseded reports whether the durable record no longer belongs to
// this generation.
func nativeSuperseded(r *Runtime, name, gen string) (bool, error) {
	rec, err := r.readRecord(name)
	if err != nil {
		return false, err
	}
	return str(rec["generation"]) != gen, nil
}

// nativeConfirmStoppedContext confirms the shutdown of the given units with
// a fresh bounded context that survives the caller's cancellation: cleanup
// effects must finish even when the parent context is done.
func nativeConfirmStoppedContext(r *Runtime, name, gen string, units Object) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), nativeRankStopTimeout)
	defer cancel()
	return nativeConfirmStopped(cctx, r, name, gen, keys(units), units)
}

// nativeMonitor watches rank death for the whole model lifetime. It holds
// the device locks until EVERY owned teardown attempt and its confirmation
// finished, and only then releases them. It returns when the record was
// superseded, when a same-generation stop marker finished the owned
// teardown, when the context was canceled (after its own ranks were
// stopped, confirmed and CASed) or with an error after a rank died.
func nativeMonitor(ctx context.Context, r *Runtime, name, gen string, s Object, nodes []string, units Object, stateDir string, locks []*os.File, started bool, attempted Object) error {
	teardown := func(owns bool) error {
		var err error
		if owns {
			if err = nativeStopRanks(r, units); err == nil {
				err = nativeConfirmStoppedContext(r, name, gen, units)
			}
		}
		nativeCloseLocks(locks)
		return err
	}
	for {
		// A superseded NEW generation owns the operation now: this
		// supervisor must never touch the new units. It releases its locks
		// and exits without effects.
		if superseded, err := nativeSuperseded(r, name, gen); superseded || err != nil {
			if err != nil {
				nativeCloseLocks(locks)
				return err
			}
			nativeCloseLocks(locks)
			return nil
		}
		// A stop marker for the SAME generation means the teardown of this
		// generation's own ranks must be finished: stop, confirm, CAS to
		// stopped, and only then release the locks.
		if markerExists(filepath.Join(stateDir, "stopping."+name+"."+gen)) {
			if err := teardown(true); err != nil {
				return fail("Model cleanup needs attention: %v.", err)
			}
			return nativeCompleteStopped(r, name, gen)
		}
		if err := ctx.Err(); err != nil {
			return nativeInterruptedTeardown(r, name, gen, units, teardown)
		}
		if err := nativeRanksActive(ctx, r, nodes, units); err != nil {
			// A rank died: release the remaining owned ranks with fresh
			// bounded contexts, then record the honest verdict. Locks are
			// released only after the cleanup attempts finished.
			stopErr := teardown(true)
			if stopErr == nil {
				if ferr := nativeFinishTeardown(r, name, gen); ferr != nil {
					return fail("A model node stopped and its verdict could not be recorded: %v.", ferr)
				}
				return fail("A model node stopped. The remaining nodes were released.")
			}
			_ = r.TransitionState(name, gen, StateFailed)
			return fail("A model node stopped and cleanup needs attention.")
		}
		select {
		case <-ctx.Done():
			return nativeInterruptedTeardown(r, name, gen, units, teardown)
		case <-time.After(nativeMonitorInterval):
		}
	}
}

// nativeInterruptedTeardown records the outcome of an interrupted supervisor
// (SIGTERM or canceled context) AFTER its owned ranks were stopped and
// confirmed: a fully confirmed teardown is honestly stopped, anything else
// retains the failed verdict. The locks were already released by the
// teardown, which only happens once every shutdown attempt finished.
func nativeInterruptedTeardown(r *Runtime, name, gen string, units Object, teardown func(bool) error) error {
	if err := teardown(true); err != nil {
		_ = r.TransitionState(name, gen, StateFailed)
		return fail("Model shutdown was interrupted and cleanup needs attention.")
	}
	return nativeFinishTeardown(r, name, gen)
}

// nativeFinishTeardown records a fully confirmed teardown through the
// generation-checked, idempotent completion helper.
func nativeFinishTeardown(r *Runtime, name, gen string) error {
	return nativeCompleteStopped(r, name, gen)
}

// nativeCompleteStopped durably records a FULLY CONFIRMED teardown as
// stopped. It is generation-checked and IDEMPOTENT: a same-generation
// record already confirmed stopped returns nil with no further CAS and no
// effects, so a concurrent owned stop (nativeStop's real unit/port
// confirmation) and the monitor's own teardown never race into a false
// "Cannot move ... to failed" error.
//
// The ENTIRE completion is one atomic transaction under the SAME record
// file lock that TransitionState uses: the state read, generation check,
// allowed-edge validation and final write all happen while the lock is
// held, so there is no TOCTOU window between the initial check and the
// CAS. (TransitionState itself is NOT called here — it would take the
// same lock again and deadlock.) The intermediate stopping write happens
// under the same lock to preserve crash-safe ownership: a crash between
// the two writes leaves a durable stopping record that the recovery
// entries can still finish. Only the allowed state edges are used (via
// allowedTransitions), unknown states error, the generation is never
// rewritten, and a newer generation's record is never modified. The
// caller must already hold REAL stop proof (confirmed inactive ranks and
// a closed port); readiness or stop proof is never faked here.
func nativeCompleteStopped(r *Runtime, name, gen string) error {
	return withFileLock(r.recordPath(name)+".lock", true, func() error {
		rec, err := r.readRecord(name)
		if err != nil {
			return err
		}
		if str(rec["generation"]) != gen {
			return fail("A newer lifecycle operation superseded this one.")
		}
		current := str(rec["state"])
		switch current {
		case StateStopped:
			// The same generation was already confirmed and CASed stopped:
			// completion is idempotent, with zero further CAS and effects.
			return nil
		case StateStopping:
			// Direct stopping -> stopped edge.
		default:
			// Any other owned state goes through the real stopping edge
			// first, durably, under the same lock (crash-safe ownership).
			if !contains(allowedTransitions[current], StateStopping) {
				return fail("Cannot move %s from %s to %s.", name, current, StateStopping)
			}
			rec["state"] = StateStopping
			rec["updated_at"] = now()
			if err := validateRecord(rec, name); err != nil {
				return err
			}
			if err := writeObject(r.recordPath(name), rec); err != nil {
				return err
			}
		}
		rec["state"] = StateStopped
		rec["updated_at"] = now()
		if err := validateRecord(rec, name); err != nil {
			return err
		}
		return writeObject(r.recordPath(name), rec)
	})
}

// nativeStopRanks stops every given rank INDEPENDENTLY and IN PARALLEL,
// each with its own fresh bounded context derived from an uncanceled
// parent, so one hung head can never consume the whole stop budget and skip
// a worker rank. All node failures are aggregated.
func nativeStopRanks(r *Runtime, units Object) error {
	var mu sync.Mutex
	failed := []string{}
	var wg sync.WaitGroup
	for node, v := range units {
		unit := str(v)
		if !serviceName.MatchString(unit) {
			mu.Lock()
			failed = append(failed, node)
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(node, unit string) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), nativeRankStopTimeout)
			defer cancel()
			if err := r.unit(cctx, node, "stop", unit); err != nil {
				mu.Lock()
				failed = append(failed, node)
				mu.Unlock()
			}
		}(node, unit)
	}
	wg.Wait()
	if len(failed) > 0 {
		sort.Strings(failed)
		return fail("Could not confirm model shutdown on: %s.", strings.Join(failed, ", "))
	}
	return nil
}

// nativeStop performs the stop and cleanup actions. It acts only on the
// actual durable recorded units of the record's current generation, and a
// stale generation or invocation can never stop a newer operation. An owned
// FAILED or STOPPING record of the SAME generation is recovered: the units
// are stopped again and, once every unit and the port are confirmed free,
// the record becomes stopped. An unknown-ownership failure is retained.
func nativeStop(ctx context.Context, r *Runtime, name, gen string, s Object, nodes []string, units Object, stateDir, action string) error {
	rec, err := r.readRecord(name)
	if err != nil {
		return err
	}
	state := str(rec["state"])
	// The invocation guard: a stop arriving from an old systemd invocation
	// (a stale ExecStopPost) must never stop a newer operation.
	if inv := str(rec["invocation_id"]); inv != "" {
		if env := os.Getenv("INVOCATION_ID"); env != "" && env != inv {
			return fail("A stale stop request cannot stop a newer operation.")
		}
	}
	// Attempt ownership: a FAILED record whose generation never attempted a
	// start owns no processes; a later stop or cleanup must be refused with
	// zero effects BEFORE any state change.
	if state == StateFailed && rec["attempted_start"] != true {
		return fail("This generation never attempted a start; refusing to stop pre-existing units.")
	}
	marker := filepath.Join(stateDir, "stopping."+name+"."+gen)
	switch action {
	case "stop":
		switch state {
		case StateReady, StateStarting, StateQueued, StateFailed:
			// CAS first: a stale generation is refused before any effect.
			// Queued->Stopping and Failed->Stopping let a supervisor death
			// and a failed startup be recovered by the same entry.
			if err := r.TransitionState(name, gen, StateStopping); err != nil {
				// A concurrent owned teardown of the SAME generation may
				// have completed the stop (or moved it to stopping)
				// between this entry's state read and this CAS. Only
				// same-generation evidence may be honored; any wrong
				// generation, unknown record or other state remains an
				// error — the original CAS error is returned.
				cur, rerr := r.readRecord(name)
				if rerr != nil || str(cur["generation"]) != gen {
					return err
				}
				switch str(cur["state"]) {
				case StateStopping:
					// Join the owned stop already in progress.
				case StateStopped:
					// The same generation was already confirmed and
					// completed by the concurrent owned teardown: hold
					// FRESH physical proof (ranks inactive, port
					// closed) with ZERO extra unit stop commands and
					// ZERO further state CAS, then return nil.
					if cerr := nativeConfirmStopped(ctx, r, name, gen, nodes, units); cerr != nil {
						return fail("%v The concurrent teardown proof failed: %v.", err, cerr)
					}
					return nil
				default:
					return err
				}
			}
		case StateStopped:
			// A concurrent owned teardown of the same generation
			// already completed between this entry's read and its own
			// effect: confirm REAL stop proof and return confirmed nil
			// with zero extra unit stops and zero state CAS.
			if err := nativeConfirmStopped(ctx, r, name, gen, nodes, units); err != nil {
				return err
			}
			return nil
		case StateStopping:
			// Idempotent re-stop of the same generation.
		default:
			return fail("%s has no operation to stop.", name)
		}
		if err := atomicWrite(marker, []byte(gen), 0600); err != nil {
			return err
		}
	case "cleanup":
		// Cleanup is idempotent ONLY for its own completed generation: the
		// marker must carry exactly the record's current generation. There
		// is never a blind cleanup of a new operation. A FAILED record of
		// the same generation is recovered without a marker.
		switch state {
		case StateStopped:
			if b, e := os.ReadFile(marker); e != nil || strings.TrimSpace(string(b)) != gen {
				return fail("Cleanup does not belong to the current generation.")
			}
			return nativeConfirmStopped(ctx, r, name, gen, nodes, units)
		case StateStopping:
			if b, e := os.ReadFile(marker); e != nil || strings.TrimSpace(string(b)) != gen {
				return fail("Cleanup does not belong to the current generation.")
			}
		case StateFailed:
			if err := r.TransitionState(name, gen, StateStopping); err != nil {
				return err
			}
			if err := atomicWrite(marker, []byte(gen), 0600); err != nil {
				return err
			}
		default:
			return fail("Cleanup does not belong to the current generation.")
		}
	}
	// The current generation is re-checked directly before ANY unit stop
	// command: a superseded generation is refused with zero effects.
	current, err := r.readRecord(name)
	if err != nil {
		return err
	}
	if str(current["generation"]) != gen {
		return fail("A newer lifecycle operation superseded this one.")
	}
	// Attempt ownership: a generation that never attempted a start owns no
	// processes. A later stop/cleanup (ExecStop, parent fallback) must never
	// stop pre-existing units; it may only confirm a queued record's still
	// inactive units with ZERO stop commands.
	targets := units
	if current["attempted_start"] != true {
		if state == StateQueued || state == StateStopping {
			targets = nil // zero stop effects: this generation never started a rank
		} else {
			return fail("This generation never attempted a start; refusing to stop pre-existing units.")
		}
	} else if att := obj(current["attempted"]); len(att) > 0 {
		targets = att // only the ranks this generation actually started
	}
	if len(targets) > 0 {
		stopErr := nativeStopRanks(r, targets)
		if stopErr != nil {
			if casErr := r.TransitionState(name, gen, StateFailed); casErr != nil {
				return fail("%v The failed state could not be recorded: %v.", stopErr, casErr)
			}
			return stopErr
		}
	}
	if err := nativeConfirmStopped(ctx, r, name, gen, nodes, units); err != nil {
		if casErr := r.TransitionState(name, gen, StateFailed); casErr != nil {
			return fail("%v The failed state could not be recorded: %v.", err, casErr)
		}
		return err
	}
	// Real stop proof is held (ranks inactive, port closed): record the
	// verdict idempotently, so a concurrent owned teardown that already
	// confirmed and stopped the same generation does not fail.
	return nativeCompleteStopped(r, name, gen)
}

// nativeConfirmStopped refuses to report stopped until every recorded rank
// is a LOADED unit with an inactive/failed ActiveState AND the shared
// backend port recorded in the FROZEN setting is confirmed closed. A
// not-found (unknown) systemd unit is NOT shutdown proof, query errors and
// missing properties block, and the generation is re-checked from the
// record itself.
func nativeConfirmStopped(ctx context.Context, r *Runtime, name, gen string, nodes []string, units Object) error {
	rec, err := r.readRecord(name)
	if err != nil {
		return fail("Cannot confirm the shutdown of %s.", name)
	}
	if str(rec["generation"]) != gen {
		return fail("A newer lifecycle operation superseded this one.")
	}
	for _, node := range nodes {
		load, active, err := nativeRankStates(ctx, r, node, str(units[node]))
		if err != nil {
			return fail("Cannot confirm the shutdown of %s.", str(units[node]))
		}
		if load != "loaded" {
			return fail("%s is not a known unit on %s.", str(units[node]), node)
		}
		if active != "inactive" && active != "failed" {
			return fail("%s is still running.", str(units[node]))
		}
	}
	return nativeBackendClosed(strings.TrimRight(str(obj(rec["setting"])["backend_url"]), "/"))
}
