"""Site configuration and live context discovery. No model context constants."""
from __future__ import annotations

import json
import difflib
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
    return Path(os.environ.get("SPARKY_CONFIG", Path.home() / ".config/sparky/client.json")).expanduser()


def state_dir():
    return Path(os.environ.get("SPARKY_STATE_DIR", Path.home() / ".local/state/sparky")).expanduser()


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
    if not isinstance(config, dict) or config.get("version") != 1:
        raise SparkError("Unsupported configuration version.")
    if not isinstance(config.get("gateway"), str):
        raise SparkError("Set gateway to an HTTPS URL in the client configuration.")
    u = urlsplit(config["gateway"])
    if (not u.hostname or u.username or u.password or u.query or u.fragment
            or u.path not in ("", "/")):
        raise SparkError("Gateway must be an origin, without credentials, /v1, or a query.")
    if u.scheme != "https" and not (u.scheme == "http" and u.hostname in ("localhost", "127.0.0.1", "::1")):
        raise SparkError("Use HTTPS for a remote gateway.")
    config["gateway"] = config["gateway"].rstrip("/")
    config.setdefault("model_prefix", "garuda/")
    config.setdefault("timeout_seconds", 7200)
    config.setdefault("compact_percent", 95)
    if not isinstance(config["model_prefix"], str) or not config["model_prefix"]:
        raise SparkError("model_prefix must be a nonempty string.")
    config.setdefault("default_model", config["model_prefix"] + "spark-auto")
    if not isinstance(config["default_model"], str):
        raise SparkError("default_model must be a model name or alias.")
    config.setdefault("models", {})
    config.setdefault("binaries", {})
    if type(config["compact_percent"]) is not int or not 50 <= config["compact_percent"] <= 95:
        raise SparkError("compact_percent must be 50..95.")
    if type(config["timeout_seconds"]) is not int or not 1 <= config["timeout_seconds"] <= 86400:
        raise SparkError("timeout_seconds must be 1..86400.")
    if not isinstance(config["models"], dict) or not isinstance(config["binaries"], dict):
        raise SparkError("Client models and binaries must be mappings. Check " + str(config_path()) + ".")
    if any(not isinstance(value, str) for value in config["binaries"].values()):
        raise SparkError("Each binaries entry must be an executable path.")
    for options in config["models"].values():
        if not isinstance(options, dict):
            raise SparkError("Client model entries must be mappings.")
        if any(key in options for key in ("context", "context_tokens", "context_length")):
            raise SparkError("Context limits come from the gateway, not the site file.")
    return config


def load_config():
    try:
        return validate_config(json.loads(config_path().read_text()))
    except json.JSONDecodeError as error:
        raise SparkError(f"Invalid JSON in {config_path()} at line {error.lineno}, column {error.colno}. Fix the client file.") from None
    except FileNotFoundError:
        raise SparkError("Client configuration is missing. Run sparky init --gateway https://your-spark:8443 first.") from None


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        # Never forward node credentials or private inference bodies to a redirect.
        return None


def request_json(url, *, payload=None, method=None, headers=None, timeout=20, expect_json=True, safe_error_body=False):
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
        # Only the local controller emits errors safe for the CLI. Never copy
        # response bodies from gateway admin endpoints or model backends.
        if safe_error_body and urlsplit(url).hostname in ('127.0.0.1', 'localhost', '::1'):
            try:
                message = json.loads(error.read(4096)).get('error')
            except (ValueError, AttributeError):
                message = None
            if isinstance(message, str) and len(message) <= 1024:
                raise SparkError(message) from None
        raise SparkError(f"Server returned HTTP {error.code}. Run sparky doctor to check gateway access.") from None
    except json.JSONDecodeError:
        raise SparkError("The server returned invalid JSON. Check the gateway URL with sparky doctor.") from None
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
        model = {k: row[k] for k in ("name", "aliases", "output_tokens", "inputs", "reasoning", "nodes") if k in row}
        model.update(id=model_id, context_tokens=context)
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
        models.append({**{k:auto[k] for k in ("name","aliases","output_tokens","inputs") if k in auto}, "id": model_id,
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
        try:
            catalog = json.loads(path.read_text())
        except (FileNotFoundError, json.JSONDecodeError):
            raise SparkError('No usable cached model list. Connect to the gateway and run sparky models first.') from None
        if catalog["gateway"] != config["gateway"]:
            raise SparkError("Cached catalog belongs to another gateway.")
        return catalog
    catalog = make_catalog(config, request_json(config["gateway"] + "/sparky/catalog"))
    write_json(path, catalog)
    return catalog


def resolve(catalog, name):
    requested = name.strip().casefold()
    aliases = {alias.casefold(): [] for m in catalog['models']
               for alias in (m['id'], m['id'].split('/', 1)[-1], *m.get('aliases', []))}
    for model in catalog['models']:
        for alias in set((model['id'], model['id'].split('/', 1)[-1], *model.get('aliases', []))):
            if model not in aliases[alias.casefold()]:
                aliases[alias.casefold()].append(model)
    matches = aliases.get(requested, [])
    if len(matches) == 1:
        return matches[0]
    if matches:
        raise SparkError('Ambiguous model alias: ' + name + '. Use one of: ' + ', '.join(m['id'] for m in matches) + '.')
    closest = difflib.get_close_matches(requested, aliases, n=1, cutoff=.6)
    hint = f' Did you mean {closest[0]}?' if closest else ''
    raise SparkError('Unknown model: ' + name + '.' + hint + ' Run sparky models to see available names.')

