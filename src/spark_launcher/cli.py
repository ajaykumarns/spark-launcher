"""Single command for native harness launches and existing Spark controls."""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import uuid
import webbrowser
from pathlib import Path

from . import __version__
from .adapters import HARNESSES, build_plan, check_passthrough, executable, launch, prepare, sync_hermes
from .catalog import (SparkError, config_path, fetch_catalog, import_models, load_config,
                      resolve, state_dir, validate_config, write_json)
from .control import admin_ssh, devices, node_device, node_resources, remote


def output(value):
    print(json.dumps(value, indent=2, ensure_ascii=False))


def parser():
    p = argparse.ArgumentParser(prog="sparky", description="One catalog for four harnesses and existing Spark services.")
    p.add_argument("--version", action="version", version="sparky " + __version__)
    sub = p.add_subparsers(dest="command")
    init = sub.add_parser("init", help="Create a private site configuration")
    init.add_argument("--gateway", required=True, help="HTTPS origin, without /v1")
    init.add_argument("--prefix", default="garuda/")
    init.add_argument("--admin-host", default="")
    init.add_argument("--worker-host", default="spark02")
    init.add_argument("--remote-root", default="")
    init.add_argument("--admin-url", default="")
    for harness in HARNESSES:
        child = sub.add_parser(harness, help="Launch " + harness)
        child.add_argument("--model", "-m")
        child.add_argument("--project", default=".")
        child.add_argument("--offline", action="store_true")
        group = child.add_mutually_exclusive_group()
        group.add_argument("--dry-run", action="store_true", help="Show the plan without changing harness settings")
        group.add_argument("--prepare", action="store_true", help="Generate settings without inference")
    for cmd in ("models",):
        child = sub.add_parser(cmd, help="Show live models and context limits")
        child.add_argument("--offline", action="store_true")
        child.add_argument("--json", action="store_true")
        child.add_argument("--installed", action="store_true")
    sub.add_parser("sync", help="Refresh Hermes's owned Spark provider")
    child = sub.add_parser("doctor", help="Check discovery and installed commands; no inference")
    child.add_argument("harness", nargs="?", choices=HARNESSES)
    for cmd in ("status", "devices", "logs", "ui"):
        sub.add_parser(cmd)
    for cmd in ("load", "unload"):
        sub.add_parser(cmd).add_argument("model")
    plan = sub.add_parser("plan", help="Show resource actions without changing services")
    plan.add_argument("action", choices=("load", "unload"))
    plan.add_argument("model")
    plan.add_argument("--on", choices=("spark01", "spark02", "both"))
    config = sub.add_parser("config", help="Read or edit the authoritative Spark 1 configuration")
    config_sub = config.add_subparsers(dest="config_action", required=True)
    config_sub.add_parser("show")
    config_sub.add_parser("validate")
    placement = config_sub.add_parser("placement")
    placement.add_argument("model")
    placement.add_argument("on", choices=("spark01", "spark02", "both"))
    workflow = sub.add_parser("run", help="Submit a ComfyUI API workflow")
    workflow.add_argument("engine", choices=("comfyui",))
    workflow.add_argument("workflow")
    workflow.add_argument("--on", choices=("spark01", "spark02"))
    admin = sub.add_parser("admin").add_subparsers(dest="action", required=True)
    for cmd in ("load", "unload"):
        admin.add_parser(cmd).add_argument("model")
    admin.add_parser("ssh").add_argument("target", nargs="?", default="spark01", choices=("spark01", "spark02"))
    admin.add_parser("enable-harnesses", help="Set up native Bifrost protocol routes")
    node = sub.add_parser("node", help="Internal node helper; no site configuration needed").add_subparsers(dest="action", required=True)
    node.add_parser("device")
    install = node.add_parser("install")
    install.add_argument("--dir", required=True)
    drain = node.add_parser("drain-comfy")
    drain.add_argument("--file", required=True)
    render = node.add_parser("render")
    render.add_argument("--payload", required=True)
    rpc = node.add_parser("rpc")
    rpc.add_argument("operation", choices=("plan", "config", "placement", "run", "validate"))
    rpc.add_argument("--payload", default="{}")
    probe = node.add_parser("probe")
    probe.add_argument("--file", required=True)
    controller = node.add_parser("controller")
    controller.add_argument("--file", required=True)
    lifecycle = node.add_parser("lifecycle")
    lifecycle.add_argument("operation", choices=("run", "stop"))
    lifecycle.add_argument("model")
    lifecycle.add_argument("--file", required=True)
    for cmd in ("status", "load", "unload", "gateway"):
        child = node.add_parser(cmd)
        child.add_argument("--root", required=True)
        if cmd in ("load", "unload"):
            child.add_argument("--model", required=True)
    return p


def initialize(args):
    if config_path().exists():
        raise SparkError("Site configuration already exists. Edit it instead of overwriting it.")
    config = {"version": 1, "gateway": args.gateway, "model_prefix": args.prefix,
              "default_model": args.prefix + "spark-auto", "timeout_seconds": 7200,
              "compact_percent": 95, "admin_ssh_host": args.admin_host, "worker_ssh_host": args.worker_host,
              "remote_root": args.remote_root, "admin_url": args.admin_url}
    write_json(config_path(), validate_config(config))
    print("Created", config_path())


def menu(config):
    if not sys.stdin.isatty():
        raise SparkError("Use sparky HARNESS --model NAME, or run sparky in a terminal.")
    catalog = fetch_catalog(config)
    print("Harness: 1 Claude Code  2 Hermes  3 Codex  4 OpenCode")
    choice = int(input("Choose [1]: ") or "1")
    if not 1 <= choice <= len(HARNESSES):
        raise SparkError("Invalid harness choice.")
    harness = HARNESSES[choice - 1]
    for index, model in enumerate(catalog["models"], 1):
        print(f'{index}  {model.get("name", model["id"])}  ({model["context_tokens"]} tokens)')
    choice = int(input("Model: "))
    if not 1 <= choice <= len(catalog["models"]):
        raise SparkError("Invalid model choice.")
    model = catalog["models"][choice - 1]
    project = Path(input(f"Project [{Path.cwd()}]: ") or Path.cwd()).expanduser().resolve()
    if not project.is_dir():
        raise SparkError("Project must be a directory.")
    plan = build_plan(config, catalog, model, harness, project, uuid.uuid4().hex)
    return launch(config, catalog, plan, [])


def run(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    if argv and argv[0] == "launch":
        argv = argv[1:]
        if not argv or argv[0] not in HARNESSES:
            raise SparkError("launch needs claude, hermes, codex, or opencode.")
    extra = []
    if "--" in argv and argv[0] in HARNESSES:
        index = argv.index("--")
        argv, extra = argv[:index], argv[index + 1:]
    args = parser().parse_args(argv)
    if args.command == "init":
        initialize(args)
        return 0
    if args.command == "node":
        if args.action == "gateway":
            from .gateway import enable_gateway
            output(enable_gateway(args.root))
        elif args.action == "install":
            from .deployment import install_head
            output(install_head(args.dir))
        elif args.action == "drain-comfy":
            from .deployment import drain_comfy
            drain_comfy(args.file)
        elif args.action == "render":
            from .parameters import render_parameters
            output(render_parameters(json.loads(sys.stdin.read() if args.payload == "-" else args.payload)))
        elif args.action == "controller":
            from .controller_server import serve
            serve(args.file)
        elif args.action == "probe":
            from .resource_runtime import probe
            output(probe(json.loads(Path(args.file).read_text())))
        elif args.action == "lifecycle":
            from .lifecycle import run_model
            run_model(args.file, args.operation, args.model)
        elif args.action == "rpc":
            from .catalog import request_json
            operation = args.operation
            payload = json.loads(sys.stdin.read() if args.payload == "-" else args.payload)
            if operation in ("config", "validate"):
                output(request_json("http://127.0.0.1:8020/control/config"))
            else:
                output(request_json("http://127.0.0.1:8020/control/" + operation, payload=payload, timeout=7200))
        elif args.action == "device":
            output(node_device())
        else:
            output(node_resources(args.root, args.action, getattr(args, "model", None)))
        return 0
    config = load_config()
    if not args.command:
        return menu(config)
    if args.command in HARNESSES:
        check_passthrough(args.command, extra)
        project = Path(args.project).expanduser().resolve()
        if not project.is_dir():
            raise SparkError("Project must be a directory.")
        catalog = fetch_catalog(config, args.offline)
        model = resolve(catalog, args.model or config["default_model"])
        plan = build_plan(config, catalog, model, args.command, project, uuid.uuid4().hex)
        if args.dry_run or args.prepare:
            if args.prepare:
                prepare(config, catalog, plan)
            plan["args"].extend(extra)
            output(plan)
            return 0
        return launch(config, catalog, plan, extra)
    if args.command == "plan":
        return resource_rpc(config, "plan", {"action": args.action, "model": args.model, "on": args.on})
    if args.command == "config":
        if args.config_action == "placement":
            return resource_rpc(config, "placement", {"model": args.model, "on": args.on})
        return resource_rpc(config, "config" if args.config_action == "show" else "validate", {})
    if args.command == "run":
        document = json.loads(Path(args.workflow).expanduser().read_text())
        # Workflow contents go over stdin, never process arguments or logs.
        return resource_rpc(config, "run", {"workflow": document, "on": args.on}, stdin_payload=True)
    if args.command == "models" and args.installed:
        return remote(config, "status")
    if args.command in ("models", "sync", "doctor"):
        catalog = fetch_catalog(config, getattr(args, "offline", False))
        if args.command == "sync":
            backup = sync_hermes(config, catalog, uuid.uuid4().hex)
            print("Hermes Spark provider updated.")
            if backup:
                print("Backup:", backup)
        elif args.command == "doctor":
            print(f'Gateway reachable. {len(catalog["models"])} models have context metadata.')
            for harness in ([args.harness] if args.harness else HARNESSES):
                print(harness + ": " + executable(config, harness))
            print("No inference was run.")
        elif args.json:
            output(catalog)
        else:
            print(f'{"MODEL":42} {"CONTEXT":>10} {"OUTPUT":>8} ALIASES')
            for model in catalog["models"]:
                print(f'{model["id"]:42} {model["context_tokens"]:10d} {model.get("output_tokens", 0):8d} {",".join(model.get("aliases", []))}')
        return 0
    if args.command == "status":
        return remote(config, "status")
    if args.command == "devices":
        return devices(config)
    if args.command == "admin" and args.action == "ssh":
        return admin_ssh(config, args.target)
    if args.command == "admin" and args.action == "enable-harnesses":
        return remote(config, "gateway")
    if args.command in ("load", "unload") or args.command == "admin":
        return remote(config, args.action if args.command == "admin" else args.command, args.model)
    if args.command == "logs":
        paths = (state_dir() / "launches").glob("*.json")
        records = sorted((json.loads(p.read_text()) for p in paths), key=lambda r: r["started_at"], reverse=True)
        output(records[:20])
    elif args.command == "ui":
        url = config.get("admin_url")
        if not url:
            raise SparkError("Set admin_url in the site file.")
        if not webbrowser.open(url):
            print(url)
    return 0


def resource_rpc(config, operation, payload, stdin_payload=False):
    import shlex
    from .control import safe_host
    command = "~/.local/bin/sparky node rpc " + shlex.quote(operation)
    if stdin_payload:
        command += " --payload -"
        return subprocess.run(["ssh", safe_host(config["admin_ssh_host"]), command], input=json.dumps(payload), text=True).returncode
    command += " --payload " + shlex.quote(json.dumps(payload))
    return subprocess.call(["ssh", safe_host(config["admin_ssh_host"]), command])


def main():
    try:
        code = run()
    except KeyboardInterrupt:
        code = 130
    except (SparkError, OSError, ValueError, KeyError, IndexError, subprocess.TimeoutExpired) as error:
        # Avoid raw subprocess bodies and config object values in routine errors.
        print("sparky:", str(error), file=sys.stderr)
        code = 1
    raise SystemExit(code or 0)
