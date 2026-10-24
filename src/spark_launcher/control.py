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
    if action != "gateway":
        args.extend(["--manager", config.get("manager_url", "")])
    if model:
        args.extend(["--model", model])
    return ["ssh", host, "~/.local/bin/spark node " + shlex.join(args)]


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
    for label, command in (("spark01", "~/.local/bin/spark node device"),
                           ("spark02", "ssh " + shlex.quote(worker) + " " + shlex.quote("~/.local/bin/spark node device"))):
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


def node_manager(root, manager, action, model=None):
    if not Path(root).is_absolute():
        raise SparkError("An absolute remote root is required.")
    u = urlsplit(manager)
    # Do not send the node credential to an arbitrary remote URL from CLI input.
    addresses = {"127.0.0.1", "localhost", "::1"}
    for args in (["hostname", "-I"], ["tailscale", "ip", "-4"]):
        try:
            result = subprocess.run(args, capture_output=True, text=True, timeout=5)
            addresses.update(result.stdout.split())
        except (OSError, subprocess.TimeoutExpired):
            pass
    if (u.scheme not in ("http", "https") or u.hostname not in addresses or u.username or u.password
            or u.query or u.fragment or u.path not in ("", "/")):
        raise SparkError("Manager URL must point to this node.")
    key = read_env(Path(root) / "vllm/api.env").get("VLLM_API_KEY")
    if not key:
        raise SparkError("Node manager credential is missing.")
    path, method = "/running", "GET"
    if action != "status":
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", model or ""):
            raise SparkError("Invalid model alias.")
        if action == "load":
            path = "/upstream/" + quote(model, safe="") + "/health"
        else:
            method, path = "POST", "/api/models/unload/" + quote(model, safe="")
    response = request_json(manager.rstrip("/") + path, method=method,
                            headers={"Authorization": "Bearer " + key}, timeout=7200, expect_json=action == "status")
    if action == "status":
        fields = ("model", "name", "state", "ttl", "description")
        return {"running": [{key: row[key] for key in fields if key in row}
                            for row in response.get("running", [])]}
    return {"action": action, "model": model, "status": "complete"}


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
