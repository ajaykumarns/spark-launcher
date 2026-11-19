package sparky

import (
	"context"
	"net/http"
	"testing"
)

func decisionPoolFixture(t *testing.T) *imageEnv {
	t.Helper()
	env := newImageEnv(t, "native")
	cfg := env.coord.Config()
	obj(obj(cfg["models"])["glm"])["api_kind"] = "decision"
	obj(obj(cfg["models"])["glm"])["output_tokens"] = 1
	cfg["decision_auto"] = Object{"routing": "bifrost", "targets": []any{"glm"}}
	if err := writeCluster(env.path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := env.coord.Refresh(); err != nil {
		t.Fatal(err)
	}
	previous := decisionAutoGatewaySyncHook
	decisionAutoGatewaySyncHook = func(context.Context, Object, []string) error { return nil }
	t.Cleanup(func() { decisionAutoGatewaySyncHook = previous })
	return env
}

func TestDecisionAutoOnlyPublishesReadyDecisionModels(t *testing.T) {
	env := decisionPoolFixture(t)
	var published []string
	decisionAutoGatewaySyncHook = func(_ context.Context, _ Object, names []string) error {
		published = append([]string{}, names...)
		return nil
	}
	if err := env.coord.ReconcileAutoRouting(); err != nil {
		t.Fatal(err)
	}
	if len(published) != 0 || boolean(env.coord.decisionAutoPool["available"]) {
		t.Fatal("stopped decision model was advertised")
	}
	env.readyRecord()
	if err := env.coord.ReconcileAutoRouting(); err != nil {
		t.Fatal(err)
	}
	if !sameStrings(published, []string{"glm"}) || !boolean(env.coord.decisionAutoPool["available"]) {
		t.Fatalf("ready decision model missing: %v %v", published, env.coord.decisionAutoPool)
	}
	if str(env.coord.decisionAutoPool["request_model"]) != "spark-decision/auto" {
		t.Fatal(env.coord.decisionAutoPool)
	}
	// Planned stops remove the route before changing the ready generation.
	if err := env.coord.syncAutoRoutesLocked(context.Background(), []string{"glm"}); err != nil {
		t.Fatal(err)
	}
	rec, err := env.rt.readRecord("glm")
	if err != nil || str(rec["state"]) != StateReady || len(published) != 0 {
		t.Fatalf("route was not removed before teardown: %v %v %v", rec, published, err)
	}
}

func TestDecisionAutoFailureBlocksUnloadAndNeverLoadsModels(t *testing.T) {
	env := decisionPoolFixture(t)
	env.readyRecord()
	decisionAutoGatewaySyncHook = func(context.Context, Object, []string) error { return fail("decision gateway unavailable") }
	if _, err := env.coord.Unload("glm", false); err == nil {
		t.Fatal("unload continued after a failed decision route update")
	}
	rec, err := env.rt.readRecord("glm")
	if err != nil || str(rec["state"]) != StateReady {
		t.Fatalf("ready generation changed: %v %v", rec, err)
	}
	for _, alias := range []string{"spark-decision/auto", "auto", decisionAutoUnavailableModel} {
		_, _, err := env.coord.LLMRequest(context.Background(), alias)
		status, ok := err.(interface{ HTTPStatus() int })
		if !ok || status.HTTPStatus() != http.StatusServiceUnavailable {
			t.Fatalf("auto alias did not fail closed: %s %v", alias, err)
		}
	}
}

func TestDecisionAutoConfigRejectsChatAndDuplicateTargets(t *testing.T) {
	cfg := baseConfig()
	cfg["models"] = Object{"chat": llmModel("spark01")}
	cfg["decision_auto"] = Object{"routing": "bifrost", "targets": []any{"chat"}}
	if validateCluster(cfg) == nil {
		t.Fatal("chat target accepted in decision pool")
	}
	obj(obj(cfg["models"])["chat"])["api_kind"] = "decision"
	if err := validateCluster(cfg); err != nil {
		t.Fatal(err)
	}
	obj(cfg["decision_auto"])["targets"] = []any{"chat", "chat"}
	if validateCluster(cfg) == nil {
		t.Fatal("duplicate decision targets accepted")
	}
}

func TestDecisionAutoGatewayKeepsChatRuleAndUsesReadyDecisionProvider(t *testing.T) {
	fake, cfg := newAutoGatewayTest(t)
	if err := syncAutoGateway(context.Background(), cfg, []string{"glm"}); err != nil {
		t.Fatal(err)
	}
	chat := copyObject(fake.named(autoRuleName))
	cfg["decision_auto"] = Object{"routing": "bifrost", "targets": []any{"knowline", "jeffan"}}
	if err := syncDecisionAutoGateway(context.Background(), cfg, []string{"knowline"}); err != nil {
		t.Fatal(err)
	}
	rule := fake.named("sparky-decision-auto")
	if str(rule["cel_expression"]) != "provider == 'spark-decision' && model == 'auto'" {
		t.Fatal(rule)
	}
	if num(rule["priority"]) == num(chat["priority"]) {
		t.Fatal("Bifrost requires distinct priorities for global rules")
	}
	targets := arr(rule["targets"])
	if len(targets) != 1 || str(obj(targets[0])["provider"]) != "spark-decisions" || str(obj(targets[0])["model"]) != "knowline" {
		t.Fatal(targets)
	}
	if !autoRuleEqual(fake.named(autoRuleName), chat) {
		t.Fatal("decision synchronization changed the chat rule")
	}
	if err := syncDecisionAutoGateway(context.Background(), cfg, nil); err != nil {
		t.Fatal(err)
	}
	empty := fake.named("sparky-decision-auto")
	target := obj(arr(empty["targets"])[0])
	if str(target["provider"]) != "spark-decision" || str(target["model"]) != decisionAutoUnavailableModel {
		t.Fatalf("empty pool must use the admitted alias and fail with 503: %v", target)
	}
	puts := fake.puts
	if err := syncDecisionAutoGateway(context.Background(), cfg, nil); err != nil || fake.puts != puts {
		t.Fatalf("unchanged decision rule was rewritten: %v", err)
	}
}
