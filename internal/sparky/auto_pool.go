package sparky

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

func bifrostAuto(cfg Object) bool { return str(obj(cfg["auto"])["routing"]) == "bifrost" }

type autoUnavailableError struct{ message string }

func (e autoUnavailableError) Error() string   { return e.message }
func (e autoUnavailableError) HTTPStatus() int { return http.StatusServiceUnavailable }

// syncAutoRoutesLocked runs under reconfigure. The same gate protects lifecycle
// changes, so an availability update cannot race a planned model stop.
func (c *Coordinator) syncAutoRoutesLocked(ctx context.Context, excluded []string) error {
	cfg := c.cfg()
	if !bifrostAuto(cfg) {
		return nil
	}
	names := []string{}
	selections := []*runtimeSelection{}
	for _, name := range stringsOf(obj(cfg["auto"])["targets"]) {
		if contains(excluded, name) {
			continue
		}
		if !contains(enabledLLMs(cfg), name) {
			continue
		}
		selection, err := c.loadedSelection(name)
		if err != nil || backendForRecipe(selection.Setting, selection.Record) == "" || !c.runtime.ready(ctx, name, selection.Setting, selection.Nodes) {
			continue
		}
		names = append(names, name)
		selections = append(selections, selection)
	}
	metadata := autoPoolMetadata(cfg, names, selections)
	if !boolean(metadata["available"]) {
		names = nil
	}
	if err := autoGatewaySyncHook(ctx, cfg, names); err != nil {
		c.mu.Lock()
		c.autoPool = Object{"available": false, "routing": "bifrost", "routing_error": err.Error()}
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	c.autoPool = metadata
	c.mu.Unlock()
	return nil
}

// The common budget includes all allowed recipes, even while one is stopped.
// Loading a previously configured smaller model therefore does not reduce the
// budget between a client's metadata lookup and its next inference request.
func autoPoolMetadata(cfg Object, names []string, selections []*runtimeSelection) Object {
	row := Object{"available": len(names) > 0, "routing": "bifrost", "request_model": "spark-auto", "ready_models": anyStrings(names)}
	if len(names) == 0 {
		return row
	}
	window, output := 0, 0
	inputs := []string(nil)
	reasoning := true
	for _, name := range stringsOf(obj(cfg["auto"])["targets"]) {
		model := obj(obj(cfg["models"])[name])
		if !contains(enabledLLMs(cfg), name) {
			continue
		}
		limit := int(num(model["output_tokens"]))
		if limit <= 0 {
			return Object{"available": false, "routing": "bifrost", "routing_error": "An automatic target has no output limit."}
		}
		if output == 0 || limit < output {
			output = limit
		}
		for _, raw := range obj(model["settings"]) {
			if !boolean(value(obj(raw), "enabled", true)) {
				continue
			}
			context := int(num(obj(raw)["context_tokens"]))
			if context > 0 && (window == 0 || context < window) {
				window = context
			}
		}
	}
	nodes := []string{}
	identities := []string{}
	for index, selection := range selections {
		context := int(num(selection.Setting["context_tokens"]))
		if context > 0 && (window == 0 || context < window) {
			window = context
		}
		limit := int(num(selection.Model["output_tokens"]))
		if limit > 0 && (output == 0 || limit < output) {
			output = limit
		}
		nodes = unionNodes(nodes, selection.Nodes)
		modelInputs := stringsOf(value(selection.Model, "inputs", []any{"text"}))
		if index == 0 {
			inputs = modelInputs
		} else {
			common := []string{}
			for _, input := range inputs {
				if contains(modelInputs, input) {
					common = append(common, input)
				}
			}
			inputs = common
		}
		reasoning = reasoning && boolean(selection.Model["reasoning"])
		identities = append(identities, names[index]+":"+str(selection.Record["generation"]))
	}
	if window <= 0 || output <= 0 || output >= window {
		return Object{"available": false, "routing": "bifrost", "routing_error": "Automatic targets have no valid common token budget."}
	}
	sort.Strings(identities)
	encoded, _ := json.Marshal([]any{identities, window, output})
	digest := sha256.Sum256(encoded)
	row["runtime_generation"] = hex.EncodeToString(digest[:])
	row["context_length"], row["context_window"] = window, window
	row["output_tokens"], row["max_output_tokens"] = output, output
	row["inputs"], row["reasoning"], row["nodes"] = anyStrings(inputs), reasoning, anyStrings(nodes)
	row["context_source"] = "common budget of permitted automatic targets"
	return row
}

func (c *Coordinator) ReconcileAutoRouting() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.reconfigure.lock(ctx); err != nil {
		return err
	}
	defer c.reconfigure.unlock()
	if err := c.refresh(); err != nil {
		return err
	}
	return c.syncAutoRoutesLocked(ctx, nil)
}

func (c *Coordinator) finishAutoOperation(result Object, operationErr error) (Object, error) {
	if !bifrostAuto(c.cfg()) {
		return result, operationErr
	}
	if syncErr := c.ReconcileAutoRouting(); syncErr != nil {
		_ = c.runtime.event("auto_routing_sync_failed", Object{"error": syncErr.Error()})
		if operationErr == nil {
			return result, fail("The model operation finished, but Bifrost availability could not be updated: %v", syncErr)
		}
	}
	return result, operationErr
}

func autoRequested(model string) bool {
	name := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "garuda/")
	return name == "auto" || name == "spark-auto"
}
