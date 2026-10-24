"""Read-only per-workload memory measurements for unified-memory Sparks.

GPU allocations and host PSS are separate observations. They must not be added
as if the device had independent CPU RAM and VRAM.
"""
from __future__ import annotations

import csv
import json
import shlex
import subprocess
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from pathlib import Path

from .catalog import SparkError, request_json
from .resources import model_units

MIB = 1024**2


def run(args):
    return subprocess.run(args, capture_output=True, text=True, timeout=10)


def gpu_processes(text):
    result = {}
    for row in csv.reader(text.splitlines()):
        if len(row) != 2:
            continue
        try:
            pid = int(row[0])
        except ValueError:
            continue
        try:
            size = int(float(row[1].strip()) * MIB)
            if size < 0:
                size = None
        except (ValueError, OverflowError):
            size = None
        result[pid] = size
    return result


def cgroup(path):
    for line in path.read_text().splitlines():
        if line.startswith('0::'):
            return line[3:]
    return ''


def host_memory(path):
    """PSS shares mapped pages proportionally across the processes using them."""
    try:
        fields = dict(line.split(':', 1) for line in (path / 'status').read_text().splitlines() if ':' in line)
        if int(fields.get('VmRSS', '0 kB').split()[0]) == 0:
            return 0
        for line in (path / 'smaps_rollup').read_text().splitlines():
            if line.startswith('Pss:'):
                return int(line.split()[1]) * 1024
    except (FileNotFoundError, ProcessLookupError):
        return 0  # The process exited during the sample.
    except (OSError, ValueError):
        pass
    return None


def classify(path, sources):
    """Match the narrowest real cgroup, never a container-name substring."""
    matches = [(len(prefix), owner) for prefix, owner in sources.items()
               if prefix and prefix != '/' and (path == prefix or path.startswith(prefix + '/'))]
    return max(matches, default=(0, 'system'))[1]


def aggregate(processes, gpu, gpu_known, names):
    groups = {}

    def group(owner):
        return groups.setdefault(owner, {'id': owner, 'name': names.get(owner, owner), 'pids': [],
                                         'host_pss_bytes': 0, 'gpu_bytes': 0 if gpu_known else None})

    seen = set()
    for process in processes:
        pid, owner = process['pid'], process['owner']
        seen.add(pid)
        row = group(owner)
        row['pids'].append(pid)
        pss = process['pss']
        row['host_pss_bytes'] = None if pss is None or row['host_pss_bytes'] is None else row['host_pss_bytes'] + pss
        if pid in gpu:
            size = gpu[pid]
            row['gpu_bytes'] = None if size is None or row['gpu_bytes'] is None else row['gpu_bytes'] + size
    # A process can exit during the scan. Keep its observed GPU allocation
    # visible, but do not assign it to a model without cgroup evidence.
    for pid, size in gpu.items():
        if pid not in seen:
            row = group('system')
            row['gpu_bytes'] = None if size is None or row['gpu_bytes'] is None else row['gpu_bytes'] + size
    return [row for row in groups.values() if row['pids'] or row['gpu_bytes']]


def collect(spec):
    """Run as the existing administrator through sudo; print no private state."""
    sources, names, warnings = {}, {'system': 'Other host processes'}, []
    expected_containers, unit_owners = {}, {}
    for owner in spec['owners']:
        names[owner['id']] = owner['name']
        expected_containers.update({name: owner['id'] for name in owner.get('containers', [])})
        unit_owners.update({unit: owner['id'] for unit in owner.get('units', [])})
    ids = run(['docker', 'ps', '-q'])
    if ids.returncode == 0 and ids.stdout.split():
        template = '{"name":{{json .Name}},"pid":{{.State.Pid}}}'
        inspected = run(['docker', 'inspect', '--format', template, *ids.stdout.split()])
        if inspected.returncode == 0:
            for line in inspected.stdout.splitlines():
                container = json.loads(line)
                name = container['name'].lstrip('/')
                owner = expected_containers.get(name, 'container:' + name)
                names.setdefault(owner, name.removeprefix('garuda-'))
                try:
                    sources[cgroup(Path('/proc') / str(container['pid']) / 'cgroup')] = owner
                except OSError:
                    continue
        else:
            warnings.append('Container ownership could not be read.')
    elif ids.returncode:
        warnings.append('Container ownership could not be read.')
    if unit_owners:
        units = run(['systemctl', 'show', *unit_owners, '-p', 'Id', '-p', 'ControlGroup'])
        for block in units.stdout.strip().split('\n\n'):
            fields = dict(line.split('=', 1) for line in block.splitlines() if '=' in line)
            if fields.get('Id') in unit_owners and fields.get('ControlGroup'):
                sources[fields['ControlGroup']] = unit_owners[fields['Id']]
    measured = run(['nvidia-smi', '--query-compute-apps=pid,used_gpu_memory', '--format=csv,noheader,nounits'])
    gpu_known = measured.returncode == 0
    gpu = gpu_processes(measured.stdout) if gpu_known else {}
    if not gpu_known:
        warnings.append('GPU memory readings are unavailable.')
    processes = []
    for path in Path('/proc').iterdir():
        if not path.name.isdecimal():
            continue
        try:
            owner = classify(cgroup(path / 'cgroup'), sources)
        except OSError:
            continue
        pss = host_memory(path)
        if pss == 0 and int(path.name) not in gpu:
            continue
        processes.append({'pid': int(path.name), 'owner': owner, 'pss': pss})
    return {'sampled_at': datetime.now(timezone.utc).isoformat(),
            'workloads': aggregate(processes, gpu, gpu_known, names), 'warnings': warnings,
            'gpu_source': 'nvidia-smi per-process allocations', 'host_source': '/proc/PID/smaps_rollup Pss',
            'unified_memory': True, 'add_columns': False}


def specification(config, node):
    owners = []
    for name, model in config['models'].items():
        if model['kind'] not in ('llm', 'audio'):
            continue
        containers = model.get('monitoring', {}).get('containers', [])
        if model['kind'] == 'audio':
            containers = model['settings'].get(node, {}).get('containers', [])
        units = model_units(model,node)
        owners.append({'id': name, 'name': (model.get('aliases') or [name])[0] if model['kind'] == 'llm' else name.title(),
                       'containers': containers, 'units': units})
    owners.append({'id': 'comfyui', 'name': 'ComfyUI', 'units': [config['nodes'][node]['comfy_unit']]})
    return {'owners': owners}


def read_node(config, node):
    from .control import run_remote_json
    helper = str(Path.home() / '.local/bin/sparky')
    args = ['sudo', '-n', helper, '--json', 'node', 'memory']
    if node != 'spark01':
        args = ['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=8', config['nodes'][node]['ssh'], shlex.join(args)]
    return run_remote_json(args, payload=json.dumps(specification(config, node)), timeout=45)


def context_limit(config, name, state):
    """Prefer the running backend's published limit; label a config fallback."""
    nodes = state.get('nodes', [])
    placement = 'both' if len(nodes) == 2 else (nodes[0] if nodes else None)
    setting = config['models'][name]['settings'].get(placement, {})
    result = {'context_tokens': setting.get('context_tokens'), 'context_source': 'configured'}
    if not setting.get('backend_url'):
        return result
    from .control import read_env
    try:
        key = read_env(Path(config['root']) / 'vllm/api.env')['VLLM_API_KEY']
        data = request_json(setting['backend_url'].rstrip('/') + '/v1/models',
                            headers={'Authorization': 'Bearer ' + key}, timeout=3)
        for model in data.get('data', []):
            if model.get('id') not in (name, 'garuda/' + name):
                continue
            for field in ('max_model_len', 'context_length', 'context_window'):
                value = model.get(field)
                if type(value) is int and value > 0:
                    return {'context_tokens': value, 'context_source': 'running backend'}
    except (SparkError, OSError, ValueError, KeyError):
        pass
    return result


def enrich(root, status):
    from .resources import read_cluster
    config = read_cluster(Path(root) / 'config/models.json')
    nodes = list(config['nodes'])

    def inspect(node):
        try:
            return read_node(config, node)
        except (SparkError, OSError, ValueError, subprocess.TimeoutExpired):
            return {'workloads': [], 'warnings': ['Memory readings are unavailable on ' + node + '.']}

    with ThreadPoolExecutor(max_workers=2) as pool:
        status['memory_usage'] = dict(zip(nodes, pool.map(inspect, nodes)))
    for name, state in status['models'].items():
        if state['kind'] == 'llm' and state.get('loaded'):
            state.update(context_limit(config, name, state))
        state['memory_by_node'] = {}
        for node, sample in status['memory_usage'].items():
            for row in sample['workloads']:
                if row['id'] == name:
                    state['memory_by_node'][node] = {field: row[field] for field in ('gpu_bytes', 'host_pss_bytes')}
                    row.update(state=state.get('state', 'running') if state.get('loaded') is not False else 'running', kind=state['kind'],
                               context_tokens=state.get('context_tokens'), context_source=state.get('context_source'))
                    if name == 'comfyui':
                        comfy = status['comfyui'].get(node, {})
                        row['state'] = f"{comfy.get('phase', 'running')} ({comfy.get('pending', 0)} queued)"
    return status
