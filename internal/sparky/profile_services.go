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

func planProfileServices(cfg Object, p clusterProfile, snapshot Object) profileServicePlan {
	var plan profileServicePlan
	models := obj(cfg["models"])
	for _, name := range keys(models) {
		m := obj(models[name])
		settings := obj(m["settings"])
		want := str(p.services[name])
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
		case "comfyui":
			for _, node := range keys(settings) {
				plan.addComfy(cfg, snapshot, name, node, want == node)
			}
		}
	}

	ocr := obj(obj(obj(obj(snapshot["nodes"])["spark01"])["containers"])["spark-ocr"])
	ocrWanted := str(p.services["spark-ocr"]) == "spark01"
	switch {
	case ocrWanted && boolean(ocr["running"]):
		plan.add("keep", "spark-ocr", "spark01", "ocr")
	case ocrWanted && boolean(snapshot["ocr_installed"]):
		plan.add("start", "spark-ocr", "spark01", "ocr")
	case ocrWanted:
		plan.blockers = append(plan.blockers, "Install Spark01 OCR before applying this profile.")
	case boolean(ocr["running"]):
		plan.add("stop", "spark-ocr", "spark01", "ocr")
	}
	return plan
}

func (p *profileServicePlan) addComfy(cfg, snapshot Object, name, node string, wanted bool) {
	meta := obj(obj(cfg["nodes"])[node])
	state := obj(obj(snapshot["comfyui"])[node])
	unit := str(meta["comfy_unit"])
	unitState := str(obj(obj(obj(snapshot["nodes"])[node])["units"])[unit])
	reachable := boolean(state["reachable"])
	if unit == "" && !wanted {
		return // No engine configured on this node.
	}
	if wanted && unit == "" {
		p.blockers = append(p.blockers, fmt.Sprintf("ComfyUI has no service unit on %s.", node))
		return
	}
	switch {
	case wanted && reachable:
		p.add("keep", name, node, "comfyui")
	case wanted && contains([]string{"inactive", "failed"}, unitState):
		p.add("start", name, node, "comfyui")
	case !wanted && reachable && !boolean(state["executing"]) && !imageProfileBusy(state):
		p.add("stop", name, node, "comfyui")
	case !wanted && contains([]string{"inactive", "failed"}, unitState) && !reachable:
		// Already stopped.
	default:
		p.blockers = append(p.blockers, fmt.Sprintf("ComfyUI on %s is busy or its state cannot be confirmed.", node))
	}
}

func (c *Coordinator) applyProfileService(ctx context.Context, kind string, step Object) error {
	action := str(step["action"])
	switch kind {
	case "audio":
		return c.runtime.applyProfileAudio(ctx, step)
	case "ocr":
		_, err := ManageOCR(c.runtime.root(), Object{"action": action})
		c.runtime.invalidate()
		return err
	case "comfyui":
		node := stringsOf(step["nodes"])[0]
		if action == "stop" {
			return c.stopProfileComfy(ctx, node)
		}
		return c.runtime.startComfy(ctx, node)
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

func (c *Coordinator) stopProfileComfy(ctx context.Context, node string) error {
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
	state, err := c.runtime.inspectNode(ctx, node)
	if err != nil {
		return err
	}
	image := obj(state["comfyui"])
	if boolean(image["executing"]) || imageProfileBusy(image) {
		return fail("ComfyUI on %s is busy or its state cannot be confirmed.", node)
	}
	meta := obj(obj(c.cfg()["nodes"])[node])
	err = c.runtime.unit(ctx, node, "stop", str(meta["comfy_unit"]))
	c.runtime.invalidate()
	return err
}
