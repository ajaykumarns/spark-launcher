"""Run installed clients against synthetic local HTTP replies. No model load."""
from __future__ import annotations
import copy
import json
import subprocess
import tempfile
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit

from spark_launcher.adapters import build_plan, child_env, prepare
from spark_launcher.catalog import fetch_catalog, load_config, state_dir, write_json

config = load_config()
catalog = fetch_catalog(config)
model = next(m for m in catalog["models"] if "nemotron" in m["id"])
received = []

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
        received.append({"path": self.path, "model": body.get("model"), "stream": body.get("stream"),
                         "header_names": list(self.headers.keys()), "client": self.headers.get("X-Client-ID"), "session": self.headers.get("X-Session-ID")})
        path = urlsplit(self.path).path
        if path.endswith("count_tokens"):
            return self.reply({"input_tokens": 12})
        if path.endswith("messages"):
            message = {"id": "msg_spark_mock", "type": "message", "role": "assistant", "model": body["model"],
                       "content": [{"type": "text", "text": "SPARK_MOCK_OK"}], "stop_reason": "end_turn",
                       "stop_sequence": None, "usage": {"input_tokens": 12, "output_tokens": 5}}
            if not body.get("stream"):
                return self.reply(message)
            self.start_stream()
            self.event("message_start", {"type": "message_start", "message": {**message, "content": [], "stop_reason": None}})
            self.event("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}})
            self.event("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "SPARK_MOCK_OK"}})
            self.event("content_block_stop", {"type": "content_block_stop", "index": 0})
            self.event("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 5}})
            self.event("message_stop", {"type": "message_stop"})
        elif path.endswith("responses"):
            output = {"id": "msg_mock", "type": "message", "role": "assistant", "status": "completed",
                      "content": [{"type": "output_text", "text": "SPARK_MOCK_OK", "annotations": []}]}
            response = {"id": "resp_mock", "object": "response", "created_at": int(time.time()),
                        "status": "completed", "model": body["model"], "output": [output],
                        "usage": {"input_tokens": 12, "output_tokens": 5, "total_tokens": 17}}
            if not body.get("stream"):
                return self.reply(response)
            self.start_stream()
            self.event("response.created", {"type": "response.created", "response": {**response, "status": "in_progress", "output": []}})
            self.event("response.output_item.added", {"type": "response.output_item.added", "output_index": 0, "item": {**output, "status": "in_progress", "content": []}})
            self.event("response.content_part.added", {"type": "response.content_part.added", "item_id": "msg_mock", "output_index": 0, "content_index": 0, "part": {"type": "output_text", "text": "", "annotations": []}})
            self.event("response.output_text.delta", {"type": "response.output_text.delta", "item_id": "msg_mock", "output_index": 0, "content_index": 0, "delta": "SPARK_MOCK_OK"})
            self.event("response.output_text.done", {"type": "response.output_text.done", "item_id": "msg_mock", "output_index": 0, "content_index": 0, "text": "SPARK_MOCK_OK"})
            self.event("response.output_item.done", {"type": "response.output_item.done", "output_index": 0, "item": output})
            self.event("response.completed", {"type": "response.completed", "response": response})
        else:
            self.reply({"error": "unexpected mock path"}, 400)

    def reply(self, value, code=200):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(value).encode())

    def start_stream(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()

    def event(self, name, value):
        self.wfile.write(("event: " + name + "\ndata: " + json.dumps(value) + "\n\n").encode())
        self.wfile.flush()

server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
results = {}
try:
    with tempfile.TemporaryDirectory(prefix="spark-mock-") as temp:
        test_config = copy.deepcopy(config)
        test_config["gateway"] = f"http://127.0.0.1:{server.server_port}"
        for harness in ("claude", "codex"):
            plan = build_plan(test_config, catalog, model, harness, temp, uuid.uuid4().hex)
            prepare(test_config, catalog, plan)
            env = child_env(plan)
            env.update(CLAUDE_CONFIG_DIR=str(Path(temp) / "claude"),
                       CODEX_HOME=str(Path(temp) / "codex"),
                       CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1", CLAUDE_CODE_SIMPLE="1",
                       DISABLE_AUTOUPDATER="1", DISABLE_ERROR_REPORTING="1", DISABLE_TELEMETRY="1")
            Path(env["CODEX_HOME"]).mkdir(exist_ok=True)
            extra = ["--print", "--no-session-persistence", "--tools", "", "--", "Reply SPARK_MOCK_OK"] if harness == "claude" else [
                "exec", "--skip-git-repo-check", "--ephemeral", "--sandbox", "read-only", "--json", "Reply SPARK_MOCK_OK"]
            result = subprocess.run([plan["binary"], *plan["args"], *extra], cwd=temp,
                                    env=env, capture_output=True, text=True, timeout=90)
            calls = [r for r in received if r["client"] == "sparky/" + harness]
            success = result.returncode == 0 and "SPARK_MOCK_OK" in result.stdout
            results[harness] = {"passed": success, "exit_code": result.returncode, "requests": calls, "all_requests": list(received)}
            if not success or not calls:
                # This is a fully isolated mock run, but keep raw client output private.
                write_json(state_dir() / f"verification/{harness}-mock-private.json",
                           {"stdout": result.stdout, "stderr": result.stderr})
            assert success and calls and all(c["model"] == model["id"] and c["session"] == plan["session"] for c in calls), harness
finally:
    server.shutdown()
    server.server_close()
    thread.join()
    write_json(state_dir() / "verification/mock-harnesses.json", results)
print(json.dumps(results, indent=2))
