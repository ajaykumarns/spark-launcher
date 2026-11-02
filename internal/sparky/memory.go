package sparky

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const memoryMiB = 1024 * 1024

// memoryProcRoot lets tests point /proc scanning at a fixture directory.
var memoryProcRoot = "/proc"

// memoryRun is a seam so tests never invoke real Docker, SSH or nvidia-smi.
var memoryRun = func(args []string, input []byte, timeout time.Duration) (CommandResult, error) {
	return command(args, input, timeout)
}

const memorySampleTimeout = 10 * time.Second

func memoryCgroup(pid string) (string, error) {
	b, err := os.ReadFile(filepath.Join(memoryProcRoot, pid, "cgroup"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "0::") {
			return strings.TrimSpace(line[3:]), nil
		}
	}
	return "", nil
}

func memoryParseGPU(text string) map[int]*int64 {
	result := map[int]*int64{}
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord = -1
	rows, err := reader.ReadAll()
	if err != nil {
		return result
	}
	for _, row := range rows {
		if len(row) != 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(row[0]))
		if err != nil {
			continue
		}
		var size *int64
		if mib, err := strconv.ParseFloat(strings.TrimSpace(row[1]), 64); err == nil && !math.IsNaN(mib) && !math.IsInf(mib, 0) {
			value := int64(mib * memoryMiB)
			if value >= 0 && mib <= float64(memoryMaxBytes/memoryMiB) {
				size = &value
			}
		}
		result[pid] = size
	}
	return result
}

// memoryHostPSS reads proportional set size for one process. A process that
// exits during the sample yields 0; other unreadable state yields nil.
func memoryHostPSS(pid string) *int64 {
	zero := int64(0)
	status, err := os.ReadFile(filepath.Join(memoryProcRoot, pid, "status"))
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			return &zero
		}
		return nil
	}
	rssSeen, rssMalformed := false, false
	rss := int64(0)
	for _, line := range strings.Split(string(status), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "VmRSS" {
			continue
		}
		rssSeen = true
		fields := strings.Fields(value)
		if len(fields) > 0 {
			if parsed, err := strconv.ParseInt(fields[0], 10, 64); err == nil && parsed >= 0 {
				rss = parsed
			} else {
				rssMalformed = true
			}
		}
		break
	}
	if rssMalformed {
		return nil
	}
	if !rssSeen || rss == 0 {
		// Kernel threads report no VmRSS; that is a measured zero.
		return &zero
	}
	rollup, err := os.ReadFile(filepath.Join(memoryProcRoot, pid, "smaps_rollup"))
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			return &zero
		}
		return nil
	}
	for _, line := range strings.Split(string(rollup), "\n") {
		if strings.HasPrefix(line, "Pss:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return nil
			}
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || kb < 0 || kb > memoryMaxBytes/1024 {
				return nil
			}
			return &[]int64{kb * 1024}[0]
		}
	}
	return nil
}

// memoryMaxBytes rejects readings beyond any plausible address space.
const memoryMaxBytes = int64(1) << 60

// memoryClassify matches the narrowest exact cgroup prefix, never a substring.
func memoryClassify(path string, sources map[string]string) string {
	best, owner := 0, "system"
	for prefix, candidate := range sources {
		if prefix == "" || prefix == "/" {
			continue
		}
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			if len(prefix) >= best {
				best, owner = len(prefix), candidate
			}
		}
	}
	return owner
}

func memoryAggregate(processes []Object, gpu map[int]*int64, gpuKnown bool, names map[string]string) []any {
	type row struct {
		id   string
		name string
		pids []int
		host *int64
		gpu  *int64
	}
	groups := map[string]*row{}
	var order []string
	group := func(owner string) *row {
		if _, ok := groups[owner]; !ok {
			name := names[owner]
			if name == "" {
				name = owner
			}
			initial := int64(0)
			var gpuInit *int64
			if gpuKnown {
				gpuInit = &initial
			}
			groups[owner] = &row{id: owner, name: name, host: &initial, gpu: gpuInit}
			order = append(order, owner)
		}
		return groups[owner]
	}
	addGPU := func(current *int64, size *int64) *int64 {
		if size == nil || current == nil {
			return nil
		}
		total := *current + *size
		return &total
	}
	seen := map[int]bool{}
	for _, process := range processes {
		pid := int(num(process["pid"]))
		owner := str(process["owner"])
		r := group(owner)
		r.pids = append(r.pids, pid)
		seen[pid] = true
		if pss, ok := process["pss"].(*int64); ok {
			if pss == nil {
				r.host = nil
			} else if r.host != nil {
				total := *r.host + *pss
				r.host = &total
			}
		}
		if size, ok := gpu[pid]; ok {
			r.gpu = addGPU(r.gpu, size)
		}
	}
	// A process can exit during the scan. Keep its observed GPU allocation
	// visible, but attribute it to the system, not to a model.
	for pid, size := range gpu {
		if !seen[pid] {
			r := group("system")
			r.gpu = addGPU(r.gpu, size)
		}
	}
	rows := []any{}
	sort.Strings(order)
	for _, owner := range order {
		r := groups[owner]
		keep := len(r.pids) > 0 || (r.gpu != nil && *r.gpu != 0)
		if !keep {
			continue
		}
		sort.Ints(r.pids)
		pids := []any{}
		for _, pid := range r.pids {
			pids = append(pids, float64(pid))
		}
		var host, gpuBytes any
		if r.host != nil {
			host = *r.host
		}
		if r.gpu != nil {
			gpuBytes = *r.gpu
		}
		rows = append(rows, Object{
			"id":             r.id,
			"name":           r.name,
			"pids":           pids,
			"host_pss_bytes": host,
			"gpu_bytes":      gpuBytes,
		})
	}
	return rows
}

// CollectMemory reports read-only per-workload memory measurements for
// unified-memory Sparks. GPU allocations and host PSS are separate
// observations and must never be added together.
func CollectMemory(spec Object) (Object, error) {
	return memoryCollect(spec)
}

// memorySafeName matches unit and container names accepted in the spec.
var memorySafeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@/-]*$`)

func memoryCollect(spec Object) (Object, error) {
	sources := map[string]string{}
	names := map[string]string{"system": "Other host processes"}
	expectedContainers := map[string]string{}
	unitOwners := map[string]string{}
	var warnings []any
	for _, raw := range arr(value(spec, "owners", nil)) {
		owner := obj(raw)
		id := str(owner["id"])
		if id == "" {
			return nil, fail("Invalid memory specification.")
		}
		names[id] = str(value(owner, "name", id))
		for _, container := range stringsOf(owner["containers"]) {
			if container == "" || !memorySafeName.MatchString(strings.TrimPrefix(container, "garuda-")) {
				return nil, fail("Invalid memory specification.")
			}
			expectedContainers[container] = id
		}
		for _, unit := range stringsOf(owner["units"]) {
			if unit == "" {
				continue
			}
			if !memorySafeName.MatchString(unit) {
				return nil, fail("Invalid memory specification.")
			}
			unitOwners[unit] = id
		}
	}

	if ids, err := memoryRun([]string{"docker", "ps", "-q"}, nil, memorySampleTimeout); err == nil && ids.Code == 0 {
		list := strings.Fields(ids.Out)
		if len(list) > 0 {
			template := `{"name":{{json .Name}},"pid":{{.State.Pid}}}`
			args := append([]string{"docker", "inspect", "--format", template}, list...)
			inspected, err := memoryRun(args, nil, memorySampleTimeout)
			if err != nil || inspected.Code != 0 {
				warnings = append(warnings, "Container ownership could not be read.")
			} else {
				for _, line := range strings.Split(inspected.Out, "\n") {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					var container map[string]any
					if err := json.Unmarshal([]byte(line), &container); err != nil {
						continue
					}
					name := strings.TrimPrefix(str(container["name"]), "/")
					if !memorySafeName.MatchString(name) {
						continue
					}
					if !memoryIsDecimal(fmt.Sprintf("%d", int64(num(container["pid"])))) {
						continue
					}
					pid := fmt.Sprintf("%d", int64(num(container["pid"])))
					owner, ok := expectedContainers[name]
					if !ok {
						owner = "container:" + name
					}
					if _, exists := names[owner]; !exists {
						names[owner] = strings.TrimPrefix(name, "garuda-")
					}
					if prefix, err := memoryCgroup(pid); err == nil {
						sources[prefix] = owner
					}
				}
			}
		}
	} else if (err != nil) || ids.Code != 0 {
		warnings = append(warnings, "Container ownership could not be read.")
	}

	if len(unitOwners) > 0 {
		unitNames := make([]string, 0, len(unitOwners))
		for unit := range unitOwners {
			unitNames = append(unitNames, unit)
		}
		sort.Strings(unitNames)
		args := append([]string{"systemctl", "show"}, unitNames...)
		args = append(args, "-p", "Id", "-p", "ControlGroup")
		result, err := memoryRun(args, nil, memorySampleTimeout)
		if err != nil || result.Code != 0 {
			warnings = append(warnings, "Service ownership could not be read.")
		} else {
			for _, block := range strings.Split(strings.TrimSpace(result.Out), "\n\n") {
				fields := Object{}
				for _, line := range strings.Split(block, "\n") {
					if key, value, ok := strings.Cut(line, "="); ok {
						fields[key] = value
					}
				}
				if owner, ok := unitOwners[str(fields["Id"])]; ok && str(fields["ControlGroup"]) != "" {
					sources[str(fields["ControlGroup"])] = owner
				}
			}
		}
	}

	measured, err := memoryRun([]string{"nvidia-smi", "--query-compute-apps=pid,used_gpu_memory", "--format=csv,noheader,nounits"}, nil, memorySampleTimeout)
	gpuKnown := err == nil && measured.Code == 0
	gpu := map[int]*int64{}
	if gpuKnown {
		gpu = memoryParseGPU(measured.Out)
	} else {
		warnings = append(warnings, "GPU memory readings are unavailable.")
	}

	processes := []Object{}
	entries, err := os.ReadDir(memoryProcRoot)
	if err != nil {
		return nil, fail("Cannot read process information.")
	}
	for _, entry := range entries {
		pid := entry.Name()
		if !memoryIsDecimal(pid) {
			continue
		}
		cgroupPath, err := memoryCgroup(pid)
		if err != nil {
			continue
		}
		owner := memoryClassify(cgroupPath, sources)
		pss := memoryHostPSS(pid)
		pidNum, _ := strconv.Atoi(pid)
		if pss != nil && *pss == 0 {
			if _, onGPU := gpu[pidNum]; !onGPU {
				continue
			}
		}
		processes = append(processes, Object{"pid": float64(pidNum), "owner": owner, "pss": pss})
	}

	if warnings == nil {
		warnings = []any{}
	}

	return Object{
		"sampled_at":     time.Now().UTC().Format(time.RFC3339Nano),
		"workloads":      memoryAggregate(processes, gpu, gpuKnown, names),
		"warnings":       warnings,
		"gpu_source":     "nvidia-smi per-process allocations",
		"host_source":    "/proc/PID/smaps_rollup Pss",
		"unified_memory": true,
		"add_columns":    false,
	}, nil
}

// memoryIsArray reports whether the value is a JSON array.
func memoryIsArray(value any) bool {
	_, ok := value.([]any)
	return ok
}

func memoryIsDecimal(s string) bool {
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

// memorySpecification derives the measured owners from the central config.
func memorySpecification(config Object, node string) Object {
	owners := []any{}
	for _, name := range keys(obj(config["models"])) {
		model := obj(obj(config["models"])[name])
		kind := str(model["kind"])
		if kind != "llm" && kind != "audio" {
			continue
		}
		containers := stringsOf(obj(model["monitoring"])["containers"])
		if kind == "audio" {
			containers = stringsOf(obj(obj(model["settings"])[node])["containers"])
		}
		display := name
		if kind == "llm" {
			aliases := stringsOf(model["aliases"])
			if len(aliases) > 0 {
				display = aliases[0]
			}
		} else {
			display = memoryTitle(name)
		}
		owners = append(owners, Object{
			"id":         name,
			"name":       display,
			"containers": anyStrings(containers),
			"units":      anyStrings(modelUnits(model, node)),
		})
	}
	comfyUnits := []string{}
	if unit := str(obj(obj(config["nodes"])[node])["comfy_unit"]); unit != "" {
		comfyUnits = append(comfyUnits, unit)
	}
	owners = append(owners, Object{
		"id":    "comfyui",
		"name":  "ComfyUI",
		"units": anyStrings(comfyUnits),
	})
	return Object{"owners": owners}
}

func memoryTitle(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// memoryShellJoin quotes words for a remote shell only when needed, matching
// the reference shlex.join behavior for plain command words.
func memoryShellJoin(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		if arg != "" && !strings.ContainsAny(arg, " \t\"'\\$`&|;<>(){}[]!*?~#\n") {
			parts = append(parts, arg)
			continue
		}
		parts = append(parts, "'"+strings.ReplaceAll(arg, "'", `'"'"'`)+"'")
	}
	return strings.Join(parts, " ")
}

// memoryReadNode collects one node's sample through the existing native
// node-memory stdin JSON protocol. The payload goes over stdin and only the
// JSON result is read back; diagnostics are never surfaced.
func memoryReadNode(config Object, node string) (Object, error) {
	helper := filepath.Join(home(), ".local/bin/sparky")
	args := []string{"sudo", "-n", helper, "--json", "node", "memory"}
	if node != "spark01" {
		ssh := str(obj(obj(config["nodes"])[node])["ssh"])
		if ssh == "" {
			return nil, fail("No SSH alias for %s.", node)
		}
		args = []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", ssh, memoryShellJoin(args)}
	}
	payload, err := json.Marshal(memorySpecification(config, node))
	if err != nil {
		return nil, err
	}
	result, err := memoryRun(args, payload, 45*time.Second)
	if err != nil {
		return nil, err
	}
	if result.Code != 0 {
		return nil, fail("Memory readings are unavailable on %s.", node)
	}
	sample, err := decode([]byte(result.Out))
	if err != nil {
		return nil, fail("Memory readings are unavailable on %s.", node)
	}
	return sample, nil
}

// memoryContextLimit prefers the running backend's published context window
// and labels the configured fallback. The API key is read from api.env and
// used only for the request; it is never returned or logged.
func memoryContextLimit(config Object, name string, state Object) Object {
	nodes := stringsOf(state["nodes"])
	placement := ""
	switch {
	case len(nodes) == 2:
		placement = "both"
	case len(nodes) == 1:
		placement = nodes[0]
	}
	setting := obj(obj(obj(obj(config["models"])[name])["settings"])[placement])
	result := Object{"context_tokens": setting["context_tokens"], "context_source": "configured"}
	backend := str(setting["backend_url"])
	if backend == "" {
		return result
	}
	env, err := readEnv(filepath.Join(str(config["root"]), "vllm/api.env"))
	if err != nil || env["VLLM_API_KEY"] == "" {
		return result
	}
	headers := map[string]string{"Authorization": "Bearer " + env["VLLM_API_KEY"]}
	data, err := requestObject(context.Background(), "GET", strings.TrimSuffix(backend, "/")+"/v1/models", nil, headers, 3*time.Second, false)
	if err != nil {
		return result
	}
	for _, raw := range arr(data["data"]) {
		model := obj(raw)
		id := str(model["id"])
		if id != name && id != "garuda/"+name {
			continue
		}
		for _, field := range []string{"max_model_len", "context_length", "context_window"} {
			if value := model[field]; value != nil && integer(value) && num(value) > 0 {
				return Object{"context_tokens": value, "context_source": "running backend"}
			}
		}
	}
	return result
}

// EnrichStatus attaches memory_usage and memory_by_node to a copy of the
// status object. The passed-in status is not mutated; the returned object is
// the enriched copy. Both nodes are collected concurrently.
func EnrichStatus(config Object, status Object) (Object, error) {
	if len(obj(config["nodes"])) == 0 {
		return nil, fail("Central configuration has no nodes.")
	}
	out := copyObject(status)
	nodes := keys(obj(config["nodes"]))
	samples := make([]Object, len(nodes))
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Add(1)
		go func(i int, node string) {
			defer wg.Done()
			sample, err := memoryReadNode(config, node)
			if err != nil || !memoryIsArray(value(sample, "workloads", nil)) {
				sample = Object{"workloads": []any{}, "warnings": []any{"Memory readings are unavailable on " + node + "."}}
			}
			samples[i] = sample
		}(i, node)
	}
	wg.Wait()
	memoryUsage := Object{}
	for i, node := range nodes {
		memoryUsage[node] = samples[i]
	}
	out["memory_usage"] = memoryUsage

	models := obj(out["models"])
	for _, name := range keys(models) {
		state := obj(models[name])
		loaded := value(state, "loaded", nil)
		if str(state["kind"]) == "llm" && loaded != false && loaded != nil {
			for field, value := range memoryContextLimit(config, name, state) {
				state[field] = value
			}
		}
		memoryByNode := Object{}
		for node, raw := range memoryUsage {
			for _, rowRaw := range arr(obj(raw)["workloads"]) {
				row := obj(rowRaw)
				if str(row["id"]) != name {
					continue
				}
				memoryByNode[node] = Object{"gpu_bytes": row["gpu_bytes"], "host_pss_bytes": row["host_pss_bytes"]}
				loadedFlag := value(state, "loaded", nil)
				rowState := "running"
				if loadedFlag != false && loadedFlag != nil {
					if s := str(state["state"]); s != "" {
						rowState = s
					}
				}
				row["state"] = rowState
				row["kind"] = state["kind"]
				row["context_tokens"] = state["context_tokens"]
				row["context_source"] = state["context_source"]
				if name == "comfyui" {
					comfy := obj(obj(out["comfyui"])[node])
					pending := int(num(value(comfy, "pending", float64(0))))
					row["state"] = fmt.Sprintf("%s (%d queued)", str(value(comfy, "phase", "running")), pending)
				}
			}
		}
		state["memory_by_node"] = memoryByNode
	}
	return out, nil
}
