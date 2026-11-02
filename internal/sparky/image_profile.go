package sparky

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Explicit image memory profiles. ComfyUI still loads models per request;
// these only bound the image engine's cgroup. Existing source model names and
// policies remain the central authority for everything else.
var imageProfiles = map[string]Object{
	"shared":    Object{"high_gib": 14, "max_gib": 16, "minimum_available_gib": 21.5},
	"dedicated": Object{"high_gib": 40, "max_gib": 48, "minimum_available_gib": 96},
}

// imageFlashNext is the exact source model name that dedicated mode requires.
const imageFlashNext = "qwen3.8-flash-next"

// imageNode is the only node whose image profile these modes change.
const imageNode = "spark02"

// giB is one gibibyte in bytes. imageProfileRun/imageProfileRequest are the
// effect seams: tests inject fakes here and fail unexpected commands;
// production wires the real runner and HTTP client.
const giB = int64(1) << 30

var imageDrainAttempts = 600
var imageReadinessAttempts = 90

var imageProfileRun = func(args []string, input []byte, timeout time.Duration) (CommandResult, error) {
	return command(args, input, timeout)
}

var imageProfileRequest = func(ctx context.Context, method, url string, payload any, timeout time.Duration) (Object, error) {
	return requestObject(ctx, method, url, payload, nil, timeout, true)
}

// imageRuntime is the injectable runtime seam: only the effects this module
// needs, backed by the real Runtime in production.
type imageRuntime interface {
	Snapshot(ctx context.Context, fresh bool) (Object, error)
	NativeActivity(ctx context.Context) (map[string]int, error)
	ClearComfy(ctx context.Context, node string) error
	InspectNode(ctx context.Context, name string) (Object, error)
	Unit(ctx context.Context, node, action, unit string) error
}

// imageRuntimeEffects adapts the concrete *Runtime to the seam. The adapter
// lives in the same package, so the lowercase methods are visible.
type imageRuntimeEffects struct{ r *Runtime }

func (a imageRuntimeEffects) Snapshot(ctx context.Context, fresh bool) (Object, error) {
	return a.r.snapshot(ctx, fresh)
}
func (a imageRuntimeEffects) NativeActivity(ctx context.Context) (map[string]int, error) {
	return a.r.legacyActivity(ctx)
}
func (a imageRuntimeEffects) ClearComfy(ctx context.Context, node string) error {
	return a.r.clearComfy(ctx, node)
}
func (a imageRuntimeEffects) InspectNode(ctx context.Context, name string) (Object, error) {
	return a.r.inspectNode(ctx, name)
}
func (a imageRuntimeEffects) Unit(ctx context.Context, node, action, unit string) error {
	return a.r.unit(ctx, node, action, unit)
}

var newImageRuntime = func(c Object, path string) imageRuntime {
	return imageRuntimeEffects{r: newRuntime(c, path)}
}

// imageProfileCount accepts exactly the Probe queue shapes: a boolean or a
// finite, nonnegative number. Anything else is unknown.
func imageProfileCount(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case json.Number:
		f, err := x.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
			return 0, false
		}
		return f, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) || x < 0 {
			return 0, false
		}
		return x, true
	case int:
		if x < 0 {
			return 0, false
		}
		return float64(x), true
	case int64:
		if x < 0 {
			return 0, false
		}
		return float64(x), true
	}
	return 0, false
}

// imageProfileBusy reports whether the image engine reports active or queued
// work, matching the exact Probe shape (reachable/running/pending). A missing
// field, null, a wrong type, a negative count, or an unreachable or unknown
// engine is busy: failures and unknowns must never be read as idle.
func imageProfileBusy(state Object) bool {
	if !boolean(state["reachable"]) {
		return true
	}
	running, ok := imageProfileCount(state["running"])
	if !ok {
		return true
	}
	pending, ok := imageProfileCount(state["pending"])
	if !ok {
		return true
	}
	return running != 0 || pending != 0
}

func imageNodeMeta(config Object, node string) Object {
	return obj(obj(config["nodes"])[node])
}

// imageProfileBytes accepts an exact integer byte count via the common
// toInt helper: absent values, malformed numbers, negatives, and fractional
// or non-finite floats (NaN/Inf) are rejected rather than truncated.
func imageProfileBytes(v any) (int64, bool) {
	n, err := toInt(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return int64(n), true
}

// EvaluateImageProfile is the pure evaluation helper: it validates the mode
// against the shared snapshot and returns the planned profile without any
// effect. The snapshot must show Flash Next ready on Spark01 and no LLM on
// Spark02 for dedicated, and a known-idle image engine on Spark02.
func EvaluateImageProfile(config, snapshot Object, mode string) (Object, error) {
	profile, ok := imageProfiles[mode]
	if !ok {
		return nil, fail("Select shared or dedicated.")
	}
	if mode == "dedicated" {
		row := obj(obj(snapshot["models"])[imageFlashNext])
		nodes := stringsOf(row["nodes"])
		if str(row["state"]) != "ready" || !sameStrings(nodes, []string{"spark01"}) {
			return nil, fail("First select and load Flash Next on Spark01 with Sparky.")
		}
		for name, raw := range obj(snapshot["models"]) {
			state := obj(raw)
			if str(state["kind"]) == "llm" && boolean(state["loaded"]) && contains(stringsOf(state["nodes"]), "spark02") {
				return nil, fail("A language model %s still uses Spark02. Keep the current image profile.", name)
			}
		}
	}
	if imageProfileBusy(obj(obj(obj(snapshot["nodes"])[imageNode])["comfyui"])) {
		return nil, fail("Image work is active. Wait for it to finish before changing the profile.")
	}
	planned := copyObject(profile)
	planned["profile"] = mode
	planned["node"] = imageNode
	planned["llm_restarted"] = false
	planned["image_service_restarted"] = false
	return planned, nil
}

// imageWorkerCommand runs one command word list on the worker through the
// configured SSH alias. Diagnostics never surface: failures carry no host or
// command output.
func imageWorkerCommand(host string, words []string, timeout time.Duration) (string, error) {
	if err := safeHost(host); err != nil {
		return "", err
	}
	result, err := imageProfileRun([]string{"ssh", "-o", "BatchMode=yes", host, shellJoin(words)}, nil, timeout)
	if err != nil || result.Code != 0 {
		return "", fail("The image service operation did not succeed.")
	}
	return result.Out, nil
}

// imageProperties reads the exact worker cgroup memory properties. Anything
// other than the three expected integer properties is an error, never a
// guessed value.
func imageProperties(host, unit string) (map[string]string, error) {
	raw, err := imageWorkerCommand(host, []string{"systemctl", "show", unit,
		"-p", "MemoryHigh", "-p", "MemoryMax", "-p", "MemorySwapMax"}, time.Minute)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			values[key] = value
		}
	}
	for _, key := range []string{"MemoryHigh", "MemoryMax", "MemorySwapMax"} {
		v, ok := values[key]
		if !ok || !imageDecimal(v) {
			return nil, fail("Cannot confirm the current image memory limits.")
		}
	}
	if len(values) != 3 {
		return nil, fail("Cannot confirm the current image memory limits.")
	}
	return values, nil
}

// imageSetPropertyStrings sends exact property strings. Old limits are
// decimal strings that may exceed the signed 64-bit range (systemd reports
// its infinity as the full unsigned 64-bit value), so restoration must send
// the exact original spelling instead of a parsed integer.
func imageSetPropertyStrings(host, unit string, values map[string]string) error {
	words := []string{"sudo", "-n", "systemctl", "set-property", unit}
	for _, key := range []string{"MemoryHigh", "MemoryMax", "MemorySwapMax"} {
		words = append(words, key+"="+values[key])
	}
	_, err := imageWorkerCommand(host, words, time.Minute)
	return err
}

func imageSetProperties(host, unit string, values map[string]int64) error {
	send := map[string]string{}
	for key, v := range values {
		send[key] = strconv.FormatInt(v, 10)
	}
	return imageSetPropertyStrings(host, unit, send)
}

// imageRestorable reports whether an original property value can be sent
// back unchanged: either an exact decimal string or systemd's infinity.
func imageRestorable(v string) bool {
	return imageDecimal(v) || v == "infinity"
}

func imageDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// imageAvailable reads the node's MemAvailable through the real probe path.
// An absent, malformed, negative, or fractional reading is an error; it is
// never truncated or guessed.
func imageAvailable(ctx context.Context, rt imageRuntime) (int64, error) {
	data, err := rt.InspectNode(ctx, imageNode)
	if err != nil {
		return 0, err
	}
	available, ok := imageProfileBytes(obj(obj(data)["memory_bytes"])["MemAvailable"])
	if !ok {
		return 0, fail("Cannot confirm available memory on the image worker.")
	}
	return available, nil
}

// imageLeases validates the controller admission report, matching the real
// Coordinator.admission() shape: active and waiting must be real arrays and
// config_transition a real boolean (it already combines fence and reloading).
// The fenced field is optional for backward compatibility: absent means
// false, but an explicitly present value must be a real boolean. Malformed
// reports are refused instead of being read as idle.
func imageLeases(leases Object) (active, waiting []any, transition, fenced bool, err error) {
	if a, ok := leases["active"].([]any); ok {
		active = a
	} else {
		return nil, nil, false, false, fail("Cannot confirm the controller admission state.")
	}
	if w, ok := leases["waiting"].([]any); ok {
		waiting = w
	} else {
		return nil, nil, false, false, fail("Cannot confirm the controller admission state.")
	}
	t, ok := leases["config_transition"].(bool)
	if !ok {
		return nil, nil, false, false, fail("Cannot confirm the controller admission state.")
	}
	if raw, present := leases["fenced"]; present {
		f, ok := raw.(bool)
		if !ok {
			return nil, nil, false, false, fail("Cannot confirm the controller admission state.")
		}
		fenced = f
	}
	return active, waiting, t, fenced, nil
}

// ActivateImageProfile shows, plans, or applies an explicit image memory
// profile on the Spark02 image engine. Show reads engine system_stats, dry
// run reports the plan with zero writes or service commands, and apply
// changes the worker cgroup limits under the controller fence and updates
// the central policy floor.
func ActivateImageProfile(root, mode string, dryRun bool) (Object, error) {
	ctx := context.Background()
	path := filepath.Join(root, "config", "models.json")
	config, err := readCluster(path)
	if err != nil {
		return nil, err
	}
	rt := newImageRuntime(config, path)
	port := controllerPort(config)
	if port == 0 {
		return nil, fail("The cluster file has no usable controller port.")
	}
	ctl := func(route string, payload any) (Object, error) {
		method := "GET"
		if payload != nil {
			method = "POST"
		}
		url := "http://127.0.0.1:" + strconv.Itoa(port) + "/control/" + route
		return imageProfileRequest(ctx, method, url, payload, 30*time.Second)
	}
	meta := imageNodeMeta(config, imageNode)
	comfyURL := strings.TrimSuffix(str(meta["comfy_url"]), "/")

	if mode == "show" {
		state, err := imageProfileRequest(ctx, "GET", comfyURL+"/system_stats", nil, 30*time.Second)
		if err != nil {
			return nil, err
		}
		limit, ok := imageProfileBytes(obj(state["system"])["ram_total"])
		if !ok {
			return nil, fail("The image API did not report a usable service memory limit.")
		}
		return Object{
			"status":                     "current",
			"service_memory_limit_bytes": json.Number(strconv.FormatInt(limit, 10)),
			"minimum_available_gib":      obj(config["policy"])["comfyui_min_available_gib"],
		}, nil
	}

	snapshot, err := rt.Snapshot(ctx, true)
	if err != nil {
		return nil, err
	}
	planned, err := EvaluateImageProfile(config, snapshot, mode)
	if err != nil {
		return nil, err
	}
	if dryRun {
		out := Object{"status": "planned"}
		for k, v := range planned {
			out[k] = v
		}
		return out, nil
	}

	// Read the original bytes and mode before raising the fence so an
	// early read or stat failure can never leak an operator fence.
	original, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	fileMode := info.Mode().Perm()
	// The initial readCluster config and these original bytes must describe
	// the same baseline. An edit that arrived between the two reads is
	// drift: abort instead of planning against a stale config or rolling
	// back over another writer's data. Semantic JSON comparison via the
	// validated Object handles exact-numeric spelling on the .json path.
	if baseline, derr := decode(original); derr != nil || !bytes.Equal(jsonBytes(baseline), jsonBytes(config)) {
		return nil, fail("The cluster settings changed during the operation.")
	}

	// Refuse before any effect when the controller report is malformed, a
	// transition is active, or an operator fence already holds admission.
	leases, err := ctl("leases", nil)
	if err != nil {
		return nil, err
	}
	_, _, transition, fenced, err := imageLeases(leases)
	if err != nil {
		return nil, err
	}
	if fenced {
		return nil, fail("A manual admission fence is active. Lift it before changing the image profile.")
	}
	if transition {
		return nil, fail("A resource change is active. Retry after it finishes.")
	}
	if _, err := ctl("fence", Object{"enabled": true}); err != nil {
		return nil, err
	}

	host := str(meta["ssh"])
	unit := str(meta["comfy_unit"])

	var oldLimits map[string]string
	var written []byte
	configWritten := false
	changed := false
	wanted := map[string]int64{
		"MemoryHigh":    int64(num(planned["high_gib"])) * giB,
		"MemoryMax":     int64(num(planned["max_gib"])) * giB,
		"MemorySwapMax": 0,
	}
	planned["image_service_restarted"] = false
	var available int64
	err = func() error {
		// Pause new work, then wait for active leases and legacy activity
		// to drain. Requests waiting behind the fence are held by it and
		// must not block the drain. Failures never infer idle.
		drained := false
		for i := 0; i < imageDrainAttempts; i++ {
			state, err := ctl("leases", nil)
			if err != nil {
				return err
			}
			active, _, _, _, err := imageLeases(state)
			if err != nil {
				return err
			}
			activity, err := rt.NativeActivity(ctx)
			if err != nil {
				return err
			}
			if len(active) == 0 && len(activity) == 0 {
				drained = true
				break
			}
			time.Sleep(time.Second)
		}
		if !drained {
			return fail("Requests did not drain. The profile was kept.")
		}
		// Re-evaluate against a fresh snapshot once the fence holds.
		snapshot, err := rt.Snapshot(ctx, true)
		if err != nil {
			return err
		}
		if _, err := EvaluateImageProfile(config, snapshot, mode); err != nil {
			return err
		}
		if err := rt.ClearComfy(ctx, imageNode); err != nil {
			return err
		}
		available, err = imageAvailable(ctx, rt)
		if err != nil {
			return err
		}
		if oldLimits, err = imageProperties(host, unit); err != nil {
			return err
		}
		// Refuse nonrestorable limits before any effect: the original
		// values must be sendable back exactly if the change later fails.
		for _, key := range []string{"MemoryHigh", "MemoryMax", "MemorySwapMax"} {
			if !imageRestorable(oldLimits[key]) {
				return fail("Cannot confirm the current image memory limits.")
			}
		}
		oldMax, perr := strconv.ParseInt(oldLimits["MemoryMax"], 10, 64)
		// A valid decimal beyond the signed 64-bit range (systemd infinity)
		// is strictly larger than any wanted limit, so it requires restart.
		restart := perr != nil || oldMax > wanted["MemoryMax"] || float64(available) < (num(planned["minimum_available_gib"])+0.5)*float64(giB)
		// Mark changed before the call: set-property can partially apply
		// on the worker and then fail, so the original limits must be
		// restored even when this returns an error.
		changed = true
		if err := imageSetProperties(host, unit, wanted); err != nil {
			return err
		}
		if restart {
			state, err := rt.InspectNode(ctx, imageNode)
			if err != nil {
				return err
			}
			if imageProfileBusy(obj(state["comfyui"])) {
				return fail("Image work arrived during the change. Keep the existing profile.")
			}
			// Only the idle image engine restarts; no LLM is ever
			// selected, loaded, or restarted by this path.
			if err := rt.Unit(ctx, imageNode, "restart", unit); err != nil {
				return err
			}
			planned["image_service_restarted"] = true
			ready := false
			for i := 0; i < imageReadinessAttempts; i++ {
				if _, err := imageProfileRequest(ctx, "GET", comfyURL+"/sparky/state", nil, 2*time.Second); err == nil {
					ready = true
					break
				}
				time.Sleep(time.Second)
			}
			if !ready {
				return fail("The idle image service did not become ready.")
			}
			if available, err = imageAvailable(ctx, rt); err != nil {
				return err
			}
		}
		if available < int64(num(planned["minimum_available_gib"])*float64(giB)) {
			return fail("Available RAM is below the tested admission floor. The profile was kept.")
		}
		confirmed, err := imageProperties(host, unit)
		if err != nil {
			return err
		}
		for _, key := range []string{"MemoryHigh", "MemoryMax", "MemorySwapMax"} {
			if confirmed[key] != strconv.FormatInt(wanted[key], 10) {
				return fail("The image service did not apply the requested memory limits.")
			}
		}
		// Detect concurrent central config edits before overwriting.
		current, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, original) {
			return fail("The cluster settings changed during the operation.")
		}
		updated := copyObject(config)
		obj(updated["policy"])["comfyui_min_available_gib"] = planned["minimum_available_gib"]
		if err := writeCluster(path, updated); err != nil {
			return err
		}
		// Track the write even if a later step fails: rollback must
		// still be attempted. writeObject emits exactly these bytes for a
		// .json config, so the expected content is derived up front and
		// never replaced with a fresh read: a fresh result could be an
		// external edit, and adopting it would make rollback overwrite
		// another writer's data. The original file mode is preserved.
		configWritten = true
		written = jsonBytes(updated)
		if err := os.Chmod(path, fileMode); err != nil {
			return err
		}
		if _, err := ctl("config", nil); err != nil {
			return err
		}
		// ComfyUI reads the current cgroup limit on each system_stats request.
		state, err := imageProfileRequest(ctx, "GET", comfyURL+"/system_stats", nil, 30*time.Second)
		if err != nil {
			return err
		}
		confirmedTotal, ok := imageProfileBytes(obj(state["system"])["ram_total"])
		if !ok || confirmedTotal != wanted["MemoryMax"] {
			return fail("The image API did not confirm the new service limit.")
		}
		return nil
	}()

	if err != nil {
		// Rollback attempts are independent: every restoration is
		// attempted even when another fails, and a failed restoration is
		// reported rather than silently claimed. External config edits
		// are never overwritten.
		restoration := []string{}
		if changed && oldLimits != nil {
			// Restore the exact original validated strings: parsing could
			// overflow on systemd's unsigned infinity and silently skip a
			// limit, restoring the default zero instead.
			if serr := imageSetPropertyStrings(host, unit, oldLimits); serr != nil {
				restoration = append(restoration, "worker cgroup limits")
			}
		}
		if configWritten {
			if current, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(current, written) {
				if werr := os.WriteFile(path, original, fileMode); werr == nil {
					if _, cerr := ctl("config", nil); cerr != nil {
						restoration = append(restoration, "controller config reload")
					}
				} else {
					restoration = append(restoration, "central config")
				}
			} else if rerr != nil {
				restoration = append(restoration, "central config (unconfirmed)")
			}
		}
		if fenceErr := func() error {
			_, ferr := ctl("fence", Object{"enabled": false})
			return ferr
		}(); fenceErr != nil {
			restoration = append(restoration, "admission fence")
		}
		if len(restoration) > 0 {
			return nil, fail("%v Restoration needs attention: %s could not be confirmed restored.",
				err, strings.Join(restoration, ", "))
		}
		return nil, err
	}
	if _, ferr := ctl("fence", Object{"enabled": false}); ferr != nil {
		return nil, fail("The profile was applied but the admission fence could not be lifted.")
	}
	out := Object{"status": "applied", "available_bytes": available}
	for k, v := range planned {
		out[k] = v
	}
	return out, nil
}
