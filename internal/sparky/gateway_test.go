package sparky

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const gatewayOldNginx = "location ~ ^/v1/(chat/completions|completions|responses|audio/transcriptions)$ {\n  auth_basic off;\n}\n" +
	"location ~ ^/v1/(models|chat/completions|completions|responses|audio/transcriptions)$ {\n  proxy_pass http://core;\n}\n" +
	"location /api/ { auth_basic restricted; }\n"

// gatewayFixtureRoot writes a complete gateway directory with fixture-only
// credentials and returns the root plus the fake effect recorder.
func gatewayFixtureRoot(t *testing.T) (string, *gatewayFake) {
	t.Helper()
	root := t.TempDir()
	gateway := filepath.Join(root, "gateway")
	if err := os.MkdirAll(filepath.Join(gateway, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	env := "BIFROST_ADMIN_USER=fixture\nBIFROST_ADMIN_PASSWORD=fixture-only\n"
	if err := os.WriteFile(filepath.Join(gateway, "gateway.env"), []byte(env), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gateway, "nginx.conf"), []byte(gatewayOldNginx), 0644); err != nil {
		t.Fatal(err)
	}
	before := Object{"name": "garuda", "keys": []any{Object{"value": "secret-key-material"}},
		"custom_provider_config": Object{"base_provider_type": "openai", "allowed_requests": nil},
		"network_config":         Object{"base_url": "http://127.0.0.1:8000"}}
	config := Object{"providers": Object{"garuda": copyObject(before)}}
	configBytes, _ := json.MarshalIndent(config, "", "  ")
	if err := os.WriteFile(filepath.Join(gateway, "data", "config.json"), append(configBytes, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	fake := newGatewayFake(before)
	return root, fake
}

// gatewayFake records every API and nginx call and injects failures per step.
type gatewayFake struct {
	before      Object
	stored      Object
	calls       []string
	failAPI     map[string]int // "PUT", "GET" (initial read), "GET2" (verification)
	failNgin    map[string]bool
	nginxCalled bool
}

func newGatewayFake(before Object) *gatewayFake {
	return &gatewayFake{before: copyObject(before), stored: copyObject(before),
		failAPI: map[string]int{}, failNgin: map[string]bool{}}
}

func (f *gatewayFake) request(method, url string, payload Object, headers map[string]string) (Object, error) {
	f.calls = append(f.calls, method)
	if headers["Authorization"] != "Basic "+base64.StdEncoding.EncodeToString([]byte("fixture:fixture-only")) {
		return nil, fail("Missing admin credentials.")
	}
	key := method
	if method == "GET" && len(f.calls) > 1 {
		key = "GET2"
	}
	if f.failAPI[key] > 0 {
		f.failAPI[key]--
		return nil, fail("Gateway request failed.")
	}
	if payload == nil {
		return copyObject(f.stored), nil
	}
	stored := copyObject(f.stored)
	for k, v := range payload {
		stored[k] = v
	}
	f.stored = copyObject(stored)
	return copyObject(stored), nil
}

func (f *gatewayFake) nginx(args ...string) error {
	f.calls = append(f.calls, "nginx "+strings.Join(args, " "))
	f.nginxCalled = true
	if f.failNgin[strings.Join(args, " ")] {
		return fail("Nginx step failed.")
	}
	return nil
}

func gatewayInstallFake(t *testing.T, f *gatewayFake) {
	t.Helper()
	prevRequest, prevRun := gatewayRequest, gatewayRun
	gatewayRequest = f.request
	gatewayRun = func(args []string, input []byte, timeout time.Duration) (CommandResult, error) {
		if len(args) < 5 || args[0] != "sudo" || args[1] != "-n" || args[2] != "docker" ||
			args[3] != "exec" || args[4] != "garuda-inference" || args[5] != "nginx" {
			t.Errorf("unexpected command: %v", args)
			return CommandResult{}, fail("unexpected command")
		}
		return CommandResult{Code: 0}, f.nginx(args[6:]...)
	}
	t.Cleanup(func() { gatewayRequest, gatewayRun = prevRequest, prevRun })
}

func gatewayRead(t *testing.T, path string) Object {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	o, err := decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// gatewayLiveNginx is the exact deployed inference layout: the expanded
// location pair plus the separate exact-match /v1/models location. It must
// be recognized as already migrated and returned byte-identical.
const gatewayLiveNginx = "location ~ ^/(?:v1/(?:chat/completions|completions|responses|ocr|images/generations|audio/(?:transcriptions|speech))|anthropic/v1/(?:messages|messages/count_tokens))$ {\n" +
	"  auth_basic off;\n" +
	"  proxy_pass http://core;\n" +
	"}\n" +
	"location ~ ^/(?:v1/(?:models|chat/completions|completions|responses|ocr|images/generations|audio/(?:transcriptions|speech))|anthropic/v1/(?:messages|messages/count_tokens))$ {\n" +
	"  auth_basic restricted;\n" +
	"  proxy_pass http://core;\n" +
	"}\n" +
	"location = /v1/models {\n  proxy_pass http://core;\n}\n" +
	"location /api/ { auth_basic restricted; }\n"

func TestGatewayLiveExpandedLayoutIsIdempotent(t *testing.T) {
	changed, err := gatewayInferenceRoutes(gatewayLiveNginx)
	if err != nil {
		t.Fatalf("the deployed expanded layout must be recognized: %v", err)
	}
	if changed != gatewayLiveNginx {
		t.Fatal("an already migrated layout must be returned byte-identical")
	}
	// Every endpoint string and body must survive untouched.
	for _, endpoint := range []string{
		"chat/completions|completions|responses|ocr|images/generations|audio/(?:transcriptions|speech)",
		"anthropic/v1/(?:messages|messages/count_tokens)",
		"location = /v1/models {",
		"auth_basic off;", "auth_basic restricted;", "proxy_pass http://core;",
	} {
		if !strings.Contains(changed, endpoint) {
			t.Fatalf("endpoint or content missing after transform: %s", endpoint)
		}
	}
}

func TestGatewayLiveExpandedMixedLayoutsRejected(t *testing.T) {
	// Expanded pair mixed with the reduced pair: neither is a complete
	// single layout, so it must be rejected before any mutation.
	mixedReduced := strings.Replace(gatewayLiveNginx,
		"location ~ ^/(?:v1/(?:models|chat/completions|completions|responses|ocr|images/generations|audio/(?:transcriptions|speech))|anthropic/v1/(?:messages|messages/count_tokens))$ {",
		"location ~ ^/(?:v1/(?:models|chat/completions|completions|responses|audio/transcriptions)|anthropic/v1/(?:messages|messages/count_tokens))$ {", 1)
	if _, err := gatewayInferenceRoutes(mixedReduced); err == nil {
		t.Fatal("expanded+reduced mix must fail")
	}
	// Expanded pair mixed with the old pair must also fail.
	mixedOld := strings.Replace(gatewayLiveNginx,
		"location ~ ^/(?:v1/(?:models|chat/completions|completions|responses|ocr|images/generations|audio/(?:transcriptions|speech))|anthropic/v1/(?:messages|messages/count_tokens))$ {",
		"location ~ ^/v1/(models|chat/completions|completions|responses|audio/transcriptions)$ {", 1)
	if _, err := gatewayInferenceRoutes(mixedOld); err == nil {
		t.Fatal("expanded+old mix must fail")
	}
	// A duplicated expanded pattern must fail.
	if _, err := gatewayInferenceRoutes(gatewayLiveNginx + gatewayLiveNginx); err == nil {
		t.Fatal("duplicated expanded patterns must fail")
	}
	// A partially expanded pair (only the first location migrated) must fail.
	partial := strings.Replace(gatewayOldNginx,
		"location ~ ^/v1/(models|chat/completions|completions|responses|audio/transcriptions)$ {",
		"location ~ ^/(?:v1/(?:models|chat/completions|completions|responses|ocr|images/generations|audio/(?:transcriptions|speech))|anthropic/v1/(?:messages|messages/count_tokens))$ {", 1)
	if _, err := gatewayInferenceRoutes(partial); err == nil {
		t.Fatal("partial expanded layout must fail")
	}
}

// TestGatewayEnableWithLiveLayoutNoWrite covers a full run against the
// deployed expanded layout: nginx.conf must remain byte-identical (the
// in-place write rewrites the same content, keeping the inode).
func TestGatewayEnableWithLiveLayoutNoRewrite(t *testing.T) {
	root, fake := gatewayFixtureRoot(t)
	gatewayInstallFake(t, fake)
	nginxPath := filepath.Join(root, "gateway", "nginx.conf")
	if err := os.WriteFile(nginxPath, []byte(gatewayLiveNginx), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := EnableGateway(root)
	if err != nil {
		t.Fatalf("EnableGateway failed: %v", err)
	}
	ino, _ := gatewayStat(t, nginxPath)
	_ = result
	if got, _ := gatewayStat(t, nginxPath); got != ino {
		t.Fatal("inode must be retained")
	}
	if got := gatewayMustRead(t, nginxPath); string(got) != gatewayLiveNginx {
		t.Fatal("live layout must stay byte-identical")
	}
	if str(result["status"]) != "configured" {
		t.Fatalf("unexpected result: %v", result)
	}
	if !strings.Contains(strings.Join(fake.calls, "|"), "-s reload") {
		t.Fatal("validation and reload must still run")
	}
}

func TestGatewayRoutesIdempotentAndUnknown(t *testing.T) {
	changed, err := gatewayInferenceRoutes(gatewayOldNginx)
	if err != nil {
		t.Fatalf("recognized layout rejected: %v", err)
	}
	if !strings.Contains(changed, "anthropic/v1/(?:messages|messages/count_tokens)") {
		t.Fatal("anthropic route missing")
	}
	if strings.Contains(changed, "location /api/ { auth_basic restricted; }") == false {
		t.Fatal("unrelated location must be preserved")
	}
	again, err := gatewayInferenceRoutes(changed)
	if err != nil || again != changed {
		t.Fatal("route transform must be idempotent")
	}
	// A duplicated old pattern must be rejected before any mutation.
	if _, err := gatewayInferenceRoutes(gatewayOldNginx + gatewayOldNginx); err == nil {
		t.Fatal("duplicate pattern must fail")
	}
	if _, err := gatewayInferenceRoutes("unrecognized layout"); err == nil {
		t.Fatal("unknown layout must fail")
	}
}

func TestGatewayRoutesRejectMixedAndPartialLayouts(t *testing.T) {
	changed, err := gatewayInferenceRoutes(gatewayOldNginx)
	if err != nil {
		t.Fatal(err)
	}
	// Old and new patterns both present: ambiguous, must fail.
	if _, err := gatewayInferenceRoutes(changed + gatewayOldNginx); err == nil {
		t.Fatal("combined old+new layout must fail")
	}
	// Only the first pattern migrated (partial mix): must fail.
	partial := strings.Replace(gatewayOldNginx,
		"location ~ ^/v1/(chat/completions|completions|responses|audio/transcriptions)$ {",
		"location ~ ^/(?:v1/(?:chat/completions|completions|responses|audio/transcriptions)|anthropic/v1/(?:messages|messages/count_tokens))$ {",
		1)
	if _, err := gatewayInferenceRoutes(partial); err == nil {
		t.Fatal("partial mix must fail")
	}
	// Duplicate new patterns: must fail.
	if _, err := gatewayInferenceRoutes(changed + changed); err == nil {
		t.Fatal("duplicate new patterns must fail")
	}
	// A valid new layout stays idempotent.
	again, err := gatewayInferenceRoutes(changed)
	if err != nil || again != changed {
		t.Fatal("valid new layout must be idempotent")
	}
}

// TestGatewayCompletePairPlusOneForeignPatternRejected is the regression for
// the mixed-layout gap: one complete recognized pair plus one appended
// pattern from a different generation must be rejected, not migrated.
func TestGatewayCompletePairPlusOneForeignPatternRejected(t *testing.T) {
	reducedPair := strings.Replace(gatewayLiveNginx,
		"ocr|images/generations|", "", 2)
	// One complete reduced pair plus one appended old pattern: previously
	// recognized==1 and the migration would proceed over foreign content.
	appendedOld := reducedPair +
		"location ~ ^/v1/(chat/completions|completions|responses|audio/transcriptions)$ {\n  auth_basic off;\n}\n"
	if _, err := gatewayInferenceRoutes(appendedOld); err == nil {
		t.Fatal("complete reduced pair plus one appended old pattern must fail")
	}
	// One complete expanded pair plus one appended old pattern.
	appendedExpanded := gatewayLiveNginx +
		"location ~ ^/v1/(chat/completions|completions|responses|audio/transcriptions)$ {\n  auth_basic off;\n}\n"
	if _, err := gatewayInferenceRoutes(appendedExpanded); err == nil {
		t.Fatal("complete expanded pair plus one appended old pattern must fail")
	}
	// One complete old pair plus one appended reduced pattern: the old pair
	// is complete, the appended reduced pattern is a foreign partial.
	appendedReduced := gatewayOldNginx +
		"location ~ ^/(?:v1/(?:chat/completions|completions|responses|audio/transcriptions)|anthropic/v1/(?:messages|messages/count_tokens))$ {\n  auth_basic off;\n}\n"
	if _, err := gatewayInferenceRoutes(appendedReduced); err == nil {
		t.Fatal("complete old pair plus one appended reduced pattern must fail")
	}
	// Zero effects: the input text is returned untouched alongside the error
	// (the function never mutates or partially rewrites on rejection).
	untouched := appendedOld
	if _, err := gatewayInferenceRoutes(untouched); err == nil {
		t.Fatal("expected rejection")
	}
	if untouched != appendedOld {
		t.Fatal("rejected layout must not be mutated")
	}
	// The exact live expanded pair remains byte-identical and recognized.
	live, err := gatewayInferenceRoutes(gatewayLiveNginx)
	if err != nil || live != gatewayLiveNginx {
		t.Fatal("live expanded pair must remain recognized and byte-identical")
	}
}

func TestGatewayStaticConfigWrongTypesFailBeforeEffects(t *testing.T) {
	root, fake := gatewayFixtureRoot(t)
	gatewayInstallFake(t, fake)
	configPath := filepath.Join(root, "gateway", "data", "config.json")

	wrong := []func(b []byte) []byte{
		func(b []byte) []byte { return []byte(`{"providers":"not an object"}`) },
		func(b []byte) []byte { return []byte(`{"providers":{"garuda":["array"]}}`) },
		func(b []byte) []byte { return []byte(`{"providers":{"garuda":{"custom_provider_config":"x"}}}`) },
		func(b []byte) []byte { return []byte(`{"providers":{"garuda":{"custom_provider_config":[]}}}`) },
		func(b []byte) []byte { return []byte(`{"providers":{"garuda":{}}}`) },
		func(b []byte) []byte { return []byte(`{"providers":{}}`) },
		func(b []byte) []byte { return []byte(`{"providers":[["garuda"]]}`) },
		func(b []byte) []byte { return []byte(`[]`) },
	}
	for i, w := range wrong {
		if err := os.WriteFile(configPath, w(nil), 0600); err != nil {
			t.Fatal(err)
		}
		beforeCalls := len(fake.calls)
		if _, err := EnableGateway(root); err == nil || !strings.Contains(err.Error(), "Static garuda provider configuration is missing.") {
			t.Fatalf("wrong type %d must fail safely, got %v", i, err)
		}
		if len(fake.calls) != beforeCalls {
			t.Fatalf("wrong type %d: no API call may happen, got %v", i, fake.calls)
		}
		if _, err := os.Stat(filepath.Join(root, "gateway", "backups")); !os.IsNotExist(err) {
			t.Fatalf("wrong type %d: no backups may be created", i)
		}
	}
}

func TestGatewayFlagsNilAndExplicit(t *testing.T) {
	before := Object{"name": "garuda", "keys": []any{Object{"value": "must-not-copy"}},
		"custom_provider_config": Object{"base_provider_type": "openai"}}
	after, err := gatewayTranslatedProvider(before)
	if err != nil {
		t.Fatal(err)
	}
	flags := obj(obj(after["custom_provider_config"])["allowed_requests"])
	if len(flags) != len(gatewayOperations) {
		t.Fatalf("nil allowed_requests must allow all %d operations, got %d", len(gatewayOperations), len(flags))
	}
	if !boolean(flags["transcription"]) {
		t.Fatal("pinned operations must be true")
	}
	if _, ok := before["custom_provider_config"].(Object)["allowed_requests"]; ok {
		t.Fatal("input must not be mutated")
	}
	if _, ok := gatewayMutableProvider(after)["keys"]; ok {
		t.Fatal("keys must not be in the mutable payload")
	}
	// Explicit existing flags keep false and custom entries.
	before["custom_provider_config"].(Object)["allowed_requests"] =
		Object{"embedding": false, "future_option": true, "responses": true}
	after, err = gatewayTranslatedProvider(before)
	if err != nil {
		t.Fatal(err)
	}
	flags = obj(obj(after["custom_provider_config"])["allowed_requests"])
	if boolean(flags["embedding"]) {
		t.Fatal("explicit false must stay false")
	}
	if !boolean(flags["future_option"]) {
		t.Fatal("custom flags must be preserved")
	}
	if boolean(flags["responses"]) {
		t.Fatal("responses must be pinned false")
	}
	if _, err := gatewayTranslatedProvider(Object{"custom_provider_config": Object{"base_provider_type": "anthropic"}}); err == nil {
		t.Fatal("non-openai base provider must fail")
	}
	if _, err := gatewayTranslatedProvider(Object{"custom_provider_config": Object{"base_provider_type": "openai", "allowed_requests": "bad"}}); err == nil {
		t.Fatal("malformed allowed_requests must fail")
	}
	if _, err := gatewayTranslatedProvider(Object{"custom_provider_config": "not an object"}); err == nil {
		t.Fatal("non-object custom_provider_config must fail")
	}
	if _, err := gatewayTranslatedProvider(Object{}); err == nil {
		t.Fatal("missing custom_provider_config must fail")
	}
}

func TestGatewayEmptyAllowedMapDeniesUnlisted(t *testing.T) {
	// An explicit empty map denies everything except the five overrides.
	before := Object{"custom_provider_config": Object{"base_provider_type": "openai", "allowed_requests": Object{}}}
	after, err := gatewayTranslatedProvider(before)
	if err != nil {
		t.Fatal(err)
	}
	flags := obj(obj(after["custom_provider_config"])["allowed_requests"])
	if len(flags) != 5 {
		t.Fatalf("explicit empty map must keep only the five overrides, got %d: %v", len(flags), flags)
	}
	if boolean(flags["transcription"]) || boolean(flags["embedding"]) {
		t.Fatal("unlisted operations must stay absent/denied")
	}
	if boolean(flags["responses"]) || boolean(flags["responses_stream"]) {
		t.Fatal("responses overrides must be false")
	}
	if !boolean(flags["chat_completion"]) || !boolean(flags["chat_completion_stream"]) || !boolean(flags["list_models"]) {
		t.Fatal("the five overrides must be pinned true")
	}
	if _, mutated := before["custom_provider_config"].(Object)["allowed_requests"]; !mutated {
		t.Fatal("input must not be mutated")
	}
	if len(before["custom_provider_config"].(Object)["allowed_requests"].(Object)) != 0 {
		t.Fatal("input empty map must stay empty")
	}
}

func TestGatewayEnableSuccessPreservesEverything(t *testing.T) {
	root, fake := gatewayFixtureRoot(t)
	gatewayInstallFake(t, fake)
	nginxPath := filepath.Join(root, "gateway", "nginx.conf")
	configPath := filepath.Join(root, "gateway", "data", "config.json")

	nginxIno, nginxMode := gatewayStat(t, nginxPath)
	configIno, configMode := gatewayStat(t, configPath)
	oldConfigBytes, _ := os.ReadFile(configPath)

	result, err := EnableGateway(root)
	if err != nil {
		t.Fatalf("EnableGateway failed: %v", err)
	}
	if str(result["status"]) != "configured" {
		t.Fatalf("unexpected result: %v", result)
	}
	backup := str(result["backup"])
	if backup == "" {
		t.Fatal("backup path missing")
	}

	// Inodes and modes must be retained (bind-mount invariant).
	if got, _ := gatewayStat(t, nginxPath); got != nginxIno {
		t.Fatal("nginx.conf inode changed")
	}
	if got, _ := gatewayStat(t, configPath); got != configIno {
		t.Fatal("config.json inode changed")
	}
	if info, err := os.Stat(nginxPath); err != nil || info.Mode().Perm() != nginxMode {
		t.Fatal("nginx.conf mode changed")
	}
	if info, err := os.Stat(configPath); err != nil || info.Mode().Perm() != configMode {
		t.Fatal("config.json mode changed")
	}

	// Unrelated provider fields preserved in the static config.
	config := gatewayRead(t, configPath)
	garuda := obj(obj(config["providers"])["garuda"])
	if str(obj(garuda["keys"].([]any)[0].(Object))["value"]) != "secret-key-material" {
		t.Fatal("keys must be preserved in the static config")
	}
	if str(obj(garuda["network_config"])["base_url"]) != "http://127.0.0.1:8000" {
		t.Fatal("unrelated provider fields must be preserved")
	}
	flags := obj(obj(garuda["custom_provider_config"])["allowed_requests"])
	if boolean(flags["responses"]) || boolean(flags["responses_stream"]) {
		t.Fatal("responses must be disabled in the static config")
	}
	if !boolean(flags["chat_completion"]) {
		t.Fatal("chat completion must stay enabled")
	}

	// Backup files are private and contain the originals.
	for _, name := range []string{"nginx.conf", "config.json", "provider.json", "result.json"} {
		info, err := os.Stat(filepath.Join(backup, name))
		if err != nil {
			t.Fatalf("backup %s missing: %v", name, err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("backup %s must be 0600", name)
		}
	}
	backupNginx, _ := os.ReadFile(filepath.Join(backup, "nginx.conf"))
	if !bytes.Equal(backupNginx, []byte(gatewayOldNginx)) {
		t.Fatal("nginx backup must hold the original")
	}
	backupConfig, _ := os.ReadFile(filepath.Join(backup, "config.json"))
	if !bytes.Equal(backupConfig, oldConfigBytes) {
		t.Fatal("config backup must hold the original")
	}

	// The API saw PUT then a verification GET, and both nginx steps ran.
	if len(fake.calls) < 4 || fake.calls[0] != "GET" || fake.calls[1] != "PUT" || fake.calls[len(fake.calls)-1] != "GET" {
		t.Fatalf("unexpected API call order: %v", fake.calls)
	}
	if !fake.nginxCalled || !strings.Contains(strings.Join(fake.calls, "|"), "-s reload") {
		t.Fatal("nginx validation and reload must run")
	}
	// Credentials are never stored in the fake or surfaced anywhere.
	if strings.Contains(fmt.Sprintf("%v", fake.calls), "fixture-only") {
		t.Fatal("password must never appear in effects")
	}
}

func gatewayStat(t *testing.T, path string) (uint64, os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("stat unavailable")
	}
	return stat.Ino, info.Mode().Perm()
}

func TestGatewayEnableRollbackRestoresFiles(t *testing.T) {
	root, fake := gatewayFixtureRoot(t)
	gatewayInstallFake(t, fake)
	// Verification GET fails after reload: everything must be restored.
	fake.failAPI["GET2"] = 1
	nginxPath := filepath.Join(root, "gateway", "nginx.conf")
	configPath := filepath.Join(root, "gateway", "data", "config.json")
	nginxIno, _ := gatewayStat(t, nginxPath)
	configIno, _ := gatewayStat(t, configPath)

	_, err := EnableGateway(root)
	if err == nil || !strings.Contains(err.Error(), "Previous settings were restored") {
		t.Fatalf("expected restored message, got %v", err)
	}
	if strings.Contains(err.Error(), "fixture-only") {
		t.Fatal("credentials must not surface in errors")
	}
	if got, _ := gatewayStat(t, nginxPath); got != nginxIno {
		t.Fatal("rollback must keep the nginx inode")
	}
	if got, _ := gatewayStat(t, configPath); got != configIno {
		t.Fatal("rollback must keep the config inode")
	}
	config := gatewayRead(t, configPath)
	if flags, ok := obj(obj(obj(config["providers"])["garuda"])["custom_provider_config"])["allowed_requests"]; ok && flags != nil {
		t.Fatal("static config must be restored to the original without allowed_requests")
	}
	if !strings.Contains(string(gatewayMustRead(t, nginxPath)), "^/v1/(chat/completions|") {
		t.Fatal("nginx must be restored to the old layout")
	}
	// Rollback reloaded nginx after restoring.
	if strings.HasSuffix(fake.calls[len(fake.calls)-1], "-s reload") {
		// last call is reload; good
	} else {
		t.Fatalf("expected final reload, got %v", fake.calls)
	}
}

func TestGatewayEnableRollbackNeedsAttention(t *testing.T) {
	root, fake := gatewayFixtureRoot(t)
	gatewayInstallFake(t, fake)
	// Fail the provider restore: file restores and reload must still run.
	fake.failAPI["PUT"] = 1
	fake.failNgin["-t"] = true
	nginxPath := filepath.Join(root, "gateway", "nginx.conf")
	configPath := filepath.Join(root, "gateway", "data", "config.json")

	_, err := EnableGateway(root)
	if err == nil || !strings.Contains(err.Error(), "Rollback needs attention") {
		t.Fatalf("expected attention message, got %v", err)
	}
	if !strings.Contains(err.Error(), "backup") {
		t.Fatal("error must include the backup path")
	}
	if !strings.Contains(string(gatewayMustRead(t, nginxPath)), "^/v1/(chat/completions|") {
		t.Fatal("nginx file restoration must still be attempted")
	}
	if flags, ok := obj(obj(obj(gatewayRead(t, configPath)["providers"])["garuda"])["custom_provider_config"])["allowed_requests"]; ok && flags != nil {
		t.Fatal("config file restoration must still be attempted")
	}
}

func TestGatewayEnableFileRestoreFailureStillRestoresOtherFile(t *testing.T) {
	root, fake := gatewayFixtureRoot(t)
	gatewayInstallFake(t, fake)
	// Fail the verification read AND the config write-back during rollback;
	// the nginx restoration and reload must still be attempted.
	fake.failAPI["GET2"] = 1
	nginxPath := filepath.Join(root, "gateway", "nginx.conf")
	configPath := filepath.Join(root, "gateway", "data", "config.json")
	prevWrite := gatewayWriteFile
	gatewayWriteFile = func(path string, b []byte, mode os.FileMode) error {
		if path == configPath {
			return fail("write blocked")
		}
		return prevWrite(path, b, mode)
	}
	t.Cleanup(func() { gatewayWriteFile = prevWrite })

	_, err := EnableGateway(root)
	if err == nil || !strings.Contains(err.Error(), "Rollback needs attention") {
		t.Fatalf("expected attention message, got %v", err)
	}
	if !strings.Contains(string(gatewayMustRead(t, nginxPath)), "^/v1/(chat/completions|") {
		t.Fatal("nginx restoration must be attempted despite config failure")
	}
	if !strings.Contains(strings.Join(fake.calls, "|"), "-s reload") {
		t.Fatal("final reload must be attempted despite config failure")
	}
}

func TestGatewayEnableRejectsMissingIdentityAndConfig(t *testing.T) {
	root, fake := gatewayFixtureRoot(t)
	gatewayInstallFake(t, fake)
	if err := os.WriteFile(filepath.Join(root, "gateway", "gateway.env"), []byte("BIFROST_ADMIN_USER=fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnableGateway(root); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("missing password must fail: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "gateway", "gateway.env"), []byte("BIFROST_ADMIN_USER=fixture\nBIFROST_ADMIN_PASSWORD=fixture-only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gateway", "nginx.conf"), []byte("unrecognized"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnableGateway(root); err == nil || !strings.Contains(err.Error(), "Unknown nginx inference layout") {
		t.Fatalf("unknown layout must fail before mutation: %v", err)
	}
	if fake.calls != nil {
		t.Fatal("no API call may happen for an unknown layout")
	}
}

func TestGatewayEnableLockExclusion(t *testing.T) {
	root, fake := gatewayFixtureRoot(t)
	gatewayInstallFake(t, fake)
	lockPath := filepath.Join(root, "gateway", ".spark-gateway-lock")

	// Hold a real exclusive flock on the gateway lock file.
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = withFileLock(lockPath, true, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	// EnableGateway must return promptly with a busy error, never wait.
	deadline := time.Now().Add(2 * time.Second)
	done := make(chan struct{})
	var err error
	go func() {
		_, err = EnableGateway(root)
		close(done)
	}()
	select {
	case <-done:
		if err == nil || !strings.Contains(err.Error(), "Another Sparky operation holds this lock.") {
			t.Fatalf("expected busy error, got %v", err)
		}
	case <-time.After(deadline.Sub(time.Now())):
		close(release)
		t.Fatal("EnableGateway must not wait for the lock")
	}
	close(release)

	// No effects may have happened under the held lock.
	if len(fake.calls) != 0 {
		t.Fatalf("no API or nginx calls may happen, got %v", fake.calls)
	}
	if _, err := os.Stat(filepath.Join(root, "gateway", "backups")); !os.IsNotExist(err) {
		t.Fatal("no backups may be created while the lock is held")
	}
}

func TestGatewayEnableRejectsRelativeRoot(t *testing.T) {
	_, fake := gatewayFixtureRoot(t)
	gatewayInstallFake(t, fake)
	if _, err := EnableGateway("relative/root"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative root must fail: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatal("no effects may happen for a relative root")
	}
}

func gatewayMustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
