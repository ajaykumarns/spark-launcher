package sparky

import (
	"net/http"
	"strings"
	"testing"
)

func catalogAuto(t *testing.T, s *controllerServer) Object {
	t.Helper()
	rec := serverRequest(t, s, "GET", "/sparky/catalog", "127.0.0.1:5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog failed: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("live model limits must not be cached by HTTP intermediaries")
	}
	for _, raw := range arr(decodeResponse(t, rec.Body.String())["data"]) {
		row := obj(raw)
		if str(row["id"]) == "garuda/spark-auto" {
			return row
		}
	}
	t.Fatal("auto route missing")
	return nil
}

func TestLiveCatalogFollowsAutoSelectionWithoutLifecycleChanges(t *testing.T) {
	env := newImageEnv(t, "native")
	altBackend := newNativeBackend(t, "alt")
	cfg := env.coord.Config()
	glm := obj(obj(cfg["models"])["glm"])
	glm["output_tokens"] = 4096
	obj(obj(glm["settings"])["spark01"])["context_tokens"] = 131072
	alt := decodeResponse(t, string(jsonBytes(glm)))
	alt["aliases"] = []any{}
	alt["output_tokens"] = 32768
	altSetting := obj(obj(alt["settings"])["spark01"])
	altSetting["context_tokens"] = 262144
	altSetting["backend_url"] = altBackend.server.URL
	obj(cfg["models"])["alt"] = alt
	obj(cfg["auto"])["targets"] = []any{"glm", "alt"}
	if err := writeCluster(env.path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := env.coord.Refresh(); err != nil {
		t.Fatal(err)
	}
	env.readyRecord()
	rec, err := env.rt.BeginOperation("alt", alt, altSetting, []string{"spark01"}, "spark01")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{StateStarting, StateReady} {
		if err := env.rt.TransitionState("alt", str(rec["generation"]), state); err != nil {
			t.Fatal(err)
		}
	}
	s := &controllerServer{ctrl: env.coord}
	row := catalogAuto(t, s)
	if str(row["resolved_model"]) != "garuda/glm" || num(row["context_length"]) != 131072 || num(row["max_output_tokens"]) != 4096 {
		t.Fatalf("limits must describe the selected target, not a catalog minimum: %v", row)
	}
	current, err := env.rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.rt.TransitionState("glm", str(current["generation"]), StateStopping); err != nil {
		t.Fatal(err)
	}
	row = catalogAuto(t, s)
	if str(row["resolved_model"]) != "garuda/alt" || num(row["context_length"]) != 262144 || num(row["max_output_tokens"]) != 32768 {
		t.Fatalf("limits stayed on the previous active model: %v", row)
	}
	for _, key := range []string{"backend_url", "api_key", "config", "setting"} {
		if _, present := row[key]; present {
			t.Fatalf("private field %s leaked", key)
		}
	}
	for _, args := range env.runnerC.all() {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, " start ") || strings.Contains(joined, " stop ") || strings.Contains(joined, " restart ") {
			t.Fatalf("metadata query changed model residency: %v", args)
		}
	}
}

func TestLiveCatalogDoesNotInventLimitsWithoutReadyTarget(t *testing.T) {
	env := newImageEnv(t, "native")
	row := catalogAuto(t, &controllerServer{ctrl: env.coord})
	if row["available"] != false || row["dynamic"] != true {
		t.Fatalf("unavailable dynamic route must be explicit: %v", row)
	}
	for _, key := range []string{"context_length", "output_tokens", "max_output_tokens", "resolved_model"} {
		if _, present := row[key]; present {
			t.Fatalf("unavailable route advertised a guessed %s", key)
		}
	}
}

func TestLiveCatalogDoesNotInventMissingOutputLimit(t *testing.T) {
	env := newImageEnv(t, "native")
	env.readyRecord()
	row := catalogAuto(t, &controllerServer{ctrl: env.coord})
	if row["available"] != true || str(row["resolved_model"]) != "garuda/glm" {
		t.Fatalf("ready selection was lost: %v", row)
	}
	if _, guessed := row["max_output_tokens"]; guessed {
		t.Fatal("an unreported output limit must not become a fixed default")
	}
}
