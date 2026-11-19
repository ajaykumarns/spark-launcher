package sparky

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Audio containers keep their catalog placement and dependency order.
func planProfileAudio(name string, settings, snapshot Object, want string) (stops []Object, start, keep Object, blocker string) {
	for _, node := range keys(settings) {
		recipe := obj(settings[node])
		names := stringsOf(recipe["containers"])
		inventory := obj(obj(obj(snapshot["nodes"])[node])["containers"])
		var running, stopped []string
		for _, container := range names {
			state, known := obj(inventory[container])["running"].(bool)
			switch {
			case known && state:
				running = append(running, container)
			case known:
				stopped = append(stopped, container)
			case want == node:
				return nil, nil, nil, fmt.Sprintf("%s container %s was not found on %s.", name, container, node)
			case inventory[container] != nil:
				return nil, nil, nil, fmt.Sprintf("Cannot determine the state of %s on %s.", container, node)
			}
		}
		if want == node {
			if len(stopped) == 0 {
				keep = profileStep("keep", name, []string{node})
			} else {
				start = profileStep("start", name, []string{node})
				start["containers"] = anyStrings(stopped)
			}
		} else if len(running) > 0 {
			slices.Reverse(running)
			step := profileStep("stop", name, []string{node})
			step["containers"] = anyStrings(running)
			stops = append(stops, step)
		}
	}
	return stops, start, keep, ""
}

func (r *Runtime) applyProfileAudio(ctx context.Context, step Object) error {
	node := stringsOf(step["nodes"])[0]
	start := str(step["action"]) == "start"
	for _, container := range stringsOf(step["containers"]) {
		verb := []string{"stop", "--time", "30"}
		if start {
			verb = []string{"start"}
		}
		if _, err := r.audioDocker(ctx, node, append(verb, container)...); err != nil {
			return fail("Could not %s %s on %s: %v", verb[0], container, node, err)
		}
		if start {
			if err := r.waitProfileAudio(ctx, node, container); err != nil {
				return err
			}
		}
	}
	r.invalidate()
	return nil
}

func (r *Runtime) waitProfileAudio(ctx context.Context, node, container string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	const status = "{{if not .State.Running}}stopped{{else if .State.Health}}{{.State.Health.Status}}{{else}}running{{end}}"
	for {
		out, err := r.audioDocker(ctx, node, "inspect", "--format", status, container)
		if err != nil {
			return err
		}
		switch strings.TrimSpace(out) {
		case "healthy", "running":
			return nil
		case "stopped", "unhealthy":
			return fail("%s on %s did not become ready.", container, node)
		}
		select {
		case <-ctx.Done():
			return fail("Timed out waiting for %s on %s.", container, node)
		case <-time.After(time.Second):
		}
	}
}
