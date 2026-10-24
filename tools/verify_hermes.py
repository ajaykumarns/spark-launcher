"""Run with Hermes's venv Python and PYTHONPATH=src. No model inference."""
import json
import os
import sys
import tempfile
from pathlib import Path
from spark_launcher.adapters import sync_hermes
from spark_launcher.catalog import fetch_catalog, load_config, state_dir, write_json

config = load_config()
catalog = fetch_catalog(config)
agent = Path(os.environ.get("HERMES_AGENT_ROOT", Path.home() / ".hermes/hermes-agent"))
sys.path.insert(0, str(agent))
with tempfile.TemporaryDirectory(prefix="spark-hermes-parser-") as temp:
    current = Path(os.environ.get("HERMES_HOME", Path.home() / ".hermes")) / "config.yaml"
    target = Path(temp) / "config.yaml"
    if current.exists():
        target.write_bytes(current.read_bytes())
        target.chmod(0o600)
    os.environ["HERMES_HOME"] = temp
    sync_hermes(config, catalog, "hermes-parser")
    import yaml
    from hermes_cli.config import get_compatible_custom_providers
    from hermes_cli.runtime_provider import frozen_runtime_config, resolve_runtime_provider
    from hermes_cli.model_switch import switch_model
    from hermes_cli.timeouts import get_provider_request_timeout, get_provider_stale_timeout
    from agent.model_metadata import get_model_context_length
    data = yaml.safe_load(target.read_text())
    providers = get_compatible_custom_providers(data)
    rows = []
    with frozen_runtime_config(data):
        for model in catalog["models"]:
            runtime = resolve_runtime_provider(requested="spark-gateway", target_model=model["id"])
            assert runtime["base_url"] == config["gateway"] + "/v1"
            result = switch_model(model["id"], current_provider="custom:local-(localhost:1234)",
                                  current_model="local", current_base_url="http://localhost:1234/v1",
                                  explicit_provider="spark-gateway", user_providers=data["providers"], custom_providers=providers)
            assert result.success and result.base_url == config["gateway"] + "/v1"
            context = get_model_context_length(model["id"], base_url=runtime["base_url"],
                                               provider="spark-gateway", custom_providers=providers)
            assert context == model["context_tokens"], model["id"]
            assert get_provider_request_timeout("spark-gateway", model["id"]) == config["timeout_seconds"]
            assert get_provider_stale_timeout("spark-gateway", model["id"]) == config["timeout_seconds"]
            rows.append({"model": model["id"], "context": context, "remote_route": True})
result = {"status": "passed", "models": rows, "inference": False}
write_json(state_dir() / "verification/hermes-parser.json", result)
print(json.dumps(result, indent=2))
