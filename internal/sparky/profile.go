package sparky

import (
	"context"
	"crypto/sha256"
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
	name         string
	description  string
	models       []profileModel
	capabilities []any
	autoWeights  Object
}

type profileFile struct {
	Version  int `yaml:"version"`
	Profiles map[string]struct {
		Description  yaml.Node `yaml:"description"`
		Capabilities yaml.Node `yaml:"capabilities"`
		AutoWeights  yaml.Node `yaml:"auto_weights"`
	} `yaml:"profiles"`
}

func loadProfileFile(path string) (profileFile, error) {
	var file profileFile
	source, err := os.Open(path)
	if err != nil {
		return file, fail("Read cluster profiles from %s: %v", path, err)
	}
	defer source.Close()
	decoder := yaml.NewDecoder(source)
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return file, fail("Invalid profile YAML in %s: %v", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return file, fail("Expected one YAML document in %s.", path)
	}
	if file.Version != 1 || file.Profiles == nil {
		return file, fail("Expected a version 1 profiles file.")
	}
	return file, nil
}

func readProfile(path, name string, cfg Object) (clusterProfile, error) {
	file, err := loadProfileFile(path)
	if err != nil {
		return clusterProfile{}, err
	}
	return file.profile(name, cfg)
}

func (file profileFile) profile(name string, cfg Object) (clusterProfile, error) {
	p := clusterProfile{name: name}
	if !simpleName.MatchString(name) {
		return p, fail("Choose a profile name with letters, digits, dots, hyphens, or underscores.")
	}
	entry, ok := file.Profiles[name]
	if !ok {
		names := make(Object, len(file.Profiles))
		for n := range file.Profiles {
			names[n] = true
		}
		return p, fail("Unknown profile %s. Available profiles: %s", name, strings.Join(keys(names), ", "))
	}
	if d := entry.Description; d.Kind != 0 && d.ShortTag() != "!!null" {
		if d.Kind != yaml.ScalarNode || d.ShortTag() != "!!str" {
			return p, fail("The description for profile %s must be text.", name)
		}
		p.description = strings.TrimSpace(d.Value)
	}
	groups := entry.Capabilities
	if groups.Kind != yaml.MappingNode {
		return p, fail("Profile %s must define capabilities. Use capabilities: {} for an empty profile.", name)
	}
	p.capabilities = []any{}
	seen, seenGroups := map[string]bool{}, map[string]bool{}
	for i := 0; i < len(groups.Content); i += 2 {
		key, list := groups.Content[i], groups.Content[i+1]
		group := key.Value
		if key.ShortTag() != "!!str" || !contains([]string{"chat", "decisions", "transcription", "speech", "images", "ocr"}, group) || seenGroups[group] {
			return p, fail("Unknown or repeated profile capability %s.", group)
		}
		seenGroups[group] = true
		if list.Kind != yaml.SequenceNode || len(list.Content) == 0 {
			return p, fail("List at least one workload for %s, or omit the capability.", group)
		}
		providers := []any{}
		for _, item := range list.Content {
			if item.Kind != yaml.MappingNode || len(item.Content) != 2 ||
				item.Content[0].ShortTag() != "!!str" || item.Content[1].ShortTag() != "!!str" {
				return p, fail("Each %s workload must use 'workload: placement'.", group)
			}
			id, on := item.Content[0].Value, item.Content[1].Value
			if placements[on] == nil {
				return p, fail("Give %s an explicit supported placement.", id)
			}
			var m, s Object
			var nodes []string
			canonical := id
			if id == "spark-ocr" {
				if len(placements[on]) != 1 {
					return p, fail("Give Spark OCR an explicit single Spark.")
				}
				nodes = []string{on}
			} else {
				var err error
				canonical, _, err = resolveWorkload(cfg, id)
				if err != nil {
					return p, err
				}
				m, s, nodes, _, err = selectPlacement(cfg, canonical, on)
				if err != nil {
					return p, err
				}
			}
			if profileCapability(canonical, m) != group {
				return p, fail("%s does not provide the %s capability.", id, group)
			}
			if seen[canonical] {
				return p, fail("List %s only once in profile %s.", id, name)
			}
			seen[canonical] = true
			if str(m["kind"]) == "llm" {
				p.models = append(p.models, profileModel{canonical, on, nodes, m, s})
			} else if canonical == "comfyui" && on == "both" {
				for _, node := range nodes {
					if _, _, _, _, err := selectPlacement(cfg, canonical, node); err != nil {
						return p, err
					}
					providers = append(providers, Object{"workload": canonical, "nodes": anyStrings([]string{node})})
				}
				continue
			} else if len(nodes) != 1 {
				return p, fail("Give service %s an explicit single Spark.", id)
			}
			providers = append(providers, Object{"workload": canonical, "nodes": anyStrings(nodes)})
		}
		p.capabilities = append(p.capabilities, Object{"name": group, "providers": providers})
	}
	weights, err := parseProfileAutoWeights(entry.AutoWeights, p, cfg)
	if err != nil {
		return p, err
	}
	p.autoWeights = weights
	return p, nil
}

func listProfiles(root string) (Object, error) {
	dir := filepath.Join(root, "config")
	cfg, err := readCluster(filepath.Join(dir, "models.json"))
	if err != nil {
		return nil, err
	}
	file, err := loadProfileFile(filepath.Join(dir, "profiles.yaml"))
	if err != nil {
		return nil, err
	}
	names := Object{}
	for name := range file.Profiles {
		names[name] = true
	}
	profiles := []any{}
	for _, name := range keys(names) {
		row := Object{"name": name}
		p, err := file.profile(name, cfg)
		if p.description != "" {
			row["description"] = p.description
		}
		if err != nil {
			row["error"] = err.Error()
		} else {
			row["capabilities"] = p.capabilities
			if len(p.autoWeights) > 0 {
				row["auto_weights"] = copyObject(p.autoWeights)
			}
		}
		profiles = append(profiles, row)
	}
	return Object{"profiles": profiles, "selected": str(selectedProfile(cfg)["name"])}, nil
}

func profileCapability(name string, model Object) string {
	if name == "spark-ocr" && model == nil {
		return "ocr"
	}
	switch str(model["kind"]) {
	case "llm":
		if str(model["api_kind"]) == "decision" {
			return "decisions"
		}
		return "chat"
	case "audio":
		if len(obj(model["tts"])) > 0 {
			return "speech"
		}
		return "transcription"
	case "comfyui":
		if name == "comfyui" {
			return "images"
		}
	}
	return ""
}

// Capabilities describe the requested providers. Report readiness separately
// so that a planned service or a stopped provider cannot look available.
func profileAvailability(groups []any, ready func(Object) bool) []any {
	out := []any{}
	for _, raw := range groups {
		group := obj(raw)
		providers := []any{}
		count := 0
		for _, rawProvider := range arr(group["providers"]) {
			provider := copyObject(obj(rawProvider))
			available := ready(provider)
			provider["ready"] = available
			if available {
				count++
			}
			providers = append(providers, provider)
		}
		state := "unavailable"
		if count > 0 {
			state = "available"
			if count != len(providers) {
				state = "partial"
			}
		}
		out = append(out, Object{"name": group["name"], "providers": providers, "state": state})
	}
	return out
}

func profileServicePlacement(groups []any, name string) string {
	nodes := profileServiceNodes(groups, name)
	if len(nodes) == 1 {
		return nodes[0]
	}
	return ""
}

func profileServiceNodes(groups []any, name string) []string {
	var result []string
	for _, raw := range groups {
		for _, item := range arr(obj(raw)["providers"]) {
			provider := obj(item)
			nodes := stringsOf(provider["nodes"])
			if str(provider["workload"]) == name {
				result = unionNodes(result, nodes)
			}
		}
	}
	return result
}

func profileProviderKept(kept []any, provider Object) bool {
	for _, raw := range kept {
		step := obj(raw)
		if str(step["workload"]) == str(provider["workload"]) &&
			sameStrings(stringsOf(step["nodes"]), stringsOf(provider["nodes"])) {
			return true
		}
	}
	return false
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
	services := planProfileServices(cfg, p.capabilities, snapshot)
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
		} else if !boolean(row["reserved"]) || len(nodes) == 0 {
			blockers = append(blockers, fmt.Sprintf("Sparky cannot confirm ownership of %s. Check its lifecycle before applying.", name))
		} else {
			steps = append(steps, profileStep("stop", name, nodes))
			released = unionNodes(released, nodes)
		}
	}

	started := []string{}
	restartable := map[string]bool{}
	for i, target := range p.models {
		if !start[target.name] && !restartable[target.name] {
			continue
		}
		step := profileStep("start", target.name, target.nodes)
		step["on"] = target.placement
		floors := obj(target.recipe["minimum_available_gib"])
		step["minimum_available_gib"] = floors
		if restartable[target.name] {
			step["only_if_stopped_for_startup"] = true
		}
		var candidates []any
		candidateNodes := []string{}
		for j := len(p.models) - 1; j > i; j-- {
			peer := p.models[j]
			row := obj(running[peer.name])
			if start[peer.name] || !boolean(row["reserved"]) || effectiveProtected(row, obj(cfg["policy"]), peer.name) {
				continue
			}
			var constrainedNodes []string
			for _, node := range peer.nodes {
				if num(floors[node]) > 0 {
					constrainedNodes = append(constrainedNodes, node)
				}
			}
			if len(constrainedNodes) > 0 {
				candidates = append(candidates, profileStep("stop", peer.name, peer.nodes))
				candidateNodes = unionNodes(candidateNodes, constrainedNodes)
				restartable[peer.name] = true
			}
		}
		if len(candidates) > 0 {
			step["stop_if_low_memory"] = candidates
		}
		for _, node := range keys(floors) {
			floor := num(floors[node])
			available := profileAvailableGiB(snapshot, node)
			if floor > 0 && available < floor && !restartable[target.name] &&
				!contains(released, node) && !contains(candidateNodes, node) {
				blockers = append(blockers, fmt.Sprintf("%s needs %.1f GiB available on %s to start. It currently has %.1f GiB and no stop is planned there.", target.name, floor, node, available))
			}
			if floor > 0 && (contains(released, node) || contains(started, node)) {
				step["recheck_memory_at_start"] = true
			}
		}
		steps = append(steps, step)
		started = unionNodes(started, target.nodes)
	}

	for _, raw := range services.start {
		step := obj(raw)
		floors := profileServiceMemoryFloors(cfg, step)
		if len(floors) > 0 {
			step["minimum_available_gib"] = copyObject(floors)
			step["recheck_memory_at_start"] = true
			for _, node := range keys(floors) {
				minimum, available := num(floors[node]), profileAvailableGiB(snapshot, node)
				if available < minimum && !contains(released, node) && !contains(started, node) {
					blockers = append(blockers, fmt.Sprintf("%s needs %.1f GiB available on %s to start. It currently has %.1f GiB and no stop is planned there.", str(step["workload"]), minimum, node, available))
				}
			}
		}
		steps = append(steps, step)
		started = unionNodes(started, stringsOf(step["nodes"]))
	}

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
	result := Object{"profile": p.name, "status": "planned", "kept": kept, "steps": steps,
		"blockers": blockers, "blocked": len(blockers) > 0, "expected_routes": routes,
		"capabilities": profileAvailability(p.capabilities, func(provider Object) bool {
			return profileProviderKept(kept, provider)
		})}
	if len(p.autoWeights) > 0 {
		result["auto_weights"] = copyObject(p.autoWeights)
	}
	if p.description != "" {
		result["description"] = p.description
	}
	return result
}

func profileAvailableGiB(snapshot Object, node string) float64 {
	memory := obj(obj(obj(snapshot["nodes"])[node])["memory_bytes"])
	return num(memory["MemAvailable"]) / (1 << 30)
}

// Use fresh memory measurements after planned stops. Stop the lowest-priority
// ready peers first and leave the remaining peers running once the start fits.
func stopProfilePeersForStartup(step Object, stopped map[string]bool, snapshot func() (Object, error), stop func(Object) error) error {
	floors := obj(step["minimum_available_gib"])
	var state Object
	for _, raw := range arr(step["stop_if_low_memory"]) {
		peer := obj(raw)
		name := str(peer["workload"])
		if stopped[name] {
			continue
		}
		if state == nil {
			var err error
			state, err = snapshot()
			if err != nil {
				return err
			}
		}
		for _, node := range stringsOf(peer["nodes"]) {
			floor := num(floors[node])
			if floor <= 0 {
				continue
			}
			available := profileAvailableGiB(state, node)
			if available <= 0 {
				return fail("Could not confirm available memory on %s before stopping %s.", node, name)
			}
			if available >= floor {
				continue
			}
			if err := stop(peer); err != nil {
				return err
			}
			stopped[name] = true
			state = nil
			break
		}
	}
	return nil
}

func (c *Coordinator) Profile(name string, apply bool, expected ...string) (Object, error) {
	planID := ""
	if len(expected) > 0 {
		planID = expected[0]
	}
	return c.profile(name, apply, planID, "")
}

func (c *Coordinator) ApplyProfile(name, expectedPlan, progressID string) (Object, error) {
	return c.profile(name, true, expectedPlan, progressID)
}

func (c *Coordinator) profile(name string, apply bool, expectedPlan, progressID string) (result Object, err error) {
	budget := 60 * time.Second
	if apply {
		budget = 2 * time.Hour
		// Force takes this lock before reconfigure and may cancel work.
		if !c.forceMu.TryLock() {
			return nil, fail("A forced model operation is in progress.")
		}
		defer c.forceMu.Unlock()
		c.beginProfileProgress(name, progressID)
		defer func() { c.finishProfileProgress(result, err) }()
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := c.reconfigure.lock(ctx); err != nil {
		return nil, err
	}
	defer c.reconfigure.unlock()
	if apply {
		c.profilePhase("Checking profile and current services", 0, 0)
	}
	if err := c.refresh(); err != nil {
		return nil, err
	}
	cfg := c.cfg()
	if str(cfg["lifecycle"]) != "native" {
		return nil, fail("Cluster profiles require the native model lifecycle.")
	}
	p, err := readProfile(filepath.Join(filepath.Dir(c.path), "profiles.yaml"), name, cfg)
	if err != nil {
		return nil, err
	}
	rt := c.runtime.frozenClone(cfg)
	preview := func() (Object, error) {
		snapshot, err := rt.snapshot(ctx, true)
		if err != nil {
			return nil, err
		}
		if node := profileServicePlacement(p.capabilities, "spark-ocr"); node != "" {
			status, err := rt.profileOCR(ctx, node, "status")
			if err != nil {
				return nil, err
			}
			snapshot["profile_ocr"] = status
		}
		return planProfile(cfg, p, snapshot, func(m profileModel) bool {
			return rt.nativeReady(ctx, m.name, m.model, m.recipe, m.nodes, m.placement)
		}), nil
	}
	result, err = preview()
	if err != nil {
		return nil, err
	}
	result["plan_id"] = fmt.Sprintf("%x", sha256.Sum256(jsonBytes(Object{
		"name": name, "description": p.description, "config": cfg, "providers": p.capabilities, "steps": result["steps"], "auto_weights": p.autoWeights,
	})))
	if !apply || boolean(result["blocked"]) {
		return result, nil
	}
	if expectedPlan != "" && expectedPlan != str(result["plan_id"]) {
		return nil, fail("The profile plan changed. Preview the new plan before applying it.")
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
	checkConfig := func() error {
		if err := c.refresh(); err != nil {
			return err
		}
		if compactJSON(c.cfg()) != compactJSON(cfg) {
			return fail("The cluster configuration changed during apply. Review a new plan.")
		}
		if c.Fenced() {
			return fail("Admission is paused. Retry after it resumes.")
		}
		return nil
	}
	steps := arr(result["steps"])
	completed, modelChanged, err = c.applyProfileSteps(ctx, cfg, steps, checkConfig)
	if err != nil {
		return failed(err)
	}
	c.profilePhase("Checking final service readiness", 0, len(steps))
	if err := checkConfig(); err != nil {
		return failed(err)
	}
	verified, err := preview()
	if err != nil {
		return failed(err)
	}
	if boolean(verified["blocked"]) || len(arr(verified["steps"])) > 0 {
		return failed(fail("The running state still differs from the profile. Review a new plan."))
	}
	if gatewayAutoEnabled(cfg) {
		c.profilePhase("Updating gateway routes", 0, len(steps))
		if err := c.syncProfileAutoRoutesLocked(ctx, nil, p); err != nil {
			return failed(err)
		}
	}
	if err := checkConfig(); err != nil {
		return failed(err)
	}
	c.profilePhase("Saving selected profile", 0, len(steps))
	state := Object{"name": name, "applied_at": time.Now().UTC(), "capabilities": p.capabilities}
	if p.description != "" {
		state["description"] = p.description
	}
	if err := writeObject(profileStatePath(cfg), state); err != nil {
		return failed(fail("Could not save the selected profile: %v", err))
	}
	result["status"], result["completed"] = "applied", completed
	result["capabilities"] = verified["capabilities"]
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
	if err != nil || !simpleName.MatchString(str(state["name"])) || arr(state["capabilities"]) == nil {
		return Object{"error": "Could not read the selected profile."}
	}
	return state
}

func (c *Coordinator) runningProfileCapabilities(ctx context.Context, groups []any, snapshot Object) []any {
	cfg := c.cfg()
	if node := profileServicePlacement(groups, "spark-ocr"); node != "" && profileOCRRunning(snapshot, node) {
		if status, err := c.runtime.profileOCR(ctx, node, "status"); err == nil {
			snapshot["profile_ocr"] = status
		}
	}
	kept := planProfileServices(cfg, groups, snapshot).keep
	return profileAvailability(groups, func(provider Object) bool {
		if profileProviderKept(kept, provider) {
			return true
		}
		name, nodes := str(provider["workload"]), stringsOf(provider["nodes"])
		row := obj(obj(snapshot["models"])[name])
		if str(row["kind"]) != "llm" || str(row["state"]) != StateReady ||
			!sameStrings(stringsOf(row["nodes"]), nodes) {
			return false
		}
		record, err := c.runtime.readRecord(name)
		if err != nil {
			return false
		}
		on := str(record["placement"])
		model, recipe, selectedNodes, _, err := selectPlacement(cfg, name, on)
		return err == nil && sameStrings(selectedNodes, nodes) &&
			c.runtime.nativeReadyWithWorkerChecks(ctx, name, model, recipe, nodes, on, false)
	})
}

func profileCapabilitySummary(groups []any) string {
	var out strings.Builder
	for _, raw := range groups {
		group := obj(raw)
		providers := []string{}
		for _, rawProvider := range arr(group["providers"]) {
			provider := obj(rawProvider)
			providers = append(providers, str(provider["workload"])+" on "+strings.Join(stringsOf(provider["nodes"]), ", "))
		}
		fmt.Fprintf(&out, "  %s: %s (%s)\n", str(group["name"]), str(group["state"]), strings.Join(providers, " + "))
	}
	return out.String()
}

func renderProfile(result Object) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Profile %s (%s)\n", str(result["profile"]), str(result["status"]))
	if description := strings.Join(strings.Fields(str(result["description"])), " "); description != "" {
		fmt.Fprintln(&out, description)
	}
	if weights := obj(result["auto_weights"]); len(weights) > 0 {
		shares := autoReadyWeights(keys(weights), weights)
		parts := []string{}
		for _, model := range keys(shares) {
			parts = append(parts, fmt.Sprintf("%s %.1f%%", model, 100*num(shares[model])))
		}
		fmt.Fprintf(&out, "spark-auto weights: %s\n", strings.Join(parts, ", "))
	}
	if groups := arr(result["capabilities"]); len(groups) > 0 {
		if str(result["status"]) == "failed" {
			out.WriteString("Capabilities before apply\n")
		} else {
			out.WriteString("Capabilities now\n")
		}
		out.WriteString(profileCapabilitySummary(groups))
	}
	if verification := routeVerificationBlock(result); verification != "" {
		out.WriteString("\n" + verification)
	}
	stepKeys := []string{"kept", "steps"}
	if str(result["status"]) == "failed" {
		stepKeys = []string{"completed"}
		out.WriteString("Completed steps\n")
		if len(arr(result["completed"])) == 0 {
			out.WriteString("  No steps completed.\n")
		}
	}
	for _, key := range stepKeys {
		for _, v := range arr(result[key]) {
			row := obj(v)
			fmt.Fprintf(&out, "  %s %s on %s", str(row["action"]), str(row["workload"]), strings.Join(stringsOf(row["nodes"]), ", "))
			if key != "completed" && boolean(row["only_if_stopped_for_startup"]) {
				out.WriteString(" (only if stopped to make room)")
			}
			if key != "completed" && boolean(row["recheck_memory_at_start"]) {
				out.WriteString(" (check available memory at startup)")
			}
			out.WriteByte('\n')
			if key == "completed" {
				continue
			}
			for _, raw := range arr(row["stop_if_low_memory"]) {
				peer := obj(raw)
				fmt.Fprintf(&out, "    May first stop %s on %s if memory is low.\n", str(peer["workload"]), strings.Join(stringsOf(peer["nodes"]), ", "))
			}
		}
	}
	if str(result["status"]) != "failed" && len(arr(result["steps"])) == 0 {
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
		fmt.Fprintf(&out, "  Failed after %d completed steps: %s\n", len(arr(result["completed"])), err)
	}
	if err := str(result["routing_error"]); err != "" {
		fmt.Fprintf(&out, "  Route update failed: %s\n", err)
	}
	return out.String()
}
