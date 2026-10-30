"""Human-readable command results. JSON serialization stays in the CLI."""
from __future__ import annotations

from datetime import datetime
from .terminal import clean, table, heading, note, key_values, notice, help_view, configure_from_args

KINDS = {'llm': 'LLM', 'audio': 'audio', 'comfyui': 'ComfyUI', 'comfy_model': 'image'}


def memory(value):
    return 'unknown' if value is None else f'{value / 1024**3:.1f} GiB'


def number(value):
    return f'{value:,}' if isinstance(value, int) else 'unknown'


def display_model(model):
    return (model.get('aliases') or [model['id'].split('/', 1)[-1]])[0]


def format_models(catalog, model=None):
    if model is not None:
        return key_values(model.get('name', model['id']), [
            ('Model ID', model['id']), ('Aliases', ', '.join(model.get('aliases', [])) or 'none'),
            ('Context', number(model['context_tokens']) + ' tokens'),
            ('Output budget', number(model.get('output_tokens')) + ' tokens'),
            ('Configured devices', ', '.join(model.get('nodes', [])) or 'automatic routing'),
            ('Input', ', '.join(model.get('inputs', ['text']))),
        ]) + '\n' + note('This shows configuration. It does not load a model.')
    rows = [(display_model(m), number(m['context_tokens']), number(m.get('output_tokens')),
             ', '.join(m.get('nodes', [])) or 'automatic', ', '.join(m.get('inputs', ['text'])))
            for m in catalog['models']]
    lines = [table(('MODEL', 'CONTEXT', 'OUTPUT', 'RUNS ON', 'INPUT'), rows, title='Available models'),
             '', 'Context and output are token counts. These are configured routes, not running models.',
             'Use sparky models NAME for details. Use sparky status for running models.']
    if catalog.get('offline'):
        lines.append(f"Cached data from {clean(catalog.get('fetched_at', 'unknown time'))}. Current server state was not checked.")
    for service in catalog.get('audio_services', []):
        lines += ['', format_speech_service(service)]
    return '\n'.join(lines)


def format_speech_service(service):
    return key_values('Speech generation', [
        ('Model', service['model']), ('Default voice', service['default_voice']),
        ('API base', service['api_base']),
        ('Speech URL', service.get('speech_url', service['api_base'].rstrip('/') + '/audio/speech')),
        ('Runs on', ', '.join(service.get('nodes', []))),
        ('Device', service.get('device', 'unknown')),
        ('API key', 'not required' if service.get('api_key_required') is False else 'see service settings'),
        ('Voices', ', '.join(service.get('voices', []))),
        ('Formats', ', '.join(service.get('formats', []))),
    ])


def format_inventory(data):
    rows = []
    for name, state in data.get('models', {}).items():
        status = 'on demand' if state['kind'] == 'comfy_model' else state.get('state', 'unknown')
        rows.append((name, KINDS.get(state['kind'], state['kind']), status, state.get('desired', 'unknown')))
    return '\n'.join([
        table(('MODEL / SERVICE', 'TYPE', 'STATE', 'CONFIGURED ON'), rows, title='Installed models and services'), '',
        'Image weights load when a ComfyUI workflow runs. Their memory state is not inferred from installed files.',
        'Use sparky status to see the actual devices in use.',
    ])


def format_plan(data):
    title = f"Plan: {data['action']} {data['workload']} on {', '.join(data['nodes'])}"
    lines = [heading(title), note('No services were changed.')]
    if data.get('wait_for'):
        lines.append('Wait for active work: ' + ', '.join(map(clean, data['wait_for'])))
    steps = []
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
        detail = clean(descriptions.get(action, action.replace('_', ' ')))
        steps.append((str(index), action.replace('_', ' '), detail))
        minimums = step.get('minimum_available_gib', {})
        if minimums:
            steps.append(('', 'memory check', ', '.join(f'{n}: {v} GiB available' for n, v in minimums.items())))
    lines.append(table(('STEP', 'ACTION', 'DETAIL'), steps))
    if data.get('protected'):
        lines.append(note('Kept loaded: ' + ', '.join(map(clean, data['protected'])) + '.', 'green'))
    if data.get('recipe_validation') == 'prepared_not_live_tested':
        lines.append(notice('Unverified placement', 'This placement recipe is prepared. A live model load has not been verified.', 'yellow'))
    lines.append(note('Active jobs will not be interrupted.'))
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
        lines += [table(('MODEL / SERVICE', 'TYPE', 'RUN ON', 'CONTEXT', 'KEEP LOADED'), rows, title='Central model settings'), '',
                  'Use sparky config show NAME for placement settings.',
                  'Use sparky config placement NAME spark01|spark02|both to change placement.',
                  'Use sparky load NAME to apply the selected placement.']
    else:
        name, model = data['model'], config
        lines += [clean(model.get('name', name)), f'Model ID: {clean(name)}',
                  f"Selected placement: {model['run_on']}",
                  f"Aliases: {', '.join(map(clean, model.get('aliases', []))) or 'none'}",
                  'Keep loaded: ' + ('yes' if model.get('keep_loaded') else 'no')]
        if model.get('monitoring', {}).get('containers'):
            lines.append('Memory containers: ' + ', '.join(model['monitoring']['containers']))
        if model['kind'] == 'audio' and model.get('tts'):
            lines.append(format_speech_service({**model['tts'], 'nodes': [model['run_on']]}))
            return '\n'.join(lines)
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
        lines += [table(('PLACEMENT', 'CONTEXT', 'SLOTS', 'SEQUENCES', 'GPU BUDGET', 'STATE'), rows, title='Supported placements'),
                  'Request slots are manager admissions. Engine sequences are the model batch limit.',
                  'Changes apply on a future load. Use sparky status for the actual running placement.']
    return '\n'.join(lines)


def format_result_text(action, data):
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


def format_result(action, data):
    titles = {'validate': 'Configuration valid', 'placement': 'Placement saved', 'run': 'Workflow submitted',
              'load': 'Model ready', 'unload': 'Model unloaded', 'gateway': 'Gateway ready'}
    return notice(titles.get(action, 'Result'), format_result_text(action, data))


def format_devices(data):
    cards = []
    for node, state in data['devices'].items():
        if state.get('error'):
            cards.append(notice(node + ': unavailable', state['error'], 'red'))
            continue
        mem, disk = state.get('memory_bytes', {}), state.get('disk_bytes', {})
        rows = [('Available RAM', f"{memory(mem.get('MemAvailable'))} / {memory(mem.get('MemTotal'))}"),
                ('Free disk', f"{memory(disk.get('free'))} / {memory(disk.get('total'))}")]
        for value in state.get('gpu', []):
            parts = value.split(',')
            if len(parts) == 4:
                name, temperature, used, total = map(str.strip, parts)
                rows += [('GPU', name), ('GPU allocation', f'{used} / {total} MiB'), ('Temperature', temperature + ' C')]
        rows.append(('Containers', ', '.join(state.get('running_containers', [])) or 'none reported'))
        cards.append(key_values(node + ' (' + state.get('hostname', 'unknown') + ')', rows))
        cards += [notice('Device warning', warning, 'yellow') for warning in state.get('warnings', [])]
    cards.append(note('CPU and GPU share memory on each Spark. Do not add the RAM and GPU figures together.'))
    return '\n'.join(cards)


def format_launch(plan):
    model = plan['model']
    title = 'Settings prepared' if plan.get('prepared') else 'Launch preview'
    rows = [('Harness', plan['harness']), ('Model', model['id']),
            ('Context', number(model['context_tokens']) + ' tokens'), ('Gateway', plan['gateway']),
            ('Project', plan['project']), ('Executable', plan['binary'])]
    rows += [('Generated file', path) for path in plan.get('generated_files', [])]
    if plan.get('extra_arguments'):
        import shlex
        rows.append(('Native arguments', shlex.join(plan['extra_arguments'])))
    lines = [key_values(title, rows), note('No harness was started.' if plan.get('prepared') else 'Launch preview. No settings were changed.')]
    if plan.get('offline'):
        lines.append(note('Using cached model metadata. The server was not checked.', 'yellow'))
    if plan.get('prepared') and plan['harness'] == 'hermes':
        lines.append(note('Only the existing Hermes Spark provider was updated.'))
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
        lines.append(table(('TIME (LOCAL)', 'HARNESS', 'MODEL', 'RESULT', 'PROJECT'), rows, title='Recent harness launches'))
    else:
        lines.append('No local harness launches recorded. Start with sparky claude --model qwen.')
    if data.get('skipped_files'):
        lines.append(f"Skipped {len(data['skipped_files'])} unreadable launch records.")
    return '\n'.join(lines)


def format_doctor(data):
    rows = [(item['name'], 'ok' if item['ok'] else 'failed', item['message']) for item in data['checks']]
    return table(('CHECK', 'RESULT', 'DETAIL'), rows, title='Connection checks') + '\n' + note('No inference was run.')
