package sparky

// Real-contract tests for installGatewayConnect: the actual function runs
// against a fake Bifrost admin API and real temporary files whose inodes are
// preserved by the in-place writer, with nginx validation/reload recorded by
// the fake. The DEFAULT installFenceState and installFence seams are
// exercised elsewhere against the real controller server; here the gateway
// seam itself is the unit under test, never overridden.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// gatewayConnectFixture builds a real gateway directory under a temp root,
// wires the fake Bifrost API and nginx seams, and returns the cluster config
// pointing at that root plus the fake.
func gatewayConnectFixture(t *testing.T, nginxText string) (Object, *gatewayFake, string) {
	t.Helper()
	root := t.TempDir()
	gateway := filepath.Join(root, "gateway")
	if err := os.MkdirAll(filepath.Join(gateway, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gateway, "gateway.env"),
		[]byte("BIFROST_ADMIN_USER=fixture\nBIFROST_ADMIN_PASSWORD=fixture-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gateway, "nginx.conf"), []byte(nginxText), 0o644); err != nil {
		t.Fatal(err)
	}
	before := Object{"name": "garuda", "keys": []any{Object{"value": "secret-key-material"}},
		"custom_provider_config": Object{"base_provider_type": "openai"},
		"network_config":         Object{"base_url": "http://127.0.0.1:8000"}}
	config := Object{"providers": Object{"garuda": copyObject(before)}, "allowed_requests": Object{"chat_completion": true}}
	configBytes, _ := json.MarshalIndent(config, "", "  ")
	if err := os.WriteFile(filepath.Join(gateway, "data", "config.json"), configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newGatewayFake(before)
	gatewayInstallFake(t, fake)
	cfg := planCluster()
	cfg["root"] = root
	return cfg, fake, gateway
}

// gatewayConnectNginx is a clean, previously untouched two-listener layout.
func gatewayConnectNginx() string {
	return "events {}\nhttp {\n" +
		"    listen 192.0.2.10:8080;\n" +
		"    location / {\n        proxy_pass http://core;\n    }\n" +
		"    listen 127.0.0.1:8082 proxy_protocol;\n" +
		"    location /v1/ {\n        proxy_pass http://core;\n    }\n" +
		"}\n"
}

// gatewayCatalogPorts asserts the exact owned block after each configured
// inference listener and returns both proxy ports.
func gatewayCatalogPorts(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	var ports []string
	for _, anchor := range []string{"listen 192.0.2.10:8080;", "listen 127.0.0.1:8082 proxy_protocol;"} {
		idx := strings.Index(text, anchor)
		if idx < 0 {
			t.Fatalf("anchor missing: %s", anchor)
		}
		tail := text[idx+len(anchor):]
		marker := "\n    " + installationCatalogMarker
		if !strings.HasPrefix(tail, marker) {
			t.Fatalf("no owned catalog block after %s:\n%s", anchor, truncate(tail, 200))
		}
		rest := tail[len(marker):]
		const pass = "\n    location = /sparky/catalog {\n        limit_except GET { deny all; }\n" +
			"        auth_basic off;\n        proxy_pass http://127.0.0.1:"
		if !strings.HasPrefix(rest, pass) {
			t.Fatalf("wrong block shape after %s:\n%s", anchor, truncate(rest, 300))
		}
		rest = rest[len(pass):]
		end := strings.Index(rest, "/sparky/catalog;")
		if end <= 0 {
			t.Fatalf("no proxy port after %s", anchor)
		}
		ports = append(ports, rest[:end])
	}
	return ports
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// A clean layout gets exactly one owned read-only catalog block per listener
// at the desired port, and the provider points at the controller.
func TestGatewayConnectCleanInsert(t *testing.T) {
	cfg, fake, gateway := gatewayConnectFixture(t, gatewayConnectNginx())
	nginxPath := filepath.Join(gateway, "nginx.conf")
	result, undo, err := installGatewayConnect(cfg, 9010, filepath.Join(gateway, "backups", "x"))
	if err != nil {
		t.Fatalf("installGatewayConnect: %v", err)
	}
	if !boolean(result["catalog_enabled"]) || str(result["base_url"]) != "http://127.0.0.1:9010" {
		t.Fatalf("result: %v", result)
	}
	ports := gatewayCatalogPorts(t, nginxPath)
	if len(ports) != 2 || ports[0] != "9010" || ports[1] != "9010" {
		t.Fatalf("catalog ports: %v", ports)
	}
	if fake.stored["network_config"].(Object)["base_url"] != "http://127.0.0.1:9010" {
		t.Fatalf("provider base_url: %v", fake.stored["network_config"])
	}
	if _, ok := gatewayMutableProvider(fake.stored)["keys"]; ok {
		t.Fatal("keys must never be rewritten")
	}
	if !fake.nginxCalled {
		t.Fatal("nginx was never validated or reloaded")
	}
	// The undo restores the exact previous nginx bytes and provider.
	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	current, _ := os.ReadFile(nginxPath)
	if string(current) != gatewayConnectNginx() {
		t.Fatalf("undo did not restore nginx:\n%s", current)
	}
	if fake.stored["network_config"].(Object)["base_url"] != "http://127.0.0.1:8000" {
		t.Fatalf("undo did not restore the provider: %v", fake.stored["network_config"])
	}
}

// A repeated run on an already-correct layout is idempotent: the nginx file
// keeps its inode and its exact bytes, and no catalog is duplicated.
func TestGatewayConnectIdempotent(t *testing.T) {
	cfg, _, gateway := gatewayConnectFixture(t, gatewayConnectNginx())
	nginxPath := filepath.Join(gateway, "nginx.conf")
	if _, _, err := installGatewayConnect(cfg, 9010, filepath.Join(gateway, "backups", "x")); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	beforeStat, err := os.Stat(nginxPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, _ := os.ReadFile(nginxPath)
	if _, _, err := installGatewayConnect(cfg, 9010, filepath.Join(gateway, "backups", "y")); err != nil {
		t.Fatalf("second connect: %v", err)
	}
	afterStat, err := os.Stat(nginxPath)
	if err != nil {
		t.Fatal(err)
	}
	if statBefore, ok := beforeStat.Sys().(*syscall.Stat_t); ok {
		statAfter := afterStat.Sys().(*syscall.Stat_t)
		if statBefore.Ino != statAfter.Ino {
			t.Fatal("the in-place write must preserve the nginx inode")
		}
	}
	afterBytes, _ := os.ReadFile(nginxPath)
	if string(afterBytes) != string(beforeBytes) {
		t.Fatalf("idempotent run rewrote nginx:\n%s", afterBytes)
	}
	ports := gatewayCatalogPorts(t, nginxPath)
	if len(ports) != 2 || ports[0] != "9010" || ports[1] != "9010" {
		t.Fatalf("catalog duplicated: %v", ports)
	}
}

// A complete recognized pair with the old port is safely updated to the
// desired port at both listeners, preserving the rest of the file.
func TestGatewayConnectOldPortUpdated(t *testing.T) {
	cfg, _, gateway := gatewayConnectFixture(t, gatewayConnectNginx())
	nginxPath := filepath.Join(gateway, "nginx.conf")
	if _, _, err := installGatewayConnect(cfg, 9010, filepath.Join(gateway, "backups", "x")); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	if _, _, err := installGatewayConnect(cfg, 9020, filepath.Join(gateway, "backups", "y")); err != nil {
		t.Fatalf("port change: %v", err)
	}
	ports := gatewayCatalogPorts(t, nginxPath)
	if len(ports) != 2 || ports[0] != "9020" || ports[1] != "9020" {
		t.Fatalf("catalog ports after change: %v", ports)
	}
	current, _ := os.ReadFile(nginxPath)
	if strings.Count(string(current), installationCatalogMarker) != 2 {
		t.Fatal("port change duplicated the catalog blocks")
	}
}

// A partial marker, a duplicate block, an unowned location or an unknown
// layout is refused with zero effects: no provider PUT, no file write, no
// nginx call.
func TestGatewayConnectRefusesLayoutsBeforeEffects(t *testing.T) {
	cases := map[string]func(text string) string{
		"partial marker one listener": func(text string) string {
			return strings.Replace(text, "listen 192.0.2.10:8080;",
				"listen 192.0.2.10:8080;\n"+installationCatalogBlock(9010), 1)
		},
		"duplicate block on one listener": func(text string) string {
			return strings.Replace(text, "listen 192.0.2.10:8080;",
				"listen 192.0.2.10:8080;\n"+installationCatalogBlock(9010)+installationCatalogBlock(9010), 1)
		},
		"unowned catalog location": func(text string) string {
			return strings.Replace(text, "    location / {",
				"    location = /sparky/catalog {\n        proxy_pass http://127.0.0.1:9;\n    }\n"+
					"    location / {", 1)
		},
		"wrong ACL shape": func(text string) string {
			broken := strings.Replace(installationCatalogBlock(9010),
				"limit_except GET { deny all; }", "allow all;", 1)
			return strings.Replace(text, "listen 127.0.0.1:8082 proxy_protocol;",
				"listen 127.0.0.1:8082 proxy_protocol;\n"+broken, 1)
		},
		"missing listener": func(text string) string {
			return strings.Replace(text, "    listen 127.0.0.1:8082 proxy_protocol;\n", "", 1)
		},
		"disagreeing ports": func(text string) string {
			text = strings.Replace(text, "listen 192.0.2.10:8080;",
				"listen 192.0.2.10:8080;\n"+installationCatalogBlock(9010), 1)
			return strings.Replace(text, "listen 127.0.0.1:8082 proxy_protocol;",
				"listen 127.0.0.1:8082 proxy_protocol;\n"+installationCatalogBlock(9030), 1)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, fake, gateway := gatewayConnectFixture(t, gatewayConnectNginx())
			nginxPath := filepath.Join(gateway, "nginx.conf")
			original, err := os.ReadFile(nginxPath)
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(gateway, "data", "config.json")
			originalConfig, _ := os.ReadFile(configPath)
			broken := mutate(string(original))
			if err := os.WriteFile(nginxPath, []byte(broken), 0o644); err != nil {
				t.Fatal(err)
			}
			beforeCalls := len(fake.calls)
			_, _, err = installGatewayConnect(cfg, 9020, filepath.Join(gateway, "backups", "x"))
			if err == nil {
				t.Fatalf("%s: layout must be refused", name)
			}
			if len(fake.calls) != beforeCalls {
				t.Fatalf("%s: refused layout must have zero effects, calls: %v", name, fake.calls)
			}
			current, _ := os.ReadFile(nginxPath)
			if string(current) != broken {
				t.Fatalf("%s: nginx was modified", name)
			}
			currentConfig, _ := os.ReadFile(configPath)
			if string(currentConfig) != string(originalConfig) {
				t.Fatalf("%s: static config was modified", name)
			}
		})
	}
}

// A failure during the forward apply restores every previous state
// independently: the provider, both files, and the nginx reload.
func TestGatewayConnectFailureUndoesEverything(t *testing.T) {
	cfg, fake, gateway := gatewayConnectFixture(t, gatewayConnectNginx())
	nginxPath := filepath.Join(gateway, "nginx.conf")
	configPath := filepath.Join(gateway, "data", "config.json")
	fake.failAPI["GET2"] = 1 // the post-apply verification read fails
	_, _, err := installGatewayConnect(cfg, 9010, filepath.Join(gateway, "backups", "x"))
	if err == nil {
		t.Fatal("a verification failure must fail the connection")
	}
	current, _ := os.ReadFile(nginxPath)
	if string(current) != gatewayConnectNginx() {
		t.Fatalf("nginx was not restored:\n%s", current)
	}
	if fake.stored["network_config"].(Object)["base_url"] != "http://127.0.0.1:8000" {
		t.Fatalf("provider was not restored: %v", fake.stored["network_config"])
	}
	static := gatewayRead(t, configPath)
	base := str(obj(obj(obj(static["providers"])["garuda"])["network_config"])["base_url"])
	if base != "http://127.0.0.1:8000" {
		t.Fatalf("static config was not restored: %s", base)
	}
	if !fake.nginxCalled {
		t.Fatal("nginx was never re-validated or reloaded during the undo")
	}
}
