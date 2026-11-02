package sparky

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deviceTestURL is set by deviceTestServer once the listener is bound.
var deviceTestURL string

// deviceTestServer serves fixed JSON bodies for the queue and guard endpoints.
func deviceTestServer(t *testing.T, queue, state string) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/queue", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(queue))
	})
	mux.HandleFunc("/sparky/state", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(state))
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deviceTestURL = "http://" + listener.Addr().String()
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func deviceSetProcRoot(t *testing.T, dir string) {
	t.Helper()
	old := deviceProcRoot
	deviceProcRoot = dir
	t.Cleanup(func() { deviceProcRoot = old })
}

type deviceCall struct {
	args    []string
	timeout time.Duration
}

type deviceRunnerStub struct {
	calls   []deviceCall
	results map[string]CommandResult
	errs    map[string]error
}

func (s *deviceRunnerStub) run(args []string, timeout time.Duration) (CommandResult, error) {
	s.calls = append(s.calls, deviceCall{args, timeout})
	key := args[0]
	if args[0] == "sudo" && len(args) > 3 {
		// Skip "sudo -n": key on the real subcommand (docker ps/inspect).
		key = args[0] + " " + args[2] + " " + args[3]
	}
	if err, ok := s.errs[key]; ok {
		return CommandResult{}, err
	}
	if result, ok := s.results[key]; ok {
		return result, nil
	}
	// Unexpected commands fail loudly instead of silently succeeding.
	return CommandResult{}, fail("unexpected device command: %s", key)
}

// deviceProbeStub installs a runner where Docker reports no containers.
func deviceProbeStub(t *testing.T) *deviceRunnerStub {
	t.Helper()
	return deviceStubRunner(t, map[string]CommandResult{"sudo docker ps": {Out: ""}}, nil)
}

func deviceStubRunner(t *testing.T, results map[string]CommandResult, errs map[string]error) *deviceRunnerStub {
	t.Helper()
	stub := &deviceRunnerStub{results: results, errs: errs}
	old := deviceRun
	deviceRun = stub.run
	t.Cleanup(func() { deviceRun = old })
	return stub
}

func deviceWriteMeminfo(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	deviceSetProcRoot(t, dir)
}

func TestDeviceUnitDerivation(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal:        16090348 kB\nMemAvailable:    8000000 kB\n")
	stub := deviceStubRunner(t, map[string]CommandResult{
		"systemctl":      {Out: "Id=vllm.service\nActiveState=active\nSubState=running\n\nId=ocr.service\nActiveState=active\nSubState=running\n\nId=comfy.service\nActiveState=inactive\nSubState=dead\n"},
		"sudo docker ps": {Out: ""},
	}, nil)
	node := Object{
		"units":       Object{"vllm": "vllm.service", "ocr": []any{"ocr.service", "vllm.service"}},
		"comfy_unit":  "comfy.service",
		"asset_roots": []any{},
	}
	result, err := Probe(node)
	if err != nil {
		t.Fatal(err)
	}
	units := obj(result["units"])
	if len(units) != 3 || units["vllm.service"] != "active" || units["comfy.service"] != "inactive" || units["ocr.service"] != "active" {
		t.Fatalf("unexpected units: %v", units)
	}
	found := false
	for _, call := range stub.calls {
		if call.args[0] == "systemctl" {
			found = true
			got := strings.Join(call.args, " ")
			if !strings.Contains(got, "vllm.service") || !strings.Contains(got, "ocr.service") || !strings.Contains(got, "comfy.service") {
				t.Fatalf("unit names missing from command: %v", call.args)
			}
		}
	}
	if !found {
		t.Fatal("systemctl was not invoked")
	}
}

func TestDeviceMemoryParsing(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal:        16090348 kB\nMemFree:         1000 kB\nMemAvailable:    8000000 kB\nSwapFree:        2000000 kB\nHugePages_Total:       0\n")
	deviceProbeStub(t)
	node := Object{"units": Object{}, "asset_roots": []any{}}
	result, err := Probe(node)
	if err != nil {
		t.Fatal(err)
	}
	mem := obj(result["memory_bytes"])
	if mem["MemTotal"] != int64(16090348)*1024 || mem["MemAvailable"] != int64(8000000)*1024 || mem["SwapFree"] != int64(2000000)*1024 {
		t.Fatalf("unexpected memory: %v", mem)
	}
	if len(mem) != 3 {
		t.Fatalf("unexpected memory keys: %v", mem)
	}
}

func TestDeviceMissingMeminfoFails(t *testing.T) {
	deviceSetProcRoot(t, t.TempDir())
	if _, err := Probe(Object{}); err == nil {
		t.Fatal("expected meminfo failure")
	}
}

func TestDeviceDockerStateAndFailures(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1 kB\nMemAvailable: 1 kB\n")
	stub := deviceStubRunner(t, map[string]CommandResult{
		"sudo docker ps": {Out: "abc123\ndef456\n"},
		"sudo docker inspect": {Out: "{\"name\":\"/garuda-inference\",\"running\":true,\"id\":\"abc123\",\"started_at\":\"2026-01-01T00:00:00.000000000Z\",\"oom_killed\":false}\n" +
			"{\"name\":\"bench\",\"running\":false,\"id\":\"def456\",\"started_at\":\"2026-01-02T00:00:00.000000000Z\",\"oom_killed\":true}\n"},
	}, nil)
	node := Object{"units": Object{}, "asset_roots": []any{}}
	result, err := Probe(node)
	if err != nil {
		t.Fatal(err)
	}
	containers := obj(result["containers"])
	if len(containers) != 2 {
		t.Fatalf("unexpected containers: %v", containers)
	}
	garuda := obj(containers["garuda-inference"])
	if garuda["running"] != true || garuda["id"] != "abc123" || boolean(garuda["oom_killed"]) {
		t.Fatalf("unexpected container metadata: %v", garuda)
	}
	if _, ok := garuda["name"]; ok {
		t.Fatal("name must not be duplicated in the container metadata")
	}
	bench := obj(containers["bench"])
	if bench["running"] != false || bench["oom_killed"] != true {
		t.Fatalf("unexpected bench metadata: %v", bench)
	}
	inspect := false
	for _, call := range stub.calls {
		if len(call.args) > 3 && call.args[2] == "docker" {
			inspect = true
			if strings.Contains(strings.Join(call.args, " "), ";") || strings.Contains(strings.Join(call.args, " "), "$(") {
				t.Fatal("shell expansion in command array")
			}
		}
	}
	if !inspect {
		t.Fatal("docker inspect was not invoked")
	}
}

func TestDeviceDockerListFailureIsNotEmptyInventory(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1 kB\nMemAvailable: 1 kB\n")
	deviceStubRunner(t, nil, map[string]error{"sudo docker ps": fail("docker unavailable")})
	if _, err := Probe(Object{"units": Object{}, "asset_roots": []any{}}); err == nil {
		t.Fatal("expected docker failure to surface as an error, not an empty inventory")
	}
}

func TestDeviceDockerInspectGarbageFails(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1 kB\nMemAvailable: 1 kB\n")
	deviceStubRunner(t, map[string]CommandResult{
		"sudo docker ps":      {Out: "abc123\n"},
		"sudo docker inspect": {Out: "not-json\n"},
	}, nil)
	if _, err := Probe(Object{"units": Object{}, "asset_roots": []any{}}); err == nil || !strings.Contains(err.Error(), "Could not read the container state") {
		t.Fatalf("expected container state error, got %v", err)
	}
}

func TestDeviceComfyQueueAndGuard(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1 kB\nMemAvailable: 1 kB\n")
	deviceProbeStub(t)
	srv := deviceTestServer(t, `{"queue_running":[["x","prompt-1"]],"queue_pending":[["y","p2"],["z","p3"]]}`, `{"paused":true}`)
	defer srv.Close()
	result, err := Probe(Object{"units": Object{}, "asset_roots": []any{}, "comfy_url": deviceTestURL})
	if err != nil {
		t.Fatal(err)
	}
	comfy := obj(result["comfyui"])
	if comfy["reachable"] != true || comfy["running"] != float64(1) || comfy["pending"] != float64(2) || comfy["guarded"] != true {
		t.Fatalf("unexpected comfy state: %v", comfy)
	}
	if stringsOf(comfy["prompt_ids"])[0] != "prompt-1" {
		t.Fatalf("unexpected prompt ids: %v", comfy["prompt_ids"])
	}
	if comfy["paused"] != true {
		t.Fatalf("guard state not merged: %v", comfy)
	}
}

func TestDeviceComfyUnreachable(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1 kB\nMemAvailable: 1 kB\n")
	deviceProbeStub(t)
	result, err := Probe(Object{"units": Object{}, "asset_roots": []any{}, "comfy_url": "http://127.0.0.1:9/queue"})
	if err != nil {
		t.Fatal(err)
	}
	comfy := obj(result["comfyui"])
	if comfy["reachable"] != false || comfy["guarded"] != false {
		t.Fatalf("unexpected comfy state: %v", comfy)
	}
}

func TestDeviceAssetsAndNoSecretLeakage(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1 kB\nMemAvailable: 1 kB\n")
	deviceProbeStub(t)
	dir := t.TempDir()
	comfyDir := filepath.Join(dir, "ComfyUI", "models")
	if err := os.MkdirAll(comfyDir, 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(comfyDir, "a.safetensors"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(comfyDir, "b.txt"), []byte("x"), 0644)
	plain := filepath.Join(dir, "models")
	os.MkdirAll(plain, 0700)
	os.WriteFile(filepath.Join(plain, "c.gguf"), []byte("x"), 0644)
	result, err := Probe(Object{"units": Object{}, "asset_roots": []any{comfyDir, plain}})
	if err != nil {
		t.Fatal(err)
	}
	assets := arr(result["assets"])
	if len(assets) != 2 {
		t.Fatalf("unexpected assets: %v", assets)
	}
	seen := map[string]bool{}
	for _, raw := range assets {
		item := obj(raw)
		seen[str(item["path"])] = true
		if item["root"] == comfyDir && item["bytes"] != int64(1) {
			t.Fatalf("unexpected asset metadata: %v", item)
		}
	}
	if !seen["a.safetensors"] || !seen["c.gguf"] {
		t.Fatalf("unexpected asset paths: %v", assets)
	}
}

func TestDeviceWarningsOnQueryFailures(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1024 kB\n")
	deviceStubRunner(t, map[string]CommandResult{
		"nvidia-smi":     {Out: "RTX, 50, 1000, 24000\n", Code: 0},
		"sudo docker ps": {Code: 1},
	}, nil)
	data, err := Device()
	if err != nil {
		t.Fatal(err)
	}
	if stringsOf(data["gpu"])[0] != "RTX, 50, 1000, 24000" {
		t.Fatalf("unexpected gpu lines: %v", data["gpu"])
	}
	if len(data["memory_bytes"].(Object)) == 0 {
		t.Fatal("expected memory on linux-proc hosts")
	}
	warnings := stringsOf(data["warnings"])
	if len(warnings) != 1 || !strings.Contains(warnings[0], "running containers query failed.") {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
}

func TestDeviceCommandUnavailableWarning(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1024 kB\n")
	deviceStubRunner(t, nil, map[string]error{"nvidia-smi": fail("missing"), "sudo docker ps": fail("missing")})
	data, err := Device()
	if err != nil {
		t.Fatal(err)
	}
	warnings := stringsOf(data["warnings"])
	if len(warnings) != 2 || !strings.Contains(warnings[0], "query is unavailable.") {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if _, ok := data["gpu"]; ok {
		t.Fatal("gpu must be absent on failure")
	}
}

func TestDeviceUnitQueryFailureEmptyAndPartial(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1 kB\nMemAvailable: 1 kB\n")
	deviceStubRunner(t, map[string]CommandResult{
		"systemctl":      {Out: "", Code: 1},
		"sudo docker ps": {Out: ""},
	}, nil)
	if _, err := Probe(Object{"units": Object{"vllm": "vllm.service"}, "asset_roots": []any{}}); err == nil {
		t.Fatal("expected empty failed systemctl output to fail")
	}
	deviceStubRunner(t, map[string]CommandResult{
		"systemctl":      {Out: "Id=vllm.service\nActiveState=active\nSubState=running\n", Code: 1},
		"sudo docker ps": {Out: ""},
	}, nil)
	if _, err := Probe(Object{"units": Object{"vllm": "vllm.service", "ocr": "ocr.service"}, "asset_roots": []any{}}); err == nil {
		t.Fatal("expected partial failed systemctl output to fail")
	}
}

func TestDeviceUnitQueryIncompleteState(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1 kB\nMemAvailable: 1 kB\n")
	deviceStubRunner(t, map[string]CommandResult{
		"systemctl":      {Out: "Id=vllm.service\nActiveState=unknown\nSubState=dead\n", Code: 0},
		"sudo docker ps": {Out: ""},
	}, nil)
	result, err := Probe(Object{"units": Object{"vllm": "vllm.service"}, "asset_roots": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if obj(result["units"])["vllm.service"] != "unknown" {
		t.Fatalf("unknown state must be preserved: %v", result["units"])
	}
	deviceStubRunner(t, map[string]CommandResult{
		"systemctl":      {Out: "Id=vllm.service\nSubState=running\n", Code: 0},
		"sudo docker ps": {Out: ""},
	}, nil)
	if _, err := Probe(Object{"units": Object{"vllm": "vllm.service"}, "asset_roots": []any{}}); err == nil {
		t.Fatal("missing ActiveState must fail")
	}
}

func TestDeviceUnitNameValidation(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1 kB\nMemAvailable: 1 kB\n")
	stub := deviceStubRunner(t, nil, nil)
	if _, err := Probe(Object{"units": Object{"vllm": "vllm.service; rm -rf /"}, "asset_roots": []any{}}); err == nil {
		t.Fatal("expected invalid unit name error")
	}
	if len(stub.calls) != 0 {
		t.Fatalf("no command may run for invalid unit names: %v", stub.calls)
	}
}

func TestDeviceDockerListNonzeroWithPartialIDs(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1 kB\nMemAvailable: 1 kB\n")
	deviceStubRunner(t, map[string]CommandResult{
		"sudo docker ps": {Out: "abc123\n", Code: 1},
	}, nil)
	if _, err := Probe(Object{"units": Object{}, "asset_roots": []any{}}); err == nil {
		t.Fatal("nonzero docker ps with partial IDs must fail")
	}
}

func TestDeviceDockerInspectNonzero(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1 kB\nMemAvailable: 1 kB\n")
	for _, out := range []string{"", "{\"name\":\"/a\",\"running\":true,\"id\":\"x\",\"started_at\":\"\",\"oom_killed\":false}\n"} {
		deviceStubRunner(t, map[string]CommandResult{
			"sudo docker ps":      {Out: "x\n"},
			"sudo docker inspect": {Out: out, Code: 1},
		}, nil)
		if _, err := Probe(Object{"units": Object{}, "asset_roots": []any{}}); err == nil || !strings.Contains(err.Error(), "Could not read the container state") {
			t.Fatalf("expected inspect failure error, got %v", err)
		}
	}
	deviceStubRunner(t, map[string]CommandResult{
		"sudo docker ps": {Out: "x\n"},
	}, map[string]error{"sudo docker inspect": fail("transport down")})
	if _, err := Probe(Object{"units": Object{}, "asset_roots": []any{}}); err == nil {
		t.Fatal("expected inspect transport failure error")
	}
	deviceStubRunner(t, nil, map[string]error{"sudo docker ps": fail("transport down")})
	if _, err := Probe(Object{"units": Object{}, "asset_roots": []any{}}); err == nil {
		t.Fatal("expected list transport failure error")
	}
}

func TestDeviceDockerCountMismatch(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: 1 kB\nMemAvailable: 1 kB\n")
	deviceStubRunner(t, map[string]CommandResult{
		"sudo docker ps":      {Out: "a\nb\n"},
		"sudo docker inspect": {Out: "{\"name\":\"/a\",\"running\":false,\"id\":\"a\",\"started_at\":\"\",\"oom_killed\":false}\n"},
	}, nil)
	if _, err := Probe(Object{"units": Object{}, "asset_roots": []any{}}); err == nil {
		t.Fatal("missing inspect rows must fail")
	}
}

func TestDeviceMalformedMemory(t *testing.T) {
	deviceWriteMeminfo(t, "MemTotal: abc kB\nMemAvailable: 1 kB\n")
	deviceProbeStub(t)
	if _, err := Probe(Object{"units": Object{}, "asset_roots": []any{}}); err == nil {
		t.Fatal("expected malformed memory error")
	}
	deviceWriteMeminfo(t, "MemTotal: 1 kB\n")
	deviceProbeStub(t)
	if _, err := Probe(Object{"units": Object{}, "asset_roots": []any{}}); err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatal("expected missing MemAvailable error")
	}
}
