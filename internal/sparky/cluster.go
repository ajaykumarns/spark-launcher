package sparky

import (
	"encoding/json"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var placements = map[string][]string{"spark01": {"spark01"}, "spark02": {"spark02"}, "both": {"spark01", "spark02"}}
var simpleName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var serviceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]*\.service$`)
var hostName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]*$`)
var containerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func safeHost(host string) error {
	if !hostName.MatchString(host) {
		return fail("Set an existing SSH host alias in the site file.")
	}
	return nil
}
func privateURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	// Ports must be explicitly numeric, within range, and never zero.
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}
func isObject(v any) bool { o, ok := v.(map[string]any); return ok && o != nil }
func isList(v any) bool   { _, ok := v.([]any); return ok }
func validStrings(v any) bool {
	if !isList(v) {
		return false
	}
	for _, s := range arr(v) {
		if _, ok := s.(string); !ok {
			return false
		}
	}
	return true
}
func boolField(o Object, k string) bool {
	v, ok := o[k]
	if !ok {
		return true
	}
	_, good := v.(bool)
	return good
}
func finite(v any) bool {
	switch x := v.(type) {
	case float64:
		return !math.IsNaN(x) && !math.IsInf(x, 0)
	case float32:
		return !math.IsNaN(float64(x)) && !math.IsInf(float64(x), 0)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return false
		}
		return !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return false
}
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, s := range a {
		if m[s] {
			return false
		}
		m[s] = true
	}
	for _, s := range b {
		if !m[s] {
			return false
		}
	}
	return true
}
func overlapNodes(a, b []string) bool {
	for _, s := range a {
		if contains(b, s) {
			return true
		}
	}
	return false
}
func unionNodes(a, b []string) []string {
	m := Object{}
	for _, s := range append(append([]string{}, a...), b...) {
		m[s] = true
	}
	return keys(m)
}

func readCluster(path string) (Object, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fail("Central configuration file not found: %s.", path)
	}
	var c Object
	if strings.EqualFold(filepath.Ext(path), ".json") {
		// Exact numeric spelling must survive into the cluster config, so
		// central JSON uses the strict number-preserving decoder.
		c, err = decode(b)
		if err != nil {
			return nil, fail("Invalid central configuration in %s.", path)
		}
	} else {
		err = yaml.Unmarshal(b, &c)
	}
	if err != nil {
		return nil, fail("Invalid central configuration in %s.", path)
	}
	if err := validateCluster(c); err != nil {
		return nil, err
	}
	return c, nil
}
func writeCluster(path string, c Object) error {
	if err := validateCluster(c); err != nil {
		return err
	}
	if strings.EqualFold(filepath.Ext(path), ".json") {
		return writeObject(path, c)
	}
	b, err := yaml.Marshal(yamlNumbers(copyValue(c)))
	if err != nil {
		return err
	}
	return atomicWrite(path, b, 0600)
}

// yamlNumbers rewrites exact json.Number values into typed YAML integers or
// floats on a deep copy, so encoding/json numbers are never emitted as
// quoted strings. The caller's config is never mutated.
func yamlNumbers(v any) any {
	switch value := v.(type) {
	case Object:
		for k, item := range value {
			value[k] = yamlNumbers(item)
		}
		return value
	case []any:
		for i, item := range value {
			value[i] = yamlNumbers(item)
		}
		return value
	case json.Number:
		if n, err := strconv.ParseInt(value.String(), 10, 64); err == nil {
			return n
		}
		if u, err := strconv.ParseUint(value.String(), 10, 64); err == nil {
			return u
		}
		if f, err := value.Float64(); err == nil {
			return f
		}
		return value
	}
	return v
}

// controllerPort resolves the controller port for actual consumers: an
// unset field means the 8020 default; an explicit invalid value yields 0 so
// the caller can fail clearly. Validation itself stays pure and never
// writes defaults back into the caller's config.
func controllerPort(c Object) int {
	v, ok := c["controller_port"]
	if !ok {
		return 8020
	}
	n, err := toInt(v)
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}

func validateCluster(c Object) error {
	if !integer(c["version"]) || num(c["version"]) != 1 {
		return fail("Unsupported cluster config version.")
	}
	if !isObject(c["nodes"]) || !sameStrings(keys(obj(c["nodes"])), []string{"spark01", "spark02"}) {
		return fail("Define spark01 and spark02.")
	}
	if !isObject(c["models"]) || !isObject(value(c, "policy", Object{})) || !filepath.IsAbs(str(c["root"])) {
		return fail("Define workload mappings, policy, and an absolute cluster root.")
	}
	port := value(c, "controller_port", 8020)
	if !integer(port) || num(port) < 1 || num(port) > 65535 {
		return fail("Invalid controller port.")
	}
	// The lifecycle mode must be an explicit, spelled-out choice; a typo
	// must fail loudly instead of silently falling back to legacy.
	if v, ok := c["lifecycle"]; ok && str(v) != "legacy" && str(v) != "native" {
		return fail("Lifecycle mode must be \"legacy\" or \"native\".")
	}
	if v, ok := c["manager_url"]; ok && !privateURL(str(v)) {
		return fail("Invalid private manager URL.")
	}
	policy := obj(c["policy"])
	if v, ok := policy["interrupt_active_jobs"]; ok {
		if _, good := v.(bool); !good {
			return fail("Interrupting active jobs must be true or false.")
		}
		if v == true {
			return fail("Interrupting active jobs is not supported.")
		}
	}
	if !boolField(policy, "comfyui_exclusive_with_llms") {
		return fail("ComfyUI exclusivity must be true or false.")
	}
	if v, ok := policy["idle_ttl_seconds"]; ok && (!integer(v) || num(v) < 0) {
		return fail("Idle timeout must be a nonnegative whole number of seconds.")
	}
	if v, ok := policy["protected_models"]; ok && !validStrings(v) {
		return fail("Protected models must be a list.")
	}
	for node, v := range obj(c["nodes"]) {
		if !isObject(v) {
			return fail("%s settings must be a mapping.", node)
		}
		meta := obj(v)
		if h, ok := meta["ssh"]; ok {
			if err := safeHost(str(h)); err != nil {
				return err
			}
		}
		if u, ok := meta["comfy_unit"]; ok && !serviceName.MatchString(str(u)) {
			return fail("Invalid ComfyUI service name.")
		}
	}
	seen := map[string]bool{}
	models := obj(c["models"])
	for name, v := range models {
		if !simpleName.MatchString(name) || name == "." || name == ".." || name == "spark-auto" || !isObject(v) {
			return fail("Workload names must be simple names without paths or router syntax.")
		}
		m := obj(v)
		settings := obj(m["settings"])
		if !isObject(m["settings"]) || !isObject(settings[str(m["run_on"])]) || placements[str(m["run_on"])] == nil {
			return fail("No settings for %s on its selected placement.", name)
		}
		kind := str(m["kind"])
		if !contains([]string{"llm", "audio", "comfyui", "comfy_model"}, kind) {
			return fail("Unknown workload kind: %s.", name)
		}
		if !boolField(m, "keep_loaded") || !boolField(m, "reasoning") {
			return fail("Model policy flags must be true or false.")
		}
		if v, ok := m["inputs"]; ok && !validStrings(v) {
			return fail("%s accepted inputs must be a list of names.", name)
		}
		if v, ok := m["name"]; ok {
			if _, ok := v.(string); !ok {
				return fail("Model display names must be strings.")
			}
		}
		if !validStrings(value(m, "aliases", []any{})) {
			return fail("%s aliases must be a list of names.", name)
		}
		monitor := value(m, "monitoring", Object{})
		if !isObject(monitor) || !validStrings(value(obj(monitor), "containers", []any{})) {
			return fail("Invalid monitoring containers for %s.", name)
		}
		for _, s := range stringsOf(obj(monitor)["containers"]) {
			if !containerName.MatchString(s) {
				return fail("Invalid monitoring container name.")
			}
		}
		for on, sv := range settings {
			if placements[on] == nil || !isObject(sv) {
				return fail("Invalid placement for %s/%s.", name, on)
			}
			s := obj(sv)
			if !boolField(s, "enabled") || !isObject(value(s, "parameters", Object{})) {
				return fail("Invalid recipe settings for %s/%s.", name, on)
			}
			mins := value(s, "minimum_available_gib", Object{})
			if !isObject(mins) {
				return fail("Invalid startup memory check.")
			}
			for n, v := range obj(mins) {
				if !contains(placements[on], n) || !finite(v) || num(v) < 0 {
					return fail("Invalid startup memory check for %s/%s.", name, on)
				}
			}
			if kind == "llm" {
				if !isObject(s["units"]) || !sameStrings(keys(obj(s["units"])), placements[on]) {
					return fail("Unit placement mismatch: %s/%s.", name, on)
				}
				for _, v := range obj(s["units"]) {
					if !serviceName.MatchString(str(v)) {
						return fail("Invalid model service name.")
					}
				}
				if !privateURL(str(s["backend_url"])) || !integer(s["context_tokens"]) || num(s["context_tokens"]) <= 0 {
					return fail("Missing backend or context: %s/%s.", name, on)
				}
				out := value(m, "output_tokens", min(16384, num(s["context_tokens"])-1))
				if !integer(out) || num(out) <= 0 || num(out) >= num(s["context_tokens"]) {
					return fail("Invalid output token budget for %s/%s.", name, on)
				}
				if !integer(s["concurrency"]) || num(s["concurrency"]) < 1 {
					return fail("Invalid request concurrency for %s.", name)
				}
				if order, ok := s["startup_order"]; ok && (!validStrings(order) || !sameStrings(stringsOf(order), placements[on])) {
					return fail("Invalid startup order for %s/%s.", name, on)
				}
				params := obj(s["parameters"])
				if _, ok := params["context_tokens"]; ok {
					return fail("context_tokens must be set once, outside parameters.")
				}
				for k, v := range params {
					if k == "gpu_memory_utilization" {
						if !finite(v) || num(v) <= 0 || num(v) > 1 {
							return fail("Invalid GPU memory fraction.")
						}
					} else if !integer(v) || num(v) < 0 {
						return fail("Invalid numeric recipe parameter: %s.", k)
					}
				}
				rv := value(s, "rendezvous", Object{})
				if !isObject(rv) {
					return fail("Invalid rendezvous.")
				}
				r := obj(rv)
				if len(r) > 0 && (!contains(placements[on], str(r["node"])) || !integer(r["port"]) || num(r["port"]) < 1 || num(r["port"]) > 65535) {
					return fail("Invalid rendezvous.")
				}
				bindings := value(s, "parameter_bindings", Object{})
				if !isObject(bindings) {
					return fail("Invalid recipe bindings.")
				}
				for n, bv := range obj(bindings) {
					if !contains(placements[on], n) || !isList(bv) {
						return fail("Invalid recipe binding devices.")
					}
					for _, x := range arr(bv) {
						b := obj(x)
						fields := obj(b["fields"])
						if !isObject(x) || !filepath.IsAbs(str(b["path"])) || !contains([]string{"nvidia_json", "env", "docker_env", "python", "command"}, str(b["format"])) || len(fields) == 0 {
							return fail("Invalid recipe binding for %s/%s.", name, on)
						}
						for field, flag := range fields {
							if field != "context_tokens" {
								if _, ok := params[field]; !ok {
									return fail("Unknown recipe field: %s.", field)
								}
							}
							if str(flag) == "" {
								return fail("Missing recipe flag.")
							}
						}
					}
				}
			} else if kind == "audio" {
				if on == "both" || !validStrings(s["containers"]) || len(arr(s["containers"])) == 0 {
					return fail("Invalid audio containers for %s/%s.", name, on)
				}
				for _, n := range stringsOf(s["containers"]) {
					if !containerName.MatchString(n) {
						return fail("Invalid audio container.")
					}
				}
			}
		}
		for _, alias := range append([]string{name}, stringsOf(m["aliases"])...) {
			low := strings.ToLower(alias)
			if alias == "" || strings.TrimSpace(alias) != alias || seen[low] || low == "auto" || low == "spark-auto" {
				return fail("Duplicate workload alias: %s.", alias)
			}
			seen[low] = true
		}
	}
	shared := value(policy, "comfyui_shared_llms", []any{})
	if !validStrings(shared) {
		return fail("ComfyUI shared models must be a list.")
	}
	for _, name := range stringsOf(shared) {
		if str(obj(models[name])["kind"]) != "llm" {
			return fail("ComfyUI shared models must name configured language models.")
		}
	}
	floor := value(policy, "comfyui_min_available_gib", 0)
	if !finite(floor) || num(floor) < 0 {
		return fail("ComfyUI memory minimum must be a finite nonnegative number.")
	}
	if !boolean(value(policy, "comfyui_exclusive_with_llms", true)) && (len(arr(shared)) == 0 || num(floor) <= 0) {
		return fail("Shared ComfyUI jobs require a language model allowlist and a positive memory minimum.")
	}
	for _, n := range stringsOf(policy["protected_models"]) {
		if !boolean(obj(models[n])["keep_loaded"]) {
			return fail("%s is required to stay loaded by the cluster policy.", n)
		}
	}
	if av, ok := c["auto"]; ok {
		a := obj(av)
		if routing := str(a["routing"]); routing != "" && routing != "bifrost" {
			return fail("Auto routing must be bifrost or omitted for priority routing.")
		}
		if !isObject(av) || !validStrings(a["targets"]) || len(arr(a["targets"])) == 0 || !contains(stringsOf(a["targets"]), str(a["default"])) {
			return fail("Auto targets and default must name enabled language models.")
		}
		for _, n := range stringsOf(a["targets"]) {
			if !contains(enabledLLMs(c), n) {
				return fail("Auto targets must name enabled language models.")
			}
		}
	}
	return nil
}
func enabledLLMs(c Object) []string {
	out := []string{}
	for _, n := range keys(obj(c["models"])) {
		m := obj(obj(c["models"])[n])
		s := obj(obj(m["settings"])[str(m["run_on"])])
		if str(m["kind"]) == "llm" && boolean(value(s, "enabled", true)) {
			out = append(out, n)
		}
	}
	return out
}
func resolveWorkload(c Object, requested string) (string, Object, error) {
	r := strings.TrimSpace(strings.ToLower(requested))
	r = strings.TrimPrefix(strings.TrimPrefix(r, "garuda/"), "parakeet/")
	for _, n := range keys(obj(c["models"])) {
		m := obj(obj(c["models"])[n])
		for _, a := range append([]string{n}, stringsOf(m["aliases"])...) {
			if strings.ToLower(a) == r {
				return n, m, nil
			}
		}
	}
	return "", nil, fail("Unknown model or service: %s. Run sparky models --installed for names.", requested)
}
func selectPlacement(c Object, name, on string) (Object, Object, []string, string, error) {
	m := obj(obj(c["models"])[name])
	if on == "" {
		on = str(m["run_on"])
	}
	s, ok := obj(m["settings"])[on]
	if !ok || placements[on] == nil {
		return nil, nil, nil, on, fail("%s has no recipe for %s.", name, on)
	}
	if !boolean(value(obj(s), "enabled", true)) {
		return nil, nil, nil, on, fail("%s", str(value(obj(s), "reason", name+" is not installed.")))
	}
	return m, obj(s), append([]string{}, placements[on]...), on, nil
}
func modelUnits(m Object, node string) []string {
	units := Object{}
	for _, s := range obj(m["settings"]) {
		if u := str(obj(obj(s)["units"])[node]); u != "" {
			units[u] = true
		}
	}
	return keys(units)
}
