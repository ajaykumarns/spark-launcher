"""Authoritative workload configuration and pure resource planning."""
from __future__ import annotations
import copy
import difflib
import itertools
import json
from pathlib import Path
from .catalog import SparkError, atomic_write

PLACEMENTS = {"spark01": ("spark01",), "spark02": ("spark02",), "both": ("spark01", "spark02")}
KINDS = {"llm", "audio", "comfyui", "comfy_model"}


def read_cluster(path):
    path = Path(path)
    try:
        text = path.read_text()
    except FileNotFoundError:
        raise SparkError(f"Central configuration file not found: {path}. Check remote_root in the client file.") from None
    if path.suffix == ".json":
        try:
            config = json.loads(text)
        except json.JSONDecodeError as error:
            raise SparkError(f"Invalid central JSON in {path} at line {error.lineno}, column {error.colno}. Fix the file and run sparky config validate.") from None
    else:
        try:
            import yaml
            config = yaml.safe_load(text)
        except ImportError:
            from ruamel.yaml import YAML
            config = YAML(typ="safe").load(text)
    validate_cluster(config)
    return config


def write_cluster(path, config):
    validate_cluster(config)
    if Path(path).suffix == ".json":
        data = json.dumps(config, indent=2) + "\n"
    else:
        import yaml
        data = yaml.safe_dump(config, sort_keys=False, allow_unicode=True)
    atomic_write(path, data)


def validate_cluster(config):
    if not isinstance(config, dict) or config.get("version") != 1:
        raise SparkError("Unsupported cluster config version.")
    if not isinstance(config.get("nodes"), dict) or set(config["nodes"]) != {"spark01", "spark02"}:
        raise SparkError("Define spark01 and spark02.")
    if not isinstance(config.get("models"), dict) or not isinstance(config.get("policy", {}), dict):
        raise SparkError("Central models and policy must be mappings. Run sparky config validate after fixing the file.")
    if config.get("policy", {}).get("interrupt_active_jobs", False):
        raise SparkError("Interrupting active jobs is not supported.")
    seen = set()
    for name, model in config.get("models", {}).items():
        if not isinstance(model, dict) or not isinstance(model.get("settings"), dict):
            raise SparkError(f"{name} and its settings must be mappings.")
        if not isinstance(model.get("aliases", []), list) or any(not isinstance(alias, str) for alias in model.get("aliases", [])):
            raise SparkError(f"{name} aliases must be a list of names.")
        if type(model.get("keep_loaded", False)) is not bool:
            raise SparkError(f"{name} keep_loaded must be true or false.")
        if model.get("kind") not in KINDS:
            raise SparkError(f"Unknown workload kind: {name}.")
        monitor = model.get("monitoring", {})
        if not isinstance(monitor, dict) or not isinstance(monitor.get("containers", []), list) or any(not isinstance(value, str) for value in monitor.get("containers", [])):
            raise SparkError(f"{name} monitoring containers must be a list of exact container names.")
        placement = model.get("run_on")
        if placement not in PLACEMENTS or placement not in model.get("settings", {}):
            raise SparkError(f"No settings for {name} on {placement}.")
        for on, settings in model["settings"].items():
            if not isinstance(settings, dict) or not isinstance(settings.get("parameters", {}), dict):
                raise SparkError(f"{name}/{on} settings and parameters must be mappings.")
            if on not in PLACEMENTS:
                raise SparkError(f"Unknown placement for {name}: {on}.")
            if model["kind"] == "llm":
                if set(settings.get("units", {})) != set(PLACEMENTS[on]):
                    raise SparkError(f"Unit placement mismatch: {name}/{on}.")
                if not settings.get("backend_url", "").startswith("http://"):
                    raise SparkError(f"Missing private backend URL: {name}/{on}.")
                if type(settings.get("context_tokens")) is not int or settings["context_tokens"] <= 0:
                    raise SparkError(f"Missing context: {name}/{on}.")
        for setting in model['settings'].values():
            if model['kind']=='llm':
                if type(setting.get('concurrency')) is not int or setting['concurrency']<1:
                    raise SparkError(f'Invalid request concurrency for {name}.')
                for key,value in setting.get('parameters',{}).items():
                    if key=='gpu_memory_utilization':
                        if isinstance(value,bool) or not isinstance(value,(int,float)) or not 0<value<=1:
                            raise SparkError(f'Invalid GPU memory fraction for {name}.')
                    elif type(value) is not int or value<0:
                        raise SparkError(f'Invalid numeric recipe parameter for {name}: {key}.')
        for alias in (name, *model.get("aliases", [])):
            if alias in seen:
                raise SparkError(f"Duplicate workload alias: {alias}.")
            seen.add(alias)
    for name in config.get('policy', {}).get('protected_models', []):
        if name not in config['models'] or not config['models'][name].get('keep_loaded'):
            raise SparkError(f'{name} is required to stay loaded by the cluster policy.')
    return config


def resolve_workload(config, requested):
    requested = requested.strip().removeprefix('garuda/').removeprefix('parakeet/').casefold()
    aliases = {}
    for name, model in config['models'].items():
        for alias in (name, *model.get('aliases', [])):
            aliases.setdefault(alias.casefold(), set()).add(name)
    matches = aliases.get(requested, set())
    if len(matches) == 1:
        name = next(iter(matches))
        return name, config['models'][name]
    if matches:
        raise SparkError('Ambiguous model alias: ' + requested + '. Use one of: ' + ', '.join(sorted(matches)) + '.')
    closest = difflib.get_close_matches(requested, aliases, n=1, cutoff=.6)
    hint = f' Did you mean {closest[0]}?' if closest else ''
    raise SparkError('Unknown model or service: ' + requested + '.' + hint + ' Run sparky models --installed for names.')


def selected(config, name, on=None):
    model = config["models"][name]
    placement = on or model["run_on"]
    if placement not in model["settings"]:
        raise SparkError(f"{name} has no recipe for {placement}. Available placements: " + ", ".join(model["settings"]) + ".")
    settings = model["settings"][placement]
    if settings.get("enabled", True) is False:
        raise SparkError(settings.get("reason", f"{name}/{placement} is not installed."))
    return model, placement, settings, PLACEMENTS[placement]


def compatible(config, left, right):
    if left == right:
        return True
    def describe(name):
        if name.startswith("comfyui@"):
            return "comfyui", {name.split("@", 1)[1]}
        m, _, _, nodes = selected(config, name)
        return m["kind"], set(nodes)
    lk, ln = describe(left)
    rk, rn = describe(right)
    # Background audio has an explicit keep-loaded policy and is included in
    # measured free memory. It is never treated as an exclusive GPU owner.
    if lk == "audio" or rk == "audio":
        return True
    return not ln.intersection(rn)


def matrix_sets(config):
    """Generate llama-swap compatibility from selected placements, not names."""
    names = [n for n, m in config["models"].items() if m["kind"] == "llm"]
    legal = []
    for size in range(1, len(names) + 1):
        for group in itertools.combinations(names, size):
            if all(compatible(config, a, b) for a, b in itertools.combinations(group, 2)):
                legal.append(set(group))
    maximal = [g for g in legal if not any(g < h for h in legal)]
    return {f"placement_{i}": " & ".join(sorted(group)) for i, group in enumerate(maximal, 1)}


def make_plan(config, name, snapshot, action="load", on=None):
    model, placement, settings, nodes = selected(config, name, on)
    steps, waiting, conflicts = [], [], []
    if action == "unload" and model.get("keep_loaded"):
        raise SparkError(f"{name} is configured to stay loaded.")
    if action == "unload":
        running = snapshot.get("models", {}).get(name, {})
        if running.get("active_requests", 0):
            waiting.append(name)
        steps.append({"action": "unload", "workload": name, "after_active_jobs": True})
    elif model["kind"] == "audio":
        steps.append({"action": "ensure_running", "workload": name, "preserve_existing": True})
    elif model["kind"] in ("comfyui", "comfy_model"):
        steps.append({"action": "ensure_comfyui", "nodes": list(nodes),
                      "note": "Weights load when a workflow executes; this does not run a workflow."})
    else:
        for other, state in snapshot.get("models", {}).items():
            if other == name or state.get("kind") != "llm" or not state.get("loaded"):
                continue
            # Use actual runtime placement even when desired config changed.
            if set(nodes).intersection(state.get("nodes", [])):
                conflicts.append(other)
                if state.get("active_requests", 0):
                    waiting.append(other)
                steps.append({"action": "unload", "workload": other, "after_active_jobs": True,
                              "reason": "The requested recipe needs its device."})
        for node in nodes:
            comfy = snapshot.get("comfyui", {}).get(node, {})
            if comfy.get("executing") or (comfy.get("running", 0) and comfy.get("phase") != "waiting"):
                waiting.append("comfyui@" + node)
            steps.append({"action": "release_idle_comfy_cache", "node": node,
                          "preserve_process": True, "after_active_jobs": True})
        running = snapshot.get("models", {}).get(name, {})
        same = running.get("loaded") and set(running.get("nodes", [])) == set(nodes)
        if same:
            steps = [{"action": "keep_running", "workload": name}]
            waiting = []
        else:
            if running.get("loaded"):
                if running.get("active_requests", 0):waiting.append(name)
                steps.append({"action": "restart_for_placement", "workload": name,
                              "from": running.get("nodes"), "to": list(nodes)})
            steps.append({"action": "load", "workload": name, "nodes": list(nodes),
                          "minimum_available_gib": settings.get("minimum_available_gib", {})})
    return {"workload": name, "kind": model["kind"], "action": action, "placement": placement,
            "nodes": list(nodes), "wait_for": sorted(set(waiting)), "steps": steps,
            "protected": [n for n, m in config["models"].items() if m.get("keep_loaded")],
            "interrupt_active_jobs": False}
