package sparky

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recipeSpec(bindings ...Object) Object {
	return Object{"parameters": Object{"max_num_seqs": float64(8), "gpu_memory_utilization": 0.9}, "bindings": recipeBindings(bindings)}
}

func recipeBindings(bindings []Object) []any {
	out := []any{}
	for _, b := range bindings {
		out = append(out, b)
	}
	return out
}

func recipeWrite(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRenderParametersNvidiaJSON(t *testing.T) {
	dir := t.TempDir()
	text := "{\n  \"serve\": {\n    \"command\": \"vllm serve --max-num-seqs 4 --gpu-memory-utilization 0.5\",\n    \"additional_commands\": [\n      {\n        \"command\": \"python bench.py --max-num-seqs 2\"\n      }\n    ]\n  }\n}\n"
	path := recipeWrite(t, dir, "serve.json", text)
	spec := recipeSpec(Object{
		"path":   path,
		"format": "nvidia_json",
		"fields": Object{"max_num_seqs": "max-num-seqs", "gpu_memory_utilization": "gpu-memory-utilization"},
	})
	result, err := RenderParameters(spec)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	joined := string(data)
	if !strings.Contains(joined, "--max-num-seqs 8") || !strings.Contains(joined, "--max-num-seqs 8") {
		t.Fatalf("expected rewritten values in %s", joined)
	}
	if !strings.Contains(joined, "--gpu-memory-utilization 0.9") {
		t.Fatalf("expected rewritten gpu util in %s", joined)
	}
	if len(stringsOf(result["changed_files"])) != 1 {
		t.Fatalf("expected one changed file: %v", result)
	}
	if backup, err := os.ReadFile(path + ".before-sparky"); err != nil || string(backup) != text {
		t.Fatalf("expected backup copy of the original")
	}
}

func TestRenderParametersWorkerArgvOverride(t *testing.T) {
	dir := t.TempDir()
	text := "args[args.index(\"--max-num-seqs\") + 1] == \"2\"\n"
	path := recipeWrite(t, dir, "worker.py", text)
	spec := Object{
		"parameters": Object{"max_num_seqs": float64(8)},
		"bindings": recipeBindings([]Object{{
			"path":   path,
			"format": "python",
			"fields": Object{"max_num_seqs": "--max-num-seqs"},
		}}),
	}
	if _, err := RenderParameters(spec); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `args[args.index("--max-num-seqs") + 1] == "8"`) {
		t.Fatalf("argv override not rewritten: %s", data)
	}
}

func TestRenderParametersEnvLinesPreserved(t *testing.T) {
	dir := t.TempDir()
	text := "MAX_NUM_SEQS=4\nOTHER=4\n# MAX_NUM_SEQS=4\n"
	path := recipeWrite(t, dir, "env.sh", text)
	spec := Object{
		"parameters": Object{"max_num_seqs": float64(8)},
		"bindings": recipeBindings([]Object{{
			"path":   path,
			"format": "env",
			"fields": Object{"max_num_seqs": "MAX_NUM_SEQS"},
		}}),
	}
	if _, err := RenderParameters(spec); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if !strings.Contains(got, "MAX_NUM_SEQS=8") {
		t.Fatalf("env line not rewritten: %s", got)
	}
	if !strings.Contains(got, "OTHER=4") || strings.Contains(got, "# MAX_NUM_SEQS=8") || strings.Count(got, "MAX_NUM_SEQS=8") != 1 {
		t.Fatalf("unrelated lines were modified: %s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("trailing content changed")
	}
}

func TestRenderParametersDockerEnv(t *testing.T) {
	dir := t.TempDir()
	text := "ENV MAX_NUM_SEQS=4\n"
	path := recipeWrite(t, dir, "Dockerfile", text)
	spec := Object{
		"parameters": Object{"max_num_seqs": float64(8)},
		"bindings": recipeBindings([]Object{{
			"path":   path,
			"format": "docker_env",
			"fields": Object{"max_num_seqs": "MAX_NUM_SEQS"},
		}}),
	}
	if _, err := RenderParameters(spec); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "ENV MAX_NUM_SEQS=8") {
		t.Fatalf("docker env not rewritten: %s", data)
	}
}

func TestRenderParametersMissingFieldIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := recipeWrite(t, dir, "env.sh", "MAX_NUM_SEQS=4\n")
	spec := Object{
		"parameters": Object{"max_num_seqs": float64(8), "gpu_memory_utilization": 0.9},
		"bindings": recipeBindings([]Object{{
			"path":   path,
			"format": "env",
			"fields": Object{"max_num_seqs": "MAX_NUM_SEQS", "gpu_memory_utilization": "GPU_UTIL"},
		}}),
	}
	_, err := RenderParameters(spec)
	if err == nil || !strings.Contains(err.Error(), "gpu_memory_utilization") {
		t.Fatalf("expected missing field error, got %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "MAX_NUM_SEQS=4\n" {
		t.Fatalf("file was modified despite failure: %s", data)
	}
	if _, err := os.Stat(path + ".before-sparky"); !os.IsNotExist(err) {
		t.Fatalf("backup written despite failure")
	}
}

func TestRenderParametersDuplicatePathRejected(t *testing.T) {
	dir := t.TempDir()
	path := recipeWrite(t, dir, "env.sh", "MAX_NUM_SEQS=4\n")
	binding := Object{"path": path, "format": "env", "fields": Object{"max_num_seqs": "MAX_NUM_SEQS"}}
	spec := Object{
		"parameters": Object{"max_num_seqs": float64(8)},
		"bindings":   recipeBindings([]Object{binding, binding}),
	}
	_, err := RenderParameters(spec)
	if err == nil || !strings.Contains(err.Error(), "one binding") {
		t.Fatalf("expected duplicate path error, got %v", err)
	}
}

func TestRenderParametersRelativePathRejected(t *testing.T) {
	spec := Object{
		"parameters": Object{"max_num_seqs": float64(8)},
		"bindings":   recipeBindings([]Object{{"path": "relative/env.sh", "format": "env", "fields": Object{"max_num_seqs": "MAX_NUM_SEQS"}}}),
	}
	if _, err := RenderParameters(spec); err == nil {
		t.Fatal("expected relative path error")
	}
}

func TestRenderParametersNonNumericRejected(t *testing.T) {
	dir := t.TempDir()
	path := recipeWrite(t, dir, "env.sh", "MAX_NUM_SEQS=4\n")
	spec := Object{
		"parameters": Object{"max_num_seqs": true},
		"bindings":   recipeBindings([]Object{{"path": path, "format": "env", "fields": Object{"max_num_seqs": "MAX_NUM_SEQS"}}}),
	}
	if _, err := RenderParameters(spec); err == nil || !strings.Contains(err.Error(), "numeric") {
		t.Fatalf("expected numeric error, got %v", err)
	}
}

func TestRenderParametersUnknownSettingRejected(t *testing.T) {
	dir := t.TempDir()
	path := recipeWrite(t, dir, "env.sh", "MAX_NUM_SEQS=4\n")
	spec := Object{
		"parameters": Object{"max_num_seqs": float64(8), "gpu_memory_utilization": 0.9},
		"bindings":   recipeBindings([]Object{{"path": path, "format": "env", "fields": Object{"max_num_seqs": "MAX_NUM_SEQS"}}}),
	}
	_, err := RenderParameters(spec)
	if err == nil || !strings.Contains(err.Error(), "gpu_memory_utilization") {
		t.Fatalf("expected unmatched setting error, got %v", err)
	}
}

func TestRenderParametersWriteFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	first := recipeWrite(t, dir, "a.env", "MAX_NUM_SEQS=1\n")
	second := recipeWrite(t, dir, "b.env", "MAX_NUM_SEQS=2\n")
	if err := os.Chmod(first, 0640); err != nil {
		t.Fatal(err)
	}
	binding := func(path string) Object {
		return Object{"path": path, "format": "env", "fields": Object{"max_num_seqs": "MAX_NUM_SEQS"}}
	}
	spec := Object{
		"parameters": Object{"max_num_seqs": float64(8)},
		"bindings":   recipeBindings([]Object{binding(first), binding(second)}),
	}
	// Fail the second write (after the first write succeeded) via the seam.
	original := recipeAtomicWrite
	recipeAtomicWrite = func(path string, b []byte, mode os.FileMode) error {
		if path == second {
			return fail("injected write failure")
		}
		return atomicWrite(path, b, mode)
	}
	defer func() { recipeAtomicWrite = original }()
	if _, err := RenderParameters(spec); err == nil {
		t.Fatal("expected write failure")
	}
	data, _ := os.ReadFile(first)
	if string(data) != "MAX_NUM_SEQS=1\n" {
		t.Fatalf("rollback failed for %s: %s", first, data)
	}
	info, _ := os.Stat(first)
	if info.Mode().Perm() != 0640 {
		t.Fatalf("mode not restored: %v", info.Mode())
	}
	secondData, _ := os.ReadFile(second)
	if string(secondData) != "MAX_NUM_SEQS=2\n" {
		t.Fatalf("rollback failed for %s: %s", second, secondData)
	}
}

func TestRenderParametersUnchangedFileMode(t *testing.T) {
	dir := t.TempDir()
	path := recipeWrite(t, dir, "env.sh", "OTHER=4\nMAX_NUM_SEQS=4\n")
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	spec := Object{
		"parameters": Object{"max_num_seqs": float64(8)},
		"bindings":   recipeBindings([]Object{{"path": path, "format": "env", "fields": Object{"max_num_seqs": "MAX_NUM_SEQS"}}}),
	}
	if _, err := RenderParameters(spec); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0640 {
		t.Fatalf("mode not preserved: %v", info.Mode())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "OTHER=4\n") {
		t.Fatalf("other lines lost: %s", data)
	}
}
