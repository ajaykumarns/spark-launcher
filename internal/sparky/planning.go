package sparky

import (
	"fmt"
	"strings"
)

// matrixSets derives llama-swap compatibility groups from actual selected
// placements, not from workload names. Each language model occupies at least
// one of the two exclusive devices, so no compatible group can contain three
// models; bounded pairwise analysis avoids a 2**N subset search. The result
// is independent of map ordering and stays pure for the manager sync task.
func matrixSets(c Object) Object {
	names := enabledLLMs(c)
	nodes := Object{}
	for _, name := range names {
		set := Object{}
		for _, n := range placements[str(obj(obj(c["models"])[name])["run_on"])] {
			set[n] = true
		}
		nodes[name] = set
	}
	groups := [][]string{}
	paired := Object{}
	for i, a := range names {
		for _, b := range names[i+1:] {
			if overlapNodes(keys(obj(nodes[a])), keys(obj(nodes[b]))) {
				continue
			}
			paired[a] = true
			paired[b] = true
			groups = append(groups, []string{a, b})
		}
	}
	for _, name := range names {
		if !boolean(paired[name]) {
			groups = append(groups, []string{name})
		}
	}
	out := Object{}
	for i, group := range groups {
		out[fmt.Sprintf("placement_%d", i+1)] = strings.Join(group, " & ")
	}
	return out
}

func makePlan(c Object, name string, snapshot Object, action, on string) (Object, error) {
	if action != "load" && action != "unload" {
		return nil, fail("Use load or unload for a resource plan.")
	}
	m, s, nodes, placement, err := selectPlacement(c, name, on)
	if err != nil {
		return nil, err
	}
	if action == "unload" && boolean(m["keep_loaded"]) {
		return nil, fail("%s is configured to stay loaded.", name)
	}
	kind := str(m["kind"])
	if action == "unload" && kind == "audio" {
		return nil, fail("No safe unload adapter is configured for this audio service.")
	}
	steps := []any{}
	waiting := Object{}
	models := obj(snapshot["models"])
	if action == "unload" {
		if num(obj(models[name])["active_requests"]) > 0 {
			waiting[name] = true
		}
		steps = append(steps, Object{"action": "unload", "workload": name, "after_active_jobs": true})
	} else if kind == "audio" {
		steps = append(steps, Object{"action": "ensure_running", "workload": name, "preserve_existing": true})
	} else if kind == "comfyui" || kind == "comfy_model" {
		steps = append(steps, Object{"action": "ensure_comfyui", "nodes": nodes, "note": "Weights load when a workflow executes; this does not run a workflow."})
	} else {
		// Conflict decisions come from the actual runtime placement of each
		// loaded model, never from the current configuration alone.
		for _, other := range keys(models) {
			row := obj(models[other])
			if other == name || str(row["kind"]) != "llm" || !boolean(row["loaded"]) || !overlapNodes(nodes, stringsOf(row["nodes"])) {
				continue
			}
			// Protected models are never evicted, not even with waiting jobs:
			// the effective policy merges the config keep_loaded and the
			// protected_models allowlist, exactly like the load path.
			policyRow := copyObject(row)
			policyRow["keep_loaded"] = boolean(obj(obj(c["models"])[other])["keep_loaded"]) || boolean(row["keep_loaded"])
			if effectiveProtected(policyRow, obj(c["policy"]), other) {
				return nil, fail("%s must stay loaded on the requested device.", other)
			}
			if num(row["active_requests"]) > 0 {
				waiting[other] = true
			}
			steps = append(steps, Object{"action": "unload", "workload": other, "after_active_jobs": true, "reason": "The requested recipe needs its device."})
		}
		for _, node := range nodes {
			comfy := obj(obj(snapshot["comfyui"])[node])
			if boolean(comfy["executing"]) || (num(comfy["running"]) > 0 && str(comfy["phase"]) != "waiting") {
				waiting["comfyui@"+node] = true
			}
			steps = append(steps, Object{"action": "release_idle_comfy_cache", "node": node, "preserve_process": true, "after_active_jobs": true})
		}
		running := obj(models[name])
		same := boolean(running["loaded"]) && sameStrings(stringsOf(running["nodes"]), nodes)
		if same {
			steps = []any{Object{"action": "keep_running", "workload": name}}
			waiting = Object{}
		} else {
			if boolean(running["loaded"]) {
				if num(running["active_requests"]) > 0 {
					waiting[name] = true
				}
				steps = append(steps, Object{"action": "restart_for_placement", "workload": name, "from": running["nodes"], "to": nodes})
			}
			steps = append(steps, Object{"action": "load", "workload": name, "nodes": nodes, "minimum_available_gib": value(s, "minimum_available_gib", Object{})})
		}
	}
	protected := []string{}
	for _, n := range keys(obj(c["models"])) {
		if boolean(obj(obj(c["models"])[n])["keep_loaded"]) {
			protected = append(protected, n)
		}
	}
	return Object{"workload": name, "kind": kind, "action": action, "placement": placement, "nodes": nodes, "wait_for": keys(waiting), "steps": steps, "protected": protected, "interrupt_active_jobs": false}, nil
}

func modelCatalog(c Object) Object {
	data := []any{}
	byID := Object{}
	for _, name := range enabledLLMs(c) {
		m, s, nodes, _, _ := selectPlacement(c, name, "")
		row := Object{"id": "garuda/" + name, "name": value(m, "name", name), "context_length": s["context_tokens"], "output_tokens": value(m, "output_tokens", min(16384, num(s["context_tokens"])-1)), "aliases": value(m, "aliases", []any{}), "inputs": value(m, "inputs", []any{"text"}), "reasoning": value(m, "reasoning", false), "nodes": nodes}
		data = append(data, row)
		byID[name] = row
	}
	// The public spark-auto entry advertises the common budget of the
	// configured auto targets only; disabled models never appear.
	if auto := obj(c["auto"]); len(auto) > 0 {
		contextLimit, outputLimit := mathMaxNumber, mathMaxNumber
		for _, name := range stringsOf(auto["targets"]) {
			row := obj(byID[name])
			contextLimit = min(contextLimit, num(row["context_length"]))
			outputLimit = min(outputLimit, num(row["output_tokens"]))
		}
		data = append(data, Object{"id": "garuda/spark-auto", "name": "Spark Auto", "context_length": contextLimit, "output_tokens": outputLimit, "aliases": []any{"auto"}, "inputs": []any{"text"}})
	}
	// Text-to-speech services stay separate from language models and expose
	// only whitelisted metadata; nothing else from the config leaks out.
	audio := []any{}
	fields := strings.Fields("model api_base speech_url default_voice voices preferred_voices languages formats device api_key_required")
	for _, name := range keys(obj(c["models"])) {
		m := obj(obj(c["models"])[name])
		tts := obj(m["tts"])
		if str(m["kind"]) != "audio" || len(tts) == 0 {
			continue
		}
		row := Object{"id": name, "kind": "tts", "name": value(m, "name", name), "nodes": placements[str(m["run_on"])]}
		for _, k := range fields {
			if v, ok := tts[k]; ok {
				row[k] = v
			}
		}
		audio = append(audio, row)
	}
	return Object{"data": data, "audio_services": audio, "authority": "spark01", "config_version": c["version"]}
}

const mathMaxNumber = float64(1 << 53)
