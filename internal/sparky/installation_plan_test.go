package sparky

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const planBasePort = 8020

// planCluster returns a minimal valid cluster configuration.
func planCluster() Object {
	return Object{
		"version":         json.Number("1"),
		"root":            "/srv/spark-cluster",
		"controller_port": json.Number("8020"),
		"policy":          Object{"interrupt_active_jobs": false},
		"models": Object{
			"glm-5.3-flash": Object{
				"kind":        "llm",
				"run_on":      "spark01",
				"keep_loaded": false,
				"reasoning":   false,
				"settings": Object{
					"spark01": Object{
						"enabled":        true,
						"units":          Object{"spark01": "spark-glm.service"},
						"backend_url":    "http://127.0.0.1:8001",
						"context_tokens": json.Number("4096"),
						"concurrency":    json.Number("4"),
						"parameters":     Object{},
					},
					"both": Object{
						"enabled":        true,
						"units":          Object{"spark01": "spark-glm-0.service", "spark02": "spark-glm-1.service"},
						"backend_url":    "http://127.0.0.1:8001",
						"context_tokens": json.Number("4096"),
						"concurrency":    json.Number("4"),
						"parameters":     Object{},
					},
				},
			},
		},
		"nodes": Object{
			"spark01": Object{
				"ssh":          "spark01",
				"direct_ip":    "198.51.100.10",
				"tailscale_ip": "192.0.2.10",
				"comfy_url":    "http://192.0.2.10:8188",
				"comfy_root":   "/srv/image-gen/ComfyUI",
				"comfy_unit":   "spark-imagegen.service",
			},
			"spark02": Object{
				"ssh":          "spark02",
				"direct_ip":    "198.51.100.11",
				"tailscale_ip": "192.0.2.11",
				"comfy_url":    "http://192.0.2.11:8188",
				"comfy_root":   "/srv/image-gen/ComfyUI",
				"comfy_unit":   "spark-imagegen.service",
			},
		},
	}
}

// planStage writes a staged models.json (with extra metadata and exact
// numeric spellings) into a temporary stage directory.
func planStage(t *testing.T, mutate func(Object)) string {
	t.Helper()
	dir := t.TempDir()
	cfg := planCluster()
	if mutate != nil {
		mutate(cfg)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func planBuild(t *testing.T, stage string) *InstallationPlan {
	t.Helper()
	plan, err := BuildInstallationPlan(stage, nil, "spark-user", "/home/spark-user/.local/bin/sparky")
	if err != nil {
		t.Fatalf("BuildInstallationPlan: %v", err)
	}
	return plan
}

func TestBuildInstallationPlanShape(t *testing.T) {
	stage := planStage(t, func(c Object) {
		obj(obj(c["nodes"])["spark01"])["gpu_count"] = json.Number("8")
	})
	plan := planBuild(t, stage)
	if plan.Root != "/srv/spark-cluster" || plan.ConfigPath != "/srv/spark-cluster/config/models.json" {
		t.Fatalf("bad root/config path: %s %s", plan.Root, plan.ConfigPath)
	}
	if plan.ControllerPort != planBasePort {
		t.Fatalf("bad controller port: %d", plan.ControllerPort)
	}
	if len(plan.Nodes) != 2 {
		t.Fatalf("expected two nodes, got %d", len(plan.Nodes))
	}
	for _, n := range plan.Nodes {
		wantDest := "/srv/spark-cluster/controller/node-" + n.Node + ".json"
		if n.NodeFileDest != wantDest {
			t.Fatalf("node file dest %s", n.NodeFileDest)
		}
		var meta Object
		if err := json.Unmarshal(n.NodeFileData, &meta); err != nil {
			t.Fatalf("node file is not JSON: %v", err)
		}
		if str(meta["comfy_unit"]) != "spark-imagegen.service" {
			t.Fatalf("node metadata lost fields: %v", meta)
		}
		if !bytes.Contains(n.NodeFileData, []byte("\"gpu_count\": 8")) && n.Node == "spark01" {
			t.Fatalf("numeric field not preserved: %s", n.NodeFileData)
		}
		if n.GuardDest != filepath.Join("/srv/image-gen/ComfyUI", "custom_nodes/SparkyGuard/__init__.py") {
			t.Fatalf("guard dest %s", n.GuardDest)
		}
		if len(n.GuardData) == 0 || !bytes.Contains(n.GuardData, []byte("def ")) {
			t.Fatalf("guard payload missing")
		}
		if n.DropinDest != "/etc/systemd/system/spark-imagegen.service.d/sparky.conf" {
			t.Fatalf("dropin dest %s", n.DropinDest)
		}
		if !strings.Contains(n.DropinText, "ExecStop=") || strings.Contains(n.DropinText, "garuda-models") {
			t.Fatalf("dropin text wrong: %s", n.DropinText)
		}
	}
	// Exact embedded guard bytes must equal the template helper's copy.
	if !bytes.Equal(plan.Nodes[0].GuardData, ComfyGuardScript()) {
		t.Fatal("guard bytes differ from the embedded asset")
	}
	if plan.ControllerUnit.Dest != "/etc/systemd/system/sparky-controller.service" {
		t.Fatalf("controller unit dest: %+v", plan.ControllerUnit)
	}
	for _, want := range []string{
		"User=spark-user",
		"WorkingDirectory=/srv/spark-cluster",
		"EnvironmentFile=/srv/spark-cluster/vllm/api.env",
		"ExecStart=:\"/home/spark-user/.local/bin/sparky\" --json node controller --file \"/srv/spark-cluster/config/models.json\"",
		"Restart=on-failure",
	} {
		if !strings.Contains(plan.ControllerUnit.Text, want) {
			t.Fatalf("controller unit missing %q in:\n%s", want, plan.ControllerUnit.Text)
		}
	}
	if plan.ModelUnitTemplate.Dest != "/etc/systemd/system/sparky-model@.service" ||
		strings.Contains(plan.ModelUnitTemplate.Text, "[Install]") ||
		strings.Contains(plan.ModelUnitTemplate.Text, "WantedBy") {
		t.Fatalf("model template must be inactive: %+v", plan.ModelUnitTemplate)
	}
	if plan.LegacyShim.Text == "" || !strings.Contains(plan.LegacyShim.Text, "supervise") {
		t.Fatal("legacy shim text missing")
	}
	if plan.LegacyShim.Dest != "/srv/spark-cluster/manager/cluster-control.sh" {
		t.Fatalf("legacy shim dest: %s", plan.LegacyShim.Dest)
	}
}

func TestBuildInstallationPlanControllerURLs(t *testing.T) {
	plan := planBuild(t, planStage(t, nil))
	var head, worker *InstallationNodePlan
	for i := range plan.Nodes {
		switch plan.Nodes[i].Node {
		case "spark01":
			head = &plan.Nodes[i]
		case "spark02":
			worker = &plan.Nodes[i]
		}
	}
	if got := str(head.PluginConfig["controller"]); got != "http://127.0.0.1:8020" {
		t.Fatalf("head controller URL: %s", got)
	}
	if got := str(worker.PluginConfig["controller"]); got != "http://198.51.100.10:8020" {
		t.Fatalf("worker controller URL: %s", got)
	}
}

func TestBuildInstallationPlanAlternatePortAndIPv6(t *testing.T) {
	stage := planStage(t, func(c Object) {
		c["controller_port"] = json.Number("9443")
		obj(obj(c["nodes"])["spark01"])["direct_ip"] = "2001:db8::10"
	})
	plan, err := BuildInstallationPlan(stage, nil, "spark-user", "/home/spark-user/.local/bin/sparky")
	if err != nil {
		t.Fatal(err)
	}
	var worker InstallationNodePlan
	for _, n := range plan.Nodes {
		if n.Node == "spark02" {
			worker = n
		}
	}
	if got := str(worker.PluginConfig["controller"]); got != "http://[2001:db8::10]:9443" {
		t.Fatalf("IPv6/alternate-port controller URL: %s", got)
	}
}

func TestBuildInstallationPlanOperatorAllowlist(t *testing.T) {
	plan := planBuild(t, planStage(t, nil))
	for _, n := range plan.Nodes {
		ops := stringsOf(n.PluginConfig["operators"])
		want := []string{"127.0.0.1", "::1"}
		if n.Node == "spark01" {
			want = append(want, "192.0.2.10", "192.0.2.10", "198.51.100.10")
		} else {
			want = append(want, "192.0.2.11", "192.0.2.10", "198.51.100.10")
		}
		if len(ops) != len(want) {
			t.Fatalf("%s allowlist: %v", n.Node, ops)
		}
		for i := range want {
			if ops[i] != want[i] {
				t.Fatalf("%s allowlist: %v", n.Node, ops)
			}
		}
	}
}

func TestBuildInstallationPlanCentralConfigPrecedenceAndRootMismatch(t *testing.T) {
	stage := planStage(t, nil)
	central := planCluster()
	central["controller_port"] = json.Number("9001")
	plan, err := BuildInstallationPlan(stage, central, "spark-user", "/home/spark-user/.local/bin/sparky")
	if err != nil {
		t.Fatal(err)
	}
	if plan.ControllerPort != 9001 {
		t.Fatalf("central config did not take precedence: %d", plan.ControllerPort)
	}
	if num(plan.Config["controller_port"]) != 9001 {
		t.Fatal("plan config is not the central config")
	}
	mismatch := planCluster()
	mismatch["root"] = "/elsewhere/cluster"
	if _, err := BuildInstallationPlan(stage, mismatch, "spark-user", "/home/spark-user/.local/bin/sparky"); err == nil {
		t.Fatal("root mismatch must abort")
	}
}

func TestBuildInstallationPlanDefensiveCopies(t *testing.T) {
	stage := planStage(t, nil)
	central := planCluster()
	plan, err := BuildInstallationPlan(stage, central, "spark-user", "/home/spark-user/.local/bin/sparky")
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's config and the produced plan must not cross.
	central["root"] = "/tampered"
	obj(plan.Config["nodes"])["spark01"] = Object{"tampered": true}
	if str(plan.Root) != "/srv/spark-cluster" {
		t.Fatal("plan root was mutated through Config")
	}
	if str(obj(obj(plan.Nodes[0].PluginConfig))["node"]) != "spark01" {
		t.Fatal("plugin config not defensively copied")
	}
	plan.Nodes[0].GuardData[0] = 'x'
	if plan.Nodes[1].GuardData[0] == 'x' {
		t.Fatal("guard bytes are shared between nodes")
	}
}

func TestBuildInstallationPlanNumberPreservation(t *testing.T) {
	stage := planStage(t, nil)
	plan := planBuild(t, stage)
	if _, ok := plan.Config["version"].(json.Number); !ok {
		t.Fatalf("version lost exact numeric spelling: %T", plan.Config["version"])
	}
	if _, ok := plan.Config["controller_port"].(json.Number); !ok {
		t.Fatalf("controller_port lost exact numeric spelling: %T", plan.Config["controller_port"])
	}
	var round Object
	if err := json.Unmarshal(jsonBytes(plan.Config), &round); err != nil {
		t.Fatal(err)
	}
	if num(round["controller_port"]) != 8020 {
		t.Fatal("plan config does not round-trip")
	}
}

func TestBuildInstallationPlanValidation(t *testing.T) {
	stage := planStage(t, nil)
	good := "spark-user"
	helper := "/home/spark-user/.local/bin/sparky"
	if _, err := BuildInstallationPlan("relative/stage", nil, good, helper); err == nil {
		t.Fatal("relative stage dir must be rejected")
	}
	if _, err := BuildInstallationPlan(stage, nil, "", helper); err == nil {
		t.Fatal("empty service user must be rejected")
	}
	if _, err := BuildInstallationPlan(stage, nil, "bad user;rm", helper); err == nil {
		t.Fatal("invalid service user must be rejected")
	}
	if _, err := BuildInstallationPlan(stage, nil, good, "sparky"); err == nil {
		t.Fatal("relative helper path must be rejected")
	}
	if _, err := BuildInstallationPlan(t.TempDir(), nil, good, helper); err == nil {
		t.Fatal("stage dir without models.json must be rejected")
	}
	// Malformed staged configuration is rejected before any plan is built.
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "models.json"), []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildInstallationPlan(bad, nil, good, helper); err == nil {
		t.Fatal("malformed staged config must be rejected")
	}
}

func TestBuildInstallationPlanInvalidNodeFields(t *testing.T) {
	cases := map[string]func(Object){
		"bad unit name":       func(c Object) { obj(obj(c["nodes"])["spark02"])["comfy_unit"] = "evil\nservice" },
		"relative root":       func(c Object) { obj(obj(c["nodes"])["spark02"])["comfy_root"] = "relative/ComfyUI" },
		"bad comfy url":       func(c Object) { obj(obj(c["nodes"])["spark02"])["comfy_url"] = "https://user:pw@h/x?y" },
		"bad tailscale":       func(c Object) { obj(obj(c["nodes"])["spark02"])["tailscale_ip"] = "not-an-ip" },
		"bad direct ip":       func(c Object) { obj(obj(c["nodes"])["spark01"])["direct_ip"] = "999.1.1.1" },
		"bad ssh host":        func(c Object) { obj(obj(c["nodes"])["spark02"])["ssh"] = "bad host" },
		"bad controller port": func(c Object) { c["controller_port"] = json.Number("0") },
	}
	for name, mutate := range cases {
		stage := planStage(t, mutate)
		if _, err := BuildInstallationPlan(stage, nil, "spark-user", "/home/spark-user/.local/bin/sparky"); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

func TestBuildInstallationPlanNoSecretsOrReadiness(t *testing.T) {
	stage := planStage(t, nil)
	plan, err := BuildInstallationPlan(stage, nil, "spark-user", "/home/spark-user/.local/bin/sparky")
	if err != nil {
		t.Fatal(err)
	}
	// The plan is a plain value; nothing was written anywhere.
	for _, n := range plan.Nodes {
		text := n.DropinText + string(n.NodeFileData) + string(n.GuardData)
		if strings.Contains(strings.ToLower(text), "api_key") ||
			strings.Contains(strings.ToLower(text), "password") {
			t.Fatal("plan carries credential-like content")
		}
	}
	if strings.Contains(plan.ModelUnitTemplate.Text, "WantedBy=multi-user.target") {
		t.Fatal("model template must not be enabled")
	}
}
