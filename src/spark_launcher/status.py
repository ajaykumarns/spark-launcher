"""Readable context and memory accounting for each Spark."""
from .presentation import table, memory, heading, note, number


def allocation(value):
    if value is None:
        return 'unknown'
    if value < 1024**3:
        return f'{value / 1024**2:.0f} MiB'
    return memory(value)


def fallback_workloads(data, node):
    """Keep running models visible when a node's measurements are missing."""
    rows = []
    for name, model in data.get('models', {}).items():
        if model.get('kind') not in ('llm', 'audio') or node not in model.get('nodes', []):
            continue
        rows.append({'id': name, 'name': model.get('display_name', name), 'kind': model['kind'],
                     'state': model.get('state', 'unknown'), 'context_tokens': model.get('context_tokens'),
                     'context_source': model.get('context_source'),
                     'gpu_bytes': None, 'host_pss_bytes': None})
    comfy = data.get('comfyui', {}).get(node, {})
    if comfy.get('reachable'):
        rows.append({'id': 'comfyui', 'name': 'ComfyUI', 'kind': 'comfyui',
                     'state': f"{comfy.get('phase', 'running')} ({comfy.get('pending', 0)} queued)",
                     'gpu_bytes': None, 'host_pss_bytes': None})
    return rows


def format_status(data):
    devices = []
    for node, state in data.get('nodes', {}).items():
        mem = state.get('memory_bytes', {})
        total, available = mem.get('MemTotal'), mem.get('MemAvailable')
        used = total - available if total is not None and available is not None else None
        devices.append((node, memory(used), memory(available), memory(total)))
    lines = [heading('Spark cluster status'), '',
             table(('DEVICE', 'IN USE', 'AVAILABLE RAM', 'TOTAL RAM'), devices, title='Device memory'), '']
    configured_context = False
    for node in data.get('nodes', {}):
        sample = data.get('memory_usage', {}).get(node, {})
        workloads = list(sample.get('workloads', []))
        known = {row['id'] for row in workloads}
        workloads += [row for row in fallback_workloads(data, node) if row['id'] not in known]
        order = {'llm': 0, 'audio': 1, 'comfyui': 2}
        workloads.sort(key=lambda row: (4 if row['id'] == 'system' else order.get(row.get('kind'), 3), row['name']))
        rows = []
        for row in workloads:
            limit = row.get('context_tokens')
            context = number(limit) if limit else ('unknown' if row.get('kind') == 'llm' else '-')
            if limit and row.get('context_source') == 'configured':
                context += '*'
                configured_context = True
            rows.append((row['name'], row.get('state', 'running'), context,
                         allocation(row.get('gpu_bytes')), allocation(row.get('host_pss_bytes'))))
        if rows:
            lines += [table(('WORKLOAD', 'STATE', 'CONTEXT', 'GPU RAM', 'HOST RAM'), rows,
                            title='Running workloads / ' + node), '']
        else:
            lines.append(note(node + ': no running workloads reported.'))
        lines += [note(warning, 'yellow') for warning in sample.get('warnings', [])]
    stopped = [name for name, state in data.get('models', {}).items()
               if state.get('kind') in ('llm', 'audio') and state.get('state') == 'stopped']
    if stopped:
        lines.append(note('Stopped models: ' + ', '.join(stopped) + '.', 'dim'))
    admission = data.get('admission', {})
    waiting = admission.get('waiting', [])
    if waiting:
        lines.append(note('Waiting: ' + ', '.join(item['workload'] for item in waiting), 'yellow'))
    else:
        lines.append('Device requests waiting: 0.')
    if admission.get('config_transition'):
        lines.append(note('New work is paused for a configuration change.', 'yellow'))
    protected = [name for name, state in data.get('models', {}).items() if state.get('keep_loaded')]
    if protected:
        lines.append(note('Kept loaded: ' + ', '.join(protected) + '.', 'green'))
    if configured_context:
        lines.append(note('* Context uses configuration; the running backend did not report its limit.', 'yellow'))
    lines += [note('GPU RAM includes allocated model weights and cache. Host RAM counts shared pages proportionally.'),
              note('GPU and host figures can overlap in shared physical RAM. Do not add them.'),
              note('Use sparky status --json for full details.')]
    return '\n'.join(lines)
