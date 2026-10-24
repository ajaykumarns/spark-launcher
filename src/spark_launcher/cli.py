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
from .control import admin_ssh, devices, node_device, node_manager, remote


def output(value):
    print(json.dumps(value, indent=2, ensure_ascii=False))


def parser():
    p = argparse.ArgumentParser(description="One catalog for four harnesses and existing Spark services.")
    p.add_argument("--version", action="version", version="spark " + __version__)
    sub = p.add_subparsers(dest="command")
    init = sub.add_parser("init", help="Create a private site configuration")
    init.add_argument("--gateway", required=True, help="HTTPS origin, without /v1")
    init.add_argument("--prefix", default="garuda/")
    init.add_argument("--admin-host", default="")
    init.add_argument("--worker-host", default="spark02")
    init.add_argument("--remote-root", default="")
    init.add_argument("--manager-url", default="")
    init.add_argument("--admin-url", default="")
    init.add_argument("--import-opencode", help="Import labels and client budgets, never keys or context")
    for harness in HARNESSES:
        child = sub.add_parser(harness, help="Launch " + harness)
        child.add_argument("--model", "-m")
        child.add_argument("--project", default=".")
        child.add_argument("--offline", action="store_true")
        group = child.add_mutually_exclusive_group()
        group.add_argument("--dry-run", action="store_true", help="Show the plan without changing harness settings")
        group.add_argument("--prepare", action="store_true", help="Generate settings without inference")
    for cmd in ("models", "list"):
        child = sub.add_parser(cmd, help="Show live models and context limits")
        child.add_argument("--offline", action="store_true")
        child.add_argument("--json", action="store_true")
    sub.add_parser("sync", help="Refresh Hermes's owned Spark provider")
    child = sub.add_parser("doctor", help="Check discovery and installed commands; no inference")
    child.add_argument("harness", nargs="?", choices=HARNESSES)
    for cmd in ("status", "devices", "logs", "ui"):
        sub.add_parser(cmd)
    for cmd in ("load", "unload"):
        sub.add_parser(cmd).add_argument("model")
    admin = sub.add_parser("admin").add_subparsers(dest="action", required=True)
    for cmd in ("load", "unload"):
        admin.add_parser(cmd).add_argument("model")
    admin.add_parser("ssh").add_argument("target", nargs="?", default="spark01", choices=("spark01", "spark02"))
    admin.add_parser("enable-harnesses", help="Set up native Bifrost protocol routes")
    node = sub.add_parser("node", help="Internal node helper; no site configuration needed").add_subparsers(dest="action", required=True)
    node.add_parser("device")
    for cmd in ("status", "load", "unload", "gateway"):
        child = node.add_parser(cmd)
        child.add_argument("--root", required=True)
        if cmd != "gateway":
            child.add_argument("--manager", required=True)
        if cmd in ("load", "unload"):
            child.add_argument("--model", required=True)
    return p


def initialize(args):
    if config_path().exists():
        raise SparkError("Site configuration already exists. Edit it instead of overwriting it.")
    config = {"version": 1, "gateway": args.gateway, "model_prefix": args.prefix,
              "default_model": args.prefix + "spark-auto", "timeout_seconds": 7200,
              "compact_percent": 95, "admin_ssh_host": args.admin_host, "worker_ssh_host": args.worker_host,
              "remote_root": args.remote_root, "manager_url": args.manager_url, "admin_url": args.admin_url,
              "models": import_models(args.import_opencode, args.prefix) if args.import_opencode else {}}
    write_json(config_path(), validate_config(config))
    print("Created", config_path())


def menu(config):
    if not sys.stdin.isatty():
        raise SparkError("Use spark HARNESS --model NAME, or run spark in a terminal.")
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
    if argv and argv[0] == "legacy-opencode":
        native, selected = [], None
        values = iter(argv[1:])
        for value in values:
            if value in ("--model", "-m"):
                selected = next(values, None)
                if not selected:
                    raise SparkError("--model needs a value.")
            elif value.startswith(("--model=", "-m=")):
                selected = value.split("=", 1)[1]
            else:
                native.append(value)
        if selected and selected.startswith("spark/"):
            selected = selected.split("/", 1)[1]
        argv = ["opencode", *(["--model", selected] if selected else []), "--", *native]
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
        elif args.action == "device":
            output(node_device())
        else:
            output(node_manager(args.root, args.manager, args.action, getattr(args, "model", None)))
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
    if args.command in ("models", "list", "sync", "doctor"):
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
        model = resolve(fetch_catalog(config), args.model)
        nodes = model.get("nodes", [])
        if len(nodes) > 1:
            print("The model manager controls conflicting loads on " + ", ".join(nodes) + ".", file=sys.stderr)
        return remote(config, args.action if args.command == "admin" else args.command,
                      model["id"].removeprefix(config["model_prefix"]))
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


def main():
    try:
        code = run()
    except KeyboardInterrupt:
        code = 130
    except (SparkError, OSError, ValueError, KeyError, IndexError, subprocess.TimeoutExpired) as error:
        # Avoid raw subprocess bodies and config object values in routine errors.
        print("spark:", str(error), file=sys.stderr)
        code = 1
    raise SystemExit(code or 0)
