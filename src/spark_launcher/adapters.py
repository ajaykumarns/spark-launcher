"""Build per-launch native settings. Keep other providers and permissions intact."""
from __future__ import annotations

import copy
import io
import json
import os
import shutil
import subprocess
from pathlib import Path

from .catalog import SparkError, atomic_write, state_dir, timestamp, write_json
from .locking import file_lock

HARNESSES = ("claude", "hermes", "codex", "opencode")


def executable(config, harness):
    binary = config.get("binaries", {}).get(harness) or shutil.which(harness)
    if binary:
        binary = str(Path(binary).expanduser())
    if not binary or not Path(binary).is_file() or not os.access(binary, os.X_OK):
        raise SparkError(f"{harness} is not installed or executable. Install it or set binaries.{harness} in the site file.")
    return str(Path(binary).expanduser().absolute())


def build_plan(config, catalog, model, harness, project, session):
    if harness not in HARNESSES:
        raise SparkError("Choose claude, hermes, codex, or opencode.")
    origin, prefix = config["gateway"], config["model_prefix"]
    mid, context = model["id"], model["context_tokens"]
    timeout = config["timeout_seconds"] * 1000
    plan = {"harness": harness, "binary": executable(config, harness), "args": [],
            "environment_overrides": {}, "environment_unset": [], "model": model,
            "project": str(Path(project).expanduser().resolve()), "session": session}
    env = plan["environment_overrides"]
    headers = {"X-Client-ID": "sparky/" + harness, "X-Session-ID": session}
    if harness == "claude":
        env.update({"ANTHROPIC_BASE_URL": origin + "/anthropic", "ANTHROPIC_AUTH_TOKEN": "spark-keyless",
                    "ANTHROPIC_MODEL": mid, "ANTHROPIC_CUSTOM_MODEL_OPTION": mid,
                    "ANTHROPIC_CUSTOM_MODEL_OPTION_NAME": model.get("name", mid),
                    "CLAUDE_CODE_MAX_CONTEXT_TOKENS": str(context),
                    "ANTHROPIC_CUSTOM_HEADERS": "\n".join(f"{k}: {v}" for k, v in headers.items()),
                    "API_TIMEOUT_MS": str(timeout)})
        for name in ("ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
                     "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL"):
            env[name] = mid
        plan["environment_unset"] = ["ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
                                     "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_AWS"]
        plan["args"] = ["--model", mid]
    elif harness == "codex":
        values = {"model_provider": "spark", "model": mid, "review_model": mid, "model_context_window": context,
                  "model_auto_compact_token_limit": context * config["compact_percent"] // 100,
                  "model_providers.spark.name": "Spark Gateway", "model_providers.spark.base_url": origin + "/v1",
                  "model_providers.spark.wire_api": "responses", "model_providers.spark.requires_openai_auth": False,
                  "model_providers.spark.supports_websockets": False,
                  "model_providers.spark.stream_idle_timeout_ms": timeout}
        values.update({f"model_providers.spark.http_headers.{k}": v for k, v in headers.items()})
        for key, value in values.items():
            # JSON scalar strings/booleans/integers are also valid TOML values.
            plan["args"].extend(["-c", key + "=" + json.dumps(value)])
    elif harness == "opencode":
        models = {}
        for item in catalog["models"]:
            limits = {"context": item["context_tokens"]}
            if item.get("output_tokens", 0) > 0:
                limits["output"] = item["output_tokens"]
            models[item["id"].removeprefix(prefix)] = {
                "id": item["id"], "name": item.get("name", item["id"]), "limit": limits,
                "toolcall": True, "reasoning": item.get("reasoning", False),
                "modalities": {"input": item.get("inputs") or ["text"], "output": ["text"]}}
        selected = "spark/" + mid.removeprefix(prefix)
        raw = {"provider": {"spark": {"npm": "@ai-sdk/openai-compatible",
               "name": "Spark Gateway", "options": {"baseURL": origin + "/v1", "apiKey": "spark-keyless",
               "timeout": timeout, "headers": headers}, "models": models}}, "model": selected, "small_model": selected}
        env["OPENCODE_CONFIG_CONTENT"] = json.dumps(raw)
        plan["environment_unset"] = ["OPENCODE_CONFIG"]
        plan["args"] = ["--model", selected]
    else:
        plan["args"] = ["--provider", "spark-gateway", "--model", mid]
        env["HERMES_INFERENCE_MODEL"] = mid
    return plan


def sync_hermes(config, catalog, session):
    from ruamel.yaml import YAML
    yaml = YAML()
    yaml.preserve_quotes = True
    root = Path(os.environ.get("HERMES_HOME", Path.home() / ".hermes")).expanduser()
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    path, lock = root / "config.yaml", root / "config.yaml.spark-lock"
    with file_lock(lock,'Another Spark process holds the Hermes configuration lock.'):
        original = path.read_bytes() if path.exists() else b""
        document = yaml.load(original.decode()) or {}
        if not isinstance(document, dict):
            raise SparkError("Hermes configuration must be a mapping.")
        before = copy.deepcopy(document)
        providers = document.setdefault("providers", {})
        if not isinstance(providers, dict):
            raise SparkError("Hermes providers must be a mapping.")
        provider = providers.setdefault("spark-gateway", {})
        if not isinstance(provider, dict):
            raise SparkError("Hermes Spark provider must be a mapping.")
        provider.update({"name": "Spark Gateway", "api": config["gateway"] + "/v1",
                         "transport": "chat_completions", "discover_models": False,
                         "request_timeout_seconds": config["timeout_seconds"],
                         "stale_timeout_seconds": config["timeout_seconds"],
                         "models": {m["id"]: {"context_length": m["context_tokens"],
                                   "timeout_seconds": config["timeout_seconds"],
                                   "stale_timeout_seconds": config["timeout_seconds"]} for m in catalog["models"]}})
        extra_headers=provider.setdefault('extra_headers',{})
        if not isinstance(extra_headers,dict):raise SparkError('Hermes Spark extra_headers must be a mapping.')
        extra_headers['X-Client-ID']='sparky/hermes'
        buffer = io.StringIO()
        yaml.dump(document, buffer)
        encoded = buffer.getvalue().encode()
        # Prove the saved result changes only the owned provider block.
        parsed = yaml.load(encoded.decode()) or {}
        for obj in (before, parsed):
            obj.setdefault("providers", {}).pop("spark-gateway", None)
        if before != parsed:
            raise SparkError("Hermes update would change unrelated settings.")
        if original == encoded:
            return None
        backup = state_dir() / "backups" / f"{session}-hermes-config.yaml"
        if original:
            atomic_write(backup, original)
        atomic_write(path, encoded)
        return str(backup) if original else ''


def codex_catalog(plan, catalog):
    result = subprocess.run([plan["binary"], "debug", "models", "--bundled"], capture_output=True,
                            text=True, timeout=30)
    if result.returncode:
        raise SparkError("Cannot read the installed Codex model schema.")
    try:
        template = json.loads(result.stdout)["models"][0]
    except (KeyError, IndexError, ValueError):
        raise SparkError("Installed Codex returned an unknown catalog schema.") from None
    models = []
    for model in catalog["models"]:
        entry = copy.deepcopy(template)
        entry.update({"slug": model["id"], "display_name": model.get("name", model["id"]),
                      "description": "Spark Gateway", "context_window": model["context_tokens"],
                      "max_context_window": model["context_tokens"], "effective_context_window_percent": 100,
                      "visibility": "list", "supported_in_api": True, "upgrade": None, "model_messages": {},
                      "availability_nux": {"message": ""}, "available_access_programs": {"cyber": []},
                      "additional_speed_tiers": [], "service_tiers": [], "support_verbosity": False,
                      "apply_patch_tool_type": "freeform", "supports_search_tool": False,
                      "use_responses_lite": False, "input_modalities": model.get("inputs") or ["text"]})
        models.append(entry)
    path = state_dir() / "generated" / plan["session"] / "codex-models.json"
    write_json(path, {"models": models})
    plan["args"].extend(["-c", "model_catalog_json=" + json.dumps(str(path))])
    plan.setdefault("generated_files", []).append(str(path))


def child_env(plan):
    env = os.environ.copy()
    for key in plan["environment_unset"]:
        env.pop(key, None)
    env.update(plan["environment_overrides"])
    if plan['harness']=='opencode' and os.environ.get('OPENCODE_CONFIG_CONTENT'):
        try:original=json.loads(os.environ['OPENCODE_CONFIG_CONTENT'])
        except ValueError:raise SparkError('OPENCODE_CONFIG_CONTENT must contain valid JSON.') from None
        if not isinstance(original,dict) or not isinstance(original.get('provider',{}),dict):
            raise SparkError('OpenCode process settings and provider must be mappings.')
        owned=json.loads(env['OPENCODE_CONFIG_CONTENT'])
        original.setdefault('provider',{}).update(owned.pop('provider'))
        original.update(owned)
        if isinstance(original.get('enabled_providers'),list) and 'spark' not in original['enabled_providers']:
            original['enabled_providers'].append('spark')
        env['OPENCODE_CONFIG_CONTENT']=json.dumps(original)
    return env


def check_passthrough(harness, extra):
    for index, arg in enumerate(extra):
        if arg=='--':break  # Following words are literal prompt text.
        if arg in ("--model", "-m", "--provider", "--oss", "--local-provider") or arg.startswith(("--model=", "--provider=", "--local-provider=")) or (arg.startswith('-m') and not arg.startswith('--')):
            raise SparkError("Select the model and provider through sparky.")
        if harness == "codex":
            value = None
            if arg in ("-c", "--config") and index + 1 < len(extra):
                value = extra[index + 1]
            elif arg.startswith("--config="):
                value = arg.split("=", 1)[1]
            elif arg.startswith("-c") and len(arg) > 2:
                value = arg[2:]
            if value is not None:
                key = value.split("=", 1)[0].strip().strip('"')
                if key in ("model", "review_model", "model_provider", "model_context_window", "model_catalog_json",
                           "model_auto_compact_token_limit") or key.startswith("model_providers"):
                    raise SparkError("Set Codex model routing and context through sparky.")


def prepare(config, catalog, plan):
    if plan["harness"] == "hermes":
        sync_hermes(config, catalog, plan["session"])
    elif plan["harness"] == "codex":
        codex_catalog(plan, catalog)


def launch(config, catalog, plan, extra):
    check_passthrough(plan["harness"], extra)
    prepare(config, catalog, plan)
    record = {"session": plan["session"], "harness": plan["harness"], "model": plan["model"]["id"],
              "context": plan["model"]["context_tokens"], "gateway": config["gateway"],
              "catalog_fetched_at": catalog["fetched_at"], "project": plan["project"], "started_at": timestamp()}
    path = state_dir() / "launches" / (plan["session"] + ".json")
    write_json(path, record)
    print(f'Sparky: {plan["harness"]} · {record["model"]} · {record["context"]} tokens', file=__import__("sys").stderr)
    code = 1
    try:
        code = subprocess.call([plan["binary"], *plan["args"], *extra], cwd=plan["project"], env=child_env(plan))
        if code<0:code=128-code
        return code
    except KeyboardInterrupt:
        code = 130
        return code
    finally:
        record.update(finished_at=timestamp(), exit_code=code)
        write_json(path, record)
