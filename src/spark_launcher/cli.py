"""Human-first commands and an explicit JSON interface for scripts."""
from __future__ import annotations

import argparse
import json
import difflib
import re
import subprocess
import sys
import uuid
import webbrowser
from pathlib import Path

from . import __version__
from .adapters import HARNESSES, build_plan, check_passthrough, executable, launch, prepare, sync_hermes
from .catalog import SparkError, config_path, fetch_catalog, load_config, resolve, state_dir, validate_config, write_json
from .control import (admin_ssh, controller_request, devices, node_device, node_resources,
                      progress, remote_result, remote_rpc, remote_status)
from . import presentation as view
from .status import format_status


class UsageError(SparkError):
    """A command syntax error, distinct from a device or gateway failure."""


class Parser(argparse.ArgumentParser):
    def format_help(self):
        return view.help_view(super().format_help())

    def error(self, message):
        if message.startswith('argument COMMAND: invalid choice:'):
            invalid = re.search(r'invalid choice: ([^ ]+)', message)
            name = invalid.group(1).strip("'\"") if invalid else ''
            choices = next((a.choices for a in self._actions if a.dest == 'command'), {})
            closest = difflib.get_close_matches(name, choices, n=1, cutoff=.6)
            hint = f' Did you mean {closest[0]}?' if closest else ''
            message = f'Unknown command: {name}.' + hint
        raise UsageError(message.rstrip('.') + f'. Run {self.prog} --help for usage.')


def common_output_options(command):
    command.add_argument('--json', action='store_true', default=argparse.SUPPRESS,
                         help='Print one JSON result for scripts')
    command.add_argument('--color', choices=('auto', 'always', 'never'), default=argparse.SUPPRESS,
                         help='Terminal colors; default: auto')


def parser():
    p = Parser(prog='sparky', description='Manage models on two Sparks and launch coding tools.',
               formatter_class=argparse.RawDescriptionHelpFormatter,
               epilog='Examples:\n  sparky status\n  sparky models\n  sparky plan load glm\n'
                      '  sparky claude --model qwen\n  sparky status --json\n\n'
                      'Use sparky COMMAND --help for command options.\n'
                      'Native harness arguments go after --, for example:\n'
                      '  sparky claude --model qwen -- --continue')
    p.add_argument('--version', action='version', version='sparky ' + __version__)
    p.add_argument('--json', action='store_true', help='Print JSON; also accepted after a command')
    p.add_argument('--color', choices=('auto', 'always', 'never'), default='auto', help='Terminal colors; default: auto')
    sub = p.add_subparsers(dest='command', title='Commands', metavar='COMMAND')

    def command(name, help_text):
        child = sub.add_parser(name, help=help_text, description=help_text)
        common_output_options(child)
        return child

    command('status', 'Show available memory, running models, and queued work')
    models = command('models', 'List model names and limits; does not load a model')
    models.add_argument('model', nargs='?', help='Model name or alias to inspect')
    models.add_argument('--offline', action='store_true', help='Use cached data without checking the server')
    models.add_argument('--installed', action='store_true', help='Include audio, ComfyUI, and installed image models')
    command('devices', 'Show memory, disk space, GPU use, and containers on each Spark')
    plan = command('plan', 'Show required resource actions without changing services')
    plan.add_argument('action', choices=('load', 'unload'))
    plan.add_argument('model', help='Installed model name or alias')
    plan.add_argument('--on', choices=('spark01', 'spark02', 'both'), help='Preview another placement without saving it')
    for name in ('load', 'unload'):
        child = command(name, 'Load a model after active work finishes' if name == 'load' else
                        'Unload a model after its active work finishes')
        child.add_argument('model', help='Installed model name or alias; see sparky models')
        child.add_argument('--force', action='store_true', help='Cancel conflicting language requests and stop their models before this operation')

    image_profile = command('image-profile', 'Select tested shared or dedicated image memory limits')
    image_profile.add_argument('mode', choices=('show', 'shared', 'dedicated'), nargs='?', default='show')
    image_profile.add_argument('--dry-run', action='store_true')

    config = command('config', 'Inspect or change the central settings on Spark 1')
    config_sub = config.add_subparsers(dest='config_action', required=True, metavar='ACTION')
    show = config_sub.add_parser('show', help='Show all settings or one model')
    show.add_argument('model', nargs='?', help='Model name or alias to inspect')
    validate = config_sub.add_parser('validate', help='Check the central file without changing it')
    placement = config_sub.add_parser('placement', help='Save a placement for the next load')
    placement.add_argument('model', help='Installed model name or alias')
    placement.add_argument('on', choices=('spark01', 'spark02', 'both'))
    for child in (show, validate, placement):
        common_output_options(child)

    workflow = command('run', 'Submit a ComfyUI API workflow; execution may wait for a device')
    workflow.add_argument('engine', choices=('comfyui',))
    workflow.add_argument('workflow', help='Path to an exported ComfyUI API workflow JSON file')
    workflow.add_argument('--on', choices=('spark01', 'spark02'), help='Device for this workflow')
    names = {'claude': 'Claude Code', 'hermes': 'Hermes', 'codex': 'Codex', 'opencode': 'OpenCode'}
    for name in HARNESSES:
        child = command(name, 'Launch ' + names[name] + ' with a gateway model')
        child.add_argument('--model', '-m', help='Model name or alias; defaults to the configured model')
        child.add_argument('--project', default='.', help='Project directory; defaults to the current directory')
        child.add_argument('--offline', action='store_true', help='Use cached model metadata')
        group = child.add_mutually_exclusive_group()
        group.add_argument('--dry-run', action='store_true', help='Preview the launch without writing settings or starting it')
        group.add_argument('--prepare', action='store_true', help='Write harness settings without starting it')
    command('sync', 'Refresh only the Hermes Spark provider from the live model list')
    logs = command('logs', 'Show local harness launch history; LLM call history stays in the gateway')
    logs.add_argument('--limit', type=positive_int, default=10, help='Number of recent launches; default: 10')
    doctor = command('doctor', 'Check gateway access and harness installation; no inference')
    doctor.add_argument('harness', nargs='?', choices=HARNESSES, help='Check one harness instead of all four')
    command('ui', 'Open the gateway UI; --json returns its URL without opening it')
    init = command('init', 'Create the Mac or client connection file')
    init.add_argument('--gateway', required=True, help='HTTPS gateway origin, without /v1')
    init.add_argument('--prefix', default='garuda/', help='Gateway model prefix; default: garuda/')
    init.add_argument('--admin-host', default='', help='Existing SSH alias for device controls, for example spark01-admin')
    init.add_argument('--worker-host', default='spark02', help='Worker SSH alias as seen from Spark 1')
    init.add_argument('--remote-root', default='', help='Absolute cluster directory on Spark 1')
    init.add_argument('--admin-url', default='', help='Gateway UI URL')

    admin = command('admin', 'Open administrator SSH or configure harness protocol routes')
    admin_sub = admin.add_subparsers(dest='action', required=True, metavar='ACTION')
    ssh = admin_sub.add_parser('ssh', help='Open an interactive administrator shell')
    ssh.add_argument('target', nargs='?', default='spark01', choices=('spark01', 'spark02'))
    setup = admin_sub.add_parser('enable-harnesses', help='Configure gateway protocol routes with backups')
    common_output_options(setup)
    # Node commands form the private machine protocol. Their output stays JSON.
    node = sub.add_parser('node', help='Internal node protocol; not needed for normal use')
    node_sub = node.add_subparsers(dest='action', required=True, metavar='ACTION')
    node_sub.add_parser('device')
    node_sub.add_parser('memory')
    install = node_sub.add_parser('install')
    install.add_argument('--dir', required=True)
    for name in ('drain-comfy', 'probe', 'controller'):
        node_sub.add_parser(name).add_argument('--file', required=True)
    node_sub.add_parser('render').add_argument('--payload', required=True)
    rpc = node_sub.add_parser('rpc')
    rpc.add_argument('operation', choices=('plan', 'config', 'placement', 'run', 'validate', 'image-profile'))
    rpc.add_argument('--root', required=True)
    rpc.add_argument('--payload', default='{}')
    for name in ('status', 'load', 'unload', 'gateway'):
        child = node_sub.add_parser(name)
        child.add_argument('--root', required=True)
        if name in ('load', 'unload'):
            child.add_argument('--model', required=True)
            child.add_argument('--force', action='store_true')
    return p


def positive_int(value):
    try:
        number = int(value)
    except ValueError:
        raise argparse.ArgumentTypeError('use a positive whole number') from None
    if number < 1:
        raise argparse.ArgumentTypeError('use a positive whole number')
    return number


def output(value):
    print(json.dumps(value, indent=2, ensure_ascii=False))


def emit(value, formatter, as_json):
    if as_json:
        output(value)
    else:
        print(formatter(value))


def initialize(args):
    if config_path().exists():
        raise SparkError(f'Client configuration already exists at {config_path()}. Edit that file instead.')
    config = {'version': 1, 'gateway': args.gateway, 'model_prefix': args.prefix,
              'default_model': args.prefix + 'spark-auto', 'timeout_seconds': 7200,
              'compact_percent': 95, 'admin_ssh_host': args.admin_host, 'worker_ssh_host': args.worker_host,
              'remote_root': args.remote_root, 'admin_url': args.admin_url}
    write_json(config_path(), validate_config(config))
    result = {'status': 'created', 'path': str(config_path())}
    emit(result, lambda d: view.notice("Client configured", f"Client configuration created: {d['path']}\nRun sparky models to check gateway access."), args.json)


def node_action(args):
    """Private protocol used by the manager and SSH client. No human rendering."""
    if args.action == 'gateway':
        from .gateway import enable_gateway
        return enable_gateway(args.root)
    if args.action == 'install':
        from .deployment import install_head
        return install_head(args.dir)
    if args.action == 'drain-comfy':
        from .deployment import drain_comfy
        drain_comfy(args.file)
        return {'status': 'drained'}
    if args.action == 'render':
        from .parameters import render_parameters
        return render_parameters(json.loads(sys.stdin.read() if args.payload == '-' else args.payload))
    if args.action == 'controller':
        from .controller_server import serve
        serve(args.file)
        return None
    if args.action == 'probe':
        from .resource_runtime import probe
        return probe(json.loads(sys.stdin.read() if args.file=='-' else Path(args.file).read_text()))
    if args.action == 'rpc':
        payload = json.loads(sys.stdin.read() if args.payload == '-' else args.payload)
        if args.operation == 'image-profile':
            from .image_profiles import activate
            return activate(args.root, payload['mode'], payload.get('dry_run', False))
        if args.operation in ('config', 'validate'):
            document = controller_request(args.root, 'config')
            if args.operation == 'validate':
                return {'valid': True, 'path': document['path'], 'model_count': len(document['config']['models'])}
            if payload.get('model'):
                from .resources import resolve_workload
                name, model = resolve_workload(document['config'], payload['model'])
                return {'path': document['path'], 'model': name, 'config': model}
            return document
        if payload.get('model'):
            from .resources import read_cluster, resolve_workload, selected
            config = read_cluster(Path(args.root) / 'config/models.json')
            payload['model'] = resolve_workload(config, payload['model'])[0]
            if args.operation in ('placement', 'plan'):
                selected(config, payload['model'], payload.get('on'))
        result = controller_request(args.root, args.operation, payload)
        if args.operation == 'plan':
            from .resources import read_cluster
            config = read_cluster(Path(args.root) / 'config/models.json')
            model = config['models'][result['workload']]
            recipe_state = model['settings'][result['placement']].get('recipe_validation')
            if recipe_state:
                result['recipe_validation'] = recipe_state
        return result
    if args.action == 'memory':
        from .memory_usage import collect
        return collect(json.loads(sys.stdin.read()))
    if args.action == 'device':
        return node_device()
    return node_resources(args.root, args.action, getattr(args, 'model', None), **({'force': True} if getattr(args, 'force', False) else {}))


def read_workflow(path):
    path = Path(path).expanduser()
    try:
        data = json.loads(path.read_text())
    except FileNotFoundError:
        raise SparkError(f'Workflow file not found: {path}. Export a ComfyUI API workflow first.') from None
    except json.JSONDecodeError as error:
        raise SparkError(f'Invalid workflow JSON in {path} at line {error.lineno}, column {error.colno}.') from None
    graph = data.get('prompt', data) if isinstance(data, dict) else None
    if not isinstance(graph, dict) or not graph or 'nodes' in graph or 'links' in graph:
        raise SparkError('Use a ComfyUI API workflow, not the UI workflow format. Export it in API format from ComfyUI.')
    if any(not isinstance(node, dict) or not isinstance(node.get('class_type'), str) or
           not isinstance(node.get('inputs'), dict) for node in graph.values()):
        raise SparkError('Each API workflow node needs class_type and inputs. Export the workflow in API format from ComfyUI.')
    return data


def read_logs(limit):
    records, skipped = [], []
    for path in (state_dir() / 'launches').glob('*.json'):
        try:
            record = json.loads(path.read_text())
            if not isinstance(record, dict) or not isinstance(record.get('started_at'), str):
                raise ValueError('Invalid launch record')
            records.append(record)
        except (ValueError, OSError):
            skipped.append(path.name)
    records.sort(key=lambda r: r['started_at'], reverse=True)
    return {'records': records[:limit], 'skipped_files': skipped}


def doctor(config, harness=None):
    checks = []
    try:
        catalog = fetch_catalog(config)
        checks.append({'name': 'gateway', 'ok': True, 'message': f"Reachable; {len(catalog['models'])} model routes."})
    except (SparkError, OSError, ValueError) as error:
        checks.append({'name': 'gateway', 'ok': False, 'message': str(error)})
    for name in ([harness] if harness else HARNESSES):
        try:
            binary = executable(config, name)
            checks.append({'name': name, 'ok': True, 'message': binary})
        except SparkError as error:
            checks.append({'name': name, 'ok': False, 'message': str(error)})
    return {'ok': all(item['ok'] for item in checks), 'checks': checks, 'inference': False}


def run(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    view.configure_from_args(argv)
    p = parser()
    if not argv:
        p.print_help()
        return 0
    extra = []
    # Only a harness consumes arguments after --. They are not Sparky options.
    before_native = argv[:argv.index('--')] if '--' in argv else argv
    harness = next((name for name in HARNESSES if name in before_native), None)
    if harness and '--' in argv:
        index = argv.index('--')
        argv, extra = argv[:index], argv[index + 1:]
    args = p.parse_args(argv)
    if not args.command:
        raise UsageError('Choose a command. Run sparky --help for examples.')
    if args.command == 'node':
        value = node_action(args)
        if value is not None:
            output(value)
        return 0
    if args.command == 'init':
        initialize(args)
        return 0
    if args.command == 'logs':
        emit(read_logs(args.limit), view.format_logs, args.json)
        return 0
    config = load_config()
    if args.command in HARNESSES:
        if args.json and not (args.dry_run or args.prepare):
            raise UsageError('Use --json with --dry-run or --prepare. Pass native harness options after --.')
        check_passthrough(args.command, extra)
        project = Path(args.project).expanduser().resolve()
        if not project.is_dir():
            raise SparkError(f'Project directory does not exist: {project}. Choose it with --project.')
        catalog = fetch_catalog(config, args.offline)
        if args.offline:
            catalog['offline'] = True
        model = resolve(catalog, args.model or config['default_model'])
        plan = build_plan(config, catalog, model, args.command, project, uuid.uuid4().hex)
        if args.dry_run or args.prepare:
            if args.prepare:
                prepare(config, catalog, plan)
            plan.update(prepared=args.prepare, gateway=config['gateway'], offline=args.offline,
                        extra_arguments=extra)
            plan['args'].extend(extra)
            emit(plan, view.format_launch, args.json)
            return 0
        if args.offline:
            print('Using cached model metadata. The server was not checked.', file=sys.stderr)
        return launch(config, catalog, plan, extra)
    if args.command == 'models':
        if args.installed:
            if args.offline or args.model:
                raise UsageError('--installed cannot be combined with --offline or a model name. Use config show NAME for installed settings.')
            emit(remote_status(config), view.format_inventory, args.json)
        else:
            catalog = fetch_catalog(config, args.offline)
            if args.offline:
                catalog['offline'] = True
            model = resolve(catalog, args.model) if args.model else None
            emit(model if model and args.json else catalog,
                 lambda d: view.format_models(d, model), args.json)
        return 0
    if args.command == 'status':
        emit(remote_status(config), format_status, args.json)
        return 0
    if args.command == 'devices':
        result = devices(config)
        emit(result, view.format_devices, args.json)
        return 0 if result['ok'] else 1
    if args.command == 'plan':
        data = remote_rpc(config, 'plan', {'action': args.action, 'model': args.model, 'on': args.on})
        emit(data, view.format_plan, args.json)
        return 0
    if args.command == 'image-profile':
        data = remote_rpc(config, 'image-profile', {'mode': args.mode, 'dry_run': args.dry_run})
        emit(data, lambda d: view.format_result('image-profile', d), args.json)
        return 0
    if args.command == 'config':
        operation = {'show': 'config', 'validate': 'validate', 'placement': 'placement'}[args.config_action]
        payload = {'model': args.model, 'on': args.on} if operation == 'placement' else {'model': getattr(args, 'model', None)}
        data = remote_rpc(config, operation, payload)
        formatter = view.format_config if operation == 'config' else lambda d: view.format_result(operation, d)
        emit(data, formatter, args.json)
        return 0
    if args.command == 'run':
        workflow = read_workflow(args.workflow)
        with progress('Submitting ComfyUI workflow...', not args.json):
            data = remote_rpc(config, 'run', {'workflow': workflow, 'on': args.on})
        if not data.get('prompt_id'):
            raise SparkError('ComfyUI did not accept the workflow. Check its node types and inputs.')
        emit(data, lambda d: view.format_result('run', d), args.json)
        return 0
    if args.command in ('load', 'unload'):
        if not args.json and not args.force:
            plan = remote_rpc(config, 'plan', {'action': args.command, 'model': args.model})
            conflicts = [step['workload'] for step in plan['steps'] if step['action'] == 'unload' and step['workload'] != plan['workload']]
            if conflicts:
                print('Will unload after active requests finish: ' + ', '.join(conflicts) + '.', file=sys.stderr)
            if plan.get('recipe_validation') == 'prepared_not_live_tested':
                print('This placement recipe has not passed a live model load test.', file=sys.stderr)
        message = (f'Loading {args.model}. This can take several minutes.' if args.command == 'load' else
                   f'Waiting to unload {args.model} after active requests finish.')
        if args.force:
            message = f'Forcing {args.command} {args.model}. Conflicting language requests will stop.'
        with progress(message, not args.json, forced=args.force):
            data = remote_result(config, args.command, args.model, **({'force': True} if args.force else {}))
        emit(data, lambda d: view.format_result(args.command, d), args.json)
        return 0
    if args.command == 'sync':
        catalog = fetch_catalog(config)
        backup = sync_hermes(config, catalog, uuid.uuid4().hex)
        data = {'status': 'updated' if backup is not None else 'current', 'backup': backup or None, 'models': len(catalog['models'])}
        emit(data, lambda d: view.notice('Hermes settings', 'Hermes Spark provider is current. Unrelated providers were kept.' +
             (f"\nPrivate backup: {d['backup']}" if d['backup'] else '')), args.json)
        return 0
    if args.command == 'doctor':
        data = doctor(config, args.harness)
        emit(data, view.format_doctor, args.json)
        return 0 if data['ok'] else 1
    if args.command == 'ui':
        url = config.get('admin_url')
        if not url:
            raise SparkError(f'Set admin_url in {config_path()} to your gateway UI URL.')
        if args.json:
            output({'url': url})
        elif webbrowser.open(url):
            print(view.notice('Gateway UI', 'Opened gateway UI: ' + url))
        else:
            print(view.notice('Gateway UI', 'Open this gateway UI URL in your browser: ' + url))
        return 0
    if args.command == 'admin':
        if args.action == 'ssh':
            if args.json:
                raise UsageError('Interactive SSH does not support --json. Run sparky admin ssh spark01.')
            return admin_ssh(config, args.target)
        with progress('Configuring gateway protocol routes...', not args.json):
            data = remote_result(config, 'gateway')
        emit(data, lambda d: view.format_result('gateway', d), args.json)
        return 0
    raise UsageError('Choose a command. Run sparky --help for examples.')


def json_requested(argv):
    options = argv[:argv.index('--')] if '--' in argv else argv
    return '--json' in options


def main():
    as_json = json_requested(sys.argv[1:])
    try:
        code = run()
    except KeyboardInterrupt:
        message = 'Stopped waiting. Remote work may still continue. Run sparky status to check it.'
        if as_json:
            print(json.dumps({'error': message, 'code': 130}), file=sys.stderr)
        else:
            print(view.notice('Interrupted', message, 'yellow', stream=sys.stderr), file=sys.stderr)
        code = 130
    except (SparkError, OSError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
        code = 2 if isinstance(error, UsageError) else 1
        if isinstance(error, subprocess.TimeoutExpired):
            message = 'The device did not respond before the timeout. Run sparky status to check it. Remote work may still continue.'
        elif isinstance(error, KeyError):
            message = 'Required data is missing. Run sparky doctor and update sparky on both Sparks.'
        else:
            message = str(error)
        if as_json:
            # Private node errors use stdout so SSH clients receive one envelope.
            command_words = [word for word in sys.argv[1:] if word != '--json']
            stream = sys.stdout if command_words and command_words[0] == 'node' else sys.stderr
            print(json.dumps({'error': message, 'code': code}), file=stream)
        else:
            print(view.notice('sparky: error', message, 'red', stream=sys.stderr), file=sys.stderr)
    raise SystemExit(code or 0)
