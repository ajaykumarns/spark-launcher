"""Read-only installed harness parser checks. No model inference."""
import json
import subprocess
import uuid
from pathlib import Path
from spark_launcher.adapters import build_plan, child_env, prepare
from spark_launcher.catalog import fetch_catalog, load_config, state_dir, write_json

config = load_config()
catalog = fetch_catalog(config)
model = next(m for m in catalog["models"] if "nemotron" in m["id"])
results = {}
for harness in ("codex", "opencode"):
    plan = build_plan(config, catalog, model, harness, Path.cwd(), uuid.uuid4().hex)
    prepare(config, catalog, plan)
    args = [*plan["args"], "debug", "models"] if harness == "codex" else ["debug", "config"]
    response = subprocess.run([plan["binary"], *args], env=child_env(plan), capture_output=True, text=True, timeout=60)
    if response.returncode:
        raise SystemExit(f"{harness}: native parser failed (output kept private).")
    data = json.loads(response.stdout)
    if harness == "codex":
        limits = {m["slug"]: m["context_window"] for m in data["models"]}
    else:
        limits = {m["id"]: m["limit"]["context"] for m in data["provider"]["spark"]["models"].values()}
        assert data["model"] == "spark/" + model["id"].removeprefix(config["model_prefix"])
    expected = {m["id"]: m["context_tokens"] for m in catalog["models"]}
    assert all(limits[key] == value for key, value in expected.items()), harness
    results[harness] = {"native_parser": "passed", "contexts": expected}
write_json(state_dir() / "verification/native-parsers.json", results)
print(json.dumps(results, indent=2))
