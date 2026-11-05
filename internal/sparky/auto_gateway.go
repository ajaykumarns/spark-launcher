package sparky

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// autoGatewayAPI is the controller's local Bifrost administration endpoint.
var autoGatewayAPI = "http://127.0.0.1:8081"

// autoGatewaySyncHook points at the sync entry point so callers stay swappable.
var autoGatewaySyncHook = syncAutoGateway

const (
	autoRuleName         = "sparky-spark-auto"
	autoRuleDescription  = "Sparky ready model pool"
	autoRuleCEL          = "model == 'spark-auto'"
	autoUnavailableModel = "sparky-auto-unavailable"
	autoMaxResponseBytes = 2 << 20
)

// syncAutoGateway makes the Bifrost rule named "sparky-spark-auto" point at
// the supplied ready models. An empty model list installs one sentinel
// target. Rules with other names are never touched.
func syncAutoGateway(ctx context.Context, cfg Object, models []string) error {
	if str(obj(cfg["auto"])["routing"]) != "bifrost" {
		return nil
	}

	user, pass, err := autoAdminCredentials(str(cfg["root"]))
	if err != nil {
		return err
	}

	targets, fallbacks := autoRuleTargets(models)
	desired := Object{
		"name":           autoRuleName,
		"description":    autoRuleDescription,
		"enabled":        true,
		"chain_rule":     false,
		"scope":          "global",
		"priority":       0,
		"cel_expression": autoRuleCEL,
		"targets":        targets,
		"fallbacks":      fallbacks,
	}

	client := httpClient(8 * time.Second)
	rules, err := autoFetchRules(ctx, client, user, pass)
	if err != nil {
		return err
	}

	var current Object
	matches := 0
	for _, raw := range rules {
		rule := obj(raw)
		if rule != nil && str(rule["name"]) == autoRuleName {
			matches++
			current = rule
		}
	}

	switch {
	case matches > 1:
		return fail("gateway has %d rules named %q, expected at most one", matches, autoRuleName)
	case matches == 0:
		created, err := autoGatewayRequest(ctx, client, user, pass, http.MethodPost, "/api/routing/rules", desired)
		if err != nil {
			return err
		}
		if str(obj(created["rule"])["id"]) == "" {
			return fail("gateway did not return the created rule")
		}
		return nil
	case autoRuleEqual(current, desired):
		return nil
	}

	id := str(current["id"])
	if id == "" {
		return fail("gateway rule %q has no id", autoRuleName)
	}
	path := "/api/routing/rules/" + url.PathEscape(id)
	if _, err := autoGatewayRequest(ctx, client, user, pass, http.MethodPut, path, desired); err != nil {
		return err
	}
	return nil
}

// autoAdminCredentials reads the Bifrost admin credentials from the isolated
// gateway env file. Error text never carries the credential values.
func autoAdminCredentials(root string) (string, string, error) {
	env, err := readEnv(filepath.Join(root, "gateway", "gateway.env"))
	if err != nil {
		return "", "", fail("read gateway admin config: %v", err)
	}
	user := env["BIFROST_ADMIN_USER"]
	pass := env["BIFROST_ADMIN_PASSWORD"]
	if user == "" || pass == "" {
		return "", "", fail("gateway admin user or password is not set in gateway.env")
	}
	return user, pass, nil
}

// autoRuleTargets builds the target list and fallback list for one sync.
// Models are sorted and deduplicated. Weights are always above zero.
func autoRuleTargets(models []string) ([]any, []any) {
	seen := make(map[string]bool, len(models))
	clean := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		clean = append(clean, model)
	}

	if len(clean) == 0 {
		sentinel := Object{"provider": "garuda", "model": autoUnavailableModel, "weight": 1.0}
		// Bifrost only replaces caller fallbacks when a rule supplies a
		// nonempty list. Keep the sentinel as the sole fallback as well.
		return []any{sentinel}, []any{"garuda/" + autoUnavailableModel}
	}

	sort.Strings(clean)
	weight := 1.0 / float64(len(clean))
	targets := make([]any, 0, len(clean))
	fallbacks := make([]any, 0, len(clean))
	for _, model := range clean {
		targets = append(targets, Object{"provider": "garuda", "model": model, "weight": weight})
		fallbacks = append(fallbacks, "garuda/"+model)
	}
	return targets, fallbacks
}

// autoFetchRules reads all routing rules that live in memory.
func autoFetchRules(ctx context.Context, client *http.Client, user, pass string) ([]any, error) {
	out, err := autoGatewayRequest(ctx, client, user, pass, http.MethodGet, "/api/routing/rules?from_memory=true", nil)
	if err != nil {
		return nil, err
	}
	raw, ok := out["rules"]
	if !ok {
		return nil, fail("gateway rule list is missing")
	}
	rules, ok := raw.([]any)
	if !ok {
		return nil, fail("gateway rule list is not a list")
	}
	return rules, nil
}

// autoGatewayRequest sends one Basic auth request to the gateway. Error
// text never carries credentials or response bodies.
func autoGatewayRequest(ctx context.Context, client *http.Client, user, pass, method, path string, body Object) (Object, error) {
	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fail("encode gateway request: %v", err)
		}
		reqBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, autoGatewayAPI+path, reqBody)
	if err != nil {
		return nil, fail("build gateway request: %v", err)
	}
	req.SetBasicAuth(user, pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fail("gateway %s %s failed: %v", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, autoMaxResponseBytes))
	if err != nil {
		return nil, fail("read gateway response: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fail("gateway %s %s returned status %d", method, path, resp.StatusCode)
	}

	var out Object
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fail("gateway %s %s returned invalid JSON", method, path)
	}
	return out, nil
}

// autoRuleEqual compares only the effective rule fields. Generated fields
// such as id, created_at, and updated_at are ignored. Absent fallbacks match
// an empty list, and an absent chain_rule matches false.
func autoRuleEqual(current, desired Object) bool {
	if str(current["name"]) != autoRuleName ||
		str(current["description"]) != autoRuleDescription ||
		!boolean(current["enabled"]) ||
		boolean(current["chain_rule"]) ||
		str(current["scope"]) != "global" ||
		num(current["priority"]) != 0 ||
		str(current["cel_expression"]) != autoRuleCEL {
		return false
	}

	currentTargets, desiredTargets := arr(current["targets"]), arr(desired["targets"])
	if len(currentTargets) != len(desiredTargets) {
		return false
	}
	for i := range currentTargets {
		a, b := obj(currentTargets[i]), obj(desiredTargets[i])
		if a == nil || b == nil {
			return false
		}
		if str(a["provider"]) != str(b["provider"]) || str(a["model"]) != str(b["model"]) || str(a["key_id"]) != "" {
			return false
		}
		if diff := num(a["weight"]) - num(b["weight"]); diff < -1e-9 || diff > 1e-9 {
			return false
		}
	}

	currentFallbacks, desiredFallbacks := arr(current["fallbacks"]), arr(desired["fallbacks"])
	if len(currentFallbacks) != len(desiredFallbacks) {
		return false
	}
	for i := range currentFallbacks {
		if str(currentFallbacks[i]) != str(desiredFallbacks[i]) {
			return false
		}
	}
	return true
}
