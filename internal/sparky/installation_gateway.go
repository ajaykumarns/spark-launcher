// Installation-time gateway connection: the actual Bifrost route connect
// performed by InstallHead. It points the existing garuda provider — live
// and static — at the newly installed controller, adds exactly one
// read-only catalog location to each of the two configured inference
// listeners, preserves every other provider field, credential and unknown
// key byte-for-byte, keeps the Docker bind-mounted file inodes through
// in-place writes, and returns a real undo that restores every previous
// state independently when a later installation step fails.
package sparky

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// installationCatalogMarker identifies an already-connected nginx layout so
// a repeated installation is idempotent instead of duplicating locations.
const installationCatalogMarker = "# sparky: authoritative read-only model catalog"

// installationCatalogBlock is the exact owned read-only catalog location
// for one inference listener at the given controller port. This pinned shape
// is the only recognized layout; no general nginx parser is invented.
func installationCatalogBlock(port int) string {
	return "    " + installationCatalogMarker + "\n" +
		"    location = /sparky/catalog {\n" +
		"        limit_except GET { deny all; }\n" +
		"        auth_basic off;\n" +
		"        proxy_pass http://127.0.0.1:" + strconv.Itoa(port) + "/sparky/catalog;\n" +
		"        proxy_set_header Host $host;\n" +
		"    }\n"
}

// installationCatalogPrefix is the fixed part of an owned block up to the
// proxy_pass port, anchored directly after its listener's listen directive.
const installationCatalogPrefix = "\n    " + installationCatalogMarker + "\n" +
	"    location = /sparky/catalog {\n" +
	"        limit_except GET { deny all; }\n" +
	"        auth_basic off;\n" +
	"        proxy_pass http://127.0.0.1:"

// installationCatalogSuffix completes the owned block after the port.
const installationCatalogSuffix = "/sparky/catalog;\n" +
	"        proxy_set_header Host $host;\n" +
	"    }\n"

// installationCatalogNginx validates the nginx inference listener layout and
// returns the desired nginx text. The two configured inference listeners must
// each carry exactly one owned catalog block of the pinned shape, or none at
// all. Zero blocks are inserted at both listeners; a complete pair with a
// different port is rewritten to the desired port; a correct pair is returned
// unchanged. A marker count that does not match the owned blocks, a block in
// the wrong place, a wrong ACL or proxy shape, a duplicated or unmarked
// catalog location, or a missing or duplicated listener is refused before
// any effect anywhere in the gateway.
func installationCatalogNginx(oldNginx []byte, cfg Object, port int) ([]byte, error) {
	text := string(oldNginx)
	markerLine := "    " + installationCatalogMarker + "\n"
	anchors := []string{
		"listen " + str(obj(obj(cfg["nodes"])["spark01"])["tailscale_ip"]) + ":8080;",
		"listen 127.0.0.1:8082 proxy_protocol;",
	}
	// Every anchor must be present exactly once: the configured inference
	// listeners are the only accepted insertion points.
	for _, anchor := range anchors {
		if strings.Count(text, anchor) != 1 {
			return nil, fail("Unrecognized inference listener layout. The gateway was not changed.")
		}
	}
	// A marker outside an owned block directly after a listener means a
	// partial or misplaced layout that must never be silently extended.
	if strings.Count(text, markerLine) > len(anchors) {
		return nil, fail("Unrecognized catalog marker layout. The gateway was not changed.")
	}
	ports := []int{}
	for _, anchor := range anchors {
		idx := strings.Index(text, anchor)
		tail := text[idx+len(anchor):]
		switch {
		case strings.HasPrefix(tail, "\n"+markerLine):
			// The owned block is inserted directly after its listener's
			// newline, so the recognized tail starts with "\n" + marker.
			// The complete pinned shape with an integer proxy port is
			// required; anything else is a wrong shape.
			if !strings.HasPrefix(tail, installationCatalogPrefix) ||
				!strings.Contains(tail[len(installationCatalogPrefix):], installationCatalogSuffix) {
				return nil, fail("Unrecognized catalog location shape. The gateway was not changed.")
			}
			digits := tail[len(installationCatalogPrefix):]
			end := strings.Index(digits, "/sparky/catalog;")
			if end <= 0 {
				return nil, fail("Unrecognized catalog proxy_pass. The gateway was not changed.")
			}
			digits = digits[:end]
			blockPort, convErr := strconv.Atoi(digits)
			if convErr != nil || digits != strconv.Itoa(blockPort) {
				return nil, fail("Unrecognized catalog proxy_pass. The gateway was not changed.")
			}
			ports = append(ports, blockPort)
		case strings.HasPrefix(tail, "\n    #"):
			// A comment immediately after a listener is a foreign or
			// partial catalog marker, never an owned block.
			return nil, fail("Unrecognized catalog marker layout. The gateway was not changed.")
		default:
			ports = append(ports, -1) // no block at this listener
		}
	}
	// Both listeners must agree: either both owned or both absent. Exactly
	// one block is a partial layout that must never be completed silently.
	if (ports[0] == -1) != (ports[1] == -1) {
		return nil, fail("A partial catalog layout was refused. The gateway was not changed.")
	}
	// No unowned catalog location may exist anywhere: an unmarked or
	// duplicate location would bypass or shadow the authoritative route.
	owned := 0
	if ports[0] != -1 {
		owned = 2
	}
	if strings.Count(text, "location = /sparky/catalog") != owned {
		return nil, fail("An unowned catalog location was found. The gateway was not changed.")
	}
	if strings.Count(text, markerLine) != owned {
		return nil, fail("Unrecognized catalog marker layout. The gateway was not changed.")
	}
	if ports[0] != -1 && ports[0] != ports[1] {
		return nil, fail("The catalog blocks disagree on their proxy port. The gateway was not changed.")
	}
	switch {
	case owned == 0:
		block := installationCatalogBlock(port)
		for _, anchor := range anchors {
			text = strings.Replace(text, anchor, anchor+"\n"+block, 1)
		}
	case ports[0] == port && ports[1] == port:
		// Already correct: byte-identical idempotent.
	default:
		// A complete recognized pair with another port: update each
		// proxy_pass to the desired port. The old port is known exactly,
		// so only the two owned blocks move.
		block := installationCatalogBlock(port)
		for _, oldPort := range []int{ports[0], ports[1]} {
			text = strings.ReplaceAll(text, installationCatalogBlock(oldPort), block)
		}
	}
	return []byte(text), nil
}

// installGatewayConnect performs the installation gateway connection under
// the cluster root. The controller port comes from the effective plan; the
// backup directory receives the exact previous bytes of every file it
// touches. All layout validation happens before the first effect: an
// unknown listener layout, a missing provider shape, or unreadable files
// leave the gateway completely untouched. The returned result carries no
// credential material, and the returned undo is safe to call even after the
// initiating context is gone (each effect re-bounds itself).
func installGatewayConnect(cfg Object, port int, backup string) (Object, func() error, error) {
	gateway := filepath.Join(str(cfg["root"]), "gateway")
	env, err := readEnv(filepath.Join(gateway, "gateway.env"))
	if err != nil || env["BIFROST_ADMIN_USER"] == "" || env["BIFROST_ADMIN_PASSWORD"] == "" {
		return nil, nil, fail("Gateway admin identity is missing.")
	}
	headers := map[string]string{"Authorization": "Basic " +
		base64.StdEncoding.EncodeToString([]byte(env["BIFROST_ADMIN_USER"]+":"+env["BIFROST_ADMIN_PASSWORD"]))}
	const apiURL = "http://127.0.0.1:8081/api/providers/garuda"
	api := func(payload Object) (Object, error) {
		method := "GET"
		var body Object
		if payload != nil {
			method = "PUT"
			body = gatewayMutableProvider(payload)
		}
		return gatewayRequest(method, apiURL, body, headers)
	}
	nginxPath := filepath.Join(gateway, "nginx.conf")
	configPath := filepath.Join(gateway, "data", "config.json")
	oldNginx, err := os.ReadFile(nginxPath)
	if err != nil {
		return nil, nil, fail("The gateway nginx configuration could not be read.")
	}
	oldConfig, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, fail("The static gateway configuration could not be read.")
	}
	// The listener layout is validated, and the desired nginx text derived,
	// before the first API read: a refused layout has zero effects anywhere.
	newNginx, err := installationCatalogNginx(oldNginx, cfg, port)
	if err != nil {
		return nil, nil, err
	}
	before, err := api(nil)
	if err != nil {
		return nil, nil, fail("The live gateway provider could not be read.")
	}
	if _, ok := before["network_config"].(Object); !ok {
		return nil, nil, fail("Expected a custom provider network configuration.")
	}
	if _, ok := before["network_config"].(Object)["base_url"].(string); !ok {
		return nil, nil, fail("Expected a custom provider base_url.")
	}
	// Only the base_url moves; every other live field, credential and
	// unknown key is preserved exactly.
	newURL := "http://127.0.0.1:" + strconv.Itoa(port)
	after := copyObject(before)
	obj(after["network_config"])["base_url"] = newURL

	// The static configuration keeps its shape: allowed_requests,
	// credentials and unknown fields stay, only network_config.base_url
	// changes, so the nginx bind mount keeps serving the same provider.
	document, err := decode(oldConfig)
	if err != nil {
		return nil, nil, fail("The static gateway configuration is not valid JSON.")
	}
	providers, ok := document["providers"].(Object)
	if !ok {
		return nil, nil, fail("Static garuda provider configuration is missing.")
	}
	garuda, ok := providers["garuda"].(Object)
	if !ok {
		return nil, nil, fail("Static garuda provider configuration is missing.")
	}
	staticNetwork, ok := garuda["network_config"].(Object)
	if !ok {
		return nil, nil, fail("Static garuda provider configuration is missing.")
	}
	if _, ok := staticNetwork["base_url"].(string); !ok {
		return nil, nil, fail("Expected a static provider base_url.")
	}
	staticNetwork["base_url"] = newURL
	newConfig := jsonBytes(document)

	// The exact previous states are archived before any effect.
	if err := os.MkdirAll(backup, 0o700); err != nil {
		return nil, nil, err
	}
	if err := atomicWrite(filepath.Join(backup, "provider-before.json"), jsonBytes(before), 0o600); err != nil {
		return nil, nil, err
	}
	if err := atomicWrite(filepath.Join(backup, "gateway-before.json"), oldConfig, 0o600); err != nil {
		return nil, nil, err
	}
	if err := atomicWrite(filepath.Join(backup, "nginx-before.conf"), oldNginx, 0o600); err != nil {
		return nil, nil, err
	}

	// Forward apply: provider update, in-place file writes (inode
	// preserving for the Docker bind mounts), validation, graceful reload,
	// then verification that only the intended fields moved.
	applyErr := func() error {
		if _, err := api(after); err != nil {
			return err
		}
		if err := gatewayWriteFile(configPath, newConfig, fileMode(configPath)); err != nil {
			return err
		}
		if err := gatewayWriteFile(nginxPath, newNginx, fileMode(nginxPath)); err != nil {
			return err
		}
		if err := gatewayNginx("-t"); err != nil {
			return err
		}
		if err := gatewayNginx("-s", "reload"); err != nil {
			return err
		}
		actual, err := api(nil)
		if err != nil {
			return err
		}
		check, prior := gatewayMutableProvider(actual), gatewayMutableProvider(before)
		check["network_config"] = prior["network_config"]
		if !gatewayObjectsEqual(check, prior) {
			return fail("Gateway provider fields changed outside the intended URL.")
		}
		if !gatewayObjectsEqual(actual["network_config"], after["network_config"]) {
			return fail("Gateway provider fields changed outside the intended URL.")
		}
		return nil
	}()
	// The undo restores every previous state independently; each attempt
	// runs even when an earlier one fails.
	undo := func() error {
		restored := true
		if _, uerr := api(before); uerr != nil {
			restored = false
		}
		if uerr := gatewayWriteFile(configPath, oldConfig, fileMode(configPath)); uerr != nil {
			restored = false
		}
		if uerr := gatewayWriteFile(nginxPath, oldNginx, fileMode(nginxPath)); uerr != nil {
			restored = false
		}
		if uerr := gatewayNginx("-t"); uerr != nil {
			restored = false
		}
		if uerr := gatewayNginx("-s", "reload"); uerr != nil {
			restored = false
		}
		if !restored {
			return fail("The gateway rollback needs attention. Backup: %s.", backup)
		}
		return nil
	}
	if applyErr != nil {
		if uerr := undo(); uerr != nil {
			return nil, nil, fail("The gateway connection failed and its rollback needs attention.")
		}
		return nil, nil, fail("The gateway connection failed. Previous settings were restored.")
	}
	result := Object{"catalog_enabled": true, "base_url": newURL,
		"provider_fields_preserved": true, "no_model_restart": true, "no_container_restart": true}
	return result, undo, nil
}
