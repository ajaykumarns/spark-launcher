package sparky

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func clientTestConfig(t *testing.T, gateway string) clientConfig {
	t.Helper()
	return clientConfig{
		Version:        1,
		Gateway:        gateway,
		ModelPrefix:    "garuda/",
		TimeoutSeconds: 7200,
		CompactPercent: 95,
		DefaultModel:   "garuda/spark-auto",
		Binaries:       map[string]string{},
	}
}

func clientTestStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SPARKY_STATE_DIR", dir)
	return filepath.Join(dir, "catalog.json")
}

func TestClientValidateConfig(t *testing.T) {
	cfg, err := clientValidateConfig(Object{"version": 1, "gateway": "https://spark.example:8443/"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Gateway != "https://spark.example:8443" {
		t.Fatalf("gateway trailing slash not trimmed: %q", cfg.Gateway)
	}
	if cfg.DefaultModel != "garuda/spark-auto" || cfg.ModelPrefix != "garuda/" {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	if cfg.CompactPercent != 95 || cfg.TimeoutSeconds != 7200 {
		t.Fatalf("bad numeric defaults: %+v", cfg)
	}
	cases := []struct {
		name string
		raw  Object
	}{
		{"wrong version", Object{"version": 2, "gateway": "https://x"}},
		{"fractional version", Object{"version": 1.5, "gateway": "https://x"}},
		{"string version", Object{"version": "1", "gateway": "https://x"}},
		{"bool version", Object{"version": true, "gateway": "https://x"}},
		{"missing gateway", Object{"version": 1}},
		{"query in gateway", Object{"version": 1, "gateway": "https://x/?q=1"}},
		{"credentials", Object{"version": 1, "gateway": "https://u:p@x/"}},
		{"path suffix", Object{"version": 1, "gateway": "https://x/v1"}},
		{"plain http remote", Object{"version": 1, "gateway": "http://x.example"}},
		{"compact too low", Object{"version": 1, "gateway": "https://x", "compact_percent": 40}},
		{"compact non-int", Object{"version": 1, "gateway": "https://x", "compact_percent": 90.5}},
		{"timeout too big", Object{"version": 1, "gateway": "https://x", "timeout_seconds": 90000}},
		{"models field", Object{"version": 1, "gateway": "https://x", "models": Object{}}},
		{"bad binaries", Object{"version": 1, "gateway": "https://x", "binaries": []any{"x"}}},
		{"empty prefix", Object{"version": 1, "gateway": "https://x", "model_prefix": ""}},
	}
	for _, c := range cases {
		if _, err := clientValidateConfig(c.raw); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
	if _, err := clientValidateConfig(Object{"version": 1, "gateway": "http://localhost:8080"}); err != nil {
		t.Errorf("localhost http should be allowed: %v", err)
	}
}

func TestClientMakeCatalogNormalization(t *testing.T) {
	cfg := clientTestConfig(t, "https://spark.example")
	wire := Object{
		"data": []any{
			Object{
				"id":             "garuda/spark-auto",
				"context_length": 200000,
				"output_tokens":  64000,
			},
			Object{
				"id":             "garuda/other-model",
				"context_length": 128000,
				"output_tokens":  32000,
				"aliases":        []any{"other"},
				"name":           "Other Model",
			},
			// Unprefixed models and unusable context rows are skipped.
			Object{"id": "openai/gpt-x", "context_length": 1000},
			Object{"id": "garuda/bad-context", "context_length": 0},
		},
		"audio_services": []any{
			Object{"kind": "tts", "id": "tts-main", "model": "m1", "api_base": "http://x", "default_voice": "v", "voices": []any{"v", "w"}},
		},
	}
	catalog, err := clientMakeCatalog(cfg, wire)
	if err != nil {
		t.Fatalf("make catalog: %v", err)
	}
	models := arr(catalog["models"])
	if len(models) != 2 {
		t.Fatalf("want 2 models, got %d", len(models))
	}
	if str(obj(models[0])["id"]) != "garuda/other-model" {
		t.Fatalf("models not sorted by id: %v", models)
	}
	auto := obj(models[1])
	if str(auto["id"]) != "garuda/spark-auto" || str(auto["context_source"]) != "gateway auto route context" {
		t.Fatalf("bad auto entry: %v", auto)
	}
	if int(num(auto["context_tokens"])) != 200000 {
		t.Fatalf("auto should prefer advertised context: %v", auto)
	}
	if int(num(obj(models[0])["context_tokens"])) != 128000 {
		t.Fatalf("bad context: %v", models[0])
	}
	if len(arr(catalog["audio_services"])) != 1 {
		t.Fatal("audio services not preserved")
	}
}

func TestClientMakeCatalogInvalidMetadata(t *testing.T) {
	cfg := clientTestConfig(t, "https://spark.example")
	cases := []struct {
		name string
		wire Object
	}{
		{"no data", Object{}},
		{"bad row", Object{"data": []any{"nope"}}},
		{"no id", Object{"data": []any{Object{}}}},
		{"dup ids", Object{"data": []any{Object{"id": "garuda/a", "context_length": 10}, Object{"id": "garuda/a", "context_length": 10}}}},
		{"bad aliases", Object{"data": []any{Object{"id": "garuda/a", "context_length": 10, "aliases": []any{""}}}}},
		{"bad budget", Object{"data": []any{Object{"id": "garuda/a", "context_length": 10, "output_tokens": 0}}}},
		{"budget exceeds context", Object{"data": []any{Object{"id": "garuda/a", "context_length": 10, "output_tokens": 100}}}},
		{"bad name", Object{"data": []any{Object{"id": "garuda/a", "context_length": 10, "name": 3}}}},
		{"auto budget exceeds", Object{"data": []any{Object{"id": "garuda/spark-auto", "context_length": 10, "output_tokens": 100}, Object{"id": "garuda/b", "context_length": 20}}}},
		{"no models", Object{"data": []any{}}},
		{"fractional context skipped", Object{"data": []any{Object{"id": "garuda/a", "context_length": 10.5}}}},
		{"fractional budget", Object{"data": []any{Object{"id": "garuda/a", "context_length": 10, "output_tokens": 5.5}}}},
		{"auto only", Object{"data": []any{Object{"id": "garuda/spark-auto", "context_length": 1000}}}},
	}
	for _, c := range cases {
		if _, err := clientMakeCatalog(cfg, c.wire); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
	badAudio := []Object{
		{"audio_services": "no"},
		{"audio_services": []any{Object{"kind": "stt"}}},
		{"audio_services": []any{Object{"kind": "tts", "id": "x", "model": 3, "api_base": "h", "default_voice": "v", "voices": []any{}}}},
		{"audio_services": []any{Object{"kind": "tts", "id": "x", "model": "m", "api_base": "h", "default_voice": "v", "voices": []any{3}}}},
	}
	for _, wire := range badAudio {
		wire["data"] = []any{Object{"id": "garuda/a", "context_length": 10}}
		if _, err := clientMakeCatalog(cfg, wire); err == nil {
			t.Errorf("expected invalid speech metadata error for %v", wire)
		}
	}
}

func TestClientFetchCatalogOfflineAndGatewayMismatch(t *testing.T) {
	path := clientTestStateDir(t)
	cfg := clientTestConfig(t, "https://spark.example")
	cache := Object{
		"gateway":    "https://other.example",
		"fetched_at": "2026-01-01T00:00:00Z",
		"models": []any{
			Object{"id": "garuda/a", "context_tokens": 1000},
		},
		"audio_services": []any{},
	}
	if err := os.WriteFile(path, jsonBytes(cache), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientFetchCatalog(cfg, true); err == nil {
		t.Fatal("expected wrong-gateway cache error")
	}
	cache["gateway"] = cfg.Gateway
	if err := os.WriteFile(path, jsonBytes(cache), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := clientFetchCatalog(cfg, true)
	if err != nil {
		t.Fatalf("offline fetch: %v", err)
	}
	if str(got["gateway"]) != cfg.Gateway {
		t.Fatalf("bad gateway: %v", got["gateway"])
	}
	// Stale/invalid cache is rejected.
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientFetchCatalog(cfg, true); err == nil {
		t.Fatal("expected stale cache error")
	}
	if err := os.WriteFile(path, jsonBytes(Object{"gateway": cfg.Gateway, "fetched_at": "x"}), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientFetchCatalog(cfg, true); err == nil {
		t.Fatal("expected invalid cache error (no models list)")
	}
	// Valid metadata checks also run against cached entries.
	bad := Object{
		"gateway":    cfg.Gateway,
		"fetched_at": "2026-01-01T00:00:00Z",
		"models": []any{
			Object{"id": "garuda/a", "context_tokens": 1000, "output_tokens": 2000},
		},
	}
	if err := os.WriteFile(path, jsonBytes(bad), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientFetchCatalog(cfg, true); err == nil {
		t.Fatal("expected cached metadata validation error")
	}
}

func TestClientFetchCatalogLive(t *testing.T) {
	path := clientTestStateDir(t)
	var mux *http.ServeMux
	mux = http.NewServeMux()
	mux.HandleFunc("/sparky/catalog", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{
				map[string]any{"id": "garuda/spark-auto", "context_length": 200000},
				map[string]any{"id": "garuda/other", "context_length": 128000},
			},
			"audio_services": []any{},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cfg := clientTestConfig(t, srv.URL)
	got, err := clientFetchCatalog(cfg, false)
	if err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	if len(arr(got["models"])) != 2 {
		t.Fatalf("unexpected models: %v", got["models"])
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
}

func TestClientFetchCatalogHTTPError(t *testing.T) {
	clientTestStateDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	cfg := clientTestConfig(t, srv.URL)
	if _, err := clientFetchCatalog(cfg, false); err == nil {
		t.Fatal("expected HTTP error")
	}
}

func TestClientResolve(t *testing.T) {
	catalog := Object{
		"models": []any{
			Object{"id": "garuda/one", "aliases": []any{"uno"}},
			Object{"id": "garuda/two/inner", "aliases": []any{"duo", "inner"}},
		},
	}
	m, err := clientResolve(catalog, " garuda/one ")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if str(m["id"]) != "garuda/one" {
		t.Fatalf("bad resolve: %v", m)
	}
	if m, err = clientResolve(catalog, "inner"); err != nil || str(m["id"]) != "garuda/two/inner" {
		t.Fatalf("suffix resolve failed: %v %v", m, err)
	}
	// Ambiguous alias.
	catalog["models"] = append(arr(catalog["models"]),
		Object{"id": "garuda/three", "aliases": []any{"duo"}})
	if _, err := clientResolve(catalog, "duo"); err == nil {
		t.Fatal("expected ambiguous alias error")
	} else if !contains([]string{err.Error()}, "") && !stringContains(err.Error(), "Ambiguous") {
		t.Fatalf("wrong error: %v", err)
	}
	if _, err := clientResolve(catalog, "nope"); err == nil || !stringContains(err.Error(), "Unknown model") {
		t.Fatalf("expected unknown model error, got %v", err)
	}
	// Near-miss suggestion.
	if _, err := clientResolve(catalog, "garuda/onw"); err == nil || !stringContains(err.Error(), "Did you mean") {
		t.Fatalf("expected suggestion hint, got %v", err)
	}
}

func stringContains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestClientConfigExtraRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.json")
	raw := `{
	  "version": 1,
	  "gateway": "https://spark.example:8443",
	  "admin_ssh_host": "spark-admin.example",
	  "worker_ssh_host": "spark-worker.example",
	  "remote_root": "/srv/spark",
	  "admin_url": "http://localhost:9090",
	  "custom_user_setting": {"nested": true}
	}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPARKY_CONFIG", path)
	cfg, err := clientLoadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	for _, k := range []string{"admin_ssh_host", "worker_ssh_host", "remote_root", "admin_url", "custom_user_setting"} {
		if _, ok := cfg.Extra[k]; !ok {
			t.Fatalf("extra field %q not retained", k)
		}
	}
	if str(cfg.Extra["admin_ssh_host"]) != "spark-admin.example" {
		t.Fatalf("bad retained host: %v", cfg.Extra["admin_ssh_host"])
	}
	// Defaults applied on top of extra fields survive round-trip.
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var back Object
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if int(num(back["timeout_seconds"])) != 7200 {
		t.Fatalf("default not preserved in round-trip: %v", back)
	}
	if _, ok := back["custom_user_setting"]; !ok {
		t.Fatal("custom setting lost in round-trip")
	}
	if int(num(back["version"])) != 1 {
		t.Fatalf("version lost: %v", back["version"])
	}
	// Credential-style fields are retained but validation errors never quote them.
	if err := os.WriteFile(path, []byte(`{"version":1,"gateway":"https://user:secret@x/"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientLoadConfig(); err == nil || stringContains(err.Error(), "secret") {
		t.Fatalf("credentials leaked in error: %v", err)
	}
}
