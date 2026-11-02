package sparky

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func memorySetProcRoot(t *testing.T, dir string) {
	t.Helper()
	old := memoryProcRoot
	memoryProcRoot = dir
	t.Cleanup(func() { memoryProcRoot = old })
}

func memoryStub(t *testing.T, results map[string]CommandResult, errs map[string]error) *memoryRunnerStub {
	t.Helper()
	stub := &memoryRunnerStub{results: results, errs: errs}
	old := memoryRun
	memoryRun = stub.run
	t.Cleanup(func() { memoryRun = old })
	return stub
}

type memoryCall struct {
	args    []string
	input   []byte
	timeout time.Duration
}

type memoryRunnerStub struct {
	mu      sync.Mutex
	calls   []memoryCall
	results map[string]CommandResult
	errs    map[string]error
}

func memoryStubKey(args []string) string {
	key := args[0]
	if args[0] == "docker" && len(args) > 1 {
		key = "docker " + args[1]
	}
	if args[0] == "sudo" && len(args) > 2 {
		key = "sudo " + args[len(args)-1]
	}
	return key
}

func (s *memoryRunnerStub) run(args []string, input []byte, timeout time.Duration) (CommandResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, memoryCall{args, input, timeout})
	s.mu.Unlock()
	key := memoryStubKey(args)
	if err, ok := s.errs[key]; ok {
		return CommandResult{}, err
	}
	if result, ok := s.results[key]; ok {
		return result, nil
	}
	return CommandResult{}, fail("unexpected memory command: %s", key)
}

func (s *memoryRunnerStub) recorded() []memoryCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]memoryCall{}, s.calls...)
}

func memoryWriteProc(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	memorySetProcRoot(t, dir)
	return dir
}

func TestMemoryParseGPU(t *testing.T) {
	gpu := memoryParseGPU("123, 100.5\n456, N/A\n789, -3\n0, abc\n1, nan\nnotapi, 5\n, 7\n7, 1e30\n")
	if got := *gpu[123]; got != int64(100.5*memoryMiB) {
		t.Fatalf("fractional MiB wrong: %d", got)
	}
	if gpu[456] != nil || gpu[789] != nil || gpu[1] != nil || gpu[0] != nil {
		t.Fatalf("N/A, negative and NaN must be null: %v", gpu)
	}
	if _, ok := gpu[5]; ok {
		t.Fatal("non-numeric pid row must be skipped")
	}
	if gpu[7] != nil {
		t.Fatal("overflow MiB must be null")
	}
}

func TestMemoryClassify(t *testing.T) {
	sources := map[string]string{
		"/system.slice/vllm.service": "vllm",
		"/system.slice":              "system",
		"/vllm":                      "prefix-substring-must-not-match",
	}
	if got := memoryClassify("/system.slice/vllm.service/cgroup", sources); got != "vllm" {
		t.Fatalf("narrowest prefix must win: %s", got)
	}
	if got := memoryClassify("/system.slice/other.service", sources); got != "system" {
		t.Fatalf("expected system fallback: %s", got)
	}
	if got := memoryClassify("/vllm-extra", sources); got != "system" {
		t.Fatalf("substring prefix must not match: %s", got)
	}
	if got := memoryClassify("/vllm/child", sources); got != "prefix-substring-must-not-match" {
		t.Fatalf("exact prefix child must match: %s", got)
	}
}

func memoryProcFixture(t *testing.T) {
	t.Helper()
	memoryWriteProc(t, map[string]string{
		"123/cgroup":        "0::/system.slice/vllm.service\n",
		"123/status":        "Name:\tvllm\nVmRSS:\t 1000 kB\n",
		"123/smaps_rollup":  "Rss:               0 kB\nPss:                 100 kB\n",
		"124/cgroup":        "0::/system.slice/other.service\n",
		"124/status":        "VmRSS:\t 2000 kB\n",
		"124/smaps_rollup":  "Pss:                 200 kB\n",
		"125/cgroup":        "0::/vllm-extra\n",
		"125/status":        "VmRSS:\t 3000 kB\n",
		"125/smaps_rollup":  "Pss:                 300 kB\n",
		"126/status":        "VmRSS:\t 4000 kB\n",
		"127/smaps_rollup":  "Pss:                 500 kB\n",
		"128/cgroup":        "0::/system.slice/vllm.service\n",
		"128/status":        "VmRSS:\t 0 kB\n",
		"129/status":        "VmRSS:\t 5000 kB\n",
		"130/status":        "VmRSS:\t 6000 kB\n",
		"130/smaps_rollup":  "Rss:                 600 kB\n",
		"131/status":        "VmRSS:\t bananas\n",
		"131/smaps_rollup":  "Pss:                 700 kB\n",
		"132/status":        "VmRSS:\t 7000 kB\n",
		"132/smaps_rollup":  "Pss:                 -800 kB\n",
		"nonnumeric/cgroup": "0::/\n",
	})
}

func TestMemoryHostPSS(t *testing.T) {
	memoryProcFixture(t)
	if pss := memoryHostPSS("123"); pss == nil || *pss != 100*1024 {
		t.Fatalf("unexpected pss: %v", pss)
	}
	// Process exited during the scan (no files at all).
	zero := int64(0)
	if pss := memoryHostPSS("999"); pss == nil || *pss != zero {
		t.Fatalf("exited process must read 0: %v", pss)
	}
	// Status present but no rollup file: the read races with exit, so 0.
	if pss := memoryHostPSS("129"); pss == nil || *pss != zero {
		t.Fatalf("missing rollup must read 0: %v", pss)
	}
	// Rollup present but no Pss line: unknown measurement is null.
	if pss := memoryHostPSS("130"); pss != nil {
		t.Fatalf("missing Pss must be null: %v", pss)
	}
	// Malformed VmRSS must not silently become a measured zero.
	if pss := memoryHostPSS("131"); pss != nil {
		t.Fatalf("malformed VmRSS must be null: %v", pss)
	}
	// Negative Pss is invalid, not a measured zero.
	if pss := memoryHostPSS("132"); pss != nil {
		t.Fatalf("negative Pss must be null: %v", pss)
	}
}

func TestMemoryCollectEndToEnd(t *testing.T) {
	memoryProcFixture(t)
	stub := memoryStub(t, map[string]CommandResult{
		"docker ps":      {Out: "cid1\n"},
		"docker inspect": {Out: "{\"name\":\"/garuda-vllm\",\"pid\":123}\n{\"name\":\"stray\",\"pid\":125}\n"},
		"systemctl":      {Out: "Id=vllm.service\nControlGroup=/system.slice/vllm.service\n"},
		"nvidia-smi":     {Out: "123, 500\n999, 50\n"},
	}, nil)
	result, err := CollectMemory(Object{"owners": []any{
		Object{"id": "vllm", "name": "VLLM", "containers": []any{"garuda-vllm"}, "units": []any{"vllm.service"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rows := arr(result["workloads"])
	byID := map[string]Object{}
	for _, raw := range rows {
		row := obj(raw)
		byID[str(row["id"])] = row
	}
	vllm := byID["vllm"]
	if vllm == nil {
		t.Fatalf("missing vllm row: %v", rows)
	}
	if int(num(vllm["gpu_bytes"])) != int(500*memoryMiB) {
		t.Fatalf("unexpected gpu bytes: %v", vllm["gpu_bytes"])
	}
	if int(num(vllm["host_pss_bytes"])) != 100*1024 {
		t.Fatalf("unexpected host pss: %v", vllm["host_pss_bytes"])
	}
	if int(num(arr(vllm["pids"])[0])) != 123 {
		t.Fatalf("unexpected pids: %v", vllm["pids"])
	}
	// Exited GPU process 999 attributed to system; 125 (stray container substring cgroup) is system too.
	system := byID["system"]
	if system == nil {
		t.Fatalf("missing system row: %v", rows)
	}
	if int(num(system["gpu_bytes"])) != int(50*memoryMiB) {
		t.Fatalf("unexpected system gpu: %v", system["gpu_bytes"])
	}
	if result["unified_memory"] != true || result["add_columns"] != false {
		t.Fatalf("unexpected flags: %v", result)
	}
	if str(result["gpu_source"]) != "nvidia-smi per-process allocations" || str(result["host_source"]) != "/proc/PID/smaps_rollup Pss" {
		t.Fatalf("unexpected sources: %v", result)
	}
	if str(result["sampled_at"]) == "" {
		t.Fatal("missing sampled_at")
	}
	_ = stub
}

func TestMemoryCollectWarnings(t *testing.T) {
	memoryProcFixture(t)
	memoryStub(t, map[string]CommandResult{
		"docker ps": {Code: 1},
		"systemctl": {Out: ""},
	}, nil)
	result, err := CollectMemory(Object{"owners": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	warnings := stringsOf(result["warnings"])
	joined := strings.Join(warnings, " | ")
	if !strings.Contains(joined, "Container ownership could not be read.") || !strings.Contains(joined, "GPU memory readings are unavailable.") {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	for _, raw := range arr(result["workloads"]) {
		if obj(raw)["gpu_bytes"] != nil {
			t.Fatalf("gpu_bytes must be null without readings: %v", raw)
		}
	}
	// Inspect failure path.
	memoryStub(t, map[string]CommandResult{
		"docker ps":      {Out: "cid\n"},
		"docker inspect": {Code: 1},
	}, nil)
	result, err = CollectMemory(Object{"owners": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(stringsOf(result["warnings"]), " "), "Container ownership could not be read.") {
		t.Fatalf("expected container warning: %v", result["warnings"])
	}
}

func TestMemorySpecification(t *testing.T) {
	config := Object{
		"nodes": Object{"spark01": Object{"comfy_unit": "comfy.service"}, "spark02": Object{"ssh": "spark02"}},
		"models": Object{
			"vllm": Object{"kind": "llm", "aliases": []any{"Garuda 70B"}, "settings": Object{
				"spark01": Object{"units": Object{"spark01": "vllm.service"}},
			}},
			"whisper": Object{"kind": "audio", "settings": Object{
				"spark01": Object{"units": Object{"spark01": "whisper.service"}, "containers": []any{"garuda-whisper"}},
			}},
		},
	}
	spec := memorySpecification(config, "spark01")
	owners := arr(spec["owners"])
	if len(owners) != 3 {
		t.Fatalf("unexpected owners: %v", owners)
	}
	first := obj(owners[0])
	if first["id"] != "vllm" || first["name"] != "Garuda 70B" || len(stringsOf(first["units"])) != 1 || stringsOf(first["units"])[0] != "vllm.service" {
		t.Fatalf("unexpected llm owner: %v", first)
	}
	second := obj(owners[1])
	if second["id"] != "whisper" || second["name"] != "Whisper" || len(stringsOf(second["containers"])) != 1 || stringsOf(second["containers"])[0] != "garuda-whisper" {
		t.Fatalf("unexpected audio owner: %v", second)
	}
	third := obj(owners[2])
	if third["id"] != "comfyui" || len(stringsOf(third["units"])) != 1 || stringsOf(third["units"])[0] != "comfy.service" {
		t.Fatalf("unexpected comfy owner: %v", third)
	}
}

func TestMemoryReadNodeProtocol(t *testing.T) {
	config := Object{
		"root":   "/tmp/spark-root",
		"nodes":  Object{"spark01": Object{"comfy_unit": "comfy.service"}, "spark02": Object{"ssh": "spark02"}},
		"models": Object{},
	}
	stub := memoryStub(t, map[string]CommandResult{
		"sudo memory": {Out: `{"workloads":[]}`},
		"ssh":         {Out: `{"workloads":[]}`},
	}, nil)
	if _, err := memoryReadNode(config, "spark01"); err != nil {
		t.Fatal(err)
	}
	calls := stub.recorded()
	if len(calls) != 1 || calls[0].args[0] != "sudo" || calls[0].args[1] != "-n" {
		t.Fatalf("unexpected local invocation: %v", calls)
	}
	var payload Object
	if err := json.Unmarshal(calls[0].input, &payload); err != nil {
		t.Fatalf("payload must be JSON on stdin: %v", calls[0].input)
	}
	if _, err := memoryReadNode(config, "spark02"); err != nil {
		t.Fatal(err)
	}
	calls = stub.recorded()
	remote := calls[len(calls)-1]
	if remote.args[0] != "ssh" || remote.args[1] != "-o" || remote.args[4] != "ConnectTimeout=8" || remote.args[5] != "spark02" {
		t.Fatalf("unexpected remote invocation: %v", remote.args)
	}
	joined := remote.args[len(remote.args)-1]
	if !strings.Contains(joined, "sudo -n") || !strings.Contains(joined, "node memory") {
		t.Fatalf("unexpected remote command string: %s", joined)
	}
	stub2 := memoryStub(t, map[string]CommandResult{
		"sudo memory": {Out: `{"error":"boom"}`, Code: 1},
	}, nil)
	if _, err := memoryReadNode(config, "spark01"); err == nil {
		t.Fatal("expected nonzero remote status to fail")
	}
	_ = stub2
}

func memoryTestServer(t *testing.T, body string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + listener.Addr().String()
}

func TestEnrichStatus(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "vllm"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vllm/api.env"), []byte("VLLM_API_KEY=test-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	backend := memoryTestServer(t, `{"data":[{"id":"garuda/vllm","max_model_len":8192}]}`)
	config := Object{
		"root":  dir,
		"nodes": Object{"spark01": Object{"comfy_unit": "comfy.service"}, "spark02": Object{"ssh": "spark02"}},
		"models": Object{
			"vllm": Object{"kind": "llm", "settings": Object{
				"both": Object{"backend_url": backend, "context_tokens": float64(4096)},
			}},
		},
	}
	status := Object{
		"models": Object{
			"vllm":    Object{"kind": "llm", "loaded": true, "state": "running", "nodes": []any{"spark01", "spark02"}},
			"comfyui": Object{"kind": "comfy", "loaded": true, "nodes": []any{"spark01"}},
		},
		"comfyui": Object{"spark01": Object{"phase": "idle", "pending": float64(3)}},
	}
	original := copyObject(status)
	sample := Object{"workloads": []any{
		Object{"id": "vllm", "name": "VLLM", "pids": []any{float64(1)}, "host_pss_bytes": float64(1024), "gpu_bytes": nil},
		Object{"id": "comfyui", "name": "ComfyUI", "pids": []any{float64(2)}, "host_pss_bytes": float64(2048), "gpu_bytes": float64(1)},
	}}
	var mu sync.Mutex
	stub := memoryStub(t, nil, nil)
	memoryRun = func(args []string, input []byte, timeout time.Duration) (CommandResult, error) {
		mu.Lock()
		defer mu.Unlock()
		stub.calls = append(stub.calls, memoryCall{args, input, timeout})
		if timeout != 45*time.Second {
			return CommandResult{}, fail("expected 45s timeout")
		}
		return CommandResult{Out: string(memoryMustJSON(t, sample))}, nil
	}
	t.Cleanup(func() {})
	result, err := EnrichStatus(config, status)
	if err != nil {
		t.Fatal(err)
	}
	usage := obj(result["memory_usage"])
	if len(usage) != 2 {
		t.Fatalf("expected both nodes sampled: %v", usage)
	}
	models := obj(result["models"])
	vllm := obj(models["vllm"])
	if num(vllm["context_tokens"]) != 8192 || vllm["context_source"] != "running backend" {
		t.Fatalf("context not taken from backend: %v", vllm)
	}
	byNode := obj(vllm["memory_by_node"])
	if len(byNode) != 2 {
		t.Fatalf("expected memory_by_node for both nodes: %v", byNode)
	}
	comfy := obj(models["comfyui"])
	rows := arr(obj(usage["spark01"])["workloads"])
	rowState := ""
	for _, raw := range rows {
		row := obj(raw)
		if str(row["id"]) == "comfyui" {
			rowState = str(row["state"])
		}
	}
	if rowState != "idle (3 queued)" {
		t.Fatalf("unexpected comfy row state: %s", rowState)
	}
	if comfy["context_tokens"] != nil {
		t.Fatalf("non-llm context must stay null: %v", comfy)
	}
	// The input status must not be mutated.
	if len(obj(original["models"])["vllm"].(Object)) != 4 {
		t.Fatalf("input status was mutated")
	}
	if len(stub.recorded()) != 2 {
		t.Fatalf("expected one sample per node: %v", stub.recorded())
	}
}

func memoryMustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEnrichStatusFallbackWarning(t *testing.T) {
	config := Object{
		"root":   "/tmp/nonexistent-root",
		"nodes":  Object{"spark01": Object{}},
		"models": Object{},
	}
	status := Object{"models": Object{}}
	memoryStub(t, nil, map[string]error{"sudo memory": fail("down")})
	result, err := EnrichStatus(config, status)
	if err != nil {
		t.Fatal(err)
	}
	usage := obj(result["memory_usage"])
	sample := obj(usage["spark01"])
	warnings := stringsOf(sample["warnings"])
	if len(warnings) != 1 || !strings.Contains(warnings[0], "Memory readings are unavailable on spark01.") {
		t.Fatalf("expected fallback warning: %v", sample)
	}
	if len(arr(sample["workloads"])) != 0 {
		t.Fatalf("fallback workloads must be empty: %v", sample)
	}
}

func TestEnrichStatusContextFallback(t *testing.T) {
	config := Object{
		"root":  "/tmp/does-not-exist",
		"nodes": Object{"spark01": Object{}},
		"models": Object{
			"vllm": Object{"kind": "llm", "settings": Object{
				"spark01": Object{"context_tokens": float64(2048)},
			}},
		},
	}
	status := Object{"models": Object{
		"vllm": Object{"kind": "llm", "loaded": true, "nodes": []any{"spark01"}},
	}}
	memoryStub(t, map[string]CommandResult{"sudo memory": {Out: `{"workloads":[]}`}}, nil)
	result, err := EnrichStatus(config, status)
	if err != nil {
		t.Fatal(err)
	}
	vllm := obj(obj(result["models"])["vllm"])
	if vllm["context_tokens"] != float64(2048) || vllm["context_source"] != "configured" {
		t.Fatalf("expected configured fallback: %v", vllm)
	}
}

func TestMemoryEmptyWarningsJSON(t *testing.T) {
	memoryProcFixture(t)
	memoryStub(t, map[string]CommandResult{
		"docker ps":  {Out: ""},
		"nvidia-smi": {Out: ""},
	}, nil)
	result, err := CollectMemory(Object{"owners": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result["warnings"])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("warnings must serialize as []: %s", encoded)
	}
}

func TestMemorySpecificationValidation(t *testing.T) {
	if _, err := CollectMemory(Object{"owners": []any{Object{"name": "x"}}}); err == nil {
		t.Fatal("missing owner id must be rejected")
	}
	if _, err := CollectMemory(Object{"owners": []any{Object{"id": "vllm", "containers": []any{"bad name!"}}}}); err == nil {
		t.Fatal("invalid container name must be rejected")
	}
	if _, err := CollectMemory(Object{"owners": []any{Object{"id": "vllm", "units": []any{"bad unit"}}}}); err == nil {
		t.Fatal("invalid unit name must be rejected")
	}
}

func TestMemorySystemctlWarning(t *testing.T) {
	memoryProcFixture(t)
	memoryStub(t, map[string]CommandResult{
		"docker ps":  {Out: ""},
		"nvidia-smi": {Out: ""},
	}, nil)
	spec := memorySpecification(Object{
		"nodes":  Object{"spark01": Object{}},
		"models": Object{"vllm": Object{"kind": "llm", "settings": Object{"spark01": Object{"units": Object{"spark01": "vllm.service"}}}}},
	}, "spark01")
	// systemctl is expected to fail; the stub fails unexpected commands too,
	// so register its failure result explicitly.
	memoryStub(t, map[string]CommandResult{
		"docker ps":  {Out: ""},
		"nvidia-smi": {Out: ""},
		"systemctl":  {Code: 1},
	}, nil)
	result, err := CollectMemory(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(stringsOf(result["warnings"]), " "), "Service ownership could not be read.") {
		t.Fatalf("expected service warning: %v", result["warnings"])
	}
}

func nilValue(t *testing.T, v any) any {
	t.Helper()
	return v
}

func TestEnrichStatusSinglePlacement(t *testing.T) {
	config := Object{
		"root":  "/tmp/does-not-exist",
		"nodes": Object{"spark01": Object{}},
		"models": Object{
			"vllm": Object{"kind": "llm", "settings": Object{
				"both":    Object{"context_tokens": float64(999)},
				"spark01": Object{"context_tokens": float64(111)},
			}},
		},
	}
	status := Object{"models": Object{
		"vllm": Object{"kind": "llm", "loaded": true, "nodes": []any{"spark01"}},
	}}
	memoryStub(t, map[string]CommandResult{"sudo memory": {Out: `{"workloads":[]}`}}, nil)
	result, err := EnrichStatus(config, status)
	if err != nil {
		t.Fatal(err)
	}
	vllm := obj(obj(result["models"])["vllm"])
	if vllm["context_tokens"] != float64(111) || vllm["context_source"] != "configured" {
		t.Fatalf("single-node placement must not fall back to both: %v", vllm)
	}
}

func TestEnrichStatusMalformedSample(t *testing.T) {
	config := Object{
		"root":  "/tmp/does-not-exist",
		"nodes": Object{"spark01": Object{}},
		"models": Object{
			"vllm": Object{"kind": "llm", "loaded": true, "nodes": []any{"spark01"}},
		},
	}
	for _, body := range []string{`[]`, `{"workloads": 5}`} {
		memoryStub(t, map[string]CommandResult{"sudo memory": {Out: body}}, nil)
		result, err := EnrichStatus(config, Object{"models": Object{}})
		if err != nil {
			t.Fatal(err)
		}
		sample := obj(obj(result["memory_usage"])["spark01"])
		if len(arr(sample["workloads"])) != 0 || len(stringsOf(sample["warnings"])) != 1 {
			t.Fatalf("malformed sample %s must yield safe fallback: %v", body, sample)
		}
		if !strings.Contains(strings.Join(stringsOf(sample["warnings"]), " "), "Memory readings are unavailable on spark01.") {
			t.Fatalf("expected warning for malformed sample: %v", sample)
		}
	}
}
