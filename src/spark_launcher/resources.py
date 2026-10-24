"""Authoritative workload configuration and pure resource planning."""
from __future__ import annotations
import difflib
import itertools
import json
import math
import re
from pathlib import Path
from urllib.parse import urlsplit
from .catalog import SparkError, atomic_write

PLACEMENTS = {"spark01": ("spark01",), "spark02": ("spark02",), "both": ("spark01", "spark02")}
KINDS = {"llm", "audio", "comfyui", "comfy_model"}


def service_name(value):
    return isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.@:-]*\.service', value) is not None


def private_url(value):
    if not isinstance(value, str):return False
    try:
        url=urlsplit(value)
        return bool(url.scheme=='http' and url.hostname and not url.username and not url.password
                    and not url.query and not url.fragment and url.path in ('','/') and url.port != 0)
    except ValueError:return False


def enabled_llms(config):
    return [name for name,model in config['models'].items()
            if model['kind']=='llm' and model['settings'][model['run_on']].get('enabled',True)]


def model_units(model,node):
    return sorted({setting['units'][node] for setting in model['settings'].values()
                   if node in setting.get('units',{})})


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
    if not isinstance(config.get('root'),str) or not Path(config['root']).is_absolute():
        raise SparkError('The cluster root must be an absolute path.')
    port=config.get('controller_port',8020)
    if type(port) is not int or not 1<=port<=65535:raise SparkError('Invalid controller port.')
    if 'manager_url' in config and not private_url(config['manager_url']):raise SparkError('Invalid private manager URL.')
    policy=config.get('policy',{})
    if type(policy.get('idle_ttl_seconds',10800)) is not int or policy.get('idle_ttl_seconds',10800)<0:
        raise SparkError('Idle timeout must be a nonnegative whole number of seconds.')
    if policy.get('comfyui_exclusive_with_llms',True) is not True:
        raise SparkError('ComfyUI GPU jobs must remain exclusive with language models on the same device.')
    if not isinstance(policy.get('protected_models',[]),list):raise SparkError('Protected models must be a list.')
    for node,meta in config['nodes'].items():
        if not isinstance(meta,dict):raise SparkError(node+' settings must be a mapping.')
        if 'ssh' in meta:
            from .control import safe_host
            safe_host(meta['ssh'])
        if 'comfy_unit' in meta and not service_name(meta['comfy_unit']):raise SparkError('Invalid ComfyUI service name.')
    if config.get("policy", {}).get("interrupt_active_jobs", False):
        raise SparkError("Interrupting active jobs is not supported.")
    seen = set()
    for name, model in config.get("models", {}).items():
        if not isinstance(name,str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]*',name) or name in ('.','..','spark-auto'):
            raise SparkError('Workload names must be simple names without paths or router syntax.')
        if not isinstance(model, dict) or not isinstance(model.get("settings"), dict):
            raise SparkError(f"{name} and its settings must be mappings.")
        if not isinstance(model.get("aliases", []), list) or any(not isinstance(alias, str) for alias in model.get("aliases", [])):
            raise SparkError(f"{name} aliases must be a list of names.")
        if type(model.get("keep_loaded", False)) is not bool:
            raise SparkError(f"{name} keep_loaded must be true or false.")
        if model.get("kind") not in KINDS:
            raise SparkError(f"Unknown workload kind: {name}.")
        if 'name' in model and not isinstance(model['name'],str):raise SparkError('Model display names must be strings.')
        if type(model.get('reasoning',False)) is not bool:raise SparkError('Model reasoning support must be true or false.')
        monitor = model.get("monitoring", {})
        if not isinstance(monitor, dict) or not isinstance(monitor.get("containers", []), list) or any(not isinstance(value, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]*',value) for value in monitor.get("containers", [])):
            raise SparkError(f"{name} monitoring containers must be a list of exact container names.")
        placement = model.get("run_on")
        if not isinstance(placement,str) or placement not in PLACEMENTS or placement not in model.get("settings", {}):
            raise SparkError(f"No settings for {name} on {placement}.")
        for on, settings in model["settings"].items():
            if not isinstance(settings, dict) or not isinstance(settings.get("parameters", {}), dict):
                raise SparkError(f"{name}/{on} settings and parameters must be mappings.")
            if on not in PLACEMENTS:
                raise SparkError(f"Unknown placement for {name}: {on}.")
            if type(settings.get('enabled',True)) is not bool:raise SparkError(f'{name}/{on} enabled must be true or false.')
            minimums=settings.get('minimum_available_gib',{})
            if not isinstance(minimums,dict) or any(n not in PLACEMENTS[on] or isinstance(v,bool) or not isinstance(v,(int,float)) or not math.isfinite(v) or v<0 for n,v in minimums.items()):
                raise SparkError(f'Invalid startup memory check for {name}/{on}.')
            if model["kind"] == "llm":
                if not isinstance(settings.get('units'),dict) or set(settings['units']) != set(PLACEMENTS[on]) or any(not service_name(unit) for unit in settings['units'].values()):
                    raise SparkError(f"Unit placement mismatch: {name}/{on}.")
                if not private_url(settings.get('backend_url')):
                    raise SparkError(f"Missing private backend URL: {name}/{on}.")
                if type(settings.get("context_tokens")) is not int or settings["context_tokens"] <= 0:
                    raise SparkError(f"Missing context: {name}/{on}.")
                output=model.get('output_tokens',min(16384,settings['context_tokens']-1))
                if type(output) is not int or not 0<output<settings['context_tokens']:
                    raise SparkError(f'Invalid output token budget for {name}/{on}.')
                order=settings.get('startup_order',list(PLACEMENTS[on]))
                if not isinstance(order,list) or len(order)!=len(PLACEMENTS[on]) or set(order)!=set(PLACEMENTS[on]):
                    raise SparkError(f'Invalid startup order for {name}/{on}.')
                if 'context_tokens' in settings.get('parameters',{}):
                    raise SparkError(f'{name}/{on}: context_tokens must be set once, outside parameters.')
                rendezvous=settings.get('rendezvous',{})
                if not isinstance(rendezvous,dict) or (rendezvous and (rendezvous.get('node') not in PLACEMENTS[on] or type(rendezvous.get('port')) is not int or not 1<=rendezvous['port']<=65535)):
                    raise SparkError(f'Invalid rendezvous for {name}/{on}.')
                bindings=settings.get('parameter_bindings',{})
                if not isinstance(bindings,dict) or any(node not in PLACEMENTS[on] for node in bindings):
                    raise SparkError(f'Invalid recipe binding devices for {name}/{on}.')
                fields={'context_tokens',*settings.get('parameters',{})}
                for node,items in bindings.items():
                    if not isinstance(items,list):raise SparkError('Recipe bindings must be a list.')
                    for item in items:
                        if not isinstance(item,dict) or not isinstance(item.get('path'),str) or not Path(item['path']).is_absolute() or item.get('format') not in ('nvidia_json','env','docker_env','python','command') or not isinstance(item.get('fields'),dict) or not item['fields'] or any(field not in fields or not isinstance(flag,str) or not flag for field,flag in item['fields'].items()):
                            raise SparkError(f'Invalid recipe binding for {name}/{on}/{node}.')
            elif model['kind']=='audio':
                if on=='both' or not isinstance(settings.get('containers'),list) or not settings['containers'] or any(not isinstance(c,str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]*',c) for c in settings['containers']):
                    raise SparkError(f'Invalid audio containers for {name}/{on}.')
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
            if not alias.strip() or alias!=alias.strip() or alias.casefold() in seen or alias.casefold() in ('auto','spark-auto'):
                raise SparkError(f"Duplicate workload alias: {alias}.")
            seen.add(alias.casefold())
    for name in config.get('policy', {}).get('protected_models', []):
        if not isinstance(name,str) or name not in config['models'] or not config['models'][name].get('keep_loaded'):
            raise SparkError(f'{name} is required to stay loaded by the cluster policy.')
    auto=config.get('auto')
    if auto is not None:
        valid=enabled_llms(config)
        if not isinstance(auto,dict) or not isinstance(auto.get('targets'),list) or not auto['targets'] or any(not isinstance(n,str) or n not in valid for n in auto['targets']) or auto.get('default') not in auto['targets']:
            raise SparkError('Auto targets and default must name enabled language models.')
    return config


def resolve_workload(config, requested):
    if not isinstance(requested,str):raise SparkError('Select a model or service by name.')
    requested = requested.strip().casefold().removeprefix('garuda/').removeprefix('parakeet/')
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
    placement = model['run_on'] if on is None else on
    if not isinstance(placement,str) or placement not in model["settings"]:
        raise SparkError(f"{name} has no recipe for {placement}. Available placements: " + ", ".join(model["settings"]) + ".")
    settings = model["settings"][placement]
    if settings.get("enabled", True) is False:
        raise SparkError(settings.get("reason", f"{name}/{placement} is not installed."))
    return model, placement, settings, PLACEMENTS[placement]


def matrix_sets(config):
    """Generate llama-swap compatibility from selected placements, not names."""
    names = enabled_llms(config)
    # Each LLM needs at least one of two exclusive devices. Therefore no
    # compatible group can contain three models. Avoid a 2**N subset search
    # on every inference request as the installed catalog grows.
    nodes={name:set(PLACEMENTS[config['models'][name]['run_on']]) for name in names}
    pairs=[{a,b} for a,b in itertools.combinations(names,2) if not nodes[a]&nodes[b]]
    paired=set().union(*pairs) if pairs else set()
    maximal=[{name} for name in names if name not in paired]+pairs
    return {f"placement_{i}": " & ".join(sorted(group)) for i, group in enumerate(maximal, 1)}


def make_plan(config, name, snapshot, action="load", on=None):
    if action not in ('load','unload'):raise SparkError('Use load or unload for a resource plan.')
    model, placement, settings, nodes = selected(config, name, on)
    steps, waiting = [], []
    if action == "unload" and model.get("keep_loaded"):
        raise SparkError(f"{name} is configured to stay loaded.")
    if action=='unload' and model['kind']=='audio':
        raise SparkError('No safe unload adapter is configured for this audio service.')
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
                if config['models'][other].get('keep_loaded'):
                    raise SparkError(other+' must stay loaded on the requested device.')
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
