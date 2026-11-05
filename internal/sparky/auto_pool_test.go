package sparky

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func bifrostPoolFixture(t *testing.T) *imageEnv {
	t.Helper()
	env := newImageEnv(t, "native")
	cfg := env.coord.Config()
	obj(cfg["auto"])["routing"] = "bifrost"
	obj(obj(cfg["models"])["glm"])["output_tokens"] = 8192
	if err := writeCluster(env.path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := env.coord.Refresh(); err != nil {
		t.Fatal(err)
	}
	previous := autoGatewaySyncHook
	autoGatewaySyncHook = func(context.Context, Object, []string) error { return nil }
	t.Cleanup(func() { autoGatewaySyncHook = previous })
	return env
}

func TestAutoPoolPublishesOnlyProvenReadyModels(t *testing.T) {
	env := bifrostPoolFixture(t)
	if err := env.coord.ReconcileAutoRouting(); err != nil {
		t.Fatal(err)
	}
	row := catalogAuto(t, &controllerServer{ctrl: env.coord})
	if row["available"] != false {
		t.Fatalf("stopped model advertised as ready: %v", row)
	}
	env.readyRecord()
	var supplied []string
	autoGatewaySyncHook = func(_ context.Context, _ Object, names []string) error {
		supplied = append([]string{}, names...)
		return nil
	}
	if err := env.coord.ReconcileAutoRouting(); err != nil {
		t.Fatal(err)
	}
	row = catalogAuto(t, &controllerServer{ctrl: env.coord})
	if !sameStrings(supplied, []string{"glm"}) || !boolean(row["available"]) || str(row["request_model"]) != "spark-auto" {
		t.Fatalf("ready pool lost: %v %v", supplied, row)
	}
	if _, exists := row["resolved_model"]; exists {
		t.Fatal("a pool must not claim one resolved model")
	}
	rec, err := env.rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.rt.TransitionState("glm", str(rec["generation"]), StateStopping); err != nil {
		t.Fatal(err)
	}
	if err := env.coord.ReconcileAutoRouting(); err != nil {
		t.Fatal(err)
	}
	if len(supplied) != 0 || boolean(catalogAuto(t, &controllerServer{ctrl: env.coord})["available"]) {
		t.Fatal("lost readiness did not remove availability")
	}
}

func TestAutoPoolSyncFailureBlocksMetadataAndUnload(t *testing.T) {
	env := bifrostPoolFixture(t)
	env.readyRecord()
	autoGatewaySyncHook = func(context.Context, Object, []string) error { return fail("synthetic gateway failure") }
	if err := env.coord.ReconcileAutoRouting(); err == nil {
		t.Fatal("sync failure was hidden")
	}
	row := catalogAuto(t, &controllerServer{ctrl: env.coord})
	if boolean(row["available"]) || !strings.Contains(str(row["routing_error"]), "synthetic") {
		t.Fatalf("sync failure metadata: %v", row)
	}
	if _, err := env.coord.Unload("glm", false); err == nil {
		t.Fatal("unload proceeded without removing availability")
	}
	rec, err := env.rt.readRecord("glm")
	if err != nil || str(rec["state"]) != StateReady {
		t.Fatalf("model stopped after failed sync: %v %v", rec, err)
	}
}

func TestAutoPoolBudgetIncludesStoppedPermittedRecipes(t *testing.T) {
	cfg := Object{"auto": Object{"targets": []any{"large", "small"}}, "models": Object{}}
	models := obj(cfg["models"])
	models["large"] = Object{"kind": "llm", "run_on": "spark01", "output_tokens": 32768, "settings": Object{"spark01": Object{"context_tokens": 262144}}}
	models["small"] = Object{"kind": "llm", "run_on": "spark02", "output_tokens": 8192, "settings": Object{"spark02": Object{"context_tokens": 131072}}}
	selection := &runtimeSelection{Model: obj(models["large"]), Setting: obj(obj(obj(models["large"])["settings"])["spark01"]), Nodes: []string{"spark01"}, Record: Object{"generation": "first"}}
	row := autoPoolMetadata(cfg, []string{"large"}, []*runtimeSelection{selection})
	if num(row["context_length"]) != 131072 || num(row["max_output_tokens"]) != 8192 {
		t.Fatalf("loading the smaller permitted model could invalidate a prepared request: %v", row)
	}
}

func TestAutoPoolRequiresBifrostAndKeepsNamedRequests(t *testing.T) {
	env := bifrostPoolFixture(t)
	env.readyRecord()
	for _, model := range []string{"spark-auto", "garuda/spark-auto", "sparky-auto-unavailable"} {
		_, _, err := env.coord.LLMRequest(context.Background(), model)
		status, ok := err.(interface{ HTTPStatus() int })
		if !ok || status.HTTPStatus() != http.StatusServiceUnavailable {
			t.Fatalf("expected HTTP 503, got %v", err)
		}
	}
	name, lease, err := env.coord.LLMRequest(context.Background(), "glm")
	if err != nil || name != "glm" {
		t.Fatalf("named route failed: %s %v", name, err)
	}
	env.coord.Release(lease)
}

func TestAutoPoolHarnessUsesBareAliasAndNativeSession(t *testing.T) {
	bin := harnessFakeBinary(t, "codex")
	cfg := harnessTestConfig(t, map[string]string{"codex": bin})
	model := harnessModel("garuda/spark-auto")
	model["routing"], model["request_model"] = "bifrost", "spark-auto"
	plan, err := buildHarnessPlan(cfg, harnessTestCatalog(), model, "codex", "/tmp/p", "launch-id")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(stringsOf(plan["args"]), " ")
	if !strings.Contains(args, `model="spark-auto"`) || strings.Contains(args, "X-Session-ID") {
		t.Fatalf("Bifrost needs the bare alias and native conversation ID: %s", args)
	}
}

func TestAutoPoolRemovesRouteBeforeUnloadDrains(t *testing.T) {
	env := bifrostPoolFixture(t)
	env.readyRecord()
	_, lease, err := env.coord.LLMRequest(context.Background(), "glm")
	if err != nil {
		t.Fatal(err)
	}
	defer env.coord.Release(lease)
	removed := false
	autoGatewaySyncHook = func(_ context.Context, _ Object, names []string) error {
		if len(names) == 0 {
			rec, err := env.rt.readRecord("glm")
			if err != nil || str(rec["state"]) != StateReady {
				t.Fatalf("availability was removed after stopping: %v %v", rec, err)
			}
			removed = true
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := env.coord.unload(ctx, "glm", false); err == nil || !removed {
		t.Fatalf("expected route removal followed by a cancelled drain: removed=%v err=%v", removed, err)
	}
}

func TestAutoPoolLoadCompletionAndBackendCrash(t *testing.T) {
	env := bifrostPoolFixture(t)
	env.readyRecord()
	var supplied []string
	autoGatewaySyncHook = func(_ context.Context, _ Object, names []string) error {
		supplied = append([]string{}, names...)
		return nil
	}
	if _, err := env.coord.Load("glm", false, ""); err != nil || !sameStrings(supplied, []string{"glm"}) {
		t.Fatalf("ready load did not publish availability: %v %v", supplied, err)
	}
	env.llm.server.Close()
	if err := env.coord.ReconcileAutoRouting(); err != nil || len(supplied) != 0 {
		t.Fatalf("failed backend remained in pool: %v %v", supplied, err)
	}
}
