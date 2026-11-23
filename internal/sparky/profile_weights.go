package sparky

import (
	"context"
	"math"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Profile weights apply only to chat. Bifrost continues to own session affinity.
func parseProfileAutoWeights(node yaml.Node, p clusterProfile, cfg Object) (Object, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode || len(node.Content) == 0 {
		return nil, fail("Profile %s auto_weights must be a nonempty map of chat models to positive numbers.", p.name)
	}
	if !bifrostAuto(cfg) {
		return nil, fail("Profile %s auto_weights requires Bifrost chat routing.", p.name)
	}
	chat := Object{}
	for _, model := range p.models {
		if profileCapability(model.name, model.model) == "chat" {
			chat[model.name] = true
		}
	}
	weights, total := Object{}, 0.0
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.ShortTag() != "!!str" {
			return nil, fail("Profile %s auto_weights keys must be chat model names.", p.name)
		}
		name, _, err := resolveWorkload(cfg, key.Value)
		if err != nil || !boolean(chat[name]) {
			return nil, fail("Profile %s auto_weights model %s is not one of its chat providers.", p.name, key.Value)
		}
		if _, exists := weights[name]; exists {
			return nil, fail("Profile %s repeats auto_weights for %s.", p.name, name)
		}
		var weight float64
		if value.Kind != yaml.ScalarNode || (value.ShortTag() != "!!float" && value.ShortTag() != "!!int") || value.Decode(&weight) != nil || weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
			return nil, fail("Profile %s auto_weights for %s must be a finite positive number.", p.name, name)
		}
		weights[name] = weight
		total += weight
	}
	if len(weights) != len(chat) {
		return nil, fail("Profile %s auto_weights must include every chat provider.", p.name)
	}
	if math.IsInf(total, 0) || total <= 0 {
		return nil, fail("Profile %s auto_weights total must be finite and positive.", p.name)
	}
	for _, value := range weights {
		if num(value)/total == 0 {
			return nil, fail("Profile %s auto_weights cannot be normalized to positive shares.", p.name)
		}
	}
	return weights, nil
}

func profileAutoRoutingConfig(cfg Object, p clusterProfile) Object {
	result := copyObject(cfg)
	auto := copyObject(obj(cfg["auto"]))
	if auto == nil {
		auto = Object{}
	}
	auto["profile_weights"], auto["profile_name"] = copyObject(p.autoWeights), p.name
	result["auto"] = auto
	return result
}

func selectedProfileAutoRoutingConfig(cfg Object, configPath string) (Object, error) {
	if !bifrostAuto(cfg) {
		return cfg, nil
	}
	state := selectedProfile(cfg)
	if state == nil {
		return cfg, nil
	}
	if str(state["error"]) != "" {
		return nil, fail("Cannot load spark-auto profile weights: %s", str(state["error"]))
	}
	p, err := readProfile(filepath.Join(filepath.Dir(configPath), "profiles.yaml"), str(state["name"]), cfg)
	if err != nil {
		return nil, err
	}
	return profileAutoRoutingConfig(cfg, p), nil
}

// Apply must sync the target profile before it saves the selected profile.
func (c *Coordinator) syncProfileAutoRoutesLocked(ctx context.Context, excluded []string, p clusterProfile) error {
	return c.syncAutoRoutesForConfigLocked(ctx, profileAutoRoutingConfig(c.cfg(), p), excluded)
}
