"""Human-readable command results. JSON serialization stays in the CLI."""
from __future__ import annotations

import shutil
import textwrap
from datetime import datetime


KINDS = {'llm': 'LLM', 'audio': 'audio', 'comfyui': 'ComfyUI', 'comfy_model': 'image'}


def clean(value):
    """Do not let remote names or paths insert terminal control sequences."""
    return ''.join(c if c.isprintable() else ' ' for c in str(value))


def table(headers, rows, width=None):
    rows = [tuple(clean(value) for value in row) for row in rows]
    headers = tuple(clean(value) for value in headers)
    widths = [max(len(row[i]) for row in [headers, *rows]) for i in range(len(headers))]
    available = width or shutil.get_terminal_size((100, 24)).columns
    minimums = [min(size, 8) for size in widths]
    if sum(minimums) + 2 * (len(widths) - 1) > available:
        return '\n\n'.join('\n'.join(textwrap.fill(f'{header}: {value}', max(available, 10))
                                      for header, value in zip(headers, row)) for row in rows)
    while sum(widths) + 2 * (len(widths) - 1) > available:
        choices = [i for i, size in enumerate(widths) if size > minimums[i]]
        if not choices:
            break
        largest = max(choices, key=lambda i: widths[i])
        widths[largest] -= 1
    lines = []
    for row in [headers, *rows]:
        cells = [textwrap.wrap(value, size, break_on_hyphens=False) or ['']
                 for value, size in zip(row, widths)]
        for index in range(max(map(len, cells))):
            lines.append('  '.join((cell[index] if index < len(cell) else '').ljust(size)
                                   for cell, size in zip(cells, widths)).rstrip())
    return '\n'.join(lines)


def memory(value):
    return 'unknown' if value is None else f'{value / 1024**3:.1f} GiB'


def number(value):
    return f'{value:,}' if isinstance(value, int) else 'unknown'


def display_model(model):
    return (model.get('aliases') or [model['id'].split('/', 1)[-1]])[0]


def format_models(catalog, model=None):
    if model is not None:
        return '\n'.join([
            clean(model.get('name', model['id'])),
            f"Model ID: {clean(model['id'])}",
            f"Aliases: {', '.join(map(clean, model.get('aliases', []))) or 'none'}",
            f"Context: {number(model['context_tokens'])} tokens",
            f"Output budget: {number(model.get('output_tokens'))} tokens",
            f"Configured devices: {', '.join(model.get('nodes', [])) or 'automatic routing'}",
            f"Input: {', '.join(model.get('inputs', ['text']))}",
            'This shows configuration. It does not load a model.',
        ])
    rows = [(display_model(m), number(m['context_tokens']), number(m.get('output_tokens')),
             ', '.join(m.get('nodes', [])) or 'automatic', ', '.join(m.get('inputs', ['text'])))
            for m in catalog['models']]
    lines = [table(('MODEL', 'CONTEXT', 'OUTPUT', 'RUNS ON', 'INPUT'), rows),
             '', 'Context and output are token counts. These are configured routes, not running models.',
             'Use sparky models NAME for details. Use sparky status for running models.']
    if catalog.get('offline'):
        lines.append(f"Cached data from {clean(catalog.get('fetched_at', 'unknown time'))}. Current server state was not checked.")
    return '\n'.join(lines)


def format_inventory(data):
    rows = []
    for name, state in data.get('models', {}).items():
        status = 'on demand' if state['kind'] == 'comfy_model' else state.get('state', 'unknown')
        rows.append((name, KINDS.get(state['kind'], state['kind']), status, state.get('desired', 'unknown')))
    return '\n'.join([
        table(('INSTALLED MODEL / SERVICE', 'TYPE', 'STATE', 'CONFIGURED ON'), rows), '',
        'Image weights load when a ComfyUI workflow runs. Their memory state is not inferred from installed files.',
        'Use sparky status to see the actual devices in use.',
    ])


def format_plan(data):
    title = f"Plan: {data['action']} {data['workload']} on {', '.join(data['nodes'])}"
    lines = [clean(title), 'No services were changed.']
    if data.get('wait_for'):
        lines.append('Wait for active work: ' + ', '.join(map(clean, data['wait_for'])))
    for index, step in enumerate(data.get('steps', []), 1):
        action = step['action']
        name = clean(step.get('workload', data['workload']))
        nodes = ', '.join(step.get('nodes', []))
        descriptions = {
            'unload': f'Unload {name} after its active requests finish.',
            'keep_running': f'Keep {name} running. No load is needed.',
            'release_idle_comfy_cache': f"Release idle ComfyUI model memory on {step.get('node')}. Keep the service running.",
            'restart_for_placement': f"Move {name} from {', '.join(step.get('from', []))} to {', '.join(step.get('to', []))} after active work finishes.",
            'load': f'Load {name} on {nodes}.',
            'ensure_running': f'Keep {name} running. Start it only if it is stopped.',
            'ensure_comfyui': f'Make ComfyUI available on {nodes}. Weights load when a workflow runs.',
        }
        lines.append(f"{index}. {clean(descriptions.get(action, action.replace('_', ' ')))}")
        minimums = step.get('minimum_available_gib', {})
        if minimums:
            lines.append('   Startup memory check: ' + ', '.join(f'{n}: {v} GiB available' for n, v in minimums.items()))
    if data.get('protected'):
        lines.append('Kept loaded: ' + ', '.join(map(clean, data['protected'])) + '.')
    if data.get('recipe_validation') == 'prepared_not_live_tested':
        lines.append('This placement recipe is prepared. A live model load has not been verified.')
    lines.append('Active jobs will not be interrupted.')
    return '\n'.join(lines)


def format_config(data):
    config = data['config']
    lines = [f"Authoritative file on Spark 1: {clean(data['path'])}"]
    if 'model' not in data:
        rows = []
        for name, model in config['models'].items():
            setting = model['settings'][model['run_on']]
            rows.append((name, KINDS[model['kind']], model['run_on'],
                         number(setting.get('context_tokens')) if model['kind'] == 'llm' else '-',
                         'yes' if model.get('keep_loaded') else 'no'))
        lines += [table(('MODEL / SERVICE', 'TYPE', 'RUN ON', 'CONTEXT', 'KEEP LOADED'), rows), '',
                  'Use sparky config show NAME for placement settings.',
                  'Use sparky config placement NAME spark01|spark02|both to change placement.',
                  'Use sparky load NAME to apply the selected placement.']
    else:
        name, model = data['model'], config
        lines += [clean(model.get('name', name)), f'Model ID: {clean(name)}',
                  f"Selected placement: {model['run_on']}",
                  f"Aliases: {', '.join(map(clean, model.get('aliases', []))) or 'none'}",
                  'Keep loaded: ' + ('yes' if model.get('keep_loaded') else 'no')]
        rows = []
        for placement, setting in model['settings'].items():
            parameters = setting.get('parameters', {})
            enabled = setting.get('enabled', True)
            rows.append((placement, number(setting.get('context_tokens')) if model['kind'] == 'llm' else '-',
                         str(setting.get('concurrency', '-')), str(parameters.get('max_sequences', '-')),
                         f"{parameters['gpu_memory_utilization']:.0%}" if 'gpu_memory_utilization' in parameters else '-',
                         'enabled' if enabled else 'disabled'))
            if setting.get('recipe_validation') == 'prepared_not_live_tested':
                lines.append(f'{placement}: recipe prepared; live model load not verified.')
            if not enabled:
                lines.append(f"{placement}: {clean(setting.get('reason', 'disabled'))}")
        lines += [table(('PLACEMENT', 'CONTEXT', 'REQUEST SLOTS', 'ENGINE SEQUENCES', 'GPU BUDGET', 'STATE'), rows),
                  'Request slots are manager admissions. Engine sequences are the model batch limit.',
                  'Changes apply on a future load. Use sparky status for the actual running placement.']
    return '\n'.join(lines)


def format_result(action, data):
    if action == 'validate':
        return f"Configuration is valid. {data['model_count']} model and service entries.\nFile on Spark 1: {clean(data['path'])}"
    if action == 'placement':
        return (f"Selected placement for {clean(data['workload'])}: {data['run_on']}.\n"
                f"Running services were not moved. Apply it with sparky load {clean(data['workload'])}.")
    if action == 'run':
        return (f"ComfyUI job submitted on {data['node']}.\nJob ID: {clean(data['prompt_id'])}\n"
                'Submission is complete. Execution may wait for a free device.\nUse sparky status to check queues.')
    if action in ('load', 'unload'):
        label = 'Ready' if action == 'load' else 'Unloaded'
        result = f"{label}: {clean(data['workload'])}."
        if data.get('note'):
            result += '\n' + clean(data['note'])
        return result
    if action == 'gateway':
        return 'Gateway protocol routes are ready. Existing model and audio services were kept.'
    raise ValueError(f'No result formatter for {action}.')


def format_devices(data):
    lines = []
    for node, state in data['devices'].items():
        if state.get('error'):
            lines += [f"{node}: unavailable. {clean(state['error'])}", '']
            continue
        mem, disk = state.get('memory_bytes', {}), state.get('disk_bytes', {})
        lines += [f"{node} ({clean(state.get('hostname', 'unknown'))})",
                  f"  Available RAM: {memory(mem.get('MemAvailable'))} / {memory(mem.get('MemTotal'))}",
                  f"  Free disk:     {memory(disk.get('free'))} / {memory(disk.get('total'))}"]
        for value in state.get('gpu', []):
            parts = value.split(',')
            if len(parts) == 4:
                name, temperature, used, total = map(str.strip, parts)
                lines.append(f'  GPU:           {clean(name)}, {temperature} C, {used} / {total} MiB allocated')
        lines.append('  Containers:    ' + (', '.join(map(clean, state.get('running_containers', []))) or 'none reported'))
        if state.get('warnings'):
            lines += ['  ' + clean(warning) for warning in state['warnings']]
        lines.append('')
    lines.append('CPU and GPU share memory on each Spark. Do not add the RAM and GPU figures together.')
    return '\n'.join(lines)


def format_launch(plan):
    model = plan['model']
    lines = ['Settings prepared. No harness was started.' if plan.get('prepared') else 'Launch preview. No settings were changed.',
             f"Harness: {plan['harness']}", f"Model: {model['id']}",
             f"Context: {number(model['context_tokens'])} tokens",
             f"Gateway: {plan['gateway']}", f"Project: {clean(plan['project'])}",
             f"Executable: {clean(plan['binary'])}"]
    if plan.get('offline'):
        lines.append('Using cached model metadata. The server was not checked.')
    lines += [f'Generated file: {clean(path)}' for path in plan.get('generated_files', [])]
    if plan.get('prepared') and plan['harness'] == 'hermes':
        lines.append('Only the existing Hermes Spark provider was updated.')
    if plan.get('extra_arguments'):
        import shlex
        lines.append('Native arguments: ' + clean(shlex.join(plan['extra_arguments'])))
    return '\n'.join(lines)


def format_logs(data):
    rows = []
    for record in data['records']:
        try:
            when = datetime.fromisoformat(record['started_at']).astimezone().strftime('%Y-%m-%d %H:%M %Z')
        except (ValueError, KeyError):
            when = 'unknown'
        if 'exit_code' in record:
            status = 'ok' if record['exit_code'] == 0 else f"exit {record['exit_code']}"
        else:
            status = 'not finished'
        rows.append((when, record.get('harness', '?'), str(record.get('model', '?')).removeprefix('garuda/'),
                     status, record.get('project', '?')))
    lines = ['Local harness launches. LLM call history stays in the gateway.']
    if rows:
        lines.append(table(('TIME (LOCAL)', 'HARNESS', 'MODEL', 'RESULT', 'PROJECT'), rows))
    else:
        lines.append('No local harness launches recorded. Start with sparky claude --model qwen.')
    if data.get('skipped_files'):
        lines.append(f"Skipped {len(data['skipped_files'])} unreadable launch records.")
    return '\n'.join(lines)


def format_doctor(data):
    rows = [(item['name'], 'ok' if item['ok'] else 'failed', item['message']) for item in data['checks']]
    return table(('CHECK', 'RESULT', 'DETAIL'), rows) + '\nNo inference was run.'
