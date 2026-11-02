package menubar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// object is the decoded JSON shape. Numbers are kept as json.Number so a
// malformed value is detected instead of silently becoming zero.
type object = map[string]any

// decodeObject parses exactly one JSON object and rejects trailing data, so
// a partial or concatenated output can never look like a valid status.
func decodeObject(b []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var o object
	if err := dec.Decode(&o); err != nil || o == nil {
		return nil, errors.New("invalid data")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("invalid data")
	}
	return o, nil
}

func jsonUnmarshalOne(b []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(target); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// bytesValue returns a non-negative byte count only when the JSON value is
// exactly a non-negative integer (possibly a json.Number or a float that is
// integral). Anything else stays unknown.
func bytesValue(v any) (*uint64, bool) {
	switch x := v.(type) {
	case json.Number:
		if u, err := strconv.ParseUint(x.String(), 10, 64); err == nil {
			return &u, true
		}
		if i, err := strconv.ParseInt(x.String(), 10, 64); err == nil && i >= 0 {
			u := uint64(i)
			return &u, true
		}
	case float64:
		if x >= 0 && x == float64(uint64(x)) && x <= float64(^uint64(0)) {
			u := uint64(x)
			return &u, true
		}
	case int:
		if x >= 0 {
			u := uint64(x)
			return &u, true
		}
	case int64:
		if x >= 0 {
			u := uint64(x)
			return &u, true
		}
	case uint64:
		return &x, true
	case uint:
		u := uint64(x)
		return &u, true
	case uint32:
		u := uint64(x)
		return &u, true
	}
	return nil, false
}

// countValue returns an integer count only when the JSON value is an exact
// non-negative integer. Unknown is nil.
func countValue(v any) (*int, bool) {
	switch x := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(x.String(), 10, 64); err == nil && i >= 0 {
			n := int(i)
			return &n, true
		}
	case float64:
		if x >= 0 && x == float64(int64(x)) {
			n := int(x)
			return &n, true
		}
	case int:
		if x >= 0 {
			return &x, true
		}
	case int64:
		if x >= 0 {
			n := int(x)
			return &n, true
		}
	}
	return nil, false
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func obj(v any) object {
	o, _ := v.(object)
	return o
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

// nodeIDList returns the node keys of a JSON object in deterministic order.
func nodeIDList(v any) []string {
	ids := make([]string, 0, len(obj(v)))
	for id := range obj(v) {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// nonLLMKinds are the workload kinds the status reports that are not
// language models. The menu's model list shows language models only, so
// loaded audio services and ComfyUI must never appear as loadable LLMs.
var nonLLMKinds = map[string]bool{
	"audio":       true,
	"comfyui":     true,
	"comfy_model": true,
}

// includeModel decides whether a status row is a language model worth
// showing. An explicit "llm" kind is included; a known non-LLM kind is
// excluded. A missing or unknown kind is included only when the row reports
// a loaded workload, so an older controller that omits kind can still show
// an actual running model without adding stopped audio/image rows.
func includeModel(row object) bool {
	kind := str(row["kind"])
	if kind != "" {
		return !nonLLMKinds[kind]
	}
	loaded, _ := row["loaded"].(bool)
	return loaded
}

// parseStatus converts `sparky --json status` into a Snapshot. A status
// without a usable "models" object is rejected; device rows that lack memory
// readings are kept with unknown bytes rather than dropped.
//
// Activity counts are preserved as unknown when the controller says it does
// not know them: an explicit global "activity_known": false or a null/missing
// per-model active_requests both yield a nil ActiveRequests, never zero.
func parseStatus(data object) (Snapshot, error) {
	modelsRaw, ok := data["models"].(object)
	if !ok || modelsRaw == nil {
		return Snapshot{}, errors.New("invalid data")
	}
	globalActivityKnown := true
	if known, isBool := data["activity_known"].(bool); isBool && !known {
		globalActivityKnown = false
	}
	models := make([]ModelStatus, 0, len(modelsRaw))
	for _, id := range sortedKeys(modelsRaw) {
		row := obj(modelsRaw[id])
		if row == nil {
			return Snapshot{}, errors.New("invalid model entry")
		}
		if !includeModel(row) {
			continue
		}
		ms := ModelStatus{ID: id}
		ms.Name = str(row["name"])
		if ms.Name == "" {
			ms.Name = id
		}
		if _, ok := row["state"].(string); ok {
			ms.State = str(row["state"])
		}
		loaded, _ := row["loaded"].(bool)
		ms.Loaded = loaded
		if nodes, ok := row["nodes"].([]any); ok {
			for _, n := range nodes {
				if s, ok := n.(string); ok && s != "" {
					ms.Nodes = append(ms.Nodes, s)
				}
			}
		}
		if ms.Nodes == nil {
			ms.Nodes = []string{}
		}
		// A per-model explicit activity_known=false also means unknown, even
		// when the global knowledge flag is true.
		modelActivityKnown := globalActivityKnown
		if known, isBool := row["activity_known"].(bool); isBool && !known {
			modelActivityKnown = false
		}
		if modelActivityKnown {
			if c, ok := countValue(row["active_requests"]); ok {
				ms.ActiveRequests = c
			}
		}
		models = append(models, ms)
	}

	devices := []DeviceStatus{}
	if nodes, ok := data["nodes"].(object); ok {
		for _, id := range sortedKeys(nodes) {
			row := obj(nodes[id])
			if row == nil {
				return Snapshot{}, errors.New("invalid node entry")
			}
			ds := DeviceStatus{ID: id}
			if mem, ok := row["memory_bytes"].(object); ok {
				if b, ok := bytesValue(mem["MemTotal"]); ok {
					ds.TotalBytes = b
				}
				if b, ok := bytesValue(mem["MemAvailable"]); ok {
					ds.AvailableBytes = b
				}
			}
			devices = append(devices, ds)
		}
	}

	return Snapshot{Models: models, Devices: devices}, nil
}

func sortedKeys(m object) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// parseCatalog converts `sparky --json models` into deterministic choices.
// Only configured canonical LLMs are offered; the synthetic spark-auto route
// is omitted because it is not a concrete model to load. Ordering is by
// canonical ID.
func parseCatalog(data object) ([]Choice, error) {
	rows, ok := data["models"].([]any)
	if !ok {
		return nil, errors.New("invalid data")
	}
	seen := map[string]bool{}
	choices := make([]Choice, 0, len(rows))
	for _, raw := range rows {
		row := obj(raw)
		if row == nil {
			return nil, errors.New("invalid model entry")
		}
		id, ok := row["id"].(string)
		if !ok || strings.TrimSpace(id) == "" {
			return nil, errors.New("invalid model entry")
		}
		if seen[id] {
			return nil, errors.New("duplicate model entry")
		}
		seen[id] = true
		if isSyntheticAuto(id) {
			continue
		}
		name := str(row["name"])
		if name == "" {
			name = shortName(id)
		}
		choices = append(choices, Choice{ID: id, Name: name})
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].ID < choices[j].ID })
	return choices, nil
}

func shortName(id string) string {
	if _, suffix, found := strings.Cut(id, "/"); found {
		return suffix
	}
	return id
}

// isSyntheticAuto reports whether an ID is the synthetic spark-auto route.
// Catalog IDs look like "spark/spark-auto" or "spark-auto".
func isSyntheticAuto(id string) bool {
	return shortName(id) == "spark-auto"
}

// resolveInCatalog matches a requested model (canonical ID, provider suffix,
// or alias) against the catalog and returns the canonical ID plus its display
// name. Matching is case-insensitive and refuses ambiguous aliases.
func resolveInCatalog(data object, requested string) (Choice, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		return Choice{}, errors.New("Choose a model to load.")
	}
	rows, ok := data["models"].([]any)
	if !ok {
		return Choice{}, errors.New("The model list is unavailable. Refresh and try again.")
	}
	type match struct {
		id   string
		name string
	}
	var matches []match
	for _, raw := range rows {
		row := obj(raw)
		if row == nil {
			continue
		}
		id := str(row["id"])
		if id == "" || isSyntheticAuto(id) {
			continue
		}
		names := []string{id, shortName(id)}
		for _, a := range arr(row["aliases"]) {
			if s, ok := a.(string); ok {
				names = append(names, s)
			}
		}
		for _, n := range names {
			if strings.ToLower(n) == requested {
				matches = append(matches, match{id: id, name: str(row["name"])})
				break
			}
		}
	}
	switch len(matches) {
	case 0:
		return Choice{}, errors.New("That model is not in the current model list. Refresh and choose again.")
	case 1:
		name := matches[0].name
		if name == "" {
			name = shortName(matches[0].id)
		}
		return Choice{ID: matches[0].id, Name: name}, nil
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.id)
		}
		return Choice{}, fmt.Errorf("That name matches more than one model: %s.", strings.Join(ids, ", "))
	}
}

// parseLoadResult reads `sparky --json load ID` and returns a short message.
// It never claims readiness on its own: the reported status must be "ready".
func parseLoadResult(data object) (string, error) {
	status := str(data["status"])
	workload := str(data["workload"])
	if workload == "" {
		workload = str(data["action"])
	}
	if workload == "" {
		workload = "The model"
	}
	switch status {
	case "ready":
		return workload + " is ready.", nil
	case "":
		return "", errors.New("The load returned invalid data. Update Sparky on this Mac.")
	case "failed", "error", "stopped", "stopping":
		return "", errors.New(workload + " did not load. Check the Sparks and try again.")
	case "starting", "queued":
		return "", errors.New(workload + " is still starting. Refresh in a moment.")
	default:
		// Any other terminal status is not a verified readiness claim.
		return "", errors.New("The load reported an unexpected status (" + sanitizeMessage(status) + "). Refresh and check the Sparks.")
	}
}
