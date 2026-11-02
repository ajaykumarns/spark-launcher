package sparky

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// deviceProcRoot lets tests point memory parsing at a fixture directory.
var deviceProcRoot = "/proc"

// deviceRun is a seam so tests never invoke real Docker, SSH or nvidia-smi.
var deviceRun = func(args []string, timeout time.Duration) (CommandResult, error) {
	return command(args, nil, timeout)
}

const deviceCommandTimeout = 30 * time.Second

func deviceMeminfo() (Object, error) {
	b, err := os.ReadFile(filepath.Join(deviceProcRoot, "meminfo"))
	if err != nil {
		return nil, err
	}
	mem := Object{}
	for _, line := range strings.Split(string(b), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "MemTotal", "MemAvailable", "SwapFree":
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				continue
			}
			if kb, err := strconv.ParseInt(strings.TrimSpace(fields[0]), 10, 64); err == nil {
				mem[strings.TrimSpace(key)] = kb * 1024
			} else {
				return nil, fail("Cannot read memory information.")
			}
		}
	}
	return mem, nil
}

// deviceValidateMeminfo requires the readings the controller relies on.
func deviceValidateMeminfo(mem Object) error {
	for _, key := range []string{"MemTotal", "MemAvailable"} {
		if _, ok := mem[key]; !ok {
			return fail("Cannot read memory information.")
		}
	}
	return nil
}

// deviceSafeName matches systemd unit names accepted from central config.
var deviceSafeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@/-]*$`)

func deviceUnitNames(node Object) ([]string, error) {
	set := map[string]bool{}
	for _, values := range obj(value(node, "units", nil)) {
		names := []string{}
		switch v := values.(type) {
		case string:
			names = []string{v}
		default:
			names = stringsOf(values)
		}
		for _, name := range names {
			if name != "" {
				set[name] = true
			}
		}
	}
	if unit := str(value(node, "comfy_unit", nil)); unit != "" {
		set[unit] = true
	}
	names := make([]string, 0, len(set))
	for name := range set {
		if !deviceSafeName.MatchString(name) {
			return nil, fail("Invalid unit name in the cluster configuration.")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func deviceQueryUnits(names []string) (Object, error) {
	units := Object{}
	if len(names) == 0 {
		return units, nil
	}
	args := append([]string{"systemctl", "show"}, names...)
	args = append(args, "-p", "Id", "-p", "ActiveState", "-p", "SubState")
	result, err := deviceRun(args, deviceCommandTimeout)
	if err != nil {
		return nil, fail("Could not query the service states.")
	}
	if result.Code != 0 {
		return nil, fail("Could not query the service states.")
	}
	requested := map[string]bool{}
	for _, name := range names {
		requested[name] = true
	}
	for _, block := range strings.Split(strings.TrimSpace(result.Out), "\n\n") {
		fields := Object{}
		for _, line := range strings.Split(block, "\n") {
			if key, value, ok := strings.Cut(line, "="); ok {
				fields[key] = value
			}
		}
		id := str(fields["Id"])
		state := str(fields["ActiveState"])
		if id == "" || state == "" || !requested[id] {
			return nil, fail("Could not determine the service states.")
		}
		units[id] = state
	}
	for _, name := range names {
		if _, ok := units[name]; !ok {
			return nil, fail("Could not determine the service states.")
		}
	}
	return units, nil
}

const deviceContainerTemplate = `{"name":{{json .Name}},"running":{{.State.Running}},"id":{{json .Id}},"started_at":{{json .State.StartedAt}},"oom_killed":{{.State.OOMKilled}}}`

func deviceQueryContainers() (Object, error) {
	ids, err := deviceRun([]string{"sudo", "-n", "docker", "ps", "-aq"}, deviceCommandTimeout)
	if err != nil {
		return nil, fail("Could not list the containers. No device action was taken.")
	}
	if ids.Code != 0 {
		return nil, fail("Could not list the containers. No device action was taken.")
	}
	idList := strings.Fields(ids.Out)
	if len(idList) == 0 {
		return Object{}, nil
	}
	args := append([]string{"sudo", "-n", "docker", "inspect", "--format", deviceContainerTemplate}, idList...)
	result, err := deviceRun(args, deviceCommandTimeout)
	if err != nil {
		return nil, fail("Could not read the container state. No device action was taken.")
	}
	if result.Code != 0 {
		return nil, fail("Could not read the container state. No device action was taken.")
	}
	containers := Object{}
	for _, line := range strings.Split(result.Out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return nil, fail("Could not read the container state. No device action was taken.")
		}
		name, ok := item["name"].(string)
		if !ok {
			return nil, fail("Could not read the container state. No device action was taken.")
		}
		delete(item, "name")
		containers[strings.TrimPrefix(name, "/")] = Object(item)
	}
	if len(containers) != len(idList) {
		return nil, fail("Could not read the container state. No device action was taken.")
	}
	return containers, nil
}

var deviceAssetSuffixes = map[string]bool{
	".safetensors": true, ".gguf": true, ".ckpt": true, ".pt": true, ".pth": true,
}

func deviceScanAssets(node Object) ([]any, error) {
	assets := []any{}
	for _, raw := range stringsOf(value(node, "asset_roots", nil)) {
		root := raw
		info, err := os.Stat(root)
		if err != nil {
			continue
		}
		if strings.Contains(root, "ComfyUI") && info.IsDir() {
			err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if entry.IsDir() || !deviceAssetSuffixes[strings.ToLower(filepath.Ext(entry.Name()))] {
					return nil
				}
				if stat, err := entry.Info(); err == nil {
					assets = append(assets, Object{"root": root, "path": relPath(root, path), "bytes": stat.Size()})
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			assets = append(assets, Object{"root": root, "path": entry.Name(), "directory": entry.IsDir()})
		}
	}
	return assets, nil
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

func deviceProbeComfy(node Object) Object {
	comfy := Object{"reachable": false, "running": float64(0), "pending": float64(0), "guarded": false}
	url := str(value(node, "comfy_url", nil))
	if url == "" {
		return comfy
	}
	queue, err := requestObject(context.Background(), "GET", strings.TrimSuffix(url, "/")+"/queue", nil, nil, 3*time.Second, false)
	if err != nil {
		return comfy
	}
	comfy["reachable"] = true
	running := arr(queue["queue_running"])
	pending := arr(queue["queue_pending"])
	comfy["running"] = float64(len(running))
	comfy["pending"] = float64(len(pending))
	promptIDs := []any{}
	for _, row := range running {
		items := arr(row)
		if len(items) > 1 {
			if id := str(items[1]); id != "" {
				promptIDs = append(promptIDs, id)
			}
		}
	}
	comfy["prompt_ids"] = promptIDs
	if state, err := requestObject(context.Background(), "GET", strings.TrimSuffix(url, "/")+"/sparky/state", nil, nil, 3*time.Second, false); err == nil {
		for key, value := range state {
			comfy[key] = value
		}
		comfy["guarded"] = true
	}
	return comfy
}

// Probe reports authoritative, minimal device state for one node.
func Probe(node Object) (Object, error) {
	mem, err := deviceMeminfo()
	if err != nil {
		return nil, fail("Cannot read memory information.")
	}
	if err := deviceValidateMeminfo(mem); err != nil {
		return nil, err
	}
	names, err := deviceUnitNames(node)
	if err != nil {
		return nil, err
	}
	units, err := deviceQueryUnits(names)
	if err != nil {
		return nil, err
	}
	containers, err := deviceQueryContainers()
	if err != nil {
		return nil, err
	}
	assets, err := deviceScanAssets(node)
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	return Object{
		"hostname":     hostname,
		"memory_bytes": mem,
		"units":        units,
		"containers":   containers,
		"assets":       assets,
		"comfyui":      deviceProbeComfy(node),
	}, nil
}

func deviceDiskBytes(path string) (Object, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, err
	}
	total := int64(stat.Blocks) * int64(stat.Bsize)
	free := int64(stat.Bavail) * int64(stat.Bsize)
	return Object{"total": total, "used": total - free, "free": free}, nil
}

// Device reports this machine's compact inventory for the controller.
func Device() (Object, error) {
	hostname, _ := os.Hostname()
	data := Object{"hostname": hostname, "warnings": []any{}}
	warn := func(message string) {
		data["warnings"] = append(data["warnings"].([]any), message)
	}
	if mem, err := deviceMeminfo(); err == nil {
		data["memory_bytes"] = mem
	} else {
		warn("memory query is unavailable.")
	}
	if disk, err := deviceDiskBytes(home()); err == nil {
		data["disk_bytes"] = disk
	} else {
		warn("disk query is unavailable.")
	}
	queries := []struct {
		key   string
		args  []string
		label string
	}{
		{"gpu", []string{"nvidia-smi", "--query-gpu=name,temperature.gpu,memory.used,memory.total", "--format=csv,noheader,nounits"}, "gpu"},
		{"running_containers", []string{"sudo", "-n", "docker", "ps", "--format", "{{.Names}}"}, "running containers"},
	}
	for _, query := range queries {
		result, err := deviceRun(query.args, 15*time.Second)
		switch {
		case err != nil:
			warn(query.label + " query is unavailable.")
		case result.Code != 0:
			warn(query.label + " query failed.")
		default:
			data[query.key] = deviceLines(result.Out)
		}
	}
	return data, nil
}

func deviceLines(out string) []any {
	lines := []any{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
