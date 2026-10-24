"""One-time Bifrost migration. Run only through the explicit admin command."""
from __future__ import annotations

import base64
import copy
import json
import os
import subprocess
from pathlib import Path

from .catalog import SparkError, atomic_write, request_json, timestamp, write_json
from .control import read_env

# Pinned Bifrost transports/v2.3.2: nil flags allow every operation. A non-nil
# map allows only explicit true flags. Preserve that default before disabling two.
OPERATIONS = """list_models model_retrieve text_completion text_completion_stream
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
cached_content_list cached_content_retrieve cached_content_update cached_content_delete""".split()


def inference_routes(text):
    old = ["location ~ ^/v1/(chat/completions|completions|responses|audio/transcriptions)$ {",
           "location ~ ^/v1/(models|chat/completions|completions|responses|audio/transcriptions)$ {"]
    new = ["location ~ ^/(?:v1/(?:chat/completions|completions|responses|audio/transcriptions)|anthropic/v1/(?:messages|messages/count_tokens))$ {",
           "location ~ ^/(?:v1/(?:models|chat/completions|completions|responses|audio/transcriptions)|anthropic/v1/(?:messages|messages/count_tokens))$ {"]
    if all(text.count(value) == 1 for value in new):
        return text
    if not all(text.count(value) == 1 for value in old):
        raise SparkError("Unknown nginx inference layout. No change was applied.")
    for before, after in zip(old, new):
        text = text.replace(before, after, 1)
    return text


def translated_provider(before):
    after = copy.deepcopy(before)
    custom = after.get("custom_provider_config", {})
    if custom.get("base_provider_type") != "openai":
        raise SparkError("Expected the existing custom OpenAI-compatible garuda provider.")
    allowed = custom.get("allowed_requests")
    if allowed is None:
        allowed = {key: True for key in OPERATIONS}
    allowed.update(responses=False, responses_stream=False, chat_completion=True,
                   chat_completion_stream=True, list_models=True)
    custom["allowed_requests"] = allowed
    return after


def mutable_provider(provider):
    excluded = {"name", "keys", "provider_status", "status", "description", "config_hash"}
    return {key: value for key, value in provider.items() if key not in excluded}


def enable_gateway(root):
    gateway = Path(root) / "gateway"
    lock = gateway / ".spark-gateway-lock"
    fd = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    os.close(fd)
    try:
        return _enable_gateway(gateway)
    finally:
        lock.unlink(missing_ok=True)


def _enable_gateway(gateway):
    env = read_env(gateway / "gateway.env")
    user, password = env.get("BIFROST_ADMIN_USER"), env.get("BIFROST_ADMIN_PASSWORD")
    if not user or not password:
        raise SparkError("Gateway admin identity is missing.")
    headers = {"Authorization": "Basic " + base64.b64encode(f"{user}:{password}".encode()).decode()}

    def api(payload=None):
        return request_json("http://127.0.0.1:8081/api/providers/garuda",
                            payload=mutable_provider(payload) if payload is not None else None,
                            method="PUT" if payload is not None else "GET", headers=headers, timeout=30)

    def nginx(*args):
        result = subprocess.run(["sudo", "-n", "docker", "exec", "garuda-inference", "nginx", *args],
                                capture_output=True, timeout=30)
        if result.returncode:
            raise SparkError("Nginx validation or graceful reload failed.")

    nginx_path, config_path = gateway / "nginx.conf", gateway / "data/config.json"
    old_nginx, old_config = nginx_path.read_bytes(), config_path.read_bytes()
    new_nginx = inference_routes(old_nginx.decode()).encode()
    config = json.loads(old_config)
    before = api()
    after = translated_provider(before)
    try:
        config["providers"]["garuda"]["custom_provider_config"]["allowed_requests"] = after["custom_provider_config"]["allowed_requests"]
    except KeyError:
        raise SparkError("Static garuda provider configuration is missing.") from None
    backup = gateway / "backups" / ("spark-harnesses-" + timestamp().replace(":", ""))
    atomic_write(backup / "nginx.conf", old_nginx)
    atomic_write(backup / "config.json", old_config)
    write_json(backup / "provider.json", before)
    try:
        api(after)
        # In-place writes retain Docker bind-mounted file inodes.
        config_path.write_text(json.dumps(config, indent=2) + "\n")
        nginx_path.write_bytes(new_nginx)
        nginx("-t")
        nginx("-s", "reload")
        actual = api()
        check, prior = mutable_provider(actual), mutable_provider(before)
        check["custom_provider_config"] = prior["custom_provider_config"]
        if check != prior or actual["custom_provider_config"] != after["custom_provider_config"]:
            raise SparkError("Provider validation failed.")
    except Exception as original:
        failures = []
        for operation in (lambda: api(before), lambda: config_path.write_bytes(old_config),
                          lambda: nginx_path.write_bytes(old_nginx), lambda: nginx("-t"),
                          lambda: nginx("-s", "reload")):
            try:
                operation()
            except Exception:
                failures.append(True)
        if failures:
            raise SparkError(f"Gateway setup failed. Rollback needs attention. Backup: {backup}") from None
        raise SparkError(f"Gateway setup failed. Previous settings were restored. Backup: {backup}") from original
    result = {"status": "configured", "anthropic_messages_route": True,
              "responses_translated_to_chat": True, "existing_provider_fields_preserved": True,
              "no_model_restart": True, "no_container_restart": True, "backup": str(backup)}
    write_json(backup / "result.json", result)
    return result
