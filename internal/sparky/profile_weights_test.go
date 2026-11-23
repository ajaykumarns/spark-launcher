package sparky

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func weightedProfileFixture() Object {
	return Object{"auto": Object{"routing": "bifrost", "targets": []any{"large", "small"}}, "models": Object{
		"large":    Object{"kind": "llm", "aliases": []any{"primary"}, "settings": Object{"spark01": Object{}}},
		"small":    Object{"kind": "llm", "settings": Object{"spark02": Object{}}},
		"decision": Object{"kind": "llm", "api_kind": "decision", "settings": Object{"spark01": Object{}}},
	}}
}
func weightedProfileYAML(weights string) string {
	return "version: 1\nprofiles:\n  pair:\n" + weights + "    capabilities:\n      chat:\n        - large: spark01\n        - small: spark02\n"
}
func TestProfileAutoWeightsValidation(t *testing.T) {
	cases := []struct {
		name, weights string
		valid         bool
	}{
		{"omitted", "", true},
		{"fractions", "    auto_weights: {large: 0.65, small: 0.35}\n", true},
		{"relative and alias", "    auto_weights: {primary: 65, small: 35}\n", true},
		{"missing provider", "    auto_weights: {large: 65}\n", false},
		{"unknown", "    auto_weights: {large: 65, missing: 35}\n", false},
		{"decision", "    auto_weights: {large: 65, small: 35, decision: 1}\n", false},
		{"duplicate alias", "    auto_weights: {large: 65, primary: 2, small: 35}\n", false},
		{"zero", "    auto_weights: {large: 0, small: 1}\n", false},
		{"negative", "    auto_weights: {large: -1, small: 1}\n", false},
		{"string", "    auto_weights: {large: '0.65', small: 0.35}\n", false},
		{"nan", "    auto_weights: {large: .nan, small: 1}\n", false},
		{"infinite", "    auto_weights: {large: .inf, small: 1}\n", false},
		{"overflow", "    auto_weights: {large: 1e308, small: 1e308}\n", false},
		{"underflow", "    auto_weights: {large: 1e-308, small: 1e308}\n", false},
		{"empty", "    auto_weights: {}\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "profiles.yaml")
			if err := os.WriteFile(file, []byte(weightedProfileYAML(tc.weights)), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := readProfile(file, "pair", weightedProfileFixture())
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v got %v", tc.valid, err)
			}
			if tc.valid && tc.weights != "" {
				if num(autoReadyWeights([]string{"large", "small"}, p.autoWeights)["large"]) != .65 {
					t.Fatal(p.autoWeights)
				}
			}
		})
	}
}
func TestWeightedGatewaySyncNormalizesReadyTargetsAndKeepsDecisionRuleEqual(t *testing.T) {
	fake, cfg := newAutoGatewayTest(t)
	obj(cfg["auto"])["profile_weights"] = Object{"large": 65.0, "small": 35.0}
	cfg["decision_auto"] = Object{"routing": "bifrost"}
	for _, models := range [][]string{{"small", "large", "large", "outside"}, {"large", "small"}} {
		if err := syncAutoGateway(context.Background(), cfg, models); err != nil {
			t.Fatal(err)
		}
	}
	rule := fake.named(autoRuleName)
	targets := arr(rule["targets"])
	if len(targets) != 2 || num(obj(targets[0])["weight"]) != .65 || num(obj(targets[1])["weight"]) != .35 || fake.puts != 0 {
		t.Fatalf("weighted rule not idempotent: %v, puts=%d", rule, fake.puts)
	}
	if err := syncAutoGateway(context.Background(), cfg, []string{"small"}); err != nil {
		t.Fatal(err)
	}
	targets = arr(fake.named(autoRuleName)["targets"])
	if len(targets) != 1 || str(obj(targets[0])["model"]) != "small" || num(obj(targets[0])["weight"]) != 1 {
		t.Fatal(targets)
	}
	if err := syncAutoGateway(context.Background(), cfg, nil); err != nil {
		t.Fatal(err)
	}
	if str(obj(arr(fake.named(autoRuleName)["targets"])[0])["model"]) != autoUnavailableModel {
		t.Fatal("empty pool lost sentinel")
	}
	if err := syncDecisionAutoGateway(context.Background(), cfg, []string{"large", "small"}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range arr(fake.named("sparky-decision-auto")["targets"]) {
		if num(obj(raw)["weight"]) != .5 {
			t.Fatal("chat weights changed decision routing")
		}
	}
}
func TestProfileWeightsOnlyApplyUsesTargetAndReconcileReloadsFile(t *testing.T) {
	env := bifrostPoolFixture(t)
	env.dev.Reachable = false
	env.dev.Units["comfyui.service"] = "inactive"
	env.readyRecord()
	file := filepath.Join(filepath.Dir(env.path), "profiles.yaml")
	write := func(weight string) {
		t.Helper()
		data := "version: 1\nprofiles:\n  old:\n    auto_weights: {glm: 1}\n    capabilities:\n      chat:\n        - glm: spark01\n  next:\n    auto_weights: {glm: " + weight + "}\n    capabilities:\n      chat:\n        - glm: spark01\n"
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("9")
	firstPlan, err := env.coord.Profile("next", false)
	if err != nil {
		t.Fatal(err)
	}
	write("4")
	changedPlan, err := env.coord.Profile("next", false)
	if err != nil || firstPlan["plan_id"] == changedPlan["plan_id"] {
		t.Fatal("weight change did not invalidate the reviewed plan")
	}
	write("9")
	if err := writeObject(profileStatePath(env.coord.Config()), Object{"name": "old", "capabilities": []any{}}); err != nil {
		t.Fatal(err)
	}
	seen := []float64{}
	autoGatewaySyncHook = func(_ context.Context, cfg Object, _ []string) error {
		seen = append(seen, num(obj(obj(cfg["auto"])["profile_weights"])["glm"]))
		return nil
	}
	before, err := env.rt.readRecord("glm")
	if err != nil {
		t.Fatal(err)
	}
	result, err := env.coord.Profile("next", true)
	if err != nil || str(result["status"]) != "applied" || len(seen) == 0 || seen[len(seen)-1] != 9 {
		t.Fatalf("target weights not synced before saving profile: %v %v %v", result, seen, err)
	}
	after, err := env.rt.readRecord("glm")
	if err != nil || str(before["generation"]) != str(after["generation"]) {
		t.Fatal("weights-only apply restarted the model")
	}
	write("4")
	if _, err := env.coord.Profile("next", true); err != nil {
		t.Fatal(err)
	}
	if seen[len(seen)-1] != 4 {
		t.Fatal("same-profile weight edit was not synced")
	}
	write("2")
	if err := env.coord.ReconcileAutoRouting(); err != nil {
		t.Fatal(err)
	}
	if seen[len(seen)-1] != 2 {
		t.Fatal("reconcile did not read saved profile weights")
	}
	write("-1")
	calls := len(seen)
	if err := env.coord.ReconcileAutoRouting(); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatal("invalid saved weights accepted")
	}
	if len(seen) != calls {
		t.Fatal("invalid configuration changed gateway rules")
	}
}
