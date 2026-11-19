package menubar

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type ProfilePlan struct {
	ID      string
	Details string
}

type ProfileCapability struct {
	Name  string
	State string
}

func (c *Controller) Profiles(ctx context.Context) ([]Choice, error) {
	data, err := c.profileCommand(ctx, "list", "")
	if err != nil {
		return nil, err
	}
	rows, ok := data["profiles"].([]any)
	if !ok {
		return nil, errors.New("Profile list returned invalid data.")
	}
	choices := make([]Choice, 0, len(rows))
	for _, raw := range rows {
		row := obj(raw)
		name := str(row["name"])
		if name == "" {
			return nil, errors.New("Profile list returned a missing name.")
		}
		if str(row["error"]) != "" {
			continue // The CLI list reports the validation error.
		}
		var groups []string
		for _, cap := range profileCapabilities(row) {
			groups = append(groups, cap.Name)
		}
		label := name
		if len(groups) > 0 {
			label += " · " + strings.Join(groups, ", ")
		}
		choices = append(choices, Choice{ID: name, Name: label})
	}
	return choices, nil
}

func (c *Controller) PlanProfile(ctx context.Context, name string) (ProfilePlan, error) {
	data, err := c.profileCommand(ctx, "plan", name)
	if err != nil {
		return ProfilePlan{}, err
	}
	steps, ok := data["steps"].([]any)
	id := str(data["plan_id"])
	if str(data["status"]) != "planned" || !ok || id == "" {
		return ProfilePlan{}, errors.New("Profile plan returned invalid data.")
	}
	var lines []string
	for _, raw := range steps {
		step := obj(raw)
		action, workload := str(step["action"]), str(step["workload"])
		if (action != "start" && action != "stop") || workload == "" {
			return ProfilePlan{}, errors.New("Profile plan returned an invalid step.")
		}
		var nodes []string
		for _, node := range objectList(step["nodes"]) {
			nodes = append(nodes, str(node))
		}
		lines = append(lines, fmt.Sprintf("%s %s on %s", action, workload, strings.Join(nodes, ", ")))
	}
	if len(lines) == 0 {
		lines = append(lines, "No workloads will start, stop, or restart.")
	}
	return ProfilePlan{ID: id, Details: strings.Join(lines, "\n")}, nil
}

func (c *Controller) ApplyProfile(ctx context.Context, name, expectedPlan string) error {
	if expectedPlan == "" {
		return errors.New("Preview this profile before applying it.")
	}
	data, err := c.profileCommand(ctx, "apply", name, "--expect-plan", expectedPlan)
	if err != nil {
		return err
	}
	if str(data["status"]) != "applied" {
		return errors.New("Sparky did not confirm the profile change. Check status.")
	}
	return nil
}

func (c *Controller) profileCommand(ctx context.Context, action, name string, flags ...string) (object, error) {
	args := append([]string{"--json", "profile", action}, flags...)
	if action != "list" {
		args = append(args, "--", name)
	}
	res, runErr := c.run(ctx, c.binary, args)
	data, decodeErr := decodeObject(res.Stdout)
	if runErr == nil && decodeErr == nil {
		if blocked, _ := data["blocked"].(bool); blocked {
			var blockers []string
			for _, raw := range objectList(data["blockers"]) {
				if msg := sanitizeMessage(str(raw)); msg != "" {
					blockers = append(blockers, msg)
				}
			}
			if len(blockers) > 0 {
				return nil, errors.New(strings.Join(blockers, " "))
			}
			return nil, errors.New("Sparky blocked this profile change.")
		}
		if res.Code == 0 && (action == "list" || str(data["profile"]) == name) {
			return data, nil
		}
	}
	return nil, friendlyError(res, runErr, "Profile "+action)
}

func profileCapabilities(data object) []ProfileCapability {
	var out []ProfileCapability
	for _, raw := range objectList(data["capabilities"]) {
		row := obj(raw)
		if name := str(row["name"]); name != "" {
			out = append(out, ProfileCapability{Name: name, State: str(row["state"])})
		}
	}
	return out
}

func objectList(v any) []any {
	rows, _ := v.([]any)
	return rows
}
