// Pure installation-plan construction: the preparation half of the source
// deployment install_head, ported into a reusable, effect-free plan. The
// builder reads ONLY the staged models.json in the explicit stage directory,
// validates the effective configuration completely, and produces a typed
// plan of payloads and destinations. It never touches live host files,
// starts processes, writes service files, fetches credentials, or infers
// model readiness; the actual binary installation is the device agent's job.
package sparky

import (
	"net"
	"path/filepath"
	"sort"
	"strconv"
)

// InstallationText is a rendered text payload and its destination.
type InstallationText struct {
	Dest string
	Text string
}

// InstallationNodePlan is one node's per-node payloads: the metadata file,
// the Comfy plugin config, the embedded SparkyGuard hook, and the ComfyUI
// drop-in.
type InstallationNodePlan struct {
	Node         string
	NodeFileDest string
	NodeFileData []byte
	PluginDest   string
	PluginConfig Object
	GuardDest    string
	GuardData    []byte
	DropinDest   string
	DropinText   string
}

// InstallationPlan is the authoritative, fully defensive install plan. Every
// Object and byte slice is a deep copy, so later edits by the caller can
// never change an already produced plan.
type InstallationPlan struct {
	Root              string
	ConfigPath        string
	ControllerPort    int
	ServiceUser       string
	HelperPath        string
	Config            Object
	ControllerUnit    InstallationText
	ModelUnitTemplate InstallationText
	LegacyShim        InstallationText
	Nodes             []InstallationNodePlan
	// QwenDualNote is an instruction only: the INACTIVE Qwen dual recipe is
	// prepared later by the device agent; no recipe is transformed here.
	QwenDualNote string
}

// validateCleanAbs requires a clean absolute path (no ".", "..", trailing
// separators, or double slashes) that is also unit-text safe.
func validateCleanAbs(kind, p string) error {
	if !filepath.IsAbs(p) {
		return fail("%s must be an absolute path", kind)
	}
	if filepath.Clean(p) != p {
		return fail("%s must be a clean path", kind)
	}
	return validateUnitText(p)
}

// validateIP requires a configured IP address literal, never a hostname.
func validateIP(kind, v string) error {
	if net.ParseIP(v) == nil {
		return fail("%s must be a valid IP address", kind)
	}
	return nil
}

// validateMeta validates one node's metadata fields needed by the plan.
func validateMeta(node string, meta Object) error {
	if !serviceName.MatchString(str(meta["comfy_unit"])) {
		return fail("Invalid ComfyUI service name for %s.", node)
	}
	if err := validateCleanAbs("ComfyUI root for "+node, str(meta["comfy_root"])); err != nil {
		return err
	}
	if !privateURL(str(meta["comfy_url"])) {
		return fail("Invalid ComfyUI address for %s.", node)
	}
	if err := validateIP("Tailscale address for "+node, str(meta["tailscale_ip"])); err != nil {
		return err
	}
	if err := safeHost(str(meta["ssh"])); err != nil {
		return err
	}
	return nil
}

// planControllerURL builds the controller URL a node's ComfyUI plugin uses:
// the head reaches the controller over loopback; the worker reaches the head
// over its direct IP. The port always comes from the effective config.
func planControllerURL(head Object, port int) (map[string]string, error) {
	direct := str(head["direct_ip"])
	if err := validateIP("direct address for spark01", direct); err != nil {
		return nil, err
	}
	return map[string]string{
		"spark01": "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		"spark02": "http://" + net.JoinHostPort(direct, strconv.Itoa(port)),
	}, nil
}

// BuildInstallationPlan constructs the pure install plan. stageDir names the
// staging directory containing models.json; centralConfig, when non-nil, is
// the existing central configuration and takes precedence over the staged
// candidate, with a root mismatch aborting before any plan exists. The
// service user and helper path are explicit for the current host. The
// builder performs no effects beyond reading the staged file.
func BuildInstallationPlan(stageDir string, centralConfig Object, serviceUser, helperPath string) (*InstallationPlan, error) {
	if err := validateCleanAbs("stage directory", stageDir); err != nil {
		return nil, err
	}
	if err := validateServiceUser(serviceUser); err != nil {
		return nil, err
	}
	if err := validateAbsolutePath("helper", helperPath); err != nil {
		return nil, fail("helper path: %v", err)
	}
	staged, err := readCluster(filepath.Join(stageDir, "models.json"))
	if err != nil {
		return nil, err
	}
	root := str(staged["root"])
	cfg := staged
	if centralConfig != nil {
		// The caller's existing central configuration is the authority; the
		// staged candidate is only a bootstrap. A root disagreement means
		// the staged payload points somewhere else entirely and is aborted.
		if err := validateCluster(centralConfig); err != nil {
			return nil, err
		}
		if str(centralConfig["root"]) != root {
			return nil, fail("The installed cluster root differs from the staged root. No services were changed.")
		}
		cfg = centralConfig
	}
	// From here on, the plan is built only from the defensive copy so caller
	// edits can never change a produced plan.
	cfg = copyObject(cfg)
	root = str(cfg["root"])
	if err := validateCleanAbs("cluster root", root); err != nil {
		return nil, err
	}
	port := controllerPort(cfg)
	if port < 1 || port > 65535 {
		return nil, fail("Invalid controller port.")
	}
	configPath := filepath.Join(root, "config", "models.json")
	controllerDir := filepath.Join(root, "controller")
	nodes := obj(cfg["nodes"])
	head := obj(nodes["spark01"])
	urls, err := planControllerURL(head, port)
	if err != nil {
		return nil, err
	}
	plan := &InstallationPlan{
		Root:           root,
		ConfigPath:     configPath,
		ControllerPort: port,
		ServiceUser:    serviceUser,
		HelperPath:     helperPath,
		Config:         cfg,
		QwenDualNote: "Prepare the INACTIVE Qwen dual recipe (sparky-qwen-dual.service, " +
			"never enabled) separately after installation; no recipe transformation happens in this plan.",
	}
	names := append([]string{}, keys(nodes)...)
	sort.Strings(names)
	for _, node := range names {
		meta := obj(nodes[node])
		if err := validateMeta(node, meta); err != nil {
			return nil, err
		}
		if node != "spark01" && node != "spark02" {
			return nil, fail("Unknown node: %s.", node)
		}
		plugin := Object{
			"node":       node,
			"controller": urls[node],
			"operators": []any{
				"127.0.0.1", "::1",
				str(meta["tailscale_ip"]),
				str(head["tailscale_ip"]),
				str(head["direct_ip"]),
			},
		}
		nodePath := filepath.Join(controllerDir, "node-"+node+".json")
		pluginPath := filepath.Join(controllerDir, "comfy-"+node+".json")
		guardPath := filepath.Join(str(meta["comfy_root"]), "custom_nodes", "SparkyGuard", "__init__.py")
		dropinDest := "/etc/systemd/system/" + str(meta["comfy_unit"]) + ".d/sparky.conf"
		dropinText, err := ComfyUIDropin(helperPath, nodePath, pluginPath)
		if err != nil {
			return nil, err
		}
		plan.Nodes = append(plan.Nodes, InstallationNodePlan{
			Node:         node,
			NodeFileDest: nodePath,
			NodeFileData: jsonBytes(copyObject(meta)),
			PluginDest:   pluginPath,
			PluginConfig: copyObject(plugin),
			GuardDest:    guardPath,
			GuardData:    ComfyGuardScript(),
			DropinDest:   dropinDest,
			DropinText:   dropinText,
		})
	}
	controllerText, err := SparkyControllerUnit(serviceUser, helperPath, configPath, root)
	if err != nil {
		return nil, err
	}
	plan.ControllerUnit = InstallationText{Dest: "/etc/systemd/system/sparky-controller.service", Text: controllerText}
	modelText, err := SparkyModelUnitTemplate(serviceUser, helperPath, configPath)
	if err != nil {
		return nil, err
	}
	plan.ModelUnitTemplate = InstallationText{Dest: "/etc/systemd/system/sparky-model@.service", Text: modelText}
	shimText, err := LegacyModelShim(helperPath, configPath)
	if err != nil {
		return nil, err
	}
	plan.LegacyShim = InstallationText{
		// The manager's existing hook filename is its private integration
		// point; the shim content replaces ROOT/manager/cluster-control.sh.
		Dest: filepath.Join(root, "manager", "cluster-control.sh"),
		Text: shimText,
	}
	return plan, nil
}
