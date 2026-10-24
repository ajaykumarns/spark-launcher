"""Explicit device operations. Credentials are read only on the head node."""
from __future__ import annotations

import json
import re
import shlex
import socket
import shutil
import subprocess
from pathlib import Path
from urllib.parse import quote, urlsplit

from .catalog import SparkError, request_json


def safe_host(host):
    if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.@-]*", host or ""):
        raise SparkError("Set an existing SSH host alias in the site file.")
    return host


def remote_command(config, action, model=None):
    host = safe_host(config.get("admin_ssh_host"))
    root = config.get("remote_root", "")
    if not Path(root).is_absolute():
        raise SparkError("Set an absolute remote_root in the site file.")
    args = [action, "--root", root]
    if model:
        args.extend(["--model", model])
    return ["ssh", host, "~/.local/bin/sparky node " + shlex.join(args)]


def remote(config, action, model=None):
    return subprocess.call(remote_command(config, action, model))


def admin_ssh(config, target):
    host = safe_host(config.get("admin_ssh_host"))
    if target == "spark01":
        return subprocess.call(["ssh", host])
    if target == "spark02":
        worker = safe_host(config.get("worker_ssh_host"))
        return subprocess.call(["ssh", "-t", host, "ssh -t " + shlex.quote(worker)])
    raise SparkError("Choose spark01 or spark02.")


def devices(config):
    host, worker = safe_host(config.get("admin_ssh_host")), safe_host(config.get("worker_ssh_host"))
    for label, command in (("spark01", "~/.local/bin/sparky node device"),
                           ("spark02", "ssh " + shlex.quote(worker) + " " + shlex.quote("~/.local/bin/sparky node device"))):
        print(label, flush=True)
        code = subprocess.call(["ssh", host, command])
        if code:
            return code
    return 0


def read_env(path):
    values = {}
    for line in Path(path).read_text().splitlines():
        key, sep, value = line.partition("=")
        if sep:
            values[key.strip()] = value.strip().strip("\"'")
    return values


def node_resources(root, action, model=None):
    from .catalog import request_json
    from .resources import read_cluster
    port = read_cluster(Path(root)/"config/models.json")["controller_port"]
    base = f"http://127.0.0.1:{port}/control/"
    if action == "status":
        return request_json(base + "status", timeout=60)
    if action in ("load", "unload"):
        return request_json(base + action, payload={"model": model}, timeout=7200)
    raise SparkError("Unknown resource operation.")


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
    for key, command in commands.items():
        try:
            result = subprocess.run(command, capture_output=True, text=True, timeout=15)
            if result.returncode == 0:
                data[key] = result.stdout.strip().splitlines()
        except (OSError, subprocess.TimeoutExpired):
            pass
    return data
