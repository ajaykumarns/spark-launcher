package sparky

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseConfig() Object {
	return Object{
		"version": 1,
		"root":    "/srv/sparky",
		"nodes":   Object{"spark01": Object{}, "spark02": Object{}},
		"policy":  Object{},
		"models":  Object{},
	}
}

func llmModel(on string) Object {
	units := Object{}
	for _, n := range placements[on] {
		units[n] = "spark-model@" + n + ".service"
	}
	return Object{"kind": "llm", "run_on": on, "settings": Object{on: Object{
		"units": units, "backend_url": "http://127.0.0.1:9000",
		"context_tokens": 32768, "concurrency": 4, "parameters": Object{},
	}}}
}

func audioModel(on string) Object {
	return Object{"kind": "audio", "run_on": on, "settings": Object{on: Object{
		"containers": []any{"spark-tts"}, "parameters": Object{},
	}}}
}

func expectFail(t *testing.T, c Object, fragment string) {
	t.Helper()
	err := validateCluster(c)
	if err == nil {
		t.Fatalf("expected failure mentioning %q, got none", fragment)
	}
	if fragment != "" && !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error %q does not mention %q", err, fragment)
	}
}

func TestValidateClusterAcceptsAValidConfig(t *testing.T) {
	c := baseConfig()
	c["models"] = Object{
		"glm":  llmModel("both"),
		"echo": llmModel("spark01"),
		"tts":  audioModel("spark02"),
		"sway": Object{"kind": "comfyui", "run_on": "both", "settings": Object{"both": Object{"parameters": Object{}}}},
		"sd15": Object{"kind": "comfy_model", "run_on": "spark01", "settings": Object{"spark01": Object{"parameters": Object{}}}},
	}
	c["auto"] = Object{"targets": []any{"glm", "echo"}, "default": "glm"}
	if err := validateCluster(c); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateClusterRejectsCorruptTypesWithoutPanic(t *testing.T) {
	c := baseConfig()
	c["version"] = "1"
	expectFail(t, c, "version")
	c = baseConfig()
	c["version"] = 1.5
	expectFail(t, c, "version")
	c = baseConfig()
	c["nodes"] = Object{"spark01": Object{}}
	expectFail(t, c, "spark01")
	c = baseConfig()
	c["models"] = nil
	expectFail(t, c, "")
	c = baseConfig()
	c["root"] = "relative/path"
	expectFail(t, c, "root")
	c = baseConfig()
	c["controller_port"] = 0
	expectFail(t, c, "port")
	c = baseConfig()
	c["controller_port"] = 65536
	expectFail(t, c, "port")
	c = baseConfig()
	c["controller_port"] = json.Number("8020")
	if err := validateCluster(c); err != nil {
		t.Fatalf("exact numeric port rejected: %v", err)
	}
	c = baseConfig()
	c["manager_url"] = "https://127.0.0.1:8080"
	expectFail(t, c, "manager URL")
	c = baseConfig()
	evil := llmModel("both")
	s := obj(evil["settings"])["both"].(Object)
	s["backend_url"] = "http://user:pass@host:1"
	c["models"] = Object{"m": llmModel("both"), "evil": evil}
	expectFail(t, c, "backend")
}

func TestValidateClusterRejectsUnknownLifecycleMode(t *testing.T) {
	c := baseConfig()
	c["lifecycle"] = "naitive"
	expectFail(t, c, "Lifecycle mode")
	c["lifecycle"] = "native"
	if err := validateCluster(c); err != nil {
		t.Fatalf("explicit native lifecycle rejected: %v", err)
	}
	c["lifecycle"] = "legacy"
	if err := validateCluster(c); err != nil {
		t.Fatalf("explicit legacy lifecycle rejected: %v", err)
	}
}

func TestValidateClusterRejectsInvalidPolicyBooleans(t *testing.T) {
	c := baseConfig()
	c["policy"] = Object{"interrupt_active_jobs": "yes"}
	expectFail(t, c, "Interrupting active jobs")
	c["policy"] = Object{"interrupt_active_jobs": true}
	expectFail(t, c, "Interrupting active jobs")
	c["policy"] = Object{"interrupt_active_jobs": false}
	if err := validateCluster(c); err != nil {
		t.Fatalf("false interrupt policy rejected: %v", err)
	}
	c["policy"] = Object{"comfyui_exclusive_with_llms": "true"}
	expectFail(t, c, "exclusivity")
}

func TestValidateClusterDoesNotMutateCallerConfig(t *testing.T) {
	c := baseConfig()
	before, _ := json.Marshal(c)
	if err := validateCluster(c); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	after, _ := json.Marshal(c)
	if string(before) != string(after) {
		t.Fatalf("validation mutated the caller config:\n%s\n%s", before, after)
	}
}

func TestPrivateURLRequiresUsablePrivateOrigin(t *testing.T) {
	valid := []string{"http://127.0.0.1:8080", "http://spark01.local", "http://h:1/", "http://h:65535"}
	invalid := []string{
		"https://h", "http://user:pass@h", "http://h/?x=1", "http://h/#f",
		"http://h/base", "http://h:0", "http://h:99999", "http://h:abc", "http://", "nonsense",
	}
	for _, u := range valid {
		if !privateURL(u) {
			t.Fatalf("%q must be a valid private URL", u)
		}
	}
	for _, u := range invalid {
		if privateURL(u) {
			t.Fatalf("%q must be rejected", u)
		}
	}
}

func TestReadAndWriteClusterJSONAndYAML(t *testing.T) {
	dir := t.TempDir()
	c := baseConfig()
	c["models"] = Object{"glm": llmModel("both")}
	jsonPath := filepath.Join(dir, "cluster.json")
	if err := writeCluster(jsonPath, c); err != nil {
		t.Fatalf("json write failed: %v", err)
	}
	back, err := readCluster(jsonPath)
	if err != nil {
		t.Fatalf("json read failed: %v", err)
	}
	if str(obj(obj(back["models"])["glm"])["kind"]) != "llm" {
		t.Fatalf("json roundtrip lost the model: %#v", back)
	}

	yamlPath := filepath.Join(dir, "cluster.yaml")
	if err := writeCluster(yamlPath, c); err != nil {
		t.Fatalf("yaml write failed: %v", err)
	}
	back, err = readCluster(yamlPath)
	if err != nil {
		t.Fatalf("yaml read failed: %v", err)
	}
	if _, ok := obj(obj(back["models"])["glm"])["settings"]; !ok {
		t.Fatalf("yaml roundtrip lost the model: %#v", back)
	}
	info, err := os.Stat(jsonPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("cluster files must be private: %v", info)
	}
}

func TestReadClusterRejectsInvalidFiles(t *testing.T) {
	dir := t.TempDir()
	missing, missingErr := readCluster(filepath.Join(dir, "absent.json"))
	_ = missing
	if missingErr == nil || !strings.Contains(missingErr.Error(), "not found") {
		t.Fatalf("missing file must be a clear error: %v", missingErr)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"version":1,`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCluster(bad); err == nil {
		t.Fatal("corrupt JSON must fail")
	}
	wrong := filepath.Join(dir, "wrong.json")
	if err := os.WriteFile(wrong, []byte(`{"version":2,"nodes":{},"models":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCluster(wrong); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("wrong version must fail clearly: %v", err)
	}
}

func TestAliasesAndSharedPolicyValidation(t *testing.T) {
	c := baseConfig()
	m := llmModel("both")
	m["aliases"] = []any{"Flash"}
	c["models"] = Object{"glm": m}
	if err := validateCluster(c); err != nil {
		t.Fatalf("case-insensitive unique aliases rejected: %v", err)
	}
	c["models"] = Object{"glm": m, "other": func() Object { x := llmModel("both"); x["aliases"] = []any{"glm"}; return x }()}
	expectFail(t, c, "alias")
	c["models"] = Object{"glm": m, "spark-auto": llmModel("both")}
	expectFail(t, c, "")
	c = baseConfig()
	c["models"] = Object{"glm": llmModel("both")}
	c["policy"] = Object{"protected_models": []any{"glm"}}
	expectFail(t, c, "stay loaded")
}

func TestControllerPortDefaultsAndInvalidValues(t *testing.T) {
	if controllerPort(baseConfig()) != 8020 {
		t.Fatal("an unset controller_port must mean the 8020 default")
	}
	if controllerPort(Object{"controller_port": json.Number("8020")}) != 8020 {
		t.Fatal("an exact numeric port must be honored")
	}
	for _, bad := range []any{0, 65536, "8020", json.Number("99999"), json.Number("18446744073709551615")} {
		if controllerPort(Object{"controller_port": bad}) != 0 {
			t.Fatalf("invalid explicit port %#v must yield 0", bad)
		}
	}
}

func TestReadEditWriteRoundtripPreservesExactMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")
	c := baseConfig()
	c["models"] = Object{"glm": llmModel("both")}
	c["metadata"] = Object{"image_seed": json.Number("18446744073709551615"), "note": nil, "ok": true}
	if err := writeCluster(path, c); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	back, err := readCluster(path)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if s, ok := obj(back["metadata"])["image_seed"].(json.Number); !ok || s.String() != "18446744073709551615" {
		t.Fatalf("the uint64 seed must survive a read: %#v", obj(back["metadata"])["image_seed"])
	}
	// Edit and rewrite; the untouched metadata must remain exact.
	obj(obj(back["models"])["glm"])["name"] = "GLM"
	if err := writeCluster(path, back); err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}
	again, err := readCluster(path)
	if err != nil {
		t.Fatalf("reread failed: %v", err)
	}
	if s, ok := obj(again["metadata"])["image_seed"].(json.Number); !ok || s.String() != "18446744073709551615" {
		t.Fatalf("the uint64 seed must survive read->edit->write->read: %#v", obj(again["metadata"])["image_seed"])
	}
	if obj(obj(again["models"])["glm"])["name"] != "GLM" {
		t.Fatal("the edit itself must persist")
	}
	if obj(again["metadata"])["note"] != nil || obj(again["metadata"])["ok"] != true {
		t.Fatalf("nulls and booleans must survive the roundtrip: %#v", again["metadata"])
	}
	// The final on-disk spelling must be exact, with no float rounding.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "18446744073709551615") {
		t.Fatalf("the file lost the exact seed: %s", string(raw))
	}
}

func expectExactInteger(t *testing.T, v any, want string) {
	t.Helper()
	switch x := v.(type) {
	case json.Number:
		if x.String() != want {
			t.Fatalf("expected exact integer %s, got %#v", want, v)
		}
	case int, int64, uint64:
		if intText(x) != want {
			t.Fatalf("expected exact integer %s, got %#v", want, v)
		}
	default:
		t.Fatalf("expected an exact integer, got %#v", v)
	}
}

func TestReadClusterPreservesYAMLIntegersAndMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.yaml")
	c := baseConfig()
	c["models"] = Object{"glm": llmModel("both")}
	c["metadata"] = Object{"seed": json.Number("18446744073709551615"), "big": json.Number("9007199254740993"), "count": json.Number("42")}
	if err := writeCluster(path, c); err != nil {
		t.Fatalf("yaml write failed: %v", err)
	}
	back, err := readCluster(path)
	if err != nil {
		t.Fatalf("yaml read failed: %v", err)
	}
	meta := obj(back["metadata"])
	expectExactInteger(t, meta["seed"], "18446744073709551615")
	expectExactInteger(t, meta["big"], "9007199254740993")
	expectExactInteger(t, meta["count"], "42")
}

func TestResolveWorkloadMatchesNamesAndAliases(t *testing.T) {
	c := baseConfig()
	c["models"] = Object{"glm": func() Object { m := llmModel("both"); m["aliases"] = []any{"Flash"}; return m }()}
	name, m, err := resolveWorkload(c, "garuda/FLASH")
	if err != nil || name != "glm" || str(m["kind"]) != "llm" {
		t.Fatalf("alias resolution failed: %v %s", err, name)
	}
	if _, _, err := resolveWorkload(c, "glm "); err != nil || name != "glm" {
		// trimmed resolution must succeed
		if _, _, e := resolveWorkload(c, "  glm "); e != nil {
			t.Fatalf("trimming failed: %v", e)
		}
	}
	if _, _, err := resolveWorkload(c, "nope"); err == nil {
		t.Fatal("unknown workload must fail")
	}
}
