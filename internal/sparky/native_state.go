package sparky

// Stage 1 of the native lifecycle: durable state records and strong native
// readiness. A lifecycle record is a single JSON file under
// controller/lifecycle/<name>.json written 0600 atomically. It stores the
// immutable selected recipe/topology snapshot, the placement and a durable
// generation token that is created BEFORE any unit is started (stage 2).
// Every state transition re-checks the on-disk generation under a file lock,
// so a stale ready/cleanup can never overwrite a newer operation.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Lifecycle record states. queued/starting are set before any unit start;
// ready means the backend answered with the exact model; stopping/stopped
// belong to teardown; failed is a terminal supervisor verdict.
const (
	StateQueued   = "queued"
	StateStarting = "starting"
	StateReady    = "ready"
	StateStopping = "stopping"
	StateStopped  = "stopped"
	StateFailed   = "failed"
)

// allowedTransitions is the full state machine; anything not listed is
// refused so a stale writer cannot corrupt an in-flight operation.
var allowedTransitions = map[string][]string{
	StateQueued:   {StateStarting, StateStopping, StateFailed},
	StateStarting: {StateReady, StateFailed, StateStopping},
	StateReady:    {StateStopping, StateFailed},
	StateStopping: {StateStopped, StateFailed},
	StateStopped:  {},
	StateFailed:   {StateStopping},
}

func validLifecycleState(s string) bool {
	_, ok := allowedTransitions[s]
	return ok
}

// normalizedModelID lowercases, trims and strips the gateway prefix: the
// backend's /v1/models id must match the catalog name exactly after this
// normalization only.
func normalizedModelID(id string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(id), "garuda/")))
}

// canonicalNumber normalizes a JSON number literal without float coercion:
// exact digits are preserved except for redundant trailing fraction zeros
// ("8.0" and "8" are semantically equal, "262144" stays "262144").
func canonicalNumber(n json.Number) string {
	s := n.String()
	if i := strings.IndexByte(s, '.'); i >= 0 {
		t := strings.TrimRight(s[i+1:], "0")
		if t == "" {
			return s[:i]
		}
		return s[:i+1] + t
	}
	return s
}

// compactJSON encodes a value as compact JSON with sorted object keys.
// jsonBytes is deliberately pretty-printed with a trailing newline for
// durable files; fingerprints must never depend on that whitespace, so the
// canonical form always uses the compact encoder. encoding/json emits exact
// integers (including the full uint64 range) and preserves json.Number
// spelling, so an object literal and a JSON read roundtrip fingerprint
// identically with no float loss.
func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Fingerprint callers only pass config-derived plain data; an
		// unencodable value fails loudly instead of silently varying.
		return "\x00unencodable"
	}
	return string(b)
}

// canonicalValue renders a value as stable JSON text: object keys sorted
// (encoding/json already sorts map keys), arrays in order, numbers exact
// and compact, without any trailing newline.
func canonicalValue(v any) string {
	switch t := v.(type) {
	case json.Number:
		return canonicalNumber(t)
	case Object:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			b.WriteString(canonicalValue(t[k]))
		}
		b.WriteByte('}')
		return b.String()
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(canonicalValue(item))
		}
		b.WriteByte(']')
		return b.String()
	default:
		return compactJSON(v)
	}
}

// lifecycleFingerprint hashes only the lifecycle-relevant recipe/topology
// subset: the canonical config model key plus the served backend id, units,
// nodes, startup order, rendezvous, backend, context, parameters, bindings
// and memory requirements. Display metadata (reasoning flags, aliases,
// keep_loaded, human descriptions) never changes the fingerprint.
func lifecycleFingerprint(key string, m, s Object, nodes []string, placement string) string {
	selected := Object{
		"model_key":             key,
		"served_id":             servedModelID(key, m),
		"backend_url":           str(s["backend_url"]),
		"units":                 obj(s["units"]),
		"startup_order":         s["startup_order"],
		"rendezvous":            s["rendezvous"],
		"context_tokens":        s["context_tokens"],
		"concurrency":           s["concurrency"],
		"parameters":            s["parameters"],
		"parameter_bindings":    s["parameter_bindings"],
		"minimum_available_gib": s["minimum_available_gib"],
		"nodes":                 anyStrings(nodes),
		"placement":             placement,
	}
	sum := sha256.Sum256([]byte(canonicalValue(selected)))
	return hex.EncodeToString(sum[:])
}

// maxRecordBytes bounds a lifecycle record: an unknown or hostile file can
// never be slurped or silently overwritten as state.
const maxRecordBytes = 256 << 10

var (
	generationShape  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	fingerprintShape = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// validateRecord enforces the record shape: known state, valid name, node
// and unit identifiers (exactly one valid unit per recorded node, no
// missing or extra node), a recipe snapshot, a placement, a valid
// generation token and fingerprint shape.
func validateRecord(rec Object, name string) error {
	if rec == nil {
		return fail("Missing lifecycle record.")
	}
	if str(rec["name"]) != name {
		return fail("Lifecycle record does not match the model.")
	}
	if !validLifecycleState(str(rec["state"])) {
		return fail("Unknown lifecycle state.")
	}
	nodes := stringsOf(rec["nodes"])
	if len(nodes) == 0 {
		return fail("A lifecycle record needs at least one node.")
	}
	for _, n := range nodes {
		if !simpleName.MatchString(n) {
			return fail("Invalid node in lifecycle record.")
		}
	}
	units := obj(rec["units"])
	if len(units) != len(nodes) {
		return fail("A lifecycle record needs exactly one unit per recorded node.")
	}
	for node, v := range units {
		unit := str(v)
		if !contains(nodes, node) || !serviceName.MatchString(unit) {
			return fail("Invalid unit mapping in lifecycle record.")
		}
	}
	if !isObject(rec["setting"]) || str(obj(rec["setting"])["backend_url"]) == "" {
		return fail("A lifecycle record needs the selected recipe snapshot.")
	}
	// A defensive config snapshot and the canonical key are written by the
	// parent reservation; an adopted legacy record without them stays valid.
	if rec["config"] != nil && !isObject(rec["config"]) {
		return fail("The lifecycle record config snapshot must be an object.")
	}
	if rec["canonicalkey"] != nil && str(rec["canonicalkey"]) == "" {
		return fail("The lifecycle record canonical key must be a non-empty string.")
	}
	if !simpleName.MatchString(str(rec["placement"])) {
		return fail("Invalid placement in lifecycle record.")
	}
	if !generationShape.MatchString(str(rec["generation"])) {
		return fail("A lifecycle record needs a valid durable generation token.")
	}
	if !fingerprintShape.MatchString(str(rec["fingerprint"])) {
		return fail("A lifecycle record needs a valid recipe fingerprint.")
	}
	return nil
}

// readRecord reads and validates the durable record for a model. The file
// must be a small regular file, never a symlink or anything else: an
// unknown file can never be adopted as state or silently overwritten.
func (r *Runtime) readRecord(name string) (Object, error) {
	if !simpleName.MatchString(name) {
		return nil, fail("Invalid model name.")
	}
	path := r.recordPath(name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fail("The lifecycle record is not a regular state file.")
	}
	if info.Size() > maxRecordBytes {
		return nil, fail("The lifecycle record is unexpectedly large.")
	}
	rec, err := readObject(path)
	if err != nil {
		return nil, err
	}
	if err := validateRecord(rec, name); err != nil {
		return nil, err
	}
	return rec, nil
}

// writeRecordChecked writes a validated record under the state lock and
// refuses when the on-disk generation no longer matches expected: a stale
// writer can never clobber a newer operation.
func (r *Runtime) writeRecordChecked(name, expectedGeneration string, rec Object) error {
	return withFileLock(r.recordPath(name)+".lock", true, func() error {
		current, err := r.readRecord(name)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if os.IsNotExist(err) && expectedGeneration != "" {
			// A stale writer must never recreate a deleted record: fresh
			// records are only ever created by BeginOperation.
			return fail("The lifecycle record vanished; refusing stale recreation.")
		}
		if err == nil && str(current["generation"]) != expectedGeneration {
			return fail("A newer lifecycle operation superseded this one.")
		}
		if err := validateRecord(rec, name); err != nil {
			return err
		}
		return writeObject(r.recordPath(name), rec)
	})
}

// ownedUnits returns the units a stop/cleanup may touch for a record: the
// ranks whose start this generation actually ATTEMPTED (the durable attempt
// flags recorded by the supervisor), or all recorded units only when the
// record reached ready. A record with zero attempts (a preflight failure)
// owns nothing and must cause zero stop commands — the local helper started
// flag alone is insufficient for later ExecStop and parent cleanup, so the
// ownership is read from the durable record every time.
func ownedUnits(rec Object) Object {
	if att := obj(rec["attempted"]); len(att) > 0 {
		return copyObject(att)
	}
	if str(rec["state"]) == StateReady {
		return copyObject(obj(rec["units"]))
	}
	return Object{}
}

// recordAttemptedRank durably records that THIS generation attempted to
// start one rank, under the record lock and generation check: the parent's
// later cleanup and ExecStop must stop only the ranks this generation owns.
func recordAttemptedRank(r *Runtime, name, gen, node, unit string) error {
	return withFileLock(r.recordPath(name)+".lock", true, func() error {
		rec, err := r.readRecord(name)
		if err != nil {
			return err
		}
		if str(rec["generation"]) != gen {
			return fail("A newer lifecycle operation superseded this one.")
		}
		// The attempt contract: recording a rank attempt means THIS
		// generation attempted a start, so the attempted_start bool is
		// set together with the attempted map under the same lock —
		// matching the real supervisor, which always marks the attempt
		// before recording the rank map.
		att := obj(rec["attempted"])
		if att == nil {
			att = Object{}
		}
		att[node] = unit
		rec["attempted"] = att
		rec["attempted_start"] = true
		rec["updated_at"] = now()
		if err := validateRecord(rec, name); err != nil {
			return err
		}
		return writeObject(r.recordPath(name), rec)
	})
}

// topologyMatches reports whether the frozen record config snapshot and the
// current config agree on the topology of the selected nodes: the ssh host
// and direct IP per node plus the controller root. The recipe fingerprint
// alone omits these, so a host change must never be adopted or claimed
// ready; display-only node metadata is ignored.
func topologyMatches(snap, cfg Object, nodes []string) bool {
	if snap == nil || cfg == nil {
		return false
	}
	if str(snap["root"]) != str(cfg["root"]) {
		return false
	}
	snapNodes, curNodes := obj(snap["nodes"]), obj(cfg["nodes"])
	for _, n := range nodes {
		sn, cn := obj(snapNodes[n]), obj(curNodes[n])
		if sn == nil || cn == nil {
			return false
		}
		if str(sn["ssh"]) != str(cn["ssh"]) || str(sn["direct_ip"]) != str(cn["direct_ip"]) {
			return false
		}
	}
	return true
}

// adoptSelf reports whether the requested model's own durable record is an
// in-progress operation with exactly the selected recipe, placement, nodes
// and topology: the caller must adopt the same generation and wait instead
// of evicting itself. A ready record additionally needs a live supervisor.
func (r *Runtime) adoptSelf(ctx context.Context, name string, m, s Object, nodes []string, placement string) bool {
	rec, err := r.readRecord(name)
	if err != nil {
		return false
	}
	state := str(rec["state"])
	if state != StateQueued && state != StateStarting && state != StateReady {
		return false
	}
	if str(rec["fingerprint"]) != lifecycleFingerprint(name, m, s, nodes, placement) ||
		!sameStrings(stringsOf(rec["nodes"]), nodes) ||
		str(rec["placement"]) != placement ||
		!reflect.DeepEqual(obj(rec["units"]), obj(s["units"])) ||
		!topologyMatches(obj(rec["config"]), r.config(), nodes) {
		return false
	}
	if state == StateReady {
		live, err := r.supervisorActive(ctx, name)
		if err != nil || !live {
			return false
		}
	}
	return true
}

// BeginOperation creates (or adopts) the durable generation for a model
// start. It must run BEFORE any unit is started. If an in-progress record
// (queued/starting) already exists with the same recipe fingerprint it is
// adopted unchanged; a different fingerprint on an in-flight model is
// refused. A missing, malformed or terminal (stopped/failed) record is
// replaced with a fresh generation.
func (r *Runtime) BeginOperation(name string, m, s Object, nodes []string, placement string) (Object, error) {
	if !simpleName.MatchString(name) {
		return nil, fail("Invalid model name.")
	}
	if len(nodes) == 0 {
		return nil, fail("A model start needs at least one node.")
	}
	fp := lifecycleFingerprint(name, m, s, nodes, placement)
	var result Object
	err := withFileLock(r.recordPath(name)+".lock", true, func() error {
		existing, e := r.readRecord(name)
		switch {
		case e == nil:
			switch str(existing["state"]) {
			case StateQueued, StateStarting:
				if str(existing["fingerprint"]) == fp {
					if !topologyMatches(obj(existing["config"]), r.config(), nodes) {
						return fail("%s's operation was written for a different host topology; clean it up first.", name)
					}
					result = existing // adopt the matching in-progress operation
					return nil
				}
				return fail("%s already has a different operation in progress.", name)
			case StateReady, StateStopping:
				return fail("%s is still loaded or stopping.", name)
			case StateStopped:
				// A stopped record has released its devices; it is replaced.
			case StateFailed:
				// A failed record is NOT proof ownership was released: its
				// rank units may still be running. Reuse requires a
				// confirmed release for this same record: every recorded
				// rank inactive AND the supervisor gone. Otherwise the
				// failed record stays owned and blocks a new generation.
				if e := r.confirmFailedRelease(existing); e != nil {
					return e
				}
			}
			// stopped/failed: a verified-terminal record is replaced.
		case os.IsNotExist(e):
			// No record yet: start fresh.
		default:
			// Corrupt, malformed, symlinked or unreadable: never silently
			// overwrite a file that might hold real ownership.
			return e
		}
		rec := Object{
			"name":         name,
			"model_id":     servedModelID(name, m),
			"canonicalkey": name,
			"state":        StateQueued,
			"nodes":        anyStrings(nodes),
			"units":        copyObject(obj(s["units"])),
			"setting":      copyObject(s),
			"config":       copyObject(r.config()),
			"placement":    placement,
			"generation":   token(),
			"fingerprint":  fp,
			"updated_at":   now(),
		}
		if e := validateRecord(rec, name); e != nil {
			return e
		}
		if e := writeObject(r.recordPath(name), rec); e != nil {
			return e
		}
		result = rec
		return nil
	})
	return result, err
}

// confirmFailedRelease verifies that a failed record's ownership is
// actually released: every recorded rank must be a LOADED unit with an
// inactive/failed ActiveState probed through the RECORD's frozen config
// hosts (never the current runtime's), the exact frozen backend port must
// answer ECONNREFUSED and the model supervisor must be inactive or failed.
// Unreachable, not-found or otherwise unknown probes never count as free —
// no generation is ever overwritten while the previous ownership is
// unverified.
func (r *Runtime) confirmFailedRelease(rec Object) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	frozen := obj(rec["config"])
	if frozen == nil {
		return fail("The failed record has no frozen config snapshot; run an explicit cleanup first.")
	}
	units := obj(rec["units"])
	nodes := stringsOf(rec["nodes"])
	// Every probe runs through the record's frozen config clone: the ssh
	// hosts are the ones the failed generation was written for, so edited
	// current metadata can never redirect (or fake) the release proof.
	fr := r.frozenClone(frozen)
	for _, node := range nodes {
		unit := str(units[node])
		if !serviceName.MatchString(unit) {
			return fail("The failed record holds an invalid unit for %s; run an explicit cleanup first.", node)
		}
		load, active, err := nativeRankStates(ctx, fr, node, unit)
		if err != nil {
			return fail("The failed model's rank on %s is still active or unverified; run an explicit cleanup first.", node)
		}
		if load != "loaded" || (active != "inactive" && active != "failed") {
			return fail("The failed model's rank on %s is still active or unverified; run an explicit cleanup first.", node)
		}
	}
	if err := nativeBackendClosed(strings.TrimRight(str(obj(rec["setting"])["backend_url"]), "/")); err != nil {
		return fail("The failed model's backend port is still in use or unverified; run an explicit cleanup first.")
	}
	state, err := fr.supervisorState(ctx, str(rec["name"]))
	if err != nil || !contains([]string{"inactive", "failed"}, state) {
		return fail("The failed model's supervisor is still active or unverified; run an explicit cleanup first.")
	}
	return nil
}

// TransitionState moves a record to newState under the generation check.
func (r *Runtime) TransitionState(name, generation, newState string) error {
	if !validLifecycleState(newState) {
		return fail("Unknown lifecycle state.")
	}
	return withFileLock(r.recordPath(name)+".lock", true, func() error {
		rec, err := r.readRecord(name)
		if err != nil {
			return err
		}
		if str(rec["generation"]) != generation {
			return fail("A newer lifecycle operation superseded this one.")
		}
		current := str(rec["state"])
		if !contains(allowedTransitions[current], newState) {
			return fail("Cannot move %s from %s to %s.", name, current, newState)
		}
		rec["state"] = newState
		rec["updated_at"] = now()
		if err := validateRecord(rec, name); err != nil {
			return err
		}
		return writeObject(r.recordPath(name), rec)
	})
}

// unitActive probes one recorded rank unit with a small systemctl is-active
// query; remote nodes go through their configured ssh host. No disk, GPU or
// Docker inventory is ever touched on the inference path.
func (r *Runtime) unitActive(ctx context.Context, node, unit string) (bool, error) {
	if !serviceName.MatchString(unit) {
		return false, fail("Invalid service operation.")
	}
	args := []string{"systemctl", "is-active", "--quiet", unit}
	if node != "spark01" {
		h := str(obj(obj(r.config()["nodes"])[node])["ssh"])
		if err := safeHost(h); err != nil {
			return false, err
		}
		args = []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", h, shellJoin(args)}
	}
	x, err := r.execute(ctx, args, nil, 12*time.Second)
	if err != nil {
		return false, err
	}
	return x.Code == 0, nil
}

// backendModelIDs lists the model ids the backend itself serves.
func backendModelIDs(ctx context.Context, backend string, headers map[string]string) ([]string, error) {
	o, err := requestObject(ctx, "GET", backend+"/v1/models", nil, headers, 5*time.Second, false)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, v := range arr(o["data"]) {
		if id := str(obj(v)["id"]); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// servedModelID resolves the exact id the backend must serve: an explicit
// model_id mapping wins, otherwise the canonical config key. The human
// display name is never used as an identity.
func servedModelID(key string, m Object) string {
	if id := str(m["model_id"]); id != "" {
		return id
	}
	return key
}

// backendForRecipe returns the backend URL a frozen recipe really binds: the
// selected setting's backend_url first, then the backend_url recorded in the
// same record's frozen setting (a recipe rewritten after the load must never
// redirect readiness or inference to another endpoint). An empty result means
// no trustworthy backend identity exists for this placement.
func backendForRecipe(s Object, rec Object) string {
	if b := strings.TrimRight(str(s["backend_url"]), "/"); b != "" {
		return b
	}
	return strings.TrimRight(str(obj(rec["setting"])["backend_url"]), "/")
}

// nativeReady is the strong native readiness: a validated READY record with
// the matching recipe fingerprint, placement, nodes and units, the current
// generation, an exactly ACTIVE supervisor, all recorded rank units ACTIVE,
// a successful /health AND the exact expected model id served by
// /v1/models. The durable state and generation are re-checked after the
// probes, so a concurrent newer operation is never reported ready.
func (r *Runtime) nativeReady(ctx context.Context, name string, m, s Object, nodes []string, placement string) bool {
	rec, err := r.readRecord(name)
	if err != nil {
		return false
	}
	if str(rec["state"]) != StateReady {
		return false
	}
	generation := str(rec["generation"])
	if generation == "" ||
		!sameStrings(stringsOf(rec["nodes"]), nodes) ||
		!reflect.DeepEqual(obj(rec["units"]), obj(s["units"])) ||
		str(rec["placement"]) != placement ||
		str(rec["fingerprint"]) != lifecycleFingerprint(name, m, s, nodes, placement) {
		return false
	}
	// The recipe fingerprint omits the node ssh hosts, direct IPs and the
	// controller root: a host change must never be claimed ready by
	// probing an unrelated host.
	if !topologyMatches(obj(rec["config"]), r.config(), nodes) {
		return false
	}
	state, err := r.supervisorState(ctx, name)
	if err != nil || state != "active" {
		return false
	}
	for node, v := range obj(rec["units"]) {
		ok, e := r.unitActive(ctx, node, str(v))
		if e != nil || !ok {
			return false
		}
	}
	// The backend identity comes from the selected recipe, then from the
	// record's own frozen setting: an edited backend_url must never claim a
	// ready record served by an unrelated endpoint.
	backend := backendForRecipe(s, rec)
	if backend == "" {
		return false
	}
	if _, _, err := request(ctx, "GET", backend+"/health", nil, r.backendHeaders(), 3*time.Second); err != nil {
		return false
	}
	ids, err := backendModelIDs(ctx, backend, r.backendHeaders())
	if err != nil {
		return false
	}
	want := normalizedModelID(servedModelID(name, m))
	found := false
	for _, id := range ids {
		if normalizedModelID(id) == want {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	fresh, err := r.readRecord(name)
	if err != nil || str(fresh["state"]) != StateReady || str(fresh["generation"]) != generation {
		return false
	}
	return true
}
