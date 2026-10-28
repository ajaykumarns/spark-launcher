"""Explicit image memory profiles. ComfyUI still loads models per request."""
from __future__ import annotations
import copy
import shlex
import time
from pathlib import Path
from .catalog import SparkError, request_json
from .control import safe_host
from .resource_runtime import Runtime, command
from .resources import read_cluster, write_cluster

PROFILES = {
    'shared': {'high_gib': 14, 'max_gib': 16, 'minimum_available_gib': 21.5},
    'dedicated': {'high_gib': 40, 'max_gib': 48, 'minimum_available_gib': 96},
}


def evaluate(config, snapshot, mode):
    if mode not in PROFILES:
        raise SparkError('Select shared or dedicated.')
    if mode == 'dedicated':
        row = snapshot['models'].get('qwen3.8-flash-next', {})
        if row.get('state') != 'ready' or row.get('nodes') != ['spark01']:
            raise SparkError('First select and load Flash Next on Spark01 with Sparky.')
        if any(r.get('loaded') and r.get('kind') == 'llm' and 'spark02' in r.get('nodes', [])
               for r in snapshot['models'].values()):
            raise SparkError('A language model still uses Spark02. Keep the current image profile.')
    node = snapshot['nodes']['spark02']
    if node['comfyui'].get('running') or node['comfyui'].get('pending'):
        raise SparkError('Image work is active. Wait for it to finish before changing the profile.')
    return {**PROFILES[mode], 'profile': mode, 'node': 'spark02', 'llm_restarted': False,
            'image_service_restarted': False}


def activate(root, mode, dry_run=False):
    path = Path(root)/'config/models.json'; config = read_cluster(path); rt = Runtime(config, path)
    def ctl(route, payload=None):
        return request_json(f"http://127.0.0.1:{config['controller_port']}/control/"+route, payload=payload, timeout=30)
    if mode == 'show':
        state = request_json(config['nodes']['spark02']['comfy_url']+'/system_stats')
        return {'status': 'current', 'service_memory_limit_bytes': state['system']['ram_total'],
                'minimum_available_gib': config['policy']['comfyui_min_available_gib']}
    planned = evaluate(config, rt.snapshot(True), mode)
    if dry_run:
        return {'status': 'planned', **planned}
    if ctl('leases')['config_transition']:
        raise SparkError('A resource change is active. Retry after it finishes.')
    ctl('fence', {'enabled': True})
    worker = safe_host(config['nodes']['spark02']['ssh']); unit = config['nodes']['spark02']['comfy_unit']
    def worker_command(words):
        return command(['ssh', '-o', 'BatchMode=yes', worker, shlex.join(words)], timeout=60)
    def properties():
        raw = worker_command(['systemctl', 'show', unit, '-p', 'MemoryHigh', '-p', 'MemoryMax', '-p', 'MemorySwapMax']).stdout
        values = dict(line.split('=', 1) for line in raw.splitlines() if '=' in line)
        if set(values) != {'MemoryHigh', 'MemoryMax', 'MemorySwapMax'} or not all(v.isdigit() for v in values.values()):
            raise SparkError('Cannot confirm the current image memory limits.')
        return values
    def set_properties(values):
        worker_command(['sudo', '-n', 'systemctl', 'set-property', unit,
                        *[k+'='+str(v) for k, v in values.items()]])
    original = path.read_bytes(); old_limits = None; changed = False; written = None
    try:
        for _ in range(600):
            if not ctl('leases')['active'] and not rt.native_activity():
                break
            time.sleep(1)
        else:
            raise SparkError('Requests did not drain. The profile was kept.')
        evaluate(config, rt.snapshot(True), mode)
        rt.clear_comfy('spark02')
        available = rt.inspect_node('spark02')['memory_bytes']['MemAvailable']
        old_limits = properties()
        wanted = {'MemoryHigh': planned['high_gib']*2**30, 'MemoryMax': planned['max_gib']*2**30, 'MemorySwapMax': 0}
        restart_image = int(old_limits['MemoryMax']) > wanted['MemoryMax'] or available < (planned['minimum_available_gib']+.5)*2**30
        changed = True; set_properties(wanted)
        if restart_image:
            state = rt.inspect_node('spark02')['comfyui']
            if state.get('running') or state.get('pending'):
                raise SparkError('Image work arrived during the change. Keep the existing profile.')
            rt.unit('spark02', 'restart', unit)
            planned['image_service_restarted'] = True
            for _ in range(90):
                try:
                    request_json(config['nodes']['spark02']['comfy_url']+'/sparky/state', timeout=2)
                    break
                except SparkError:
                    time.sleep(1)
            else:
                raise SparkError('The idle image service did not become ready.')
            available = rt.inspect_node('spark02')['memory_bytes']['MemAvailable']
        if available < planned['minimum_available_gib']*2**30:
            raise SparkError('Available RAM is below the tested admission floor. The profile was kept.')
        if properties() != {k: str(v) for k, v in wanted.items()}:
            raise SparkError('The image service did not apply the requested memory limits.')
        if path.read_bytes() != original:
            raise SparkError('The cluster settings changed during the operation.')
        updated = copy.deepcopy(config); updated['policy']['comfyui_min_available_gib'] = planned['minimum_available_gib']
        write_cluster(path, updated); written = path.read_bytes(); ctl('config')
        # ComfyUI reads the current cgroup limit on each system_stats request.
        state = request_json(config['nodes']['spark02']['comfy_url']+'/system_stats')
        if state['system']['ram_total'] != wanted['MemoryMax']:
            raise SparkError('The image API did not confirm the new service limit.')
        return {'status': 'applied', **planned, 'available_bytes': available}
    except BaseException:
        if changed and old_limits is not None:
            set_properties(old_limits)
            if written is not None and path.read_bytes() == written:
                path.write_bytes(original); ctl('config')
        raise
    finally:
        ctl('fence', {'enabled': False})
