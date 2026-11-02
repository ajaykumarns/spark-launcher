package sparky

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// harnessNames lists the supported CLI harnesses.
var harnessNames = []string{"claude", "hermes", "codex", "opencode"}

// harnessExecutable resolves the harness binary from the site config or PATH,
// expanding ~ and returning an absolute path to an executable file.
func harnessExecutable(cfg clientConfig, harness string) (string, error) {
	binary := ""
	if b, ok := cfg.Binaries[harness]; ok && b != "" {
		binary = expand(b)
	} else if p, err := exec.LookPath(harness); err == nil {
		binary = p
	}
	binary = expand(binary)
	if binary != "" {
		if abs, err := filepath.Abs(binary); err == nil {
			binary = abs
		}
	}
	info, err := os.Stat(binary)
	if err != nil || info.IsDir() || !harnessRunnableFor(info, os.Getuid(), groupIDs()) {
		return "", fail("%s is not installed or executable. Install it or set binaries.%s in the site file.", harness, harness)
	}
	return binary, nil
}

// groupIDs returns the current user's supplementary groups.
func groupIDs() []int {
	gids, err := os.Getgroups()
	if err != nil {
		return nil
	}
	return gids
}

// harnessRunnableFor is the pure access check: the owner-execute bit applies
// when the file belongs to the user, the group bit when the user is in the
// file's group, and otherwise the other-execute bit.
func harnessRunnableFor(info os.FileInfo, uid int, groups []int) bool {
	perm := info.Mode().Perm()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat != nil {
		switch {
		case uint32(uid) == stat.Uid:
			return perm&0100 != 0
		case containsInt(groups, int(stat.Gid)):
			return perm&0010 != 0
		}
		return perm&0001 != 0
	}
	return perm&0111 != 0
}

// containsInt reports whether xs contains n.
func containsInt(xs []int, n int) bool {
	for _, x := range xs {
		if x == n {
			return true
		}
	}
	return false
}

// buildHarnessPlan builds the per-launch native plan. The JSON shape mirrors
// Python build_plan: harness, binary, args, environment_overrides,
// environment_unset, model, project, session.
func buildHarnessPlan(cfg clientConfig, catalog Object, model Object, harness, project, session string) (Object, error) {
	if !contains(harnessNames, harness) {
		return nil, fail("Choose claude, hermes, codex, or opencode.")
	}
	origin, prefix := cfg.Gateway, cfg.ModelPrefix
	modelID := str(model["id"])
	if !integer(model["context_tokens"]) || int(num(model["context_tokens"])) <= 0 {
		return nil, fail("Model context budget is invalid for %s.", modelID)
	}
	if _, present := model["output_tokens"]; present {
		if !integer(model["output_tokens"]) || int(num(model["output_tokens"])) < 0 {
			return nil, fail("Model output budget is invalid for %s.", modelID)
		}
	}
	context := int(num(model["context_tokens"]))
	timeout := cfg.TimeoutSeconds * 1000
	binary, err := harnessExecutable(cfg, harness)
	if err != nil {
		return nil, err
	}
	plan := Object{
		"harness":               harness,
		"binary":                binary,
		"args":                  []any{},
		"environment_overrides": Object{},
		"environment_unset":     []any{},
		"model":                 model,
		"project":               clientResolveProject(project),
		"session":               session,
	}
	env := obj(plan["environment_overrides"])
	headers := Object{"X-Client-ID": "sparky/" + harness, "X-Session-ID": session}
	headerText := ""
	for _, k := range keys(headers) {
		if headerText != "" {
			headerText += "\n"
		}
		headerText += k + ": " + str(headers[k])
	}
	args := func(xs ...string) {
		for _, x := range xs {
			plan["args"] = append(arr(plan["args"]), x)
		}
	}
	switch harness {
	case "claude":
		env["ANTHROPIC_BASE_URL"] = origin + "/anthropic"
		env["ANTHROPIC_AUTH_TOKEN"] = "spark-keyless"
		env["ANTHROPIC_MODEL"] = modelID
		env["ANTHROPIC_CUSTOM_MODEL_OPTION"] = modelID
		env["ANTHROPIC_CUSTOM_MODEL_OPTION_NAME"] = str(value(model, "name", modelID))
		env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] = itoa(context)
		env["ANTHROPIC_CUSTOM_HEADERS"] = headerText
		env["API_TIMEOUT_MS"] = itoa(timeout)
		for _, name := range []string{"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL"} {
			env[name] = modelID
		}
		plan["environment_unset"] = anyStrings([]string{
			"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
			"CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_AWS"})
		args("--model", modelID)
	case "codex":
		values := Object{
			"model_provider":                               "spark",
			"model":                                        modelID,
			"review_model":                                 modelID,
			"model_context_window":                         context,
			"model_auto_compact_token_limit":               context * cfg.CompactPercent / 100,
			"model_providers.spark.name":                   "Spark Gateway",
			"model_providers.spark.base_url":               origin + "/v1",
			"model_providers.spark.wire_api":               "responses",
			"model_providers.spark.requires_openai_auth":   false,
			"model_providers.spark.supports_websockets":    false,
			"model_providers.spark.stream_idle_timeout_ms": timeout,
		}
		for _, k := range keys(headers) {
			values["model_providers.spark.http_headers."+k] = str(headers[k])
		}
		for _, k := range keys(values) {
			v := values[k]
			if s, ok := v.(string); ok {
				args("-c", k+"="+jsonString(s))
			} else {
				b, _ := json.Marshal(v)
				args("-c", k+"="+string(b))
			}
		}
	case "opencode":
		models := Object{}
		for _, itemAny := range arr(catalog["models"]) {
			item := obj(itemAny)
			id := str(item["id"])
			limits := Object{"context": int(num(item["context_tokens"]))}
			if int(num(value(item, "output_tokens", 0))) > 0 {
				limits["output"] = int(num(item["output_tokens"]))
			}
			inputs := stringsOf(item["inputs"])
			if len(inputs) == 0 {
				inputs = []string{"text"}
			}
			models[strings.TrimPrefix(id, prefix)] = Object{
				"id": id, "name": str(value(item, "name", id)), "limit": limits,
				"toolcall": true, "reasoning": boolean(value(item, "reasoning", false)),
				"modalities": Object{"input": anyStrings(inputs), "output": anyStrings([]string{"text"})},
			}
		}
		selected := "spark/" + strings.TrimPrefix(modelID, prefix)
		raw := Object{
			"enabled_providers": anyStrings([]string{"spark"}),
			"provider": Object{"spark": Object{
				"npm": "@ai-sdk/openai-compatible", "name": "Spark Gateway",
				"options": Object{"baseURL": origin + "/v1", "apiKey": "spark-keyless",
					"timeout": timeout, "headers": headers},
				"models": models}},
			"model":       selected,
			"small_model": selected,
		}
		env["OPENCODE_CONFIG_CONTENT"] = string(jsonBytes(raw))
		args("--model", selected)
	default: // hermes
		args("--provider", "spark-gateway", "--model", modelID)
		env["HERMES_INFERENCE_MODEL"] = modelID
	}
	return plan, nil
}

// clientResolveProject expands ~ and makes the project path absolute.
func clientResolveProject(project string) string {
	p := expand(project)
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// harnessChildEnv computes the child process environment: removals, then
// overrides, then the OpenCode inherited-config merge.
func harnessChildEnv(plan Object, original map[string]string) (map[string]string, error) {
	env := map[string]string{}
	for k, v := range original {
		env[k] = v
	}
	for _, k := range stringsOf(plan["environment_unset"]) {
		delete(env, k)
	}
	for k, v := range obj(plan["environment_overrides"]) {
		env[k] = str(v)
	}
	if str(plan["harness"]) == "opencode" {
		inherited, ok := original["OPENCODE_CONFIG_CONTENT"]
		if ok && inherited != "" {
			var parsed Object
			if err := json.Unmarshal([]byte(inherited), &parsed); err != nil || parsed == nil {
				return nil, fail("OPENCODE_CONFIG_CONTENT must contain valid JSON.")
			}
			// Absent provider defaults to an empty mapping (Python setdefault);
			// only an explicitly non-object provider is rejected.
			if v, present := parsed["provider"]; present {
				if _, ok := v.(map[string]any); !ok {
					return nil, fail("OpenCode process settings and provider must be mappings.")
				}
			}
			var owned Object
			if err := json.Unmarshal([]byte(str(obj(plan["environment_overrides"])["OPENCODE_CONFIG_CONTENT"])), &owned); err != nil {
				return nil, fail("OpenCode process settings and provider must be mappings.")
			}
			// Capture the inherited enabled_providers list before the owned
			// settings overwrite it (matching Python child_env semantics).
			enabled, hadEnabled := parsed["enabled_providers"].([]any)
			ownedProvider := obj(owned["provider"])
			delete(owned, "provider")
			providers := obj(parsed["provider"])
			for k, v := range ownedProvider {
				providers[k] = v
			}
			parsed["provider"] = providers
			for k, v := range owned {
				parsed[k] = v
			}
			if hadEnabled {
				seen := map[string]bool{}
				merged := []any{}
				for _, e := range append(enabled, "spark") {
					s := str(e)
					if !seen[s] {
						seen[s] = true
						merged = append(merged, s)
					}
				}
				parsed["enabled_providers"] = merged
			}
			b, _ := json.Marshal(parsed)
			env["OPENCODE_CONFIG_CONTENT"] = string(b)
		}
	}
	return env, nil
}

// checkHarnessPassthrough rejects user routing/model/provider/context/catalog
// overrides. After a literal "--" delimiter, all text is prompt and allowed.
func checkHarnessPassthrough(harness string, extra []string) error {
	for index, arg := range extra {
		if arg == "--" {
			break
		}
		if arg == "--model" || arg == "-m" || arg == "--provider" || arg == "--oss" || arg == "--local-provider" ||
			strings.HasPrefix(arg, "--model=") || strings.HasPrefix(arg, "--provider=") ||
			strings.HasPrefix(arg, "--local-provider=") ||
			(strings.HasPrefix(arg, "-m") && !strings.HasPrefix(arg, "--")) {
			return fail("Select the model and provider through sparky.")
		}
		if harness == "codex" {
			value := ""
			have := false
			switch {
			case arg == "-c" || arg == "--config":
				if index+1 < len(extra) {
					value, have = extra[index+1], true
				}
			case strings.HasPrefix(arg, "--config="):
				value, have = strings.SplitN(arg, "=", 2)[1], true
			case strings.HasPrefix(arg, "-c") && len(arg) > 2:
				value, have = arg[2:], true
			}
			if have {
				key := harnessTOMLKey(value)
				if contains([]string{"model", "review_model", "model_provider", "model_context_window",
					"model_catalog_json", "model_auto_compact_token_limit"}, key) ||
					strings.HasPrefix(key, "model_providers") {
					return fail("Set Codex model routing and context through sparky.")
				}
			}
		}
	}
	return nil
}

// harnessTOMLKey extracts the normalized root key from a `-c key=value`
// fragment: whitespace-trimmed, with matching surrounding single or double
// quotes and any trailing whitespace removed.
func harnessTOMLKey(value string) string {
	keyPart := strings.TrimSpace(strings.SplitN(value, "=", 2)[0])
	for {
		keyPart = strings.TrimSpace(keyPart)
		if len(keyPart) >= 2 &&
			((keyPart[0] == '"' && keyPart[len(keyPart)-1] == '"') ||
				(keyPart[0] == '\'' && keyPart[len(keyPart)-1] == '\'')) {
			keyPart = keyPart[1 : len(keyPart)-1]
			continue
		}
		break
	}
	return keyPart
}

// jsonString encodes a Go string as a JSON scalar (valid TOML string too).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// --- prepare/launch effects (ported from adapters.py) ---

// harnessExecRunner is the injectable command runner used for Codex schema
// inspection and child launches (defaults to the shared runner).
var harnessExecRunner = runCommand

// harnessOwnedProvider builds the owned Hermes spark-gateway provider mapping.
func harnessOwnedProvider(cfg clientConfig, catalog Object) Object {
	timeout := cfg.TimeoutSeconds
	models := Object{}
	for _, mAny := range arr(catalog["models"]) {
		m := obj(mAny)
		models[str(m["id"])] = Object{
			"context_length":        int(num(m["context_tokens"])),
			"timeout_seconds":       timeout,
			"stale_timeout_seconds": timeout,
		}
	}
	return Object{
		"name":                    "Spark Gateway",
		"api":                     cfg.Gateway + "/v1",
		"transport":               "chat_completions",
		"discover_models":         false,
		"request_timeout_seconds": timeout,
		"stale_timeout_seconds":   timeout,
		"models":                  models,
		"extra_headers":           Object{"X-Client-ID": "sparky/hermes"},
	}
}

// harnessAnchorsIn collects anchor names defined within a node subtree.
func harnessAnchorsIn(node *yaml.Node, into map[string]bool) {
	if node == nil {
		return
	}
	if node.Anchor != "" {
		into[node.Anchor] = true
	}
	for _, child := range node.Content {
		harnessAnchorsIn(child, into)
	}
}

// harnessAliasesTo reports whether the subtree contains an alias node to any
// of the given anchor names.
func harnessAliasesTo(node *yaml.Node, anchors map[string]bool) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.AliasNode && anchors[node.Value] {
		return true
	}
	for _, child := range node.Content {
		if harnessAliasesTo(child, anchors) {
			return true
		}
	}
	return false
}

// harnessAliasesToExcept reports whether the subtree contains an alias to any
// of the given anchor names, ignoring the skip subtree (the owned block).
func harnessAliasesToExcept(node *yaml.Node, anchors map[string]bool, skip *yaml.Node) bool {
	if node == nil || node == skip {
		return false
	}
	if node.Kind == yaml.AliasNode && anchors[node.Value] {
		return true
	}
	for _, child := range node.Content {
		if harnessAliasesToExcept(child, anchors, skip) {
			return true
		}
	}
	return false
}

// harnessAliasesOutside reports whether the subtree aliases any anchor that
// is not defined within the subtree itself.
func harnessAliasesOutside(node *yaml.Node, anchors map[string]bool) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.AliasNode && !anchors[node.Value] {
		return true
	}
	for _, child := range node.Content {
		if harnessAliasesOutside(child, anchors) {
			return true
		}
	}
	return false
}

// harnessCheckDuplicateKeys rejects ambiguous duplicate mapping keys anywhere
// in the document before any write is attempted.
func harnessCheckDuplicateKeys(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i].Value
			if seen[key] {
				return fail("Hermes configuration has a duplicate key %q.", key)
			}
			seen[key] = true
		}
	}
	for _, child := range node.Content {
		if err := harnessCheckDuplicateKeys(child); err != nil {
			return err
		}
	}
	return nil
}

// harnessSetMappingValue sets a key's value node in a mapping, replacing the
// existing value in place (transferring its comments) or appending the key.
func harnessSetMappingValue(node *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			old := node.Content[i+1]
			val.HeadComment = old.HeadComment
			val.LineComment = old.LineComment
			val.FootComment = old.FootComment
			node.Content[i+1] = val
			return
		}
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

// harnessNodeFromValue encodes a Go value as a styleless yaml node.
func harnessNodeFromValue(v any) (*yaml.Node, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	n := doc.Content[0]
	n.Style = 0
	return n, nil
}

// harnessSyncHermes updates only the providers.spark-gateway block of the
// Hermes config, preserving all other structure (comments, quotes, anchors).
// Returns the backup path when a change was written, "" when unchanged.
func harnessSyncHermes(cfg clientConfig, catalog Object, session string) (string, error) {
	backup, _, err := harnessSyncHermesChanged(cfg, catalog, session)
	return backup, err
}

// harnessSyncHermesChanged is harnessSyncHermes with an explicit changed
// result: an empty backup means BOTH "unchanged" and "newly created", so
// callers must use changed, never the backup path, to report the status.
func harnessSyncHermesChanged(cfg clientConfig, catalog Object, session string) (string, bool, error) {
	root := expand(envDefault("HERMES_HOME", filepath.Join(home(), ".hermes")))
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", false, err
	}
	path := filepath.Join(root, "config.yaml")
	lock := filepath.Join(root, "config.yaml.spark-lock")
	var backup string
	var changed bool
	err := withFileLock(lock, false, func() error {
		original, readErr := os.ReadFile(path)
		if readErr != nil && !os.IsNotExist(readErr) {
			return readErr
		}
		var doc yaml.Node
		if len(original) > 0 {
			// Reject multi-document streams deliberately before any write.
			decoder := yaml.NewDecoder(bytes.NewReader(original))
			var first yaml.Node
			if err := decoder.Decode(&first); err != nil && !errors.Is(err, io.EOF) {
				return fail("Hermes configuration must be a mapping.")
			}
			var extra yaml.Node
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				return fail("Hermes configuration must be a single document.")
			}
			doc = first
		}
		// Empty document (blank or comment-only): synthesize a mapping root,
		// carrying over any leading comment lines as its header comment.
		if doc.Kind == 0 {
			root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			var lines []string
			for _, line := range strings.Split(string(original), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "#") {
					lines = append(lines, trimmed)
					continue
				}
				if trimmed != "" {
					break
				}
			}
			root.HeadComment = strings.Join(lines, "\n")
			doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
		}
		rootNode := doc.Content[0]
		// Null document (comment-only or "null"): convert the root in place
		// so header/footer comments attached to it are preserved.
		if rootNode.Kind == yaml.ScalarNode && rootNode.Tag == "!!null" {
			rootNode.Kind = yaml.MappingNode
			rootNode.Tag = "!!map"
			rootNode.Value = ""
			rootNode.Style = 0
			rootNode.Content = nil
		}
		if rootNode.Kind != yaml.MappingNode {
			return fail("Hermes configuration must be a mapping.")
		}
		// Duplicate keys are ambiguous and must fail before any write.
		if err := harnessCheckDuplicateKeys(rootNode); err != nil {
			return err
		}
		// Locate providers mapping, validating shapes.
		var providersNode *yaml.Node
		for i := 0; i+1 < len(rootNode.Content); i += 2 {
			if rootNode.Content[i].Value == "providers" {
				providersNode = rootNode.Content[i+1]
			}
		}
		if providersNode == nil {
			providersNode = &yaml.Node{Kind: yaml.MappingNode}
			rootNode.Content = append(rootNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "providers"}, providersNode)
		}
		if providersNode.Kind != yaml.MappingNode {
			return fail("Hermes providers must be a mapping.")
		}
		// Anchors into the owned provider may appear anywhere in the document
		// (root level, nested mappings, sequences). Reject them document-wide
		// so replacing the owned block never silently alters other settings.
		// Anchors defined elsewhere but aliased FROM the owned block are also
		// rejected: the update must not freeze another block's value by value.
		providerIndex := -1
		var ownedOld *yaml.Node
		for i := 0; i+1 < len(providersNode.Content); i += 2 {
			if providersNode.Content[i].Value == "spark-gateway" {
				providerIndex = i
				ownedOld = providersNode.Content[i+1]
			}
		}
		if ownedOld != nil && ownedOld.Kind != yaml.MappingNode {
			return fail("Hermes Spark provider must be a mapping.")
		}
		ownedAnchors := map[string]bool{}
		harnessAnchorsIn(ownedOld, ownedAnchors)
		if harnessAliasesOutside(ownedOld, ownedAnchors) {
			return fail("Hermes Spark provider aliases into other configuration; restructure the file to update it safely.")
		}
		if harnessAliasesToExcept(rootNode, ownedAnchors, ownedOld) {
			return fail("Hermes provider aliases into the Spark provider block; restructure the file to update it safely.")
		}
		// Validate existing extra_headers mapping if present.
		if ownedOld != nil {
			for i := 0; i+1 < len(ownedOld.Content); i += 2 {
				if ownedOld.Content[i].Value == "extra_headers" && ownedOld.Content[i+1].Kind != yaml.MappingNode {
					return fail("Hermes Spark extra_headers must be a mapping.")
				}
			}
		}
		// Update the existing provider node IN PLACE: unknown keys keep their
		// original nodes (comments and quote styles intact); only owned
		// fields and extra_headers.X-Client-ID are updated or added.
		owned := harnessOwnedProvider(cfg, catalog)
		var ownedNode *yaml.Node
		if ownedOld != nil {
			// extra_headers: update/add only X-Client-ID.
			var headersNode *yaml.Node
			for i := 0; i+1 < len(ownedOld.Content); i += 2 {
				if ownedOld.Content[i].Value == "extra_headers" {
					headersNode = ownedOld.Content[i+1]
				}
			}
			if headersNode == nil {
				headersNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				harnessSetMappingValue(ownedOld, "extra_headers",
					&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
			}
			harnessSetMappingValue(headersNode, "X-Client-ID",
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "sparky/hermes"})
			// Owned fields: replace value nodes, keeping key nodes (and their
			// comments) and transferring relevant value comments.
			for _, k := range []string{"name", "api", "transport", "discover_models",
				"request_timeout_seconds", "stale_timeout_seconds", "models"} {
				v, err := harnessNodeFromValue(owned[k])
				if err != nil {
					return err
				}
				harnessSetMappingValue(ownedOld, k, v)
			}
			ownedNode = ownedOld
		} else {
			var err error
			ownedNode, err = harnessNodeFromValue(owned)
			if err != nil {
				return err
			}
		}
		if providerIndex >= 0 {
			providersNode.Content[providerIndex+1] = ownedNode
		} else {
			providersNode.Content = append(providersNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "spark-gateway"}, ownedNode)
		}
		encoded, err := yaml.Marshal(&doc)
		if err != nil {
			return err
		}
		// Prove unrelated semantic structure is unchanged: re-parse both
		// versions, drop spark-gateway, and compare canonically.
		strip := func(data []byte) ([]byte, error) {
			var probe map[string]any
			if err := yaml.Unmarshal(data, &probe); err != nil {
				return nil, err
			}
			if probe == nil {
				probe = map[string]any{}
			}
			// Normalize a missing/null providers key to an empty mapping on
			// BOTH structures so new files install cleanly.
			providers, ok := probe["providers"].(map[string]any)
			if !ok {
				providers = map[string]any{}
			}
			delete(providers, "spark-gateway")
			if len(providers) == 0 {
				delete(probe, "providers")
			} else {
				probe["providers"] = providers
			}
			return json.Marshal(probe)
		}
		before, err := strip(original)
		after, err := strip(encoded)
		if err != nil {
			return fail("Hermes update would change unrelated settings.")
		}
		if string(before) != string(after) {
			return fail("Hermes update would change unrelated settings.")
		}
		if string(original) == string(encoded) {
			return nil
		}
		if len(original) > 0 {
			backup = filepath.Join(stateDir(), "backups", session+"-hermes-config.yaml")
			if err := atomicWrite(backup, original, 0600); err != nil {
				return err
			}
		}
		changed = true
		return atomicWrite(path, encoded, 0600)
	})
	if err != nil {
		return "", false, err
	}
	return backup, changed, nil
}

// harnessCodexCatalog generates the private per-launch Codex model catalog
// from the installed schema template and appends the -c catalog argument.
func harnessCodexCatalog(cfg clientConfig, catalog Object, plan Object) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := harnessExecRunner(ctx, []string{str(plan["binary"]), "debug", "models", "--bundled"}, nil)
	if err != nil {
		return fail("Cannot read the installed Codex model schema.")
	}
	if result.Code != 0 {
		return fail("Cannot read the installed Codex model schema.")
	}
	var parsed Object
	if err := json.Unmarshal([]byte(result.Out), &parsed); err != nil {
		return fail("Installed Codex returned an unknown catalog schema.")
	}
	modelsOut, ok := parsed["models"].([]any)
	if !ok || len(modelsOut) == 0 {
		return fail("Installed Codex returned an unknown catalog schema.")
	}
	templateRaw, err := json.Marshal(modelsOut[0])
	if err != nil || modelsOut[0] == nil {
		return fail("Installed Codex returned an unknown catalog schema.")
	}
	entries := []any{}
	for _, mAny := range arr(catalog["models"]) {
		m := obj(mAny)
		// Deep copy the full current schema template so unknown required
		// fields are retained, then update the exact owned fields.
		var entry map[string]any
		if err := json.Unmarshal(templateRaw, &entry); err != nil {
			return fail("Installed Codex returned an unknown catalog schema.")
		}
		inputs := []string{}
		for _, kind := range stringsOf(value(m, "inputs", anyStrings([]string{"text"}))) {
			if contains([]string{"text", "image", "audio"}, kind) {
				inputs = append(inputs, kind)
			}
		}
		if len(inputs) == 0 {
			inputs = []string{"text"}
		}
		entry["slug"] = str(m["id"])
		entry["display_name"] = str(value(m, "name", str(m["id"])))
		entry["description"] = "Spark Gateway"
		entry["context_window"] = int(num(m["context_tokens"]))
		entry["max_context_window"] = int(num(m["context_tokens"]))
		entry["effective_context_window_percent"] = 100
		entry["visibility"] = "list"
		entry["supported_in_api"] = true
		entry["upgrade"] = nil
		entry["model_messages"] = Object{}
		entry["availability_nux"] = Object{"message": ""}
		entry["available_access_programs"] = Object{"cyber": []any{}}
		entry["additional_speed_tiers"] = []any{}
		entry["service_tiers"] = []any{}
		entry["support_verbosity"] = false
		entry["apply_patch_tool_type"] = "freeform"
		entry["supports_search_tool"] = false
		entry["use_responses_lite"] = false
		entry["input_modalities"] = anyStrings(inputs)
		entries = append(entries, entry)
	}
	path := filepath.Join(stateDir(), "generated", str(plan["session"]), "codex-models.json")
	if err := writeObject(path, Object{"models": entries}); err != nil {
		return err
	}
	plan["args"] = append(arr(plan["args"]), "-c", "model_catalog_json="+jsonString(path))
	files := stringsOf(plan["generated_files"])
	files = append(files, path)
	plan["generated_files"] = anyStrings(files)
	return nil
}

// harnessPrepare applies per-launch side effects (Hermes config sync, Codex
// catalog generation) before the child process starts.
func harnessPrepare(cfg clientConfig, catalog Object, plan Object) error {
	switch str(plan["harness"]) {
	case "hermes":
		_, err := harnessSyncHermes(cfg, catalog, str(plan["session"]))
		return err
	case "codex":
		return harnessCodexCatalog(cfg, catalog, plan)
	}
	return nil
}

// harnessSignalContext is the exact pending CLI integration seam: it returns
// the context whose cancellation terminates a running harness child. The
// default is context.Background (never canceled). Later, CLI main must
// install it ONCE at startup, e.g.:
//
//	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
//	defer stop()
//	sparky.SetHarnessSignalContext(ctx)
//
// with any handler restored/unregistered via the returned stop(). Because the
// child is NOT put in a background process group, terminal SIGINT/SIGTERM
// reach it directly through the foreground group; the injected context covers
// programmatic cancellation (timeouts, supervisor shutdown) and is the only
// supported cancellation path for tests.
var harnessSignalContext = context.Background

// errInterruptCause is the context cause used by the public CLI when the
// user interrupts (SIGINT). The harness Cancel seam forwards SIGINT (not
// SIGTERM) to the child whenever the context carries this cause.
var errInterruptCause = errors.New("interrupted")

// harnessLaunch checks passthrough, prepares side effects, records the launch
// and execs the harness child with preserved stdio, cwd and environment.
// It returns the child exit status (128+n for fatal signals) without exposing
// credentials in any output. The completion record is finalized on all paths
// after the start record exists, including env and spawn failures.
func harnessLaunch(cfg clientConfig, catalog Object, plan Object, extra []string, in io.Reader, out, errout io.Writer) (int, error) {
	if err := checkHarnessPassthrough(str(plan["harness"]), extra); err != nil {
		return 1, err
	}
	if err := harnessPrepare(cfg, catalog, plan); err != nil {
		return 1, err
	}
	model := obj(plan["model"])
	record := Object{
		"session":            str(plan["session"]),
		"harness":            str(plan["harness"]),
		"model":              str(model["id"]),
		"context":            int(num(model["context_tokens"])),
		"gateway":            cfg.Gateway,
		"catalog_fetched_at": str(catalog["fetched_at"]),
		"project":            str(plan["project"]),
		"started_at":         now(),
	}
	recordPath := filepath.Join(stateDir(), "launches", str(plan["session"])+".json")
	if err := writeObject(recordPath, record); err != nil {
		return 1, err
	}
	code := 1
	finalize := func() error {
		record["finished_at"] = now()
		record["exit_code"] = code
		return writeObject(recordPath, record)
	}
	// Banner belongs before the child starts, like the Python launcher.
	fmt.Fprintf(errout, "Sparky: %s · %s · %d tokens\n", record["harness"], record["model"], record["context"])
	childEnv, err := harnessChildEnv(plan, harnessOSEnviron())
	if err != nil {
		fErr := finalize()
		return 1, firstError(err, fErr)
	}
	envSlice := make([]string, 0, len(childEnv))
	for k, v := range childEnv {
		envSlice = append(envSlice, k+"="+v)
	}
	args := []string{str(plan["binary"])}
	for _, a := range stringsOf(plan["args"]) {
		args = append(args, a)
	}
	args = append(args, extra...)
	// Interactive seam: the child stays in the CURRENT foreground process
	// group (no Setpgid), so it keeps direct access to the controlling
	// terminal and interactive stdin, and terminal Ctrl-C reaches it the
	// same way it reaches sparky itself. Cancellation is driven by the
	// injected context (see harnessSignalContext): Cancel first forwards
	// SIGTERM for a graceful stop, and WaitDelay bounds the wait — after
	// the delay any leftover pipe-copying grandchildren are forcibly
	// detached and the command errors instead of blocking forever.
	ctx := harnessSignalContext()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			// Forward the user's intent: a user interrupt (SIGINT cause)
			// forwards SIGINT; any other cancellation sends SIGTERM for a
			// graceful stop. WaitDelay forces termination if the child (or a
			// pipe-holding grandchild) does not exit in time.
			sig := syscall.SIGTERM
			if errors.Is(context.Cause(ctx), errInterruptCause) {
				sig = syscall.SIGINT
			}
			if err := cmd.Process.Signal(sig); err != nil {
				return cmd.Process.Kill()
			}
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second
	cmd.Dir = str(plan["project"])
	cmd.Env = envSlice
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errout
	runErr := cmd.Run()
	if runErr == nil {
		code = 0
	} else {
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) {
			// Spawn failure (missing binary, permission, cwd): report safely
			// after finalizing the record.
			fErr := finalize()
			return 1, firstError(runErr, fErr)
		}
		code = exit.ExitCode()
		if code < 0 {
			// Fatal signal: report 128+n like a shell would.
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				code = 128 + int(status.Signal())
			} else {
				code = 128 - code
			}
		}
	}
	fErr := finalize()
	if fErr != nil {
		return code, fErr
	}
	return code, nil
}

// firstError returns the first non-nil error, if any.
func firstError(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// harnessOSEnviron returns the current process environment as a map.
func harnessOSEnviron() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}
