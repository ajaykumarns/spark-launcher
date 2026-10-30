"""Explicit device operations. Credentials are read only on the head node."""
from __future__ import annotations

import json
import re
import shlex
import socket
import sys
import threading
import time
from contextlib import contextmanager
from concurrent.futures import ThreadPoolExecutor
import shutil
import subprocess
from pathlib import Path

from .catalog import SparkError, request_json


def safe_host(host):
    if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.@-]*", host or ""):
        raise SparkError("Set an existing SSH host alias in the site file.")
    return host


def remote_command(config, action, model=None, *, force=False):
    host = safe_host(config.get("admin_ssh_host"))
    root = config.get("remote_root", "")
    if not Path(root).is_absolute():
        raise SparkError("Set an absolute remote_root in the site file.")
    if force and action not in ('load', 'unload'):
        raise SparkError('--force is available only for load and unload.')
    args = [action, "--root", root]
    if force:args.append('--force')
    if model:
        args.extend(["--model", model])
    return ["ssh", "-o", "ConnectTimeout=10", host, "~/.local/bin/sparky --json node " + shlex.join(args)]


def identity_check_message(stderr):
    if isinstance(stderr, bytes):
        stderr = stderr.decode(errors='replace')
    check = re.search(r'https://login\.tailscale\.com/a/[A-Za-z0-9]+', stderr or '')
    if check:
        return ('Tailscale needs an identity check. Open ' + check.group() +
                ', then run the command again.')
    return None


def remote_failure_message(code, stderr):
    """Explain known failures without exposing arbitrary remote diagnostics."""
    check = identity_check_message(stderr)
    if check:
        return check
    if code != 255:
        return (f'The device command failed (exit {code}). The '
                'device did not return an error report. Run sparky doctor to check the installation.')
    detail = (stderr or '').lower()
    reasons = (
        (('host key verification failed', 'remote host identification has changed'),
         'SSH could not verify the device identity. Check its host key before changing known_hosts.'),
        (('permission denied', 'authentication failed'),
         'SSH authentication failed. Run sparky admin ssh spark01 to check your access.'),
        (('connection timed out', 'operation timed out', 'no route to host', 'network is unreachable'),
         'The SSH connection timed out or the device is unreachable. Check Tailscale and the device network.'),
        (('connection refused',),
         'The device refused the SSH connection. Check that its SSH service is running.'),
        (('could not resolve hostname', 'name or service not known'),
         'SSH could not find the configured device address. Check the SSH host alias.'),
        (('connection reset', 'connection closed', 'broken pipe'),
         'The SSH connection closed before the device returned data. Run the command again.'),
    )
    for patterns, message in reasons:
        if any(pattern in detail for pattern in patterns):
            return message
    return ('The SSH connection failed (exit 255). Run sparky admin ssh spark01 '
            'to see the connection details.')


def run_remote_json(command, *, payload=None, timeout=90):
    """Read the private SSH protocol without printing its JSON or diagnostics.

    Inspect partial stderr while waiting so a Tailscale sign-in link does not
    stay hidden behind a long model-load timeout. Only the known login URL is
    shown. Raw SSH or application diagnostics are not copied to user output.
    """
    with subprocess.Popen(command, stdin=subprocess.PIPE if payload is not None else subprocess.DEVNULL,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True) as process:
        deadline = time.monotonic() + timeout
        first_input = payload
        try:
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise subprocess.TimeoutExpired(command, timeout)
                try:
                    stdout, stderr = process.communicate(first_input, timeout=min(5, remaining))
                    break
                except subprocess.TimeoutExpired as error:
                    first_input = None  # communicate resumes the original input buffer.
                    message = identity_check_message(error.stderr)
                    if message:
                        raise SparkError(message) from None
        except BaseException:
            process.kill()
            process.communicate()
            raise
        code = process.returncode
    try:
        data = json.loads(stdout)
    except ValueError:
        data = None
    if code:
        if isinstance(data, dict) and isinstance(data.get('error'), str):
            raise SparkError(data['error'])
        raise SparkError(remote_failure_message(code, stderr))
    if not isinstance(data, dict):
        raise SparkError('The device returned invalid data. Update sparky on both Sparks.')
    return data


def remote_status(config):
    return run_remote_json(remote_command(config, 'status'))


def remote_result(config, action, model=None, *, force=False):
    timeout = 7230 if action in ('load', 'unload') else 120
    return run_remote_json(remote_command(config, action, model, force=force), timeout=timeout)


def remote_rpc(config, operation, payload):
    root = config.get('remote_root', '')
    if not Path(root).is_absolute():
        raise SparkError('Set an absolute remote_root in the client configuration.')
    words = ['rpc', operation, '--root', root, '--payload', '-']
    command = ['ssh', '-o', 'ConnectTimeout=10', safe_host(config.get('admin_ssh_host')),
               '~/.local/bin/sparky --json node ' + shlex.join(words)]
    timeout = 900 if operation == 'image-profile' else (180 if operation == 'run' else 120)
    return run_remote_json(command, payload=json.dumps(payload), timeout=timeout)


@contextmanager
def progress(message, enabled=True, *, forced=False):
    """Send occasional progress to stderr. JSON stdout stays one document."""
    if not enabled:
        yield
        return
    done = threading.Event()
    print(message, file=sys.stderr, flush=True)

    def report():
        seconds = 0
        while not done.wait(15):
            seconds += 15
            detail = 'The forced model operation is still running.' if forced else 'Active work must finish before a device changes.'
            print(f'Still waiting ({seconds}s). {detail}',
                  file=sys.stderr, flush=True)

    thread = threading.Thread(target=report, daemon=True)
    thread.start()
    try:
        yield
    finally:
        done.set()
        thread.join(timeout=1)


def admin_ssh(config, target):
    host = safe_host(config.get("admin_ssh_host"))
    if target == "spark01":
        return subprocess.call(["ssh", host])
    if target == "spark02":
        worker = safe_host(config.get("worker_ssh_host"))
        return subprocess.call(["ssh", "-t", host, "ssh -t " + shlex.quote(worker)])
    raise SparkError("Choose spark01 or spark02.")


def devices(config):
    host = safe_host(config.get('admin_ssh_host'))
    worker = safe_host(config.get('worker_ssh_host'))
    commands = {
        'spark01': '~/.local/bin/sparky --json node device',
        'spark02': 'ssh ' + shlex.quote(worker) + ' ' +
                   shlex.quote('~/.local/bin/sparky --json node device'),
    }

    def inspect(item):
        name, command = item
        try:
            return name, run_remote_json(['ssh', '-o', 'ConnectTimeout=10', host, command], timeout=60)
        except (SparkError, subprocess.TimeoutExpired) as error:
            message = str(error) if isinstance(error, SparkError) else 'Device query timed out.'
            return name, {'error': message}

    with ThreadPoolExecutor(max_workers=2) as pool:
        results = dict(pool.map(inspect, commands.items()))
    return {'devices': results, 'ok': all('error' not in d for d in results.values())}


def controller_request(root, operation, payload=None):
    from .resources import read_cluster
    config = read_cluster(Path(root) / 'config/models.json')
    port = config['controller_port']
    timeout = 7200 if operation in ('load', 'unload') else 120
    return request_json(f'http://127.0.0.1:{port}/control/' + operation,
                        payload=payload, timeout=timeout, safe_error_body=True)


def read_env(path):
    values = {}
    for line in Path(path).read_text().splitlines():
        key, sep, value = line.partition("=")
        if sep:
            values[key.strip()] = value.strip().strip("\"'")
    return values


def node_resources(root, action, model=None, *, force=False):
    if action == 'status':
        from .memory_usage import enrich
        return enrich(root, controller_request(root, 'status'))
    if action in ('load', 'unload'):
        from .resources import read_cluster, resolve_workload
        config = read_cluster(Path(root) / 'config/models.json')
        name, _ = resolve_workload(config, model)
        payload = {'model': name}
        if force:payload['force'] = True
        return controller_request(root, action, payload)
    raise SparkError('Unknown resource operation.')


def node_device():
    memory = {}
    for line in Path("/proc/meminfo").read_text().splitlines():
        name, _, rest = line.partition(":")
        if name in ("MemAvailable", "MemTotal", "SwapFree"):
            memory[name] = int(rest.split()[0]) * 1024
    disk = shutil.disk_usage(str(Path.home()))
    data = {"hostname": socket.gethostname(), "memory_bytes": memory,
            "disk_bytes": {"total": disk.total, "used": disk.used, "free": disk.free}}
    commands = {
        "gpu": ["nvidia-smi", "--query-gpu=name,temperature.gpu,memory.used,memory.total", "--format=csv,noheader,nounits"],
        "running_containers": ["sudo", "-n", "docker", "ps", "--format", "{{.Names}}"]}
    data["warnings"] = []
    for key, command in commands.items():
        try:
            result = subprocess.run(command, capture_output=True, text=True, timeout=15)
            if result.returncode == 0:
                data[key] = result.stdout.strip().splitlines()
            else:
                data["warnings"].append(key.replace("_", " ") + " query failed.")
        except (OSError, subprocess.TimeoutExpired):
            data["warnings"].append(key.replace("_", " ") + " query is unavailable.")
    return data
