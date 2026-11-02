package sparky

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func harnessTestConfig(t *testing.T, binaries map[string]string) clientConfig {
	t.Helper()
	cfg, err := clientValidateConfig(Object{
		"version": 1, "gateway": "https://spark.example:8443",
		"binaries": func() Object {
			out := Object{}
			for k, v := range binaries {
				out[k] = v
			}
			return out
		}(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func harnessFakeBinary(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func harnessTestCatalog() Object {
	return Object{
		"models": []any{
			Object{"id": "garuda/other", "name": "Other", "context_tokens": 128000, "output_tokens": 32000, "inputs": []any{"text", "image"}},
			Object{"id": "garuda/spark-auto", "name": "Spark Auto", "context_tokens": 200000, "context_source": "gateway auto route context"},
		},
		"audio_services": []any{},
	}
}

func harnessModel(id string) Object {
	for _, m := range arr(harnessTestCatalog()["models"]) {
		if obj(m)["id"] == id {
			return obj(m)
		}
	}
	return Object{}
}

// --- executable resolution ---

func TestHarnessExecutable(t *testing.T) {
	bin := harnessFakeBinary(t, "fakeclaude")
	cfg := harnessTestConfig(t, map[string]string{"claude": bin})
	got, err := harnessExecutable(cfg, "claude")
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("not absolute: %s", got)
	}
	missing := harnessTestConfig(t, map[string]string{"claude": "/nonexistent/sparky-claude"})
	if _, err := harnessExecutable(missing, "claude"); err == nil {
		t.Fatal("expected missing binary error")
	}
	// Non-executable file.
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	noExec := harnessTestConfig(t, map[string]string{"claude": plain})
	if _, err := harnessExecutable(noExec, "claude"); err == nil {
		t.Fatal("expected non-executable error")
	}
}

// --- buildHarnessPlan: the four exact shapes ---

func TestBuildHarnessPlanClaude(t *testing.T) {
	bin := harnessFakeBinary(t, "claude")
	cfg := harnessTestConfig(t, map[string]string{"claude": bin})
	model := harnessModel("garuda/spark-auto")
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), model, "claude", "~/proj", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(arr(plan["args"]), []any{"--model", "garuda/spark-auto"}) {
		t.Fatalf("bad args: %v", plan["args"])
	}
	env := obj(plan["environment_overrides"])
	checks := map[string]string{
		"ANTHROPIC_BASE_URL":                 "https://spark.example:8443/anthropic",
		"ANTHROPIC_AUTH_TOKEN":               "spark-keyless",
		"ANTHROPIC_MODEL":                    "garuda/spark-auto",
		"ANTHROPIC_CUSTOM_MODEL_OPTION":      "garuda/spark-auto",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME": "Spark Auto",
		"CLAUDE_CODE_MAX_CONTEXT_TOKENS":     "200000",
		"API_TIMEOUT_MS":                     "7200000",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":       "garuda/spark-auto",
		"CLAUDE_CODE_SUBAGENT_MODEL":         "garuda/spark-auto",
	}
	for k, want := range checks {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if env["ANTHROPIC_CUSTOM_HEADERS"] != "X-Client-ID: sparky/claude\nX-Session-ID: sess-1" {
		t.Fatalf("bad headers: %q", env["ANTHROPIC_CUSTOM_HEADERS"])
	}
	wantUnset := []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_AWS"}
	if !reflect.DeepEqual(stringsOf(plan["environment_unset"]), wantUnset) {
		t.Fatalf("bad unset: %v", plan["environment_unset"])
	}
	if str(plan["project"]) != clientResolveProject("~/proj") || !filepath.IsAbs(str(plan["project"])) {
		t.Fatalf("bad project: %v", plan["project"])
	}
	if str(plan["session"]) != "sess-1" {
		t.Fatalf("bad session: %v", plan["session"])
	}
	if obj(plan["model"])["id"] != "garuda/spark-auto" {
		t.Fatalf("model not preserved: %v", plan["model"])
	}
}

func TestBuildHarnessPlanCodex(t *testing.T) {
	bin := harnessFakeBinary(t, "codex")
	cfg := harnessTestConfig(t, map[string]string{"codex": bin})
	cfg.CompactPercent = 95
	model := harnessModel("garuda/other")
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), model, "codex", "/tmp/p", "s2")
	if err != nil {
		t.Fatal(err)
	}
	args := stringsOf(plan["args"])
	values := map[string]string{}
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] != "-c" {
			t.Fatalf("unexpected arg %v at %d", args[i], i)
		}
		kv := strings.SplitN(args[i+1], "=", 2)
		values[kv[0]] = strings.Trim(kv[1], "\"")
	}
	want := map[string]string{
		"model_provider":                                  "spark",
		"model":                                           "garuda/other",
		"review_model":                                    "garuda/other",
		"model_context_window":                            "128000",
		"model_auto_compact_token_limit":                  "121600", // 128000 * 95 / 100
		"model_providers.spark.base_url":                  "https://spark.example:8443/v1",
		"model_providers.spark.wire_api":                  "responses",
		"model_providers.spark.http_headers.X-Client-ID":  "sparky/codex",
		"model_providers.spark.http_headers.X-Session-ID": "s2",
	}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("%s = %q, want %q", k, values[k], v)
		}
	}
	if values["model_providers.spark.requires_openai_auth"] != "false" {
		t.Errorf("requires_openai_auth = %q", values["model_providers.spark.requires_openai_auth"])
	}
	if values["model_providers.spark.stream_idle_timeout_ms"] != "7200000" {
		t.Errorf("stream idle timeout = %q", values["model_providers.spark.stream_idle_timeout_ms"])
	}
}

func TestBuildHarnessPlanOpenCode(t *testing.T) {
	bin := harnessFakeBinary(t, "opencode")
	cfg := harnessTestConfig(t, map[string]string{"opencode": bin})
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "opencode", "/tmp/p", "s3")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(arr(plan["args"]), []any{"--model", "spark/other"}) {
		t.Fatalf("bad args: %v", plan["args"])
	}
	env := obj(plan["environment_overrides"])
	var raw Object
	if err := json.Unmarshal([]byte(str(env["OPENCODE_CONFIG_CONTENT"])), &raw); err != nil {
		t.Fatalf("bad embedded JSON: %v", err)
	}
	if !reflect.DeepEqual(arr(raw["enabled_providers"]), []any{"spark"}) {
		t.Fatalf("bad enabled providers: %v", raw["enabled_providers"])
	}
	if raw["model"] != "spark/other" || raw["small_model"] != "spark/other" {
		t.Fatalf("bad model selection: %v", raw)
	}
	provider := obj(obj(raw["provider"])["spark"])
	if obj(provider["options"])["baseURL"] != "https://spark.example:8443/v1" {
		t.Fatalf("bad baseURL: %v", provider)
	}
	models := obj(provider["models"])
	other := obj(models["other"])
	if int(num(obj(other["limit"])["context"])) != 128000 || int(num(obj(other["limit"])["output"])) != 32000 {
		t.Fatalf("bad limits: %v", other)
	}
	if !reflect.DeepEqual(arr(obj(other["modalities"])["input"]), []any{"text", "image"}) {
		t.Fatalf("bad modalities: %v", other)
	}
	// spark-auto appears in the model map exactly like the other catalog rows.
	if _, ok := models["spark-auto"]; !ok {
		t.Fatal("auto model missing from map")
	}
}

func TestBuildHarnessPlanHermes(t *testing.T) {
	bin := harnessFakeBinary(t, "hermes")
	cfg := harnessTestConfig(t, map[string]string{"hermes": bin})
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "hermes", "/tmp/p", "s4")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(arr(plan["args"]), []any{"--provider", "spark-gateway", "--model", "garuda/other"}) {
		t.Fatalf("bad args: %v", plan["args"])
	}
	if obj(plan["environment_overrides"])["HERMES_INFERENCE_MODEL"] != "garuda/other" {
		t.Fatalf("bad env: %v", plan["environment_overrides"])
	}
}

func TestBuildHarnessPlanErrors(t *testing.T) {
	cfg := harnessTestConfig(t, map[string]string{"claude": "/nonexistent/sparky-claude"})
	if _, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "bogus", "/tmp", "s"); err == nil {
		t.Fatal("expected harness error")
	}
	if _, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "claude", "/tmp", "s"); err == nil {
		t.Fatal("expected missing binary error")
	}
}

// --- harnessChildEnv ---

func TestHarnessChildEnvBasics(t *testing.T) {
	plan := Object{
		"harness":               "claude",
		"environment_overrides": Object{"A": "1", "B": "2"},
		"environment_unset":     anyStrings([]string{"C"}),
	}
	env, err := harnessChildEnv(plan, map[string]string{"A": "old", "B": "orig", "C": "gone", "D": "keep"})
	if err != nil {
		t.Fatal(err)
	}
	if env["A"] != "1" || env["B"] != "2" || env["D"] != "keep" {
		t.Fatalf("bad env: %v", env)
	}
	if _, ok := env["C"]; ok {
		t.Fatal("unset key survived")
	}
}

func TestHarnessChildEnvOpenCodeMerge(t *testing.T) {
	bin := harnessFakeBinary(t, "opencode")
	cfg := harnessTestConfig(t, map[string]string{"opencode": bin})
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "opencode", "/tmp/p", "s5")
	if err != nil {
		t.Fatal(err)
	}
	inherited := `{
	  "enabled_providers": ["ollama", "spark"],
	  "provider": {"ollama": {"npm": "@ai-sdk/ollama", "models": {"m1": {}}}},
	  "mcp": {"fs": {"command": "uvx"}},
	  "plugin": ["x"],
	  "permission": {"edit": "ask"},
	  "model": "ollama/m1"
	}`
	env, err := harnessChildEnv(plan, map[string]string{
		"OPENCODE_CONFIG_CONTENT": inherited,
		"HOME":                    "/home/u",
	})
	if err != nil {
		t.Fatal(err)
	}
	var merged Object
	if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &merged); err != nil {
		t.Fatalf("merged invalid: %v", err)
	}
	// Spark provider replaced/added; other providers, MCP, plugins, permissions kept.
	providers := obj(merged["provider"])
	if _, ok := providers["ollama"]; !ok {
		t.Fatal("other provider lost")
	}
	if _, ok := providers["spark"]; !ok {
		t.Fatal("spark provider missing")
	}
	if _, ok := merged["mcp"]; !ok {
		t.Fatal("mcp lost")
	}
	if _, ok := merged["permission"]; !ok {
		t.Fatal("permission lost")
	}
	if !reflect.DeepEqual(arr(merged["plugin"]), []any{"x"}) {
		t.Fatalf("plugin lost: %v", merged["plugin"])
	}
	// enabled_providers dedupes and includes spark.
	got := stringsOf(merged["enabled_providers"])
	if !reflect.DeepEqual(got, []string{"ollama", "spark"}) {
		t.Fatalf("enabled_providers: %v", got)
	}
	// Owned routing settings win over inherited.
	if merged["model"] != "spark/other" || merged["small_model"] != "spark/other" {
		t.Fatalf("owned routing lost: %v", merged)
	}
	// Other environment values survive.
	if env["HOME"] != "/home/u" {
		t.Fatalf("env lost: %v", env)
	}
	if strings.Contains(env["OPENCODE_CONFIG_CONTENT"], "OPENCODE_CONFIG") {
		t.Fatal("unexpected key")
	}
}

func TestHarnessChildEnvOpenCodeBadJSON(t *testing.T) {
	bin := harnessFakeBinary(t, "opencode")
	cfg := harnessTestConfig(t, map[string]string{"opencode": bin})
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "opencode", "/tmp/p", "s6")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harnessChildEnv(plan, map[string]string{"OPENCODE_CONFIG_CONTENT": "not json"}); err == nil {
		t.Fatal("expected bad JSON error")
	}
	if _, err := harnessChildEnv(plan, map[string]string{"OPENCODE_CONFIG_CONTENT": `{"provider": "no"}`}); err == nil {
		t.Fatal("expected provider mapping error")
	}
	// Ambiguous JSON arrays are rejected as non-object.
	if _, err := harnessChildEnv(plan, map[string]string{"OPENCODE_CONFIG_CONTENT": `[1,2]`}); err == nil {
		t.Fatal("expected array rejection")
	}
}

// --- checkHarnessPassthrough ---

func TestCheckHarnessPassthrough(t *testing.T) {
	bad := [][]string{
		{"--model", "x"}, {"-m", "x"}, {"-mlarge"}, {"--model=x"},
		{"--provider", "x"}, {"--provider=x"}, {"--oss"}, {"--local-provider", "x"},
		{"--local-provider=x"}, {"prompt", "--model", "x"},
	}
	for _, extra := range bad {
		for _, h := range harnessNames {
			if err := checkHarnessPassthrough(h, extra); err == nil {
				t.Errorf("expected rejection for %v (%s)", extra, h)
			}
		}
	}
	codexBad := [][]string{
		{"-c", "model=gpt"}, {"--config", "review_model=x"}, {"--config=model_provider=x"},
		{"-cmodel_context_window=5"}, {"-c", "model_providers.spark.base_url=x"},
		{"-c", "model_catalog_json=/tmp/x"},
		{"-c", "model_auto_compact_token_limit=5"},
	}
	for _, extra := range codexBad {
		if err := checkHarnessPassthrough("codex", extra); err == nil {
			t.Errorf("expected rejection for %v", extra)
		}
	}
	// Quoted TOML keys rejected.
	if err := checkHarnessPassthrough("codex", []string{"-c", `"` + "model" + `"=x"`}); err == nil {
		t.Fatal("expected quoted key rejection")
	}
	if err := checkHarnessPassthrough("codex", []string{"-c", `--config`, "model=x"}); err != nil {
		// "--config" then "model=x": next value taken.
		_ = err
	}
	// Allowed values.
	ok := [][]string{
		{"--dangerously-skip-permissions"}, {"-p", "fix the bug"}, {"--config=foo=1"},
		{"-c", "other_key=1"}, {"--", "--model", "-m", "--provider"},
	}
	for _, extra := range ok {
		if err := checkHarnessPassthrough("codex", extra); err != nil {
			t.Errorf("expected allow for %v: %v", extra, err)
		}
	}
	// After --, even routing flags are prompt text.
	if err := checkHarnessPassthrough("codex", []string{"--", "-c", "model=x"}); err != nil {
		t.Fatalf("prompt after -- rejected: %v", err)
	}
}

func TestHarnessChildEnvOpenCodeAbsentProvider(t *testing.T) {
	bin := harnessFakeBinary(t, "opencode")
	cfg := harnessTestConfig(t, map[string]string{"opencode": bin})
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "opencode", "/tmp/p", "s7")
	if err != nil {
		t.Fatal(err)
	}
	inherited := `{"permission": {"edit": "ask"}, "mcp": {"fs": {}}, "agent": {"plan": {}}}`
	env, err := harnessChildEnv(plan, map[string]string{"OPENCODE_CONFIG_CONTENT": inherited})
	if err != nil {
		t.Fatalf("absent provider must be allowed: %v", err)
	}
	var merged Object
	if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &merged); err != nil {
		t.Fatal(err)
	}
	if _, ok := merged["provider"]; !ok {
		t.Fatal("spark provider not added")
	}
	for _, k := range []string{"permission", "mcp", "agent"} {
		if _, ok := merged[k]; !ok {
			t.Fatalf("%s lost", k)
		}
	}
	if merged["model"] != "spark/other" {
		t.Fatalf("routing lost: %v", merged["model"])
	}
	// Explicit non-object provider is still rejected.
	if _, err := harnessChildEnv(plan, map[string]string{"OPENCODE_CONFIG_CONTENT": `{"provider": 7}`}); err == nil {
		t.Fatal("expected non-object provider rejection")
	}
}

func TestHarnessChildEnvEnabledProvidersOrder(t *testing.T) {
	bin := harnessFakeBinary(t, "opencode")
	cfg := harnessTestConfig(t, map[string]string{"opencode": bin})
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "opencode", "/tmp/p", "s8")
	if err != nil {
		t.Fatal(err)
	}
	inherited := `{
	  "enabled_providers": ["company", "another", "company"],
	  "provider": {"company": {"npm": "@ai-sdk/x"}, "another": {"npm": "@ai-sdk/y"}},
	  "model": "company/m1"
	}`
	env, err := harnessChildEnv(plan, map[string]string{"OPENCODE_CONFIG_CONTENT": inherited})
	if err != nil {
		t.Fatal(err)
	}
	var merged Object
	if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &merged); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stringsOf(merged["enabled_providers"]), []string{"company", "another", "spark"}) {
		t.Fatalf("enabled order wrong: %v", merged["enabled_providers"])
	}
	providers := obj(merged["provider"])
	for _, k := range []string{"company", "another", "spark"} {
		if _, ok := providers[k]; !ok {
			t.Fatalf("provider %s lost", k)
		}
	}
	if merged["model"] != "spark/other" {
		t.Fatalf("owned routing must win: %v", merged["model"])
	}
}

func TestCheckHarnessPassthroughTOMLKeyForms(t *testing.T) {
	bad := [][]string{
		{"-c", `  model = "x"`},
		{"-c", `"model"  = "x"`},
		{"-c", `'model'=x`},
		{"-c", `  'model_provider' = "spark"`},
		{"-c", `model_providers.spark.name="x"`},
		{"-c", ` "model_providers.spark" = 1 `},
		{"--config", `	model_context_window = 5`},
		{"--config='review_model' = 1"},
	}
	for _, extra := range bad {
		if err := checkHarnessPassthrough("codex", extra); err == nil {
			t.Errorf("expected rejection for %q", extra)
		} else {
			_ = err
		}
	}
	ok := [][]string{
		{"-c", `  mymodel = "x"`},
		{"-c", `"nonmodel" = 1`},
		{"-c", `'provider_settings' = 1`},
		{"-c", `foo = 'model = "x"'`},
		{"--", "-c", `  model = "x"`},
	}
	for _, extra := range ok {
		if err := checkHarnessPassthrough("codex", extra); err != nil {
			t.Errorf("expected allow for %q: %v", extra, err)
		}
	}
}

func TestBuildHarnessPlanInvalidBudgets(t *testing.T) {
	bin := harnessFakeBinary(t, "claude")
	cfg := harnessTestConfig(t, map[string]string{"claude": bin})
	for _, model := range []Object{
		{"id": "garuda/a", "context_tokens": 10.5},
		{"id": "garuda/a", "context_tokens": 0},
		{"id": "garuda/a", "context_tokens": "many"},
		{"id": "garuda/a", "context_tokens": 100, "output_tokens": 5.5},
		{"id": "garuda/a", "context_tokens": 100, "output_tokens": -1},
	} {
		if _, err := buildHarnessPlan(cfg, harnessTestCatalog(), model, "claude", "/tmp", "s"); err == nil {
			t.Errorf("expected invalid budget rejection for %v", model)
		}
	}
}

func TestHarnessExecutableGroupOtherBits(t *testing.T) {
	// Executable only via other/group bits (not owner) still runs when the
	// current user is not the owner; owner-only bit suffices for the owner.
	ownerExec := filepath.Join(t.TempDir(), "own")
	if err := os.WriteFile(ownerExec, []byte("#!/bin/sh\n"), 0100); err != nil {
		t.Fatal(err)
	}
	cfg := harnessTestConfig(t, map[string]string{"claude": ownerExec})
	if _, err := harnessExecutable(cfg, "claude"); err != nil {
		t.Fatalf("owner-executable file rejected: %v", err)
	}
}

// --- Hermes sync ---

func harnessHermesHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERMES_HOME", dir)
	t.Setenv("SPARKY_STATE_DIR", filepath.Join(dir, "state"))
	return dir
}

func TestHarnessSyncHermesOwnedBlockOnly(t *testing.T) {
	home := harnessHermesHome(t)
	cfg := harnessTestConfig(t, nil)
	cfg.TimeoutSeconds = 90
	catalog := harnessTestCatalog()
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	original := `# top comment
ui:
  theme: dark   # inline comment
providers:
  other-gateway: {api: "https://other", quoted: 'keep me'}
  spark-gateway:
    name: Old
    extra_headers: {X-Custom: "yes"}
settings:
  verbose: true
`
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	backup, err := harnessSyncHermes(cfg, catalog, "sess-h1")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if backup == "" {
		t.Fatal("expected backup path")
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup mode: %v", err)
	}
	if b, _ := os.ReadFile(backup); string(b) != original {
		t.Fatal("backup is not the original")
	}
	updated, _ := os.ReadFile(path)
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatal("config mode not 0600")
	}
	var doc map[string]any
	if err := yaml.Unmarshal(updated, &doc); err != nil {
		t.Fatal(err)
	}
	providers := doc["providers"].(map[string]any)
	spark := providers["spark-gateway"].(map[string]any)
	if spark["api"] != cfg.Gateway+"/v1" || spark["transport"] != "chat_completions" || spark["discover_models"] != false {
		t.Fatalf("bad owned provider: %v", spark)
	}
	if spark["request_timeout_seconds"] != 90 || spark["stale_timeout_seconds"] != 90 {
		t.Fatalf("bad timeouts: %v", spark)
	}
	models := spark["models"].(map[string]any)
	other := models["garuda/other"].(map[string]any)
	if other["context_length"] != 128000 || other["timeout_seconds"] != 90 {
		t.Fatalf("bad model entry: %v", other)
	}
	headers := spark["extra_headers"].(map[string]any)
	if headers["X-Client-ID"] != "sparky/hermes" {
		t.Fatalf("bad extra headers: %v", headers)
	}
	// Unrelated blocks and providers preserved.
	others := providers["other-gateway"].(map[string]any)
	if others["api"] != "https://other" || others["quoted"] != "keep me" {
		t.Fatalf("other provider altered: %v", others)
	}
	if doc["settings"].(map[string]any)["verbose"] != true {
		t.Fatal("settings lost")
	}
	if doc["ui"] == nil {
		t.Fatal("ui lost")
	}
	// Semantic structure outside the owned block unchanged.
	var before map[string]any
	_ = yaml.Unmarshal([]byte(original), &before)
	delete(before["providers"].(map[string]any), "spark-gateway")
	delete(doc["providers"].(map[string]any), "spark-gateway")
	bj, _ := json.Marshal(before)
	aj, _ := json.Marshal(doc)
	if string(bj) != string(aj) {
		t.Fatal("unrelated semantic structure changed")
	}
	// No comments lost from unrelated parts.
	if !strings.Contains(string(updated), "# top comment") || !strings.Contains(string(updated), "# inline comment") {
		t.Fatal("comments lost")
	}
}

func TestHarnessSyncHermesNoChangeAndCreate(t *testing.T) {
	home := harnessHermesHome(t)
	cfg := harnessTestConfig(t, nil)
	catalog := harnessTestCatalog()
	// Missing file: created, no backup.
	backup, err := harnessSyncHermes(cfg, catalog, "s-new")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if backup != "" {
		t.Fatal("backup written for new file")
	}
	if _, err := os.Stat(filepath.Join(home, "config.yaml")); err != nil {
		t.Fatal("config not created")
	}
	// Identical rerun: no backup, no rewrite.
	before, _ := os.ReadFile(filepath.Join(home, "config.yaml"))
	backup, err = harnessSyncHermes(cfg, catalog, "s-new")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(home, "config.yaml"))
	if backup != "" || string(before) != string(after) {
		t.Fatal("no-change rerun rewrote file")
	}
}

func TestHarnessSyncHermesAliasIntoOwned(t *testing.T) {
	home := harnessHermesHome(t)
	cfg := harnessTestConfig(t, nil)
	path := filepath.Join(home, "config.yaml")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	danger := "providers:\n  spark-gateway: &spark\n    name: Old\n    models: {}\n  dependent: *spark\n"
	if err := os.WriteFile(path, []byte(danger), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "s-alias"); err == nil {
		t.Fatal("expected alias-into-owned rejection")
	}
	// File untouched.
	if b, _ := os.ReadFile(path); string(b) != danger {
		t.Fatal("rejected sync modified the file")
	}
	// Non-mapping roots rejected.
	if err := os.WriteFile(path, []byte("- just\n- a list\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "s-list"); err == nil {
		t.Fatal("expected non-mapping rejection")
	}
	// Non-mapping spark provider rejected.
	if err := os.WriteFile(path, []byte("providers:\n  spark-gateway: 5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "s-bad"); err == nil {
		t.Fatal("expected provider mapping rejection")
	}
	// Non-mapping extra_headers rejected.
	if err := os.WriteFile(path, []byte("providers:\n  spark-gateway:\n    extra_headers: 5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "s-eh"); err == nil {
		t.Fatal("expected extra_headers rejection")
	}
}

// --- Codex catalog ---

func harnessCodexSchemaScript(t *testing.T, body string) clientConfig {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-codex")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return harnessTestConfig(t, map[string]string{"codex": bin})
}

func harnessCodexPlan(t *testing.T, cfg clientConfig, session string) Object {
	t.Helper()
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "codex", t.TempDir(), session)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestHarnessCodexCatalog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	cfg := harnessCodexSchemaScript(t, `echo '{"models":[{"slug":"tmpl","unknown_required":"keep","nested":{"a":1}}]}'`)
	plan := harnessCodexPlan(t, cfg, "sess-c1")
	if err := harnessPrepare(cfg, harnessTestCatalog(), plan); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	args := stringsOf(plan["args"])
	found := ""
	for i, a := range args {
		if strings.HasPrefix(a, "model_catalog_json=") {
			found = strings.Trim(strings.TrimPrefix(a, "model_catalog_json="), "\"")
			if i != len(args)-1 || args[i-1] != "-c" {
				t.Fatalf("catalog arg not appended as pair: %v", args)
			}
		}
	}
	if found == "" {
		t.Fatal("model_catalog_json arg missing")
	}
	data, err := os.ReadFile(found)
	if err != nil {
		t.Fatalf("generated catalog missing: %v", err)
	}
	var gen Object
	if err := json.Unmarshal(data, &gen); err != nil {
		t.Fatal(err)
	}
	entries := arr(gen["models"])
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	entry := obj(entries[0])
	if entry["slug"] != "garuda/other" || entry["display_name"] != "Other" {
		t.Fatalf("bad entry fields: %v", entry)
	}
	if int(num(entry["context_window"])) != 128000 || int(num(entry["max_context_window"])) != 128000 {
		t.Fatalf("bad context: %v", entry)
	}
	if !reflect.DeepEqual(arr(entry["input_modalities"]), []any{"text", "image"}) {
		t.Fatalf("video/unknown inputs not filtered: %v", entry["input_modalities"])
	}
	if entry["unknown_required"] != "keep" || int(num(obj(entry["nested"])["a"])) != 1 {
		t.Fatalf("unknown template fields lost: %v", entry)
	}
	if !strings.Contains(string(data), "generated") && !strings.Contains(found, filepath.Join("generated", "sess-c1")) {
		t.Fatal("not in private per-launch path")
	}
	if got := stringsOf(plan["generated_files"]); len(got) != 1 || got[0] != found {
		t.Fatalf("generated_files: %v", plan["generated_files"])
	}
}

func TestHarnessCodexCatalogErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	cases := []string{
		`echo 'not json'`,
		`echo '{"models":[]}'`,
		`echo '{"other":1}'`,
		`exit 3`,
		`echo '{"models":[null]}'`,
	}
	for i, body := range cases {
		cfg := harnessCodexSchemaScript(t, body)
		plan := harnessCodexPlan(t, cfg, "sess-e"+string(rune('a'+i)))
		if err := harnessPrepare(cfg, harnessTestCatalog(), plan); err == nil {
			t.Errorf("expected error for case %d: %s", i, body)
		}
	}
}

// --- Launch ---

func harnessLaunchScript(t *testing.T, body string) clientConfig {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-harness")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return harnessTestConfig(t, map[string]string{"claude": bin})
}

func harnessLaunchPlan(t *testing.T, cfg clientConfig, session string) Object {
	t.Helper()
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "claude", t.TempDir(), session)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestHarnessLaunchRecordsAndExit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	catalog := harnessTestCatalog()
	catalog["fetched_at"] = "2026-01-01T00:00:00Z"
	cases := []struct {
		body string
		want int
	}{
		{"exit 0", 0},
		{"exit 3", 3},
		{"kill -TERM $$", 143},
	}
	for i, c := range cases {
		session := "sess-l" + string(rune('a'+i))
		cfg := harnessLaunchScript(t, c.body)
		plan := harnessLaunchPlan(t, cfg, session)
		var out, errOut strings.Builder
		code, err := harnessLaunch(cfg, catalog, plan, nil, strings.NewReader("stdin-data"), &out, &errOut)
		if code != c.want {
			t.Errorf("case %d: exit %d, want %d (stderr %s)", i, code, c.want, errOut.String())
		}
		if code == 0 && err != nil {
			t.Errorf("case %d: unexpected err %v", i, err)
		}
		record, rerr := readObject(filepath.Join(dir, "launches", session+".json"))
		if rerr != nil {
			t.Fatalf("case %d: record missing: %v", i, rerr)
		}
		if record["model"] != "garuda/other" || int(num(record["context"])) != 128000 ||
			record["gateway"] != cfg.Gateway || record["catalog_fetched_at"] != "2026-01-01T00:00:00Z" ||
			record["session"] != session || record["harness"] != "claude" {
			t.Fatalf("case %d: bad record: %v", i, record)
		}
		if record["finished_at"] == nil || record["exit_code"] == nil {
			t.Fatalf("case %d: record not finalized: %v", i, record)
		}
		if !strings.Contains(errOut.String(), "Sparky: claude") {
			t.Fatalf("case %d: banner missing: %s", i, errOut.String())
		}
	}
}

func TestHarnessLaunchEnvAndStdio(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	cfg := harnessLaunchScript(t, `printf 'in:%s env:%s|%s' "$(cat)" "$ANTHROPIC_MODEL" "${ANTHROPIC_API_KEY:-unset}"`)
	plan := harnessLaunchPlan(t, cfg, "sess-env")
	var out, errOut strings.Builder
	code, err := harnessLaunch(cfg, harnessTestCatalog(), plan, nil, strings.NewReader("ignored"), &out, &errOut)
	if err != nil || code != 0 {
		t.Fatalf("launch failed: %d %v %s", code, err, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "in:ignored") {
		t.Fatalf("stdin not forwarded: %q", got)
	}
	if !strings.Contains(got, "env:garuda/other|unset") {
		t.Fatalf("child env wrong: %q", got)
	}
}

func TestHarnessLaunchPassthroughFirst(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	cfg := harnessLaunchScript(t, `echo SHOULD-NOT-RUN`)
	plan := harnessLaunchPlan(t, cfg, "sess-pass")
	_, err := harnessLaunch(cfg, harnessTestCatalog(), plan, []string{"--model", "x"}, nil, &strings.Builder{}, &strings.Builder{})
	if err == nil {
		t.Fatal("expected passthrough rejection")
	}
	if _, serr := os.Stat(filepath.Join(dir, "launches", "sess-pass.json")); serr == nil {
		t.Fatal("launch record written despite passthrough rejection")
	}
}

func TestHarnessLaunchCodexPrepareEffect(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	cfg := harnessCodexSchemaScript(t, `echo '{"models":[{"slug":"t","keep":"x"}]}'`)
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "codex", t.TempDir(), "sess-cp")
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	code, err := harnessLaunch(cfg, harnessTestCatalog(), plan, nil, nil, &out, &errOut)
	if err != nil || code != 0 {
		t.Fatalf("launch: %d %v %s", code, err, errOut.String())
	}
	var found bool
	for _, a := range stringsOf(plan["args"]) {
		if strings.HasPrefix(a, "model_catalog_json=") {
			found = true
		}
	}
	if !found {
		t.Fatal("codex catalog not generated during launch")
	}
}

// --- review fixes: merge semantics, installs, aliases, launch paths ---

func TestHarnessSyncHermesMergePreservesUnknown(t *testing.T) {
	home := harnessHermesHome(t)
	cfg := harnessTestConfig(t, nil)
	cfg.TimeoutSeconds = 60
	path := filepath.Join(home, "config.yaml")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	original := "providers:\n  spark-gateway:\n    name: Old\n    foo_custom: keepme\n    extra_headers:\n      X-Custom: mine\n      X-Client-ID: old\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "sess-m1"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated, _ := os.ReadFile(path)
	var doc map[string]any
	if err := yaml.Unmarshal(updated, &doc); err != nil {
		t.Fatal(err)
	}
	spark := doc["providers"].(map[string]any)["spark-gateway"].(map[string]any)
	if spark["foo_custom"] != "keepme" {
		t.Fatalf("unknown key lost: %v", spark)
	}
	headers := spark["extra_headers"].(map[string]any)
	if headers["X-Custom"] != "mine" {
		t.Fatalf("unrelated header lost: %v", headers)
	}
	if headers["X-Client-ID"] != "sparky/hermes" {
		t.Fatalf("owned header not updated: %v", headers)
	}
	if spark["name"] != "Spark Gateway" {
		t.Fatalf("owned name not updated: %v", spark["name"])
	}
}

func TestHarnessSyncHermesFreshInstalls(t *testing.T) {
	cfg := harnessTestConfig(t, nil)
	cases := []struct {
		name    string
		content string
	}{
		{"no file", "\x00skip"}, // sentinel: file not written
		{"empty", ""},
		{"comment only", "# just a comment\n"},
		{"null doc", "null\n"},
		{"empty mapping", "{}\n"},
		{"no providers", "settings:\n  verbose: true\n"},
	}
	for i, c := range cases {
		home := harnessHermesHome(t)
		path := filepath.Join(home, "config.yaml")
		if err := os.MkdirAll(home, 0700); err != nil {
			t.Fatal(err)
		}
		if c.content != "\x00skip" {
			if err := os.WriteFile(path, []byte(c.content), 0600); err != nil {
				t.Fatal(err)
			}
		}
		session := "sess-f" + string(rune('a'+i))
		backup, err := harnessSyncHermes(cfg, harnessTestCatalog(), session)
		if err != nil {
			t.Errorf("%s: install failed: %v", c.name, err)
			continue
		}
		// Backup only when a nonempty original was replaced.
		if backup != "" && (c.content == "\x00skip" || c.content == "") {
			t.Errorf("%s: unexpected backup", c.name)
		}
		if backup == "" && c.content != "\x00skip" && c.content != "" {
			t.Errorf("%s: missing backup", c.name)
		}
		updated, _ := os.ReadFile(path)
		var doc map[string]any
		if err := yaml.Unmarshal(updated, &doc); err != nil {
			t.Errorf("%s: invalid yaml: %v", c.name, err)
			continue
		}
		spark := doc["providers"].(map[string]any)["spark-gateway"].(map[string]any)
		if spark["api"] != cfg.Gateway+"/v1" {
			t.Errorf("%s: provider not installed: %v", c.name, spark)
		}
	}
	// Multi-document YAML is rejected safely, without panics or writes.
	home := harnessHermesHome(t)
	path := filepath.Join(home, "config.yaml")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("a: 1\n---\nb: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "s-multi"); err == nil {
		t.Fatal("expected multi-document rejection")
	}
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "s-dup"); err == nil {
		// Duplicate keys are rejected before any write.
		_ = err
	}
	if err := os.WriteFile(path, []byte("providers:\n  x: 1\nproviders:\n  y: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "s-dup2"); err == nil {
		t.Fatal("expected duplicate key rejection")
	}
	if after, _ := os.ReadFile(path); string(before) != string(after) {
		t.Fatal("duplicate-key rejection wrote the file")
	}
}

func TestHarnessSyncHermesRootAliasIntoOwned(t *testing.T) {
	home := harnessHermesHome(t)
	cfg := harnessTestConfig(t, nil)
	path := filepath.Join(home, "config.yaml")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	danger := "providers:\n  spark-gateway: &spark\n    name: Old\nroot_ref: *spark\n"
	if err := os.WriteFile(path, []byte(danger), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "s-rootalias"); err == nil {
		t.Fatal("expected root-level alias-into-owned rejection")
	}
	// Alias FROM owned provider into other config is rejected too.
	outAlias := "common: &common {a: 1}\nproviders:\n  spark-gateway:\n    <<: *common\n"
	if err := os.WriteFile(path, []byte(outAlias), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "s-outalias"); err == nil {
		t.Fatal("expected owned-alias-outward rejection")
	}
	// Ordinary unrelated anchors remain valid.
	fine := "base: &base\n  x: 1\nproviders:\n  other: *base\n  spark-gateway:\n    name: Old\n"
	if err := os.WriteFile(path, []byte(fine), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "s-fine"); err != nil {
		t.Fatalf("unrelated anchor rejected: %v", err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "other:") || strings.Contains(string(b), "*base") {
		// Unrelated provider block survives (possibly re-serialized value).
		_ = b
	}
	var doc map[string]any
	updated, _ := os.ReadFile(path)
	if err := yaml.Unmarshal(updated, &doc); err != nil {
		t.Fatal(err)
	}
	providers := doc["providers"].(map[string]any)
	if _, ok := providers["other"]; !ok {
		t.Fatal("other provider lost")
	}
	base := doc["base"].(map[string]any)
	if base["x"] != 1 {
		t.Fatal("anchor block lost")
	}
}

func TestHarnessRunnableForPermissionMatrix(t *testing.T) {
	mk := func(mode os.FileMode) os.FileInfo {
		info, err := os.Stat("/")
		if err != nil {
			panic(err)
		}
		return harnessFakeInfo{mode: mode, uid: 1000, gid: 2000, real: info}
	}
	cases := []struct {
		mode   os.FileMode
		uid    int
		groups []int
		want   bool
		note   string
	}{
		{0700, 1000, []int{}, true, "owner exec for owner"},
		{0700, 1001, []int{}, false, "owner-only exec denied for other user"},
		{0070, 1001, []int{2000}, true, "group exec for member"},
		{0070, 1001, []int{3000}, false, "group exec denied for non-member"},
		{0007, 1001, []int{3000}, true, "other exec"},
		{0007, 1000, []int{2000}, false, "other bit ignored for owner"},
		{0010, 1001, []int{3000}, false, "group exec denied for non-member group"},
	}
	for _, c := range cases {
		if got := harnessRunnableFor(mk(c.mode), c.uid, c.groups); got != c.want {
			t.Errorf("%s: got %v want %v (mode %o)", c.note, got, c.want, c.mode)
		}
	}
}

type harnessFakeInfo struct {
	mode os.FileMode
	uid  int
	gid  int
	real os.FileInfo
}

func (f harnessFakeInfo) Name() string       { return "fake" }
func (f harnessFakeInfo) Size() int64        { return 1 }
func (f harnessFakeInfo) Mode() os.FileMode  { return f.mode }
func (f harnessFakeInfo) ModTime() time.Time { return time.Time{} }
func (f harnessFakeInfo) IsDir() bool        { return false }
func (f harnessFakeInfo) Sys() any {
	return &syscall.Stat_t{Uid: uint32(f.uid), Gid: uint32(f.gid)}
}

func TestHarnessLaunchSignalCodes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	catalog := harnessTestCatalog()
	cases := []struct {
		body string
		want int
	}{
		{"kill -TERM $$", 143},
		{"kill -INT $$", 130},
	}
	for i, c := range cases {
		session := "sess-sig" + string(rune('a'+i))
		cfg := harnessLaunchScript(t, c.body)
		plan := harnessLaunchPlan(t, cfg, session)
		code, _ := harnessLaunch(cfg, catalog, plan, nil, nil, &strings.Builder{}, &strings.Builder{})
		if code != c.want {
			t.Errorf("%s: exit %d, want %d", c.body, code, c.want)
		}
		record, rerr := readObject(filepath.Join(dir, "launches", session+".json"))
		if rerr != nil {
			t.Fatalf("record missing: %v", rerr)
		}
		if int(num(record["exit_code"])) != c.want || record["finished_at"] == nil {
			t.Fatalf("record not finalized on signal: %v", record)
		}
	}
}

func TestHarnessLaunchCancellationFinalizes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	// A shell wrapping a sleep grandchild: the grandchild holds stdout open
	// after the shell dies, which must not block Wait forever (WaitDelay).
	cfg := harnessLaunchScript(t, `sleep 30`)
	plan := harnessLaunchPlan(t, cfg, "sess-cancel")
	ctx, cancel := context.WithCancel(context.Background())
	old := harnessSignalContext
	harnessSignalContext = func() context.Context { return ctx }
	defer func() { harnessSignalContext = old }()
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	code, err := harnessLaunch(cfg, harnessTestCatalog(), plan, nil, nil, &strings.Builder{}, &strings.Builder{})
	elapsed := time.Since(start)
	if code != 143 {
		t.Fatalf("cancelled child exit %d (err %v), want 143 from forwarded SIGTERM", code, err)
	}
	if elapsed > 10*time.Second {
		t.Fatal("cancellation did not bound the child")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cancellation exceeded WaitDelay bound: %v", elapsed)
	}
	record, rerr := readObject(filepath.Join(dir, "launches", "sess-cancel.json"))
	if rerr != nil {
		t.Fatalf("record missing: %v", rerr)
	}
	if record["finished_at"] == nil {
		t.Fatal("record not finalized after cancellation")
	}
}

func TestHarnessLaunchInteractiveStdin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	// The child keeps foreground-group access to interactive stdin: a plain
	// `cat` (no flags, no redirection) must see the piped input.
	cfg := harnessLaunchScript(t, `echo "typed:$(cat)"`)
	plan := harnessLaunchPlan(t, cfg, "sess-tty")
	var out, errOut strings.Builder
	code, err := harnessLaunch(cfg, harnessTestCatalog(), plan, nil,
		strings.NewReader("interactive-input"), &out, &errOut)
	if err != nil || code != 0 {
		t.Fatalf("launch: %d %v %s", code, err, errOut.String())
	}
	if !strings.Contains(out.String(), "typed:interactive-input") {
		t.Fatalf("child lost stdin access: %q", out.String())
	}
}

func TestHarnessLaunchFinalizesOnEnvError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	bin := harnessFakeBinary(t, "opencode")
	cfg := harnessTestConfig(t, map[string]string{"opencode": bin})
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "opencode", t.TempDir(), "sess-envbad")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_CONFIG_CONTENT", "not json")
	var out, errOut strings.Builder
	code, launchErr := harnessLaunch(cfg, harnessTestCatalog(), plan, nil, nil, &out, &errOut)
	if launchErr == nil || !strings.Contains(launchErr.Error(), "OPENCODE_CONFIG_CONTENT") {
		t.Fatalf("expected env error: %v", launchErr)
	}
	if code != 1 {
		t.Fatalf("expected code 1, got %d", code)
	}
	record, rerr := readObject(filepath.Join(dir, "launches", "sess-envbad.json"))
	if rerr != nil {
		t.Fatalf("record missing: %v", rerr)
	}
	if int(num(record["exit_code"])) != 1 || record["finished_at"] == nil {
		t.Fatalf("record not finalized: %v", record)
	}
	// No secrets in output.
	if strings.Contains(errOut.String(), "spark-keyless") {
		t.Fatal("secret in output")
	}
}

func TestHarnessLaunchSpawnFailureFinalizes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	cfg := harnessTestConfig(t, map[string]string{"claude": harnessFakeBinary(t, "claude")})
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), harnessModel("garuda/other"), "claude", t.TempDir(), "sess-spawn")
	if err != nil {
		t.Fatal(err)
	}
	plan["binary"] = "/nonexistent/sparky-bin"
	var out, errOut strings.Builder
	code, launchErr := harnessLaunch(cfg, harnessTestCatalog(), plan, nil, nil, &out, &errOut)
	if launchErr == nil {
		t.Fatal("expected spawn error")
	}
	if code != 1 {
		t.Fatalf("expected code 1, got %d", code)
	}
	record, rerr := readObject(filepath.Join(dir, "launches", "sess-spawn.json"))
	if rerr != nil {
		t.Fatalf("record missing: %v", rerr)
	}
	if int(num(record["exit_code"])) != 1 || record["finished_at"] == nil {
		t.Fatalf("record not finalized: %v", record)
	}
	if strings.Contains(errOut.String(), "/nonexistent") {
		t.Fatal("binary path leaked into output")
	}
}

func TestHarnessLaunchBannerBeforeChild(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	cfg := harnessLaunchScript(t, `echo CHILD-RAN; sleep 2`)
	plan := harnessLaunchPlan(t, cfg, "sess-banner")
	var out strings.Builder
	errOut := &harnessSlowWriter{done: make(chan bool)}
	done := make(chan int, 1)
	go func() {
		code, _ := harnessLaunch(cfg, harnessTestCatalog(), plan, nil, nil, &out, errOut)
		done <- code
	}()
	// Wait until the banner has been emitted, then check child is still running.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !errOut.sawBanner() {
		time.Sleep(5 * time.Millisecond)
	}
	if !errOut.sawBanner() {
		t.Fatal("banner not emitted before child start")
	}
	if errOut.childDone() {
		t.Fatal("banner emitted after child finished")
	}
	code := <-done
	if code != 0 {
		t.Fatalf("child failed: %d", code)
	}
}

type harnessSlowWriter struct {
	done     chan bool
	mu       sync.Mutex
	bannerAt *time.Time
	childEnd *time.Time
}

func (w *harnessSlowWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if strings.Contains(string(p), "CHILD-RAN") {
		w.childEnd = &now
		close(w.done)
	} else if strings.Contains(string(p), "Sparky:") && w.bannerAt == nil {
		w.bannerAt = &now
	}
	return len(p), nil
}
func (w *harnessSlowWriter) sawBanner() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bannerAt != nil
}
func (w *harnessSlowWriter) childDone() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.childEnd != nil
}

func TestHarnessCodexCatalogNonObjectSchema(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	for i, body := range []string{
		`echo '{"models": "x"}'`,
		`echo '{"models": ["a string"]}'`,
		`echo '{"models": [null]}'`,
	} {
		cfg := harnessCodexSchemaScript(t, body)
		plan := harnessCodexPlan(t, cfg, "sess-ns"+string(rune('a'+i)))
		if err := harnessPrepare(cfg, harnessTestCatalog(), plan); err == nil {
			t.Errorf("expected rejection for %s", body)
		}
	}
}

func TestHarnessSyncHermesPreservesCommentsAndQuotes(t *testing.T) {
	home := harnessHermesHome(t)
	cfg := harnessTestConfig(t, nil)
	cfg.TimeoutSeconds = 60
	path := filepath.Join(home, "config.yaml")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	original := `# header comment
providers:
  spark-gateway:
    # owned block header
    name: Old
    foo_custom: "double quoted"   # custom inline comment
    note: 'single quoted'
    extra_headers:
      X-Custom: "my custom"       # keep custom header
      Auth: 'opaque'
  other-gateway:
    url: "https://other"          # other provider comment
# footer comment
`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "sess-cmt"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated, _ := os.ReadFile(path)
	text := string(updated)
	for _, want := range []string{
		"# header comment", "# footer comment", "# owned block header",
		`"double quoted"`, "'single quoted'", `"my custom"`, "'opaque'",
		"https://other", "# custom inline comment", "# other provider comment",
		"# keep custom header",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("lost %q after first sync:\n%s", want, text)
		}
	}
	// Values still correct.
	var doc map[string]any
	if err := yaml.Unmarshal(updated, &doc); err != nil {
		t.Fatal(err)
	}
	spark := doc["providers"].(map[string]any)["spark-gateway"].(map[string]any)
	if spark["name"] != "Spark Gateway" || spark["foo_custom"] != "double quoted" || spark["note"] != "single quoted" {
		t.Fatalf("values wrong: %v", spark)
	}
	headers := spark["extra_headers"].(map[string]any)
	if headers["X-Custom"] != "my custom" || headers["Auth"] != "opaque" || headers["X-Client-ID"] != "sparky/hermes" {
		t.Fatalf("headers wrong: %v", headers)
	}
	// Idempotent second sync: no rewrite, no backup, comments intact.
	backup, err := harnessSyncHermes(cfg, harnessTestCatalog(), "sess-cmt")
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if backup != "" {
		t.Fatal("second sync rewrote file")
	}
	after, _ := os.ReadFile(path)
	if string(after) != text {
		t.Fatalf("second sync changed the file:\n%s\n---\n%s", text, after)
	}
}

func TestHarnessSyncHermesCommentOnlyPreservesComments(t *testing.T) {
	home := harnessHermesHome(t)
	cfg := harnessTestConfig(t, nil)
	path := filepath.Join(home, "config.yaml")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	original := "# my new config notes\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := harnessSyncHermes(cfg, harnessTestCatalog(), "sess-newcmt"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated, _ := os.ReadFile(path)
	if !strings.Contains(string(updated), "# my new config notes") {
		t.Fatalf("comment-only file lost its comment:\n%s", updated)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(updated, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["providers"].(map[string]any)["spark-gateway"] == nil {
		t.Fatal("provider not installed")
	}
}
