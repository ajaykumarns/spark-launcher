package sparky

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func identityEnv(t *testing.T) *imageEnv {
	t.Helper()
	e := newImageEnv(t, "native")
	cfg := e.coord.Config()
	base := obj(obj(cfg["models"])["glm"])
	shared := decodeResponse(t, string(jsonBytes(base)))
	shared["public_model"] = "glm"
	shared["model_id"] = "glm"
	shared["name"] = "GLM shared recipe"
	shared["aliases"] = []any{"shared"}
	obj(obj(shared["settings"])["spark01"])["context_tokens"] = 131072
	obj(cfg["models"])["glm-shared"] = shared
	if err := writeCluster(e.path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.coord.Refresh(); err != nil {
		t.Fatal(err)
	}
	return e
}

func identityReady(t *testing.T, e *imageEnv, name string) {
	t.Helper()
	m, s, nodes, on, _ := selectPlacement(e.coord.Config(), name, "")
	rec, err := e.rt.BeginOperation(name, m, s, nodes, on)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{StateStarting, StateReady} {
		if err := e.rt.TransitionState(name, str(rec["generation"]), state); err != nil {
			t.Fatal(err)
		}
	}
}

func identityStop(t *testing.T, e *imageEnv, name string) {
	t.Helper()
	rec, err := e.rt.readRecord(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{StateStopping, StateStopped} {
		if err := e.rt.TransitionState(name, str(rec["generation"]), state); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIdentityRoutesCanonicalAndLegacyIDsAfterRuntimeChange(t *testing.T) {
	e := identityEnv(t)
	for _, active := range []string{"glm-shared", "glm", "glm-shared"} {
		identityReady(t, e, active)
		for _, requested := range []string{"garuda/glm", "glm", "glm-shared", "garuda/glm-shared", "shared"} {
			name, token, err := e.coord.llmRequest(context.Background(), requested)
			if err != nil || name != active {
				t.Fatalf("%s routed to %s instead of %s: %v", requested, name, active, err)
			}
			e.coord.Release(token)
		}
		wire, err := e.coord.LLMWire()
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, raw := range arr(wire["data"]) {
			if str(obj(raw)["id"]) == "glm-shared" {
				t.Fatal("legacy runtime ID leaked")
			}
			if str(obj(raw)["id"]) == "glm" {
				found++
				if str(obj(obj(raw)["status"])["value"]) != "loaded" {
					t.Fatal("ready runtime was advertised as unloaded")
				}
			}
		}
		if found != 1 {
			t.Fatal("public ID missing")
		}
		identityStop(t, e, active)
	}
	if _, _, err := e.coord.llmRequest(context.Background(), "glm"); err == nil {
		t.Fatal("a stopped family must refuse inference")
	}
	for _, args := range e.runnerC.all() {
		if strings.Contains(strings.Join(args, " "), " start ") || strings.Contains(strings.Join(args, " "), " stop ") {
			t.Fatal("inference changed model residency")
		}
	}
}

func TestIdentityCatalogAndOpenCodeStayStable(t *testing.T) {
	e := identityEnv(t)
	catalog, err := clientMakeCatalog(harnessTestConfig(t, nil), decodeResponse(t, string(jsonBytes(modelCatalog(e.coord.Config())))))
	if err != nil {
		t.Fatal(err)
	}
	model, err := clientResolve(catalog, "glm-shared")
	if err != nil || str(model["id"]) != "garuda/glm" {
		t.Fatalf("legacy alias failed: %v %v", model, err)
	}
	bin := harnessFakeBinary(t, "opencode")
	cfg := harnessTestConfig(t, map[string]string{"opencode": bin})
	plan, err := buildHarnessPlan(cfg, catalog, model, "opencode", t.TempDir(), "stable")
	if err != nil {
		t.Fatal(err)
	}
	raw := decodeResponse(t, str(obj(plan["environment_overrides"])["OPENCODE_CONFIG_CONTENT"]))
	if str(raw["model"]) != "spark/glm" {
		t.Fatal("OpenCode selection used a runtime ID")
	}
	models := obj(obj(obj(raw["provider"])["spark"])["models"])
	for _, key := range []string{"glm", "glm-shared"} {
		if str(obj(models[key])["id"]) != "garuda/glm" {
			t.Fatal("OpenCode legacy key did not use the public API ID")
		}
	}
	if num(obj(obj(models["glm"])["limit"])["context"]) != 131072 {
		t.Fatal("public context must fit both recipes")
	}
	s := &controllerServer{ctrl: e.coord}
	rec := serverRequest(t, s, "GET", "/sparky/catalog", "127.0.0.1:5", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"id":"garuda/glm-shared"`) {
		t.Fatal("HTTP catalog failed")
	}
}

func TestIdentityValidationRejectsInvalidGroups(t *testing.T) {
	for _, bad := range []any{"missing", "glm-shared", 42} {
		e := identityEnv(t)
		cfg := e.coord.Config()
		obj(obj(cfg["models"])["glm-shared"])["public_model"] = bad
		if validateCluster(cfg) == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	e := identityEnv(t)
	cfg := e.coord.Config()
	obj(obj(cfg["models"])["glm-shared"])["api_kind"] = "decision"
	if validateCluster(cfg) == nil {
		t.Fatal("accepted chat and decision in one group")
	}
}
