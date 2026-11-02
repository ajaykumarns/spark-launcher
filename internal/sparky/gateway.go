// One-time Bifrost gateway migration. It runs only through the explicit
// admin command, keeps Docker bind-mounted file inodes via in-place writes,
// and rolls back every effect independently when any step fails.
package sparky

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// gatewayOperations are the pinned Bifrost transports/v2.3.2 operations. A
// nil allowed_requests map allows every operation; a non-nil map allows only
// explicit true flags. That default is preserved before disabling two.
var gatewayOperations = strings.Fields(`list_models model_retrieve text_completion text_completion_stream
chat_completion chat_completion_stream responses responses_stream responses_retrieve
responses_delete responses_cancel responses_input_items count_tokens compaction
embedding rerank decisions ocr speech speech_stream transcription transcription_stream
image_generation image_generation_stream image_edit image_edit_stream image_variation
video_generation video_edit video_retrieve video_download video_delete video_list video_remix
batch_create batch_list batch_retrieve batch_cancel batch_delete batch_results
file_upload file_list file_retrieve file_delete file_content
container_create container_list container_retrieve container_delete container_file_create
container_file_list container_file_retrieve container_file_content container_file_delete
passthrough passthrough_stream websocket_responses realtime live cached_content_create
cached_content_list cached_content_retrieve cached_content_update cached_content_delete`)

// gatewayRun is the injectable command runner; tests never invoke sudo/docker.
var gatewayRun = command

// gatewayRequest is the injectable HTTP seam for the Bifrost admin API.
var gatewayRequest = func(method, url string, payload Object, headers map[string]string) (Object, error) {
	return requestObject(context.Background(), method, url, payload, headers, 30*time.Second, false)
}

// gatewayWriteFile is the injectable in-place write seam. In-place writes
// retain the inode of Docker bind-mounted files; an atomic rename would
// break the container's view of the file.
var gatewayWriteFile = gatewayWriteInPlace

// gatewayWriteInPlace truncates and rewrites an existing file without
// replacing it, keeping its inode and mode intact.
func gatewayWriteInPlace(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Truncate(int64(len(b))); err != nil {
		return err
	}
	return f.Chmod(mode)
}

// gatewayInferenceRoutes migrates the nginx inference locations to the exact
// recognized new layout. It accepts only pinned layouts — the old pair, the
// reduced migrated pair, or the deployed expanded pair — each pattern exactly
// once, is byte-identical idempotent, and fails any unknown, mixed, or
// duplicated layout before any mutation happens.
func gatewayInferenceRoutes(text string) (string, error) {
	old := []string{
		"location ~ ^/v1/(chat/completions|completions|responses|audio/transcriptions)$ {",
		"location ~ ^/v1/(models|chat/completions|completions|responses|audio/transcriptions)$ {",
	}
	reduced := []string{
		"location ~ ^/(?:v1/(?:chat/completions|completions|responses|audio/transcriptions)|anthropic/v1/(?:messages|messages/count_tokens))$ {",
		"location ~ ^/(?:v1/(?:models|chat/completions|completions|responses|audio/transcriptions)|anthropic/v1/(?:messages|messages/count_tokens))$ {",
	}
	expanded := []string{
		"location ~ ^/(?:v1/(?:chat/completions|completions|responses|ocr|images/generations|audio/(?:transcriptions|speech))|anthropic/v1/(?:messages|messages/count_tokens))$ {",
		"location ~ ^/(?:v1/(?:models|chat/completions|completions|responses|ocr|images/generations|audio/(?:transcriptions|speech))|anthropic/v1/(?:messages|messages/count_tokens))$ {",
	}
	// Each group is complete only when every pattern appears exactly once.
	// A group with any present pattern that is not complete is a partial
	// pattern from a mixed or duplicated layout.
	group := func(patterns []string) (complete, present bool) {
		complete, present = true, false
		for _, p := range patterns {
			c := strings.Count(text, p)
			if c != 1 {
				complete = false
			}
			if c > 0 {
				present = true
			}
		}
		return complete, present
	}
	oldC, oldP := group(old)
	redC, redP := group(reduced)
	expC, expP := group(expanded)
	// Exactly one complete recognized group is required, and every pattern
	// of the two non-selected generations must be absent: one complete pair
	// plus one partial pattern from another generation is still a mixed
	// layout and must never be migrated. Zero recognized groups, duplicated
	// patterns, or any mixture leave the text untouched.
	complete := 0
	for _, c := range []bool{oldC, redC, expC} {
		if c {
			complete++
		}
	}
	if complete != 1 || (oldP && !oldC) || (redP && !redC) || (expP && !expC) {
		return "", fail("Unknown nginx inference layout. No change was applied.")
	}
	if oldC {
		for i, before := range old {
			text = strings.Replace(text, before, reduced[i], 1)
		}
	}
	return text, nil
}

// gatewayTranslatedProvider deep-copies the live provider and applies the
// pinned operation semantics. The existing custom provider must be the
// OpenAI-compatible garuda provider. A nil or absent allowed_requests means
// every pinned operation is allowed; an explicit map (including an empty
// one, which denies everything not listed) keeps all existing flags and only
// pins the five migrated operations. The input is never mutated.
func gatewayTranslatedProvider(before Object) (Object, error) {
	after := copyObject(before)
	raw, present := after["custom_provider_config"]
	if !present {
		return nil, fail("Expected the existing custom OpenAI-compatible garuda provider.")
	}
	custom, ok := raw.(Object)
	if !ok {
		return nil, fail("Expected the existing custom OpenAI-compatible garuda provider.")
	}
	if str(custom["base_provider_type"]) != "openai" {
		return nil, fail("Expected the existing custom OpenAI-compatible garuda provider.")
	}
	var allowed Object
	switch existing := custom["allowed_requests"].(type) {
	case nil:
		// Nil means allow all pinned operations.
		allowed = Object{}
		for _, op := range gatewayOperations {
			allowed[op] = true
		}
	case Object:
		// An explicit map — including an empty one — keeps every existing
		// flag; anything absent stays denied. Only the five migrated
		// operations are pinned onto it.
		allowed = copyObject(existing)
	default:
		return nil, fail("Expected an allowed_requests object.")
	}
	allowed["responses"] = false
	allowed["responses_stream"] = false
	allowed["chat_completion"] = true
	allowed["chat_completion_stream"] = true
	allowed["list_models"] = true
	custom["allowed_requests"] = allowed
	after["custom_provider_config"] = custom
	return after, nil
}

// gatewayMutableProvider returns the mutable provider payload. Read-only
// fields are excluded so credentials and keys are never rewritten.
func gatewayMutableProvider(provider Object) Object {
	excluded := []string{"name", "keys", "provider_status", "status", "description", "config_hash"}
	out := Object{}
	for k, v := range provider {
		if contains(excluded, k) {
			continue
		}
		out[k] = v
	}
	return out
}

func gatewayObjectsEqual(a, b any) bool { return reflect.DeepEqual(a, b) }

// gatewayNginx validates or gracefully reloads nginx inside the running
// inference container. No container or model is ever restarted.
func gatewayNginx(args ...string) error {
	full := append([]string{"sudo", "-n", "docker", "exec", "garuda-inference", "nginx"}, args...)
	result, err := gatewayRun(full, nil, 30*time.Second)
	if err != nil {
		return fail("Nginx validation or graceful reload failed.")
	}
	if result.Code != 0 {
		return fail("Nginx validation or graceful reload failed.")
	}
	return nil
}

// EnableGateway performs the one-time gateway migration under the gateway
// root, protected by an exclusive nonblocking admin file lock. A concurrent
// run returns a busy error instead of waiting indefinitely.
func EnableGateway(root string) (Object, error) {
	if !filepath.IsAbs(root) {
		return nil, fail("Expected an absolute gateway root.")
	}
	gateway := filepath.Join(root, "gateway")
	var result Object
	err := withFileLock(gateway+"/.spark-gateway-lock", false, func() error {
		var inner error
		result, inner = gatewayEnable(gateway)
		return inner
	})
	return result, err
}

func gatewayEnable(gateway string) (Object, error) {
	env, err := readEnv(gateway + "/gateway.env")
	if err != nil {
		return nil, fail("Gateway admin identity is missing.")
	}
	user, password := env["BIFROST_ADMIN_USER"], env["BIFROST_ADMIN_PASSWORD"]
	if user == "" || password == "" {
		return nil, fail("Gateway admin identity is missing.")
	}
	headers := map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))}
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

	nginxPath, configPath := gateway+"/nginx.conf", gateway+"/data/config.json"
	oldNginx, err := os.ReadFile(nginxPath)
	if err != nil {
		return nil, err
	}
	oldConfig, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	newNginxText, err := gatewayInferenceRoutes(string(oldNginx))
	if err != nil {
		return nil, err
	}
	newNginx := []byte(newNginxText)
	config, err := decode(oldConfig)
	if err != nil {
		return nil, fail("Static garuda provider configuration is missing.")
	}
	if _, err := gatewayStaticAllowed(config); err != nil {
		return nil, err
	}
	before, err := api(nil)
	if err != nil {
		return nil, err
	}
	after, err := gatewayTranslatedProvider(before)
	if err != nil {
		return nil, err
	}
	garudaCustom, err := gatewayStaticAllowed(config)
	if err != nil {
		return nil, err
	}
	garudaCustom["allowed_requests"] = after["custom_provider_config"].(Object)["allowed_requests"]

	backup := gateway + "/backups/spark-harnesses-" + strings.ReplaceAll(strings.ReplaceAll(now(), ":", ""), ".", "")
	if err := atomicWrite(backup+"/nginx.conf", oldNginx, 0600); err != nil {
		return nil, err
	}
	if err := atomicWrite(backup+"/config.json", oldConfig, 0600); err != nil {
		return nil, err
	}
	if err := writeObject(backup+"/provider.json", before); err != nil {
		return nil, err
	}

	if err := gatewayApply(after, config, newNginx, configPath, nginxPath, before, api); err != nil {
		if rollbackErr := gatewayRollback(before, oldConfig, oldNginx, configPath, nginxPath, api); rollbackErr != nil {
			return nil, fail("Gateway setup failed. Rollback needs attention. Backup: %s.", backup)
		}
		// The original failure is deliberately not embedded: API or nginx
		// errors must never leak admin credentials into messages.
		return nil, fail("Gateway setup failed. Previous settings were restored. Backup: %s.", backup)
	}
	result := Object{"status": "configured", "anthropic_messages_route": true,
		"responses_translated_to_chat": true, "existing_provider_fields_preserved": true,
		"no_model_restart": true, "no_container_restart": true, "backup": backup}
	if err := writeObject(backup+"/result.json", result); err != nil {
		return nil, err
	}
	return result, nil
}

// gatewayStaticAllowed returns the static config's garuda
// custom_provider_config object. Every level must be an actual JSON object;
// arrays, strings or missing levels fail before any API PUT or file effect.
func gatewayStaticAllowed(config Object) (Object, error) {
	providers, ok := config["providers"].(Object)
	if !ok {
		return nil, fail("Static garuda provider configuration is missing.")
	}
	garuda, ok := providers["garuda"].(Object)
	if !ok {
		return nil, fail("Static garuda provider configuration is missing.")
	}
	custom, ok := garuda["custom_provider_config"].(Object)
	if !ok {
		return nil, fail("Static garuda provider configuration is missing.")
	}
	return custom, nil
}

// gatewayApply runs the forward migration: provider API update, in-place
// static config and nginx writes, nginx validation and graceful reload,
// then verification that only the intended mutable fields changed.
func gatewayApply(after Object, config Object, newNginx []byte, configPath, nginxPath string, before Object, api func(Object) (Object, error)) error {
	if _, err := api(after); err != nil {
		return err
	}
	if err := gatewayWriteFile(configPath, jsonBytes(config), fileMode(configPath)); err != nil {
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
	check["custom_provider_config"] = prior["custom_provider_config"]
	if !gatewayObjectsEqual(check, prior) || !gatewayObjectsEqual(actual["custom_provider_config"], after["custom_provider_config"]) {
		return fail("Provider validation failed.")
	}
	return nil
}

func fileMode(path string) os.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return 0600
	}
	return info.Mode().Perm()
}

// gatewayRollback attempts every independent restore step even when an
// earlier one fails, and always re-validates and reloads nginx at the end.
func gatewayRollback(before Object, oldConfig, oldNginx []byte, configPath, nginxPath string, api func(Object) (Object, error)) error {
	restored := true
	if _, err := api(before); err != nil {
		restored = false
	}
	if err := gatewayWriteFile(configPath, oldConfig, fileMode(configPath)); err != nil {
		restored = false
	}
	if err := gatewayWriteFile(nginxPath, oldNginx, fileMode(nginxPath)); err != nil {
		restored = false
	}
	if err := gatewayNginx("-t"); err != nil {
		restored = false
	}
	if err := gatewayNginx("-s", "reload"); err != nil {
		restored = false
	}
	if !restored {
		return fail("One or more rollback steps failed.")
	}
	return nil
}
