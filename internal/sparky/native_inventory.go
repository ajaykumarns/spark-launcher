package sparky

// Native durable ownership inventory. This is a READ-ONLY scan of the
// durable controller/lifecycle records: it reports which model canonical
// names hold resource occupancy (reserved rank nodes) right now, so the
// coordinator can never admit image or new-model work onto nodes that a
// queued/starting/ready/stopping/failed record still owns — even when the
// model is disabled, deleted from the current config or its placement was
// edited, and even across a controller restart. It never runs, starts or
// stops anything and never writes state; physical RSS/GPU probes stay in
// the existing node inspection, and strong readiness stays in nativeReady.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// nativeOwnership scans controller/lifecycle/*.json with the same strict
// readRecord used everywhere else and returns a map of canonical model name
// to a minimal status row. A missing lifecycle directory is an empty
// inventory; any present record that is corrupt, malformed, symlinked,
// oversized or internally inconsistent (state, generation, nodes, units or
// config snapshot) is an ERROR, never a free/empty result: unknown bytes on
// disk must never let image or new-model work reuse a reserved GPU.
func (r *Runtime) nativeOwnership(ctx context.Context) (Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(r.root(), "controller/lifecycle")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return Object{}, nil
		}
		return nil, fail("Cannot read the lifecycle ownership directory: %v.", err)
	}
	names := []string{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue // ".lock" companions and anything else are not records
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		// A record filename must be the canonical model name: anything else
		// under the lifecycle directory is unexpected and refuses the scan.
		if !simpleName.MatchString(name) {
			return nil, fail("Invalid lifecycle ownership record name: %s.", e.Name())
		}
		names = append(names, name)
	}
	out := Object{}
	cfg := r.config()
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rec, err := r.readRecord(name)
		if err != nil {
			return nil, err
		}
		// The record must carry the whole config snapshot written by the
		// Core producer, and its root and node set must be consistent: a
		// record for an unknown node can never be treated as released.
		snap := obj(rec["config"])
		if snap == nil {
			return nil, fail("The %s lifecycle record is missing its config snapshot.", name)
		}
		if str(snap["root"]) != r.root() {
			return nil, fail("The %s lifecycle record was written for a different root.", name)
		}
		// The snapshot's "nodes" is the frozen config's node map (name ->
		// node settings), not a string list: the known set is its keys.
		known := map[string]bool{}
		for n := range obj(snap["nodes"]) {
			known[n] = true
		}
		for _, n := range stringsOf(rec["nodes"]) {
			if !known[n] {
				return nil, fail("The %s lifecycle record holds an unknown node.", name)
			}
		}
		// The canonical key is part of the frozen identity: when present it
		// must equal the file's canonical model name. Legacy records without
		// one stay valid.
		if rec["canonicalkey"] != nil && str(rec["canonicalkey"]) != name {
			return nil, fail("The %s lifecycle record canonical key does not match its file.", name)
		}
		// The frozen snapshot must still contain the model and the selected
		// placement setting, and the record's recipe fingerprint must match
		// what that frozen recipe hashes to: a tampered units/nodes/placement
		// or served id must never be reported as a released or resolvable
		// owner. This is a pure read-only recomputation over the frozen
		// bytes — display metadata and the live current config are never
		// consulted here, so disabled or deleted models stay valid owners.
		frozen := obj(obj(snap["models"])[name])
		placement := str(rec["placement"])
		if frozen == nil {
			return nil, fail("The %s lifecycle record is missing its frozen model.", name)
		}
		frozenSetting := obj(obj(frozen["settings"])[placement])
		if frozenSetting == nil {
			return nil, fail("The %s lifecycle record is missing its frozen placement setting.", name)
		}
		if str(rec["fingerprint"]) != lifecycleFingerprint(name, frozen, frozenSetting, stringsOf(rec["nodes"]), placement) {
			return nil, fail("The %s lifecycle record fingerprint does not match its frozen recipe.", name)
		}
		state := str(rec["state"])
		// Occupancy is a RESERVATION, not resident GPU weights: every
		// non-stopped record keeps holding its recorded node set even if no
		// rank has started yet or the supervisor is restarting. Only a
		// validated stopped record has released it.
		reserved := state != StateStopped
		nodes := []string{}
		if reserved {
			nodes = unionNodes(stringsOf(rec["nodes"]), nil)
		}
		// keep_loaded is the effective current policy when the canonical
		// name still exists in the live config, otherwise the policy frozen
		// in the record's config snapshot. desired is the current run_on
		// only — the current placement is never guessed for a removed or
		// disabled model.
		var desired any
		keep := false
		if isObject(obj(cfg["models"])[name]) {
			m := obj(obj(cfg["models"])[name])
			desired = m["run_on"]
			keep = boolean(m["keep_loaded"])
		} else {
			keep = boolean(obj(obj(snap["models"])[name])["keep_loaded"])
		}
		out[name] = Object{
			"kind":            "llm",
			"shared_device":   boolean(frozenSetting["shared_device"]),
			"state":           state,
			"nodes":           nodes,
			"loaded":          reserved,
			"reserved":        reserved,
			"keep_loaded":     keep,
			"desired":         desired,
			"active_requests": 0,
		}
	}
	return out, nil
}
