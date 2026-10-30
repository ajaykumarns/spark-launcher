import copy
import json
import os
import stat
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch

from spark_launcher.adapters import build_plan, check_passthrough, child_env, codex_catalog, launch, sync_hermes
from spark_launcher.catalog import SparkError, fetch_catalog, make_catalog, resolve, validate_config
from spark_launcher.control import remote_command, safe_host
from spark_launcher.gateway import OPERATIONS, inference_routes, mutable_provider, translated_provider
from spark_launcher.locking import file_lock


class LauncherTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.env = patch.dict(os.environ, {"SPARKY_STATE_DIR": str(self.root / "state"),
                                          "HERMES_HOME": str(self.root / "hermes")})
        self.env.start()
        self.c = validate_config({"version": 1, "gateway": "https://spark.example:8443",
                                  "default_model": "garuda/test"})
        self.c["binaries"] = {h: "/usr/bin/true" for h in ("claude", "hermes", "codex", "opencode")}
        self.wire = {"data": [{"id": "garuda/test", "context_length": 20000, "aliases": ["test"], "output_tokens":100},
                              {"id": "garuda/second", "context_length": 30000},
                              {"id": "garuda/router"}, {"id": "parakeet/en", "context_length": 500},
                              {"id": "garuda/spark-auto"}]}
        self.cat = make_catalog(self.c, self.wire)

    def tearDown(self):
        self.env.stop()
        self.temp.cleanup()

    def test_live_context_all_adapters(self):
        self.wire["data"][0]["context_length"] = 77000
        catalog = make_catalog(self.c, self.wire)
        model = resolve(catalog, "test")
        for harness in ("claude", "hermes", "codex", "opencode"):
            plan = build_plan(self.c, catalog, model, harness, self.root, "test")
            self.assertEqual(plan["model"]["context_tokens"], 77000)
            self.assertFalse(any("skip-permissions" in v or "dangerously" in v for v in plan["args"]))
        self.assertEqual(resolve(catalog, "spark-auto")["context_tokens"], 30000)
        self.assertEqual(len(catalog["models"]), 3)
        codex = build_plan(self.c, catalog, model, "codex", self.root, "test")
        self.assertIn("model_context_window=77000", codex["args"])
        self.assertIn('model_providers.spark.http_headers.X-Client-ID="sparky/codex"', codex["args"])
        self.assertIn('review_model="garuda/test"', codex["args"])
        opencode = build_plan(self.c, catalog, model, "opencode", self.root, "test")
        raw = json.loads(opencode["environment_overrides"]["OPENCODE_CONFIG_CONTENT"])
        self.assertEqual(raw["provider"]["spark"]["models"]["test"]["limit"]["context"], 77000)

    def test_discovery_and_explicit_offline(self):
        with patch("spark_launcher.catalog.request_json", return_value=self.wire):
            fresh = fetch_catalog(self.c)
        with patch("spark_launcher.catalog.request_json", side_effect=SparkError("offline")):
            with self.assertRaises(SparkError):
                fetch_catalog(self.c)
            self.assertEqual(fetch_catalog(self.c, True), fresh)
        other = {**self.c, "gateway": "https://other.example"}
        with self.assertRaises(SparkError):
            fetch_catalog(other, True)

    def test_alias_and_limits(self):
        catalog = copy.deepcopy(self.cat)
        catalog["models"][0]["aliases"] = ["same"]
        catalog["models"][1]["aliases"] = ["same"]
        with self.assertRaises(SparkError):
            resolve(catalog, "same")
        with self.assertRaises(SparkError):
            resolve(catalog, "missing")
        self.wire["data"][0]["output_tokens"] = 99999
        with self.assertRaises(SparkError):
            make_catalog(self.c, self.wire)

    def test_remote_gateway_validation(self):
        for url in ("http://remote.example", "https://user:pass@host", "https://host/v1", "https://host?token=x"):
            with self.assertRaises(SparkError):
                validate_config({"version": 1, "gateway": url})

    def test_environment_preserves_unrelated_values(self):
        plan = build_plan(self.c, self.cat, resolve(self.cat, "test"), "claude", self.root, "session")
        with patch.dict(os.environ, {"ANTHROPIC_API_KEY": "do-not-log", "SOME_PROJECT_SETTING": "keep"}):
            env = child_env(plan)
        self.assertNotIn("ANTHROPIC_API_KEY", env)
        self.assertEqual(env["SOME_PROJECT_SETTING"], "keep")
        self.assertEqual(env["ANTHROPIC_BASE_URL"], self.c["gateway"] + "/anthropic")
        self.assertEqual(env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"], "20000")

    def test_hermes_preserves_settings_comments_credentials(self):
        from ruamel.yaml import YAML
        root = self.root / "hermes"
        root.mkdir()
        path = root / "config.yaml"
        original = """# Keep my comment
model:
  provider: other
  default: other/model
mcp_servers:
  example: {command: example}
providers:
  custom: {api_key: keep-secret, request_timeout_seconds: 7}
  spark-gateway:
    custom_option: keep
    extra_headers: {X-Another: keep}
    models: {}
"""
        path.write_text(original)
        backup = sync_hermes(self.c, self.cat, "test")
        self.assertEqual(Path(backup).read_text(), original)
        self.assertEqual(stat.S_IMODE(Path(backup).stat().st_mode), 0o600)
        self.assertIn("# Keep my comment", path.read_text())
        data = YAML().load(path.read_text())
        self.assertEqual(data["model"]["provider"], "other")
        self.assertEqual(data["providers"]["custom"]["api_key"], "keep-secret")
        provider = data["providers"]["spark-gateway"]
        self.assertEqual(provider["custom_option"], "keep")
        self.assertEqual(provider["extra_headers"]["X-Another"], "keep")
        self.assertEqual(provider["models"]["garuda/test"]["context_length"], 20000)
        self.assertNotIn("X-Session-ID", provider["extra_headers"])
        self.assertIsNone(sync_hermes(self.c, self.cat, "again"))
        with file_lock(root / "config.yaml.spark-lock", "locked"):
            with self.assertRaises(SparkError):
                sync_hermes(self.c, self.cat, "locked")

    def test_hermes_fresh_config(self):
        sync_hermes(self.c, self.cat, "fresh")
        self.assertTrue((self.root / "hermes/config.yaml").exists())

    def test_codex_catalog_uses_installed_schema(self):
        plan = build_plan(self.c, self.cat, resolve(self.cat, "test"), "codex", self.root, "test")
        result = subprocess.CompletedProcess([], 0, json.dumps({"models": [{"slug": "cloud", "required_field": "keep"}]}), "")
        with patch("spark_launcher.adapters.subprocess.run", return_value=result):
            codex_catalog(plan, self.cat)
        models = json.loads(Path(plan["generated_files"][0]).read_text())["models"]
        self.assertEqual(models[0]["required_field"], "keep")
        self.assertTrue(all(m["upgrade"] is None for m in models))
        self.assertTrue(all(m["model_messages"] == {} for m in models))

    def test_codex_catalog_filters_unsupported_input_types(self):
        cases = [(["text", "image", "video"], ["text", "image"]),
                 (["text", "audio"], ["text", "audio"]),
                 (["video", "future-input"], ["text"]),
                 ([], ["text"]), (None, ["text"])]
        for inputs, expected in cases:
            with self.subTest(inputs=inputs):
                catalog = copy.deepcopy(self.cat)
                catalog["models"][0]["inputs"] = inputs
                before = copy.deepcopy(catalog)
                plan = build_plan(self.c, catalog, resolve(catalog, "test"), "codex", self.root, "test")
                result = subprocess.CompletedProcess([], 0, json.dumps({"models": [{"slug": "cloud"}]}), "")
                with patch("spark_launcher.adapters.subprocess.run", return_value=result):
                    codex_catalog(plan, catalog)
                models = json.loads(Path(plan["generated_files"][0]).read_text())["models"]
                self.assertEqual(models[0]["input_modalities"], expected)
                self.assertEqual(catalog, before)
                self.assertEqual(models[0]["context_window"], catalog["models"][0]["context_tokens"])

    def test_launch_fake_child_and_private_metadata(self):
        child = self.root / "child.py"
        child.write_text("#!/usr/bin/env python3\nimport os,sys,json\njson.dump({'args':sys.argv[1:],'model':os.environ['ANTHROPIC_MODEL']},open('child.json','w'))\nsys.exit(7)\n")
        child.chmod(0o700)
        self.c["binaries"]["claude"] = str(child)
        plan = build_plan(self.c, self.cat, resolve(self.cat, "test"), "claude", self.root, "test")
        self.assertEqual(launch(self.c, self.cat, plan, ["--continue"]), 7)
        actual = json.loads((self.root / "child.json").read_text())
        self.assertEqual(actual["model"], "garuda/test")
        self.assertIn("--continue", actual["args"])
        log = self.root / "state/launches/test.json"
        self.assertEqual(json.loads(log.read_text())["exit_code"], 7)
        self.assertNotIn("ANTHROPIC", log.read_text())
        self.assertEqual(stat.S_IMODE(log.stat().st_mode), 0o600)

    def test_passthrough_prevents_route_drift(self):
        for args in (["--model", "bad"], ["--model=bad"], ["-m", "bad"],
                     ["-c", 'model_provider="cloud"'], ["--config=model_context_window=1"], ["--oss"]):
            with self.assertRaises(SparkError):
                check_passthrough("codex", args)
        check_passthrough("codex", ["-c", 'approval_policy="on-request"', "exec", "hello"])
        check_passthrough("hermes", ["--resume", "my-session"])

    def test_remote_command_shell_safety(self):
        config = {**self.c, "admin_ssh_host": "spark01-admin", "remote_root": "/home/test/a ' b",
                  "manager_url": "http://127.0.0.1:8000"}
        args = remote_command(config, "load", "test")
        import shlex
        words = shlex.split(args[-1].split(" node ", 1)[1])
        self.assertEqual(words[2], config["remote_root"])
        self.assertNotIn("Authorization", args[-1])
        with self.assertRaises(SparkError):
            safe_host("-oProxyCommand=evil")

    def test_gateway_change_is_narrow_and_idempotent(self):
        text = """# Preserve all these settings
location ~ ^/v1/(chat/completions|completions|responses|audio/transcriptions)$ {
  auth_basic off;
}
location ~ ^/v1/(models|chat/completions|completions|responses|audio/transcriptions)$ {
  proxy_pass http://core;
}
location /api/ { auth_basic restricted; }
"""
        changed = inference_routes(text)
        self.assertEqual(inference_routes(changed), changed)
        self.assertIn("location /api/ { auth_basic restricted; }", changed)
        self.assertNotIn("anthropic/.*", changed)
        with self.assertRaises(SparkError):
            inference_routes("unrecognized layout")

    def test_bifrost_flags_preserve_all_existing_operations(self):
        before = {"name": "garuda", "keys": [{"value": "must-not-copy"}],
                  "network_config": {"timeout": 9}, "custom_provider_config": {"base_provider_type": "openai"}}
        after = translated_provider(before)
        flags = after["custom_provider_config"]["allowed_requests"]
        self.assertEqual(len(flags), len(OPERATIONS))
        self.assertFalse(flags["responses"])
        self.assertTrue(flags["transcription"])
        self.assertNotIn("keys", mutable_provider(after))
        self.assertNotIn("allowed_requests", before["custom_provider_config"])
        before["custom_provider_config"]["allowed_requests"] = {"embedding": False, "future_option": True}
        flags = translated_provider(before)["custom_provider_config"]["allowed_requests"]
        self.assertFalse(flags["embedding"])
        self.assertTrue(flags["future_option"])


    def test_http_discovery_against_real_socket(self):
        wire = self.wire
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps(wire).encode())
            def log_message(self, *args):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            config = {**self.c, "gateway": f"http://127.0.0.1:{server.server_port}"}
            self.assertEqual(len(fetch_catalog(config)["models"]), 3)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    unittest.main()
