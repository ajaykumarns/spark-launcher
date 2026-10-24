"""Site configuration and live context discovery. No model context constants."""
from __future__ import annotations

import json
import os
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import Request, HTTPRedirectHandler, build_opener


class SparkError(Exception):
    """An error safe to show without remote bodies or credentials."""


def timestamp():
    return datetime.now(timezone.utc).isoformat()


def config_path():
    return Path(os.environ.get("SPARK_CONFIG", Path.home() / ".config/spark/config.json")).expanduser()


def state_dir():
    return Path(os.environ.get("SPARK_STATE_DIR", Path.home() / ".local/state/spark")).expanduser()


def atomic_write(path, data, mode=0o600):
    path = Path(path)
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, temp = tempfile.mkstemp(prefix=".spark-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as out:
            os.fchmod(out.fileno(), mode)
            out.write(data if isinstance(data, bytes) else data.encode())
            out.flush()
            os.fsync(out.fileno())
        os.replace(temp, path)
    finally:
        Path(temp).unlink(missing_ok=True)


def write_json(path, value):
    atomic_write(path, json.dumps(value, indent=2, ensure_ascii=False) + "\n")


def validate_config(config):
    if config.get("version") != 1:
        raise SparkError("Unsupported configuration version.")
    u = urlsplit(config.get("gateway", ""))
    if (not u.hostname or u.username or u.password or u.query or u.fragment
            or u.path not in ("", "/")):
        raise SparkError("Gateway must be an origin, without credentials, /v1, or a query.")
    if u.scheme != "https" and not (u.scheme == "http" and u.hostname in ("localhost", "127.0.0.1", "::1")):
        raise SparkError("Use HTTPS for a remote gateway.")
    config["gateway"] = config["gateway"].rstrip("/")
    config.setdefault("model_prefix", "garuda/")
    config.setdefault("timeout_seconds", 7200)
    config.setdefault("compact_percent", 95)
    config.setdefault("models", {})
    config.setdefault("binaries", {})
    if not 50 <= config["compact_percent"] <= 95:
        raise SparkError("compact_percent must be 50..95.")
    if not 1 <= config["timeout_seconds"] <= 86400:
        raise SparkError("timeout_seconds must be 1..86400.")
    for options in config["models"].values():
        if any(key in options for key in ("context", "context_tokens", "context_length")):
            raise SparkError("Context limits come from the gateway, not the site file.")
    return config


def load_config():
    try:
        return validate_config(json.loads(config_path().read_text()))
    except FileNotFoundError:
        raise SparkError("Run spark init --gateway https://your-spark:8443 first.") from None


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        # Never forward node credentials or private inference bodies to a redirect.
        return None


def request_json(url, *, payload=None, method=None, headers=None, timeout=20, expect_json=True):
    body = None if payload is None else json.dumps(payload).encode()
    request = Request(url, data=body, method=method, headers=headers or {})
    if body is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with build_opener(NoRedirect).open(request, timeout=timeout) as response:
            data = response.read(8 * 1024 * 1024 + 1)
            if len(data) > 8 * 1024 * 1024:
                raise SparkError("Response exceeds the metadata size limit.")
            return (json.loads(data) if data else {}) if expect_json else {}
    except HTTPError as error:
        # Never include the response body: admin endpoints can return secrets.
        raise SparkError(f"Server returned HTTP {error.code}.") from None
    except (URLError, TimeoutError):
        raise SparkError("Cannot reach the server. Check Tailscale and the gateway.") from None


def make_catalog(config, wire):
    prefix = config["model_prefix"]
    models, auto = [], None
    for row in wire.get("data", []):
        model_id = row.get("id", "")
        if not model_id.startswith(prefix):
            continue
        if model_id == prefix + "spark-auto":
            auto = row
            continue
        context = row.get("context_length", 0)
        if not isinstance(context, int) or context <= 0:
            continue
        model = {**config["models"].get(model_id, {}), "id": model_id, "context_tokens": context}
        model.setdefault("name", row.get("name") or model_id.removeprefix(prefix))
        if model.get("output_tokens", 0) >= context:
            raise SparkError(f"Output budget exceeds the live context for {model_id}.")
        models.append(model)
    if auto and models:
        # Auto may route to any language model. Its common budget is their minimum.
        minimum = min(m["context_tokens"] for m in models)
        advertised = auto.get("context_length", 0)
        if isinstance(advertised, int) and advertised > 0:
            minimum = min(minimum, advertised)
        model_id = auto["id"]
        models.append({**config["models"].get(model_id, {}), "id": model_id,
                       "context_tokens": minimum, "context_source": "minimum routed model context"})
        models[-1].setdefault("name", "Spark Auto")
        if models[-1].get("output_tokens", 0) >= minimum:
            raise SparkError("Auto output budget exceeds the shared context.")
    if not models:
        raise SparkError("No language models with usable context metadata were found.")
    return {"gateway": config["gateway"], "fetched_at": timestamp(),
            "models": sorted(models, key=lambda m: m["id"])}


def fetch_catalog(config, offline=False):
    path = state_dir() / "catalog.json"
    if offline:
        catalog = json.loads(path.read_text())
        if catalog["gateway"] != config["gateway"]:
            raise SparkError("Cached catalog belongs to another gateway.")
        # Keep aliases and labels current even with explicitly cached context metadata.
        for model in catalog["models"]:
            model.update(config["models"].get(model["id"], {}))
        return catalog
    catalog = make_catalog(config, request_json(config["gateway"] + "/v1/models"))
    write_json(path, catalog)
    return catalog


def resolve(catalog, name):
    matches = [m for m in catalog["models"] if name in
               (m["id"], m["id"].split("/", 1)[-1], *m.get("aliases", []))]
    if len(matches) != 1:
        reason = "Ambiguous model alias" if matches else "Unknown model"
        raise SparkError(f"{reason}: {name}. Run spark models.")
    return matches[0]


def import_models(path, prefix):
    """Import only model metadata. Never copy API keys or context constants."""
    source = json.loads(Path(path).expanduser().read_text())
    models = {}
    for provider in source.get("provider", {}).values():
        for key, value in provider.get("models", {}).items():
            model_id = value.get("id") or key
            if not model_id.startswith(prefix):
                model_id = prefix + model_id
            tail = model_id.removeprefix(prefix)
            opts = {"name": value.get("name", tail), "output_tokens": value.get("limit", {}).get("output", 0),
                    "inputs": value.get("modalities", {}).get("input", ["text"]),
                    "reasoning": value.get("reasoning", False)}
            aliases = [family for family in ("nemotron", "glm", "deepseek", "mimo") if tail.startswith(family)]
            if "qwen" in tail:
                aliases = ["qwen27b" if "27b" in tail else "qwen"]
            if tail == "spark-auto":
                aliases = ["auto"]
            opts["aliases"] = aliases
            label = opts["name"].lower()
            if "2 sparks" in label or "two sparks" in label:
                opts["nodes"] = ["spark01", "spark02"]
            elif "spark02" in label:
                opts["nodes"] = ["spark02"]
            elif "spark01" in label:
                opts["nodes"] = ["spark01"]
            models[model_id] = opts
    return models
