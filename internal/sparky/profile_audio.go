package sparky

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The profile owns Parakeet membership. Its installed Docker recipes still
// define where the service can run and which containers belong to it.
func planProfileParakeet(cfg, snapshot Object, want string) (stops []Object, start, keep Object, blocker string) {
	model := obj(obj(cfg["models"])["parakeet"])
	settings := obj(model["settings"])
	if want != "" {
		_, _, _, _, err := selectPlacement(cfg, "parakeet", want)
		if err != nil || str(model["kind"]) != "audio" {
			return nil, nil, nil, fmt.Sprintf("Parakeet has no installed audio recipe on %s.", want)
		}
	}
	if len(settings) == 0 || str(model["kind"]) != "audio" {
		return nil, nil, nil, ""
	}
	for _, node := range keys(settings) {
		recipe := obj(settings[node])
		names := stringsOf(recipe["containers"])
		inventory := obj(obj(obj(snapshot["nodes"])[node])["containers"])
		var running, stopped []string
		for _, name := range names {
			state, known := obj(inventory[name])["running"].(bool)
			switch {
			case known && state:
				running = append(running, name)
			case known:
				stopped = append(stopped, name)
			case want == node:
				return nil, nil, nil, fmt.Sprintf("Parakeet container %s was not found on %s.", name, node)
			case inventory[name] != nil:
				return nil, nil, nil, fmt.Sprintf("Cannot determine the state of %s on %s.", name, node)
			}
		}
		if want == node {
			if len(stopped) == 0 {
				keep = profileStep("keep", "parakeet", []string{node})
			} else {
				start = profileStep("start", "parakeet", []string{node})
				start["containers"] = anyStrings(stopped)
			}
		} else if len(running) > 0 {
			slices.Reverse(running)
			step := profileStep("stop", "parakeet", []string{node})
			step["containers"] = anyStrings(running)
			stops = append(stops, step)
		}
	}
	return stops, start, keep, ""
}

func (r *Runtime) applyProfileParakeet(ctx context.Context, step Object) error {
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
