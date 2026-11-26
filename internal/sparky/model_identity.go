package sparky

import (
	"context"
	"strings"
)

// public_model joins runtime recipes under one stable API identity. Lifecycle
// operations continue to use the runtime key, units and frozen recipe.
func publicModel(cfg Object, name string) string {
	return str(value(obj(obj(cfg["models"])[name]), "public_model", name))
}

func validateIdentity(cfg Object) error {
	models := obj(cfg["models"])
	for name, raw := range models {
		m := obj(raw)
		if _, ok := m["public_model"]; !ok {
			continue
		}
		id := str(m["public_model"])
		base := obj(models[id])
		if id == "" || id == name || str(base["public_model"]) != "" || str(m["kind"]) != "llm" || str(base["kind"]) != "llm" {
			return fail("%s public_model must name a canonical language model without another public_model.", name)
		}
		if str(m["api_kind"]) != str(base["api_kind"]) || boolean(m["reasoning"]) != boolean(base["reasoning"]) || !sameStrings(stringsOf(value(m, "inputs", []any{"text"})), stringsOf(value(base, "inputs", []any{"text"}))) {
			return fail("%s and public_model %s must have the same API kind, inputs, and reasoning support.", name, id)
		}
	}
	return nil
}

func publicCatalog(cfg Object) Object {
	groups := Object{}
	for _, name := range enabledLLMs(cfg) {
		id := publicModel(cfg, name)
		groups[id] = append(arr(groups[id]), name)
	}
	data := []any{}
	for _, id := range keys(groups) {
		base := obj(obj(cfg["models"])[id])
		window, output := mathMaxNumber, mathMaxNumber
		nodes, aliases := []string{}, []string{}
		legacy := []string{}
		for _, name := range stringsOf(groups[id]) {
			m, s, on, _, _ := selectPlacement(cfg, name, "")
			window = min(window, num(s["context_tokens"]))
			output = min(output, num(value(m, "output_tokens", min(16384, num(s["context_tokens"])-1))))
			// A grouped identity must advertise a budget valid for every
			// placement, including one selected by a different profile.
			if len(arr(groups[id])) > 1 {
				for _, raw := range obj(m["settings"]) {
					if boolean(value(obj(raw), "enabled", true)) {
						window = min(window, num(obj(raw)["context_tokens"]))
						output = min(output, num(value(m, "output_tokens", min(16384, num(obj(raw)["context_tokens"])-1))))
					}
				}
			}
			nodes = unionNodes(nodes, on)
			aliases = unionNodes(aliases, stringsOf(m["aliases"]))
			if name != id {
				aliases = unionNodes(aliases, []string{name})
				legacy = append(legacy, name)
			}
		}
		row := Object{"id": "garuda/" + id, "name": value(base, "name", id), "context_length": window, "output_tokens": output,
			"aliases": anyStrings(aliases), "inputs": value(base, "inputs", []any{"text"}), "reasoning": value(base, "reasoning", false), "nodes": nodes}
		if kind := str(base["api_kind"]); kind != "" {
			row["api_kind"] = kind
		}
		if len(legacy) > 0 {
			row["legacy_ids"] = anyStrings(legacy)
		}
		data = append(data, row)
	}
	return Object{"data": data}
}

// Called under reconfigure. Requests never start a model. Legacy runtime IDs
// resolve to the same ready runtime as the public ID.
func (c *Coordinator) resolveInference(ctx context.Context, requested string) (string, error) {
	name, _, err := resolveWorkload(c.cfg(), requested)
	if err != nil {
		return "", err
	}
	canonical := publicModel(c.cfg(), name)
	candidates := []string{}
	for _, candidate := range enabledLLMs(c.cfg()) {
		if publicModel(c.cfg(), candidate) == canonical {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	ready := ""
	for _, candidate := range candidates {
		sel, err := c.loadedSelection(candidate)
		if err != nil || !c.runtime.ready(ctx, candidate, sel.Setting, sel.Nodes) {
			continue
		}
		if ready != "" {
			return "", fail("More than one runtime is ready for %s. Select one runtime through a profile.", canonical)
		}
		ready = candidate
	}
	if ready == "" {
		return name, nil
	}
	return ready, nil
}

func (c *Coordinator) publicWire() []any {
	ctx, cancel := context.WithTimeout(context.Background(), imageWireBudget)
	defer cancel()
	rows := []any{}
	for _, raw := range arr(publicCatalog(c.cfg())["data"]) {
		id := strings.TrimPrefix(str(obj(raw)["id"]), "garuda/")
		state := "unloaded"
		if name, err := c.resolveInference(ctx, id); err == nil {
			sel, err := c.loadedSelection(name)
			if err == nil && c.runtime.ready(ctx, name, sel.Setting, sel.Nodes) {
				state = "loaded"
			}
		}
		rows = append(rows, Object{"id": id, "object": "model", "owned_by": "sparky", "status": Object{"value": state}})
	}
	return rows
}
