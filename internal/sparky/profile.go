package sparky

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type profileModel struct {
	name, placement string
	nodes           []string
	model, recipe   Object
}

type clusterProfile struct {
	name     string
	models   []profileModel
	services Object
}

func readProfile(path, name string, cfg Object) (clusterProfile, error) {
	p := clusterProfile{name: name}
	if !simpleName.MatchString(name) {
		return p, fail("Choose a profile name with letters, digits, dots, hyphens, or underscores.")
	}
	source, err := os.Open(path)
	if err != nil {
		return p, fail("Read cluster profiles from %s: %v", path, err)
	}
	defer source.Close()
	decoder := yaml.NewDecoder(source)
	var file Object
	if err := decoder.Decode(&file); err != nil {
		return p, fail("Invalid profile YAML in %s: %v", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return p, fail("Expected one YAML document in %s.", path)
	}
	if num(file["version"]) != 1 || !isObject(file["profiles"]) || len(file) != 2 {
		return p, fail("Expected a version 1 profiles file.")
	}
	entry := obj(obj(file["profiles"])[name])
	if entry == nil {
		return p, fail("Unknown profile %s. Available profiles: %s", name, strings.Join(keys(obj(file["profiles"])), ", "))
	}
	if _, ok := entry["models"].([]any); !ok {
		return p, fail("Profile %s must list its models, even if the list is empty.", name)
	}
	seen := map[string]bool{}
	for _, v := range arr(entry["models"]) {
		row := obj(v)
		names := keys(row)
		if len(names) != 1 || placements[str(row[names[0]])] == nil {
			return p, fail("Each model in %s must use 'model: placement', for example 'glm: both'.", name)
		}
		id, on := names[0], str(row[names[0]])
		canonical, _, err := resolveWorkload(cfg, id)
		if err != nil {
			return p, err
		}
		m, s, nodes, _, err := selectPlacement(cfg, canonical, on)
		if err != nil {
			return p, err
		}
		if str(m["kind"]) != "llm" || seen[canonical] {
			return p, fail("Profile %s needs distinct language or decision models. Check %s.", name, id)
		}
		seen[canonical] = true
		p.models = append(p.models, profileModel{canonical, on, nodes, m, s})
	}

	if services, ok := entry["services"]; ok && !isObject(services) {
		return p, fail("Profile %s services must map service names to Sparks.", name)
	}
	p.services = obj(entry["services"])
	for _, service := range keys(p.services) {
		node := str(p.services[service])
		if node != "spark01" && node != "spark02" {
			return p, fail("Give service %s an explicit single Spark.", service)
		}
		if service == "spark-ocr" && node == "spark01" {
			continue
		}
		m, _, _, _, err := selectPlacement(cfg, service, node)
		if err != nil || !contains([]string{"audio", "comfyui"}, str(m["kind"])) {
			return p, fail("No installed profile service %s on %s.", service, node)
		}
	}
	for _, field := range keys(entry) {
		if field != "models" && field != "services" {
			return p, fail("Unknown profile field %s. Use services for AI workloads.", field)
		}
	}
	return p, nil
}

func profileStep(action, name string, nodes []string) Object {
	return Object{"action": action, "workload": name, "nodes": anyStrings(nodes)}
}

// planProfile is also used by apply. The model list gives startup order because
// a model's startup memory floor can exceed its steady-state requirements.
func planProfile(cfg Object, p clusterProfile, snapshot Object, ready func(profileModel) bool) Object {
	running := obj(snapshot["models"])
	steps, kept, blockers := []any{}, []any{}, []any{}
	targets := map[string]bool{}
	stop, start := map[string]bool{}, map[string]bool{}
	for i, target := range p.models {
		targets[target.name] = true
		current := obj(running[target.name])
		if boolean(current["loaded"]) && sameStrings(stringsOf(current["nodes"]), target.nodes) && ready(target) {
			kept = append(kept, profileStep("keep", target.name, target.nodes))
		} else {
			start[target.name] = true
			stop[target.name] = boolean(current["loaded"])
		}
		for _, other := range p.models[:i] {
			if overlapNodes(target.nodes, other.nodes) &&
				(!boolean(target.recipe["shared_device"]) || !boolean(other.recipe["shared_device"])) {
				blockers = append(blockers, fmt.Sprintf("%s and %s cannot share their selected Sparks.", other.name, target.name))
			}
		}
	}

	released := []string{}
	services := planProfileServices(cfg, p, snapshot)
	steps = append(steps, services.stop...)
	kept = append(kept, services.keep...)
	blockers = append(blockers, services.blockers...)
	for _, raw := range services.stop {
		released = unionNodes(released, stringsOf(obj(raw)["nodes"]))
	}
	for _, name := range keys(running) {
		row := obj(running[name])
		if str(row["kind"]) != "llm" || !boolean(row["loaded"]) || (targets[name] && !stop[name]) {
			continue
		}
		nodes := stringsOf(row["nodes"])
		if effectiveProtected(row, obj(cfg["policy"]), name) {
			blockers = append(blockers, fmt.Sprintf("%s is protected and cannot be stopped.", name))
		} else if !boolean(row["reserved"]) || len(nodes) == 0 || str(row["state"]) != StateReady {
			blockers = append(blockers, fmt.Sprintf("Sparky cannot confirm ready ownership of %s. Check its lifecycle before applying.", name))
		} else {
			steps = append(steps, profileStep("stop", name, nodes))
			released = unionNodes(released, nodes)
		}
	}

	started := []string{}
	for _, target := range p.models {
		if !start[target.name] {
			continue
		}
		step := profileStep("start", target.name, target.nodes)
		step["on"] = target.placement
		step["minimum_available_gib"] = obj(target.recipe["minimum_available_gib"])
		steps = append(steps, step)
		for _, node := range keys(obj(target.recipe["minimum_available_gib"])) {
			floor := num(obj(target.recipe["minimum_available_gib"])[node])
			memory := obj(obj(obj(snapshot["nodes"])[node])["memory_bytes"])
			available := num(memory["MemAvailable"]) / (1 << 30)
			if floor > 0 && available < floor && !contains(released, node) {
				blockers = append(blockers, fmt.Sprintf("%s needs %.1f GiB available on %s to start. It currently has %.1f GiB and no stop is planned there.", target.name, floor, node, available))
			}
			if (contains(released, node) || contains(started, node)) && floor > 0 {
				step["recheck_memory_at_start"] = true
			}
		}
		started = unionNodes(started, target.nodes)
	}

	steps = append(steps, services.start...)

	routes := Object{}
	for _, route := range []struct{ key, alias string }{{"auto", "spark-auto"}, {"decision_auto", "spark-decision/auto"}} {
		if str(obj(cfg[route.key])["routing"]) != "bifrost" {
			continue
		}
		eligible := []any{}
		for _, name := range stringsOf(obj(cfg[route.key])["targets"]) {
			if targets[name] {
				eligible = append(eligible, name)
			}
		}
		routes[route.alias] = eligible
	}
	return Object{"profile": p.name, "status": "planned", "kept": kept, "steps": steps,
		"blockers": blockers, "blocked": len(blockers) > 0, "expected_routes": routes}
}

func (c *Coordinator) Profile(name string, apply bool) (Object, error) {
	budget := 60 * time.Second
	if apply {
		budget = 2 * time.Hour
		// Force takes this lock before reconfigure and may cancel work.
		if !c.forceMu.TryLock() {
			return nil, fail("A forced model operation is in progress.")
		}
		defer c.forceMu.Unlock()
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := c.reconfigure.lock(ctx); err != nil {
		return nil, err
	}
	defer c.reconfigure.unlock()
	if err := c.refresh(); err != nil {
		return nil, err
	}
	cfg := c.cfg()
	if !c.runtime.native() {
		return nil, fail("Cluster profiles require the native model lifecycle.")
	}
	p, err := readProfile(filepath.Join(filepath.Dir(c.path), "profiles.yaml"), name, cfg)
	if err != nil {
		return nil, err
	}
	preview := func() (Object, error) {
		snapshot, err := c.runtime.snapshot(ctx, true)
		if err != nil {
			return nil, err
		}
		if str(p.services["spark-ocr"]) == "spark01" {
			ocr, err := newOCRService(c.runtime.root())
			if err != nil {
				return nil, err
			}
			source, err := ocr.ocrSource()
			if err != nil {
				return nil, err
			}
			snapshot["ocr_installed"] = ocrFileExists(filepath.Join(source, "deploy", "compose.yaml")) &&
				ocrFileExists(ocr.ocrEnvFile(source))
		}
		return planProfile(cfg, p, snapshot, func(m profileModel) bool {
			return c.runtime.nativeReady(ctx, m.name, m.model, m.recipe, m.nodes, m.placement)
		}), nil
	}
	result, err := preview()
	if err != nil || !apply || boolean(result["blocked"]) {
		return result, err
	}
	if c.Fenced() {
		return nil, fail("Admission is paused. Retry after it resumes.")
	}

	completed := []any{}
	modelChanged := false
	failed := func(err error) (Object, error) {
		result["status"], result["error"], result["completed"] = "failed", err.Error(), completed
		if modelChanged {
			syncCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if syncErr := c.syncAutoRoutesLocked(syncCtx, nil); syncErr != nil {
				result["routing_error"] = syncErr.Error()
			}
		}
		return result, nil
	}
	for _, v := range arr(result["steps"]) {
		if err := c.refresh(); err != nil {
			return failed(err)
		}
		if compactJSON(c.cfg()) != compactJSON(cfg) {
			return failed(fail("The cluster configuration changed during apply. Review a new plan."))
		}
		step := obj(v)
		model := str(step["workload"])
		if kind := str(step["service_kind"]); kind != "" {
			err = c.applyProfileService(ctx, kind, step)
		} else {
			modelChanged = true
			if str(step["action"]) == "stop" {
				_, err = c.unloadLocked(ctx, model)
			} else {
				_, err = c.loadLocked(ctx, model, "", str(step["on"]), c.generationCopy(), true)
			}
		}
		if err != nil {
			return failed(err)
		}
		completed = append(completed, step)
	}
	if err := c.refresh(); err != nil {
		return failed(err)
	}
	if compactJSON(c.cfg()) != compactJSON(cfg) {
		return failed(fail("The cluster configuration changed during apply. Review a new plan."))
	}
	verified, err := preview()
	if err != nil {
		return failed(err)
	}
	if boolean(verified["blocked"]) || len(arr(verified["steps"])) > 0 {
		return failed(fail("The running state still differs from the profile. Review a new plan."))
	}
	if modelChanged {
		if err := c.syncAutoRoutesLocked(ctx, nil); err != nil {
			return failed(err)
		}
	}
	if err := writeObject(profileStatePath(cfg), Object{"name": name, "applied_at": time.Now().UTC(), "services": p.services}); err != nil {
		return failed(fail("Could not save the selected profile: %v", err))
	}
	result["status"], result["completed"] = "applied", completed
	return result, nil
}

func profileStatePath(cfg Object) string {
	return filepath.Join(str(cfg["root"]), "controller", "profile.json")
}

func selectedProfile(cfg Object) Object {
	state, err := readObject(profileStatePath(cfg))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !simpleName.MatchString(str(state["name"])) {
		return Object{"error": "Could not read the selected profile."}
	}
	return state
}

func renderProfile(result Object) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Profile %s (%s)\n", str(result["profile"]), str(result["status"]))
	for _, key := range []string{"kept", "steps"} {
		for _, v := range arr(result[key]) {
			row := obj(v)
			fmt.Fprintf(&out, "  %s %s on %s", str(row["action"]), str(row["workload"]), strings.Join(stringsOf(row["nodes"]), ", "))
			if boolean(row["recheck_memory_at_start"]) {
				out.WriteString(" (check available memory at startup)")
			}
			out.WriteByte('\n')
		}
	}
	if len(arr(result["steps"])) == 0 {
		out.WriteString("  No model or service starts, stops, or restarts.\n")
	}
	routes := obj(result["expected_routes"])
	for _, route := range keys(routes) {
		names := stringsOf(routes[route])
		if len(names) == 0 {
			names = []string{"unavailable"}
		}
		fmt.Fprintf(&out, "  Expected: %s → %s\n", route, strings.Join(names, ", "))
	}
	for _, v := range arr(result["blockers"]) {
		fmt.Fprintf(&out, "  Blocked: %s\n", str(v))
	}
	if err := str(result["error"]); err != "" {
		fmt.Fprintf(&out, "  Stopped after %d steps: %s\n", len(arr(result["completed"])), err)
	}
	if err := str(result["routing_error"]); err != "" {
		fmt.Fprintf(&out, "  Route update failed: %s\n", err)
	}
	return out.String()
}
