package sparky

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGateway is a small stand-in for the Bifrost routing rules API.
type fakeGateway struct {
	mu       sync.Mutex
	rules    []any
	nextID   int
	gets     int
	posts    int
	puts     int
	failGET  bool
	failPost bool
	failPUT  bool
}

func newAutoGatewayTest(t *testing.T) (*fakeGateway, Object) {
	t.Helper()

	fake := &fakeGateway{nextID: 100, rules: []any{}}
	srv := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(srv.Close)

	previous := autoGatewayAPI
	autoGatewayAPI = srv.URL
	t.Cleanup(func() { autoGatewayAPI = previous })

	root := t.TempDir()
	gatewayDir := filepath.Join(root, "gateway")
	if err := os.MkdirAll(gatewayDir, 0o755); err != nil {
		t.Fatalf("create gateway dir: %v", err)
	}
	env := "BIFROST_ADMIN_USER=admin\nBIFROST_ADMIN_PASSWORD=s3cret-password-value\n"
	if err := os.WriteFile(filepath.Join(gatewayDir, "gateway.env"), []byte(env), 0o600); err != nil {
		t.Fatalf("write gateway.env: %v", err)
	}

	cfg := Object{"root": root, "auto": Object{"routing": "bifrost"}}
	return fake, cfg
}

func (f *fakeGateway) serve(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != "admin" || pass != "s3cret-password-value" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/routing/rules":
		f.gets++
		if f.failGET {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"BIFROST_ADMIN_PASSWORD=s3cret-password-value leaked"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(Object{"rules": f.rules, "count": len(f.rules)})

	case r.Method == http.MethodPost && r.URL.Path == "/api/routing/rules":
		f.posts++
		if f.failPost {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid rule body"}`))
			return
		}
		var body Object
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.nextID++
		created := obj(body)
		created["id"] = "rule-" + strconv.Itoa(f.nextID)
		created["created_at"] = "2026-11-05T00:00:00Z"
		f.rules = append(f.rules, created)
		_ = json.NewEncoder(w).Encode(Object{"rule": created})

	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/routing/rules/"):
		f.puts++
		if f.failPUT {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/routing/rules/")
		var body Object
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for i, raw := range f.rules {
			if str(obj(raw)["id"]) == id {
				updated := obj(body)
				updated["id"] = id
				updated["created_at"] = obj(raw)["created_at"]
				updated["updated_at"] = "2026-11-05T00:00:01Z"
				f.rules[i] = updated
				_ = json.NewEncoder(w).Encode(Object{"rule": updated})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeGateway) named(name string) Object {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, raw := range f.rules {
		if rule := obj(raw); str(rule["name"]) == name {
			return rule
		}
	}
	return nil
}

func TestAutoGatewayCreatesRuleWithSortedEqualWeights(t *testing.T) {
	fake, cfg := newAutoGatewayTest(t)

	err := autoGatewaySyncHook(context.Background(), cfg, []string{"gamma", "alpha", "gamma", " ", "alpha"})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if fake.posts != 1 || fake.puts != 0 {
		t.Fatalf("posts=%d puts=%d, want 1 POST and 0 PUT", fake.posts, fake.puts)
	}

	rule := fake.named("sparky-spark-auto")
	if rule == nil {
		t.Fatal("rule was not created")
	}
	if str(rule["description"]) != "Sparky ready model pool" || str(rule["cel_expression"]) != "model == 'spark-auto'" {
		t.Fatalf("unexpected rule identity: %v", rule)
	}
	if !boolean(rule["enabled"]) || str(rule["scope"]) != "global" || num(rule["priority"]) != 0 {
		t.Fatalf("unexpected rule control fields: %v", rule)
	}
	if boolean(rule["chain_rule"]) {
		t.Fatalf("chain_rule should be false: %v", rule)
	}

	targets := arr(rule["targets"])
	if len(targets) != 2 {
		t.Fatalf("targets = %v, want two", targets)
	}
	wantModels := []string{"alpha", "gamma"}
	for i, raw := range targets {
		target := obj(raw)
		if str(target["provider"]) != "garuda" || str(target["model"]) != wantModels[i] {
			t.Fatalf("target %d = %v, want garuda/%s", i, target, wantModels[i])
		}
		if num(target["weight"]) != 0.5 {
			t.Fatalf("target %d weight = %v, want 0.5", i, num(target["weight"]))
		}
	}

	fallbacks := arr(rule["fallbacks"])
	if len(fallbacks) != 2 || str(fallbacks[0]) != "garuda/alpha" || str(fallbacks[1]) != "garuda/gamma" {
		t.Fatalf("fallbacks = %v, want [garuda/alpha garuda/gamma]", fallbacks)
	}
}

func TestAutoGatewayRepeatIsNoOp(t *testing.T) {
	fake, cfg := newAutoGatewayTest(t)

	models := []string{"alpha", "beta"}
	if err := autoGatewaySyncHook(context.Background(), cfg, models); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := autoGatewaySyncHook(context.Background(), cfg, []string{"beta", "alpha", "beta"}); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if fake.posts != 1 {
		t.Fatalf("posts = %d, want 1", fake.posts)
	}
	if fake.puts != 0 {
		t.Fatalf("puts = %d, want 0 for an unchanged rule", fake.puts)
	}
}

func TestAutoGatewayUpdatesChangedModels(t *testing.T) {
	fake, cfg := newAutoGatewayTest(t)

	if err := autoGatewaySyncHook(context.Background(), cfg, []string{"alpha"}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := autoGatewaySyncHook(context.Background(), cfg, []string{"beta", "gamma"}); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if fake.posts != 1 || fake.puts != 1 {
		t.Fatalf("posts=%d puts=%d, want 1 POST and 1 PUT", fake.posts, fake.puts)
	}

	targets := arr(fake.named("sparky-spark-auto")["targets"])
	if len(targets) != 2 || str(obj(targets[0])["model"]) != "beta" || str(obj(targets[1])["model"]) != "gamma" {
		t.Fatalf("targets = %v, want beta and gamma", targets)
	}
	if num(obj(targets[0])["weight"]) != 0.5 || num(obj(targets[1])["weight"]) != 0.5 {
		t.Fatalf("weights = %v, want 0.5 each", targets)
	}
}

func TestAutoGatewayEmptyModelsUsesSentinel(t *testing.T) {
	fake, cfg := newAutoGatewayTest(t)

	if err := autoGatewaySyncHook(context.Background(), cfg, nil); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rule := fake.named("sparky-spark-auto")
	targets := arr(rule["targets"])
	if len(targets) != 1 {
		t.Fatalf("targets = %v, want one sentinel", targets)
	}
	sentinel := obj(targets[0])
	if str(sentinel["provider"]) != "garuda" || str(sentinel["model"]) != "sparky-auto-unavailable" {
		t.Fatalf("sentinel = %v", sentinel)
	}
	if num(sentinel["weight"]) != 1 {
		t.Fatalf("sentinel weight = %v, want 1", num(sentinel["weight"]))
	}
	if fallbacks := stringsOf(rule["fallbacks"]); !sameStrings(fallbacks, []string{"garuda/sparky-auto-unavailable"}) {
		t.Fatalf("fallbacks must override caller routes with the unavailable sentinel: %v", fallbacks)
	}
}

func TestAutoGatewayAPIFailure(t *testing.T) {
	fake, cfg := newAutoGatewayTest(t)
	fake.failGET = true

	err := autoGatewaySyncHook(context.Background(), cfg, []string{"alpha"})
	if err == nil {
		t.Fatal("expected an error when the gateway fails")
	}
	if strings.Contains(err.Error(), "s3cret-password-value") {
		t.Fatalf("error leaks the admin password: %v", err)
	}
	if strings.Contains(err.Error(), "admin") {
		t.Fatalf("error leaks the admin user: %v", err)
	}
	if fake.posts != 0 {
		t.Fatalf("posts = %d, want 0 after a failed read", fake.posts)
	}
}

func TestAutoGatewayKeepsUnrelatedRule(t *testing.T) {
	fake, cfg := newAutoGatewayTest(t)

	other := Object{
		"id":          "rule-other",
		"name":        "loan-router",
		"description": "keep me",
		"enabled":     true,
		"scope":       "global",
		"priority":    5,
		"targets":     []any{Object{"provider": "garuda", "model": "keep", "weight": 1.0}},
	}
	fake.rules = append(fake.rules, other)

	if err := autoGatewaySyncHook(context.Background(), cfg, []string{"alpha"}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	kept := fake.named("loan-router")
	if kept == nil {
		t.Fatal("unrelated rule was removed")
	}
	if str(kept["description"]) != "keep me" || num(kept["priority"]) != 5 {
		t.Fatalf("unrelated rule changed: %v", kept)
	}
	if len(arr(kept["targets"])) != 1 {
		t.Fatalf("unrelated rule targets changed: %v", kept["targets"])
	}
}

func TestAutoGatewaySkipsWhenRoutingIsNotBifrost(t *testing.T) {
	fake, cfg := newAutoGatewayTest(t)

	cfg["auto"] = Object{"routing": "direct"}
	if err := autoGatewaySyncHook(context.Background(), cfg, []string{"alpha"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if fake.gets != 0 || fake.posts != 0 || fake.puts != 0 {
		t.Fatalf("gateway was called for non-bifrost routing: gets=%d posts=%d puts=%d", fake.gets, fake.posts, fake.puts)
	}
}

func TestAutoGatewayDuplicateNameIsSafeError(t *testing.T) {
	fake, cfg := newAutoGatewayTest(t)

	for i := 0; i < 2; i++ {
		fake.rules = append(fake.rules, Object{
			"id":      "rule-dup-" + strconv.Itoa(i),
			"name":    "sparky-spark-auto",
			"targets": []any{Object{"provider": "garuda", "model": "alpha", "weight": 1.0}},
		})
	}

	err := autoGatewaySyncHook(context.Background(), cfg, []string{"alpha"})
	if err == nil {
		t.Fatal("expected an error for duplicate rule names")
	}
	if fake.posts != 0 || fake.puts != 0 {
		t.Fatalf("posts=%d puts=%d, want no writes for a duplicate rule", fake.posts, fake.puts)
	}
}
