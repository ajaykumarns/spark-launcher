package sparky

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type profileServicePlan struct {
	stop, start, keep, blockers []any
}

func profileServiceMemoryFloors(cfg, step Object) Object {
	model := obj(obj(cfg["models"])[str(step["workload"])])
	nodes := stringsOf(step["nodes"])
	if len(nodes) != 1 {
		return nil
	}
	return obj(obj(obj(model["settings"])[nodes[0]])["minimum_available_gib"])
}

func (p *profileServicePlan) add(action, name, node, kind string) {
	step := profileStep(action, name, []string{node})
	switch action {
	case "stop":
		step["service_kind"] = kind
		p.stop = append(p.stop, step)
	case "start":
		step["service_kind"] = kind
		p.start = append(p.start, step)
	case "keep":
		p.keep = append(p.keep, step)
	}
}

func planProfileServices(cfg Object, groups []any, snapshot Object) profileServicePlan {
	var plan profileServicePlan
	models := obj(cfg["models"])
	for _, name := range keys(models) {
		m := obj(models[name])
		settings := obj(m["settings"])
		want := profileServicePlacement(groups, name)
		switch str(m["kind"]) {
		case "audio":
			stops, start, keep, problem := planProfileAudio(name, settings, snapshot, want)
			if problem != "" {
				plan.blockers = append(plan.blockers, problem)
				continue
			}
			for _, step := range stops {
				step["service_kind"] = "audio"
				plan.stop = append(plan.stop, step)
			}
			if start != nil {
				start["service_kind"] = "audio"
				plan.start = append(plan.start, start)
			}
			if keep != nil {
				plan.keep = append(plan.keep, keep)
			}
		}
	}

	imageNode := profileServicePlacement(groups, "comfyui")
	for _, node := range keys(obj(cfg["nodes"])) {
		plan.addComfy(cfg, snapshot, node, imageNode == node)
	}

	ocrNode := profileServicePlacement(groups, "spark-ocr")
	ocrStatus := obj(snapshot["profile_ocr"])
	for _, node := range keys(obj(snapshot["nodes"])) {
		running, wanted := profileOCRRunning(snapshot, node), ocrNode == node
		switch {
		case wanted && running && boolean(ocrStatus["api_ready"]):
			plan.add("keep", "spark-ocr", node, "ocr")
		case wanted && running:
			plan.blockers = append(plan.blockers, fmt.Sprintf("Spark OCR is running without a ready API on %s.", node))
		case wanted && boolean(ocrStatus["installed"]):
			plan.add("start", "spark-ocr", node, "ocr")
		case wanted:
			plan.blockers = append(plan.blockers, fmt.Sprintf("Install OCR on %s before applying this profile.", node))
		case running:
			plan.add("stop", "spark-ocr", node, "ocr")
		}
	}
	return plan
}

func profileOCRRunning(snapshot Object, node string) bool {
	containers := obj(obj(obj(snapshot["nodes"])[node])["containers"])
	return boolean(obj(containers["spark-ocr"])["running"])
}

func (r *Runtime) profileOCR(ctx context.Context, node, action string) (Object, error) {
	payload := Object{"action": action}
	if node == "spark01" {
		return ManageOCR(r.root(), payload)
	}
	host := str(obj(obj(r.config()["nodes"])[node])["ssh"])
	if err := safeHost(host); err != nil {
		return nil, err
	}
	args := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", host,
		remoteSparkyPrefix + shellJoin([]string{"ocr", "--root", r.root(), "--payload", "-"})}
	timeout := 2 * time.Minute
	if action == "status" {
		timeout = 20 * time.Second
	}
	out, err := r.runChecked(ctx, args, jsonBytes(payload), timeout)
	if err != nil {
		return nil, err
	}
	return decode([]byte(out))
}

func (p *profileServicePlan) addComfy(cfg, snapshot Object, node string, wanted bool) {
	meta := obj(obj(cfg["nodes"])[node])
	state := obj(obj(snapshot["comfyui"])[node])
	unit := str(meta["comfy_unit"])
	unitState := str(obj(obj(obj(snapshot["nodes"])[node])["units"])[unit])
	reachable := boolean(state["reachable"])
	if unit == "" && !wanted && !reachable {
		return // No engine configured on this node.
	}
	if unit == "" {
		p.blockers = append(p.blockers, fmt.Sprintf("ComfyUI has no service unit on %s.", node))
		return
	}
	stopped := unitState == "inactive" || unitState == "failed"
	switch {
	case wanted && reachable && unitState == "active":
		p.add("keep", "comfyui", node, "comfyui")
	case wanted && !reachable && stopped:
		p.add("start", "comfyui", node, "comfyui")
	case !wanted && unitState == "active" && !boolean(state["executing"]) && !imageProfileBusy(state):
		p.add("stop", "comfyui", node, "comfyui")
	case !wanted && stopped && !reachable:
		return
	default:
		p.blockers = append(p.blockers, fmt.Sprintf("ComfyUI on %s is busy or its service state cannot be confirmed.", node))
	}
}

func (c *Coordinator) applyProfileService(ctx context.Context, rt *Runtime, kind string, step Object) error {
	defer c.runtime.invalidate()
	action := str(step["action"])
	switch kind {
	case "audio":
		return rt.applyProfileAudio(ctx, step)
	case "ocr":
		_, err := rt.profileOCR(ctx, stringsOf(step["nodes"])[0], action)
		return err
	case "comfyui":
		node := stringsOf(step["nodes"])[0]
		if action == "stop" {
			return c.stopProfileComfy(ctx, rt, node)
		}
		return rt.startComfy(ctx, node)
	default:
		return fail("Unknown profile service kind %s.", kind)
	}
}

func (r *Runtime) startComfy(ctx context.Context, node string) error {
	meta := obj(obj(r.config()["nodes"])[node])
	if err := r.unit(ctx, node, "start", str(meta["comfy_unit"])); err != nil {
		return err
	}
	r.invalidate()
	ctx, cancel := context.WithTimeout(ctx, comfyReadinessWait)
	defer cancel()
	for {
		state, err := r.inspectNode(ctx, node)
		if err == nil && boolean(obj(state["comfyui"])["reachable"]) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fail("ComfyUI on %s did not become ready in time.", node)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (c *Coordinator) stopProfileComfy(ctx context.Context, rt *Runtime, node string) error {
	// Pause admission during the final idle check and stop. The apply
	// operation holds reconfigure, so no legacy reload can change this flag.
	c.mu.Lock()
	if c.fenced || c.reloading {
		c.mu.Unlock()
		return fail("Admission is paused. Retry the profile switch later.")
	}
	for _, lease := range c.leases {
		if imageLease(lease.Name) && (contains(lease.Nodes, node) || strings.HasPrefix(lease.RequestID, node+":")) {
			c.mu.Unlock()
			return fail("ComfyUI on %s has an active job. Retry after it finishes.", node)
		}
	}
	c.reloading = true
	c.signal()
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.reloading = false
		c.signal()
		c.mu.Unlock()
	}()
	state, err := rt.inspectNode(ctx, node)
	if err != nil {
		return err
	}
	unit := str(obj(obj(rt.config()["nodes"])[node])["comfy_unit"])
	image := obj(state["comfyui"])
	if str(obj(state["units"])[unit]) != "active" || boolean(image["executing"]) || imageProfileBusy(image) {
		return fail("ComfyUI on %s is busy or its state cannot be confirmed.", node)
	}
	return rt.unit(ctx, node, "stop", unit)
}
