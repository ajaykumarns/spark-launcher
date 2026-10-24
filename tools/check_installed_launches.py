"""Start each installed harness's help/version path. No inference."""
import json
import subprocess
from pathlib import Path
from spark_launcher.catalog import state_dir, write_json

spark = str(Path.home() / '.local/bin/spark')
commands = {
    'claude': [spark, 'claude', '--model', 'nemotron', '--', '--version'],
    'hermes': [spark, 'hermes', '--model', 'glm', '--', '--help'],
    'codex': [spark, 'codex', '--model', 'deepseek', '--', '--version'],
    'opencode': [str(Path.home() / '.local/bin/opencode-spark'), '--model', 'garuda/qwen3.8-flash-next', '--version'],
    'legacy-model-list': [str(Path.home() / '.local/bin/spark-models'), 'list', '--json'],
}
results = {}
for name, args in commands.items():
    result = subprocess.run(args, capture_output=True, text=True, timeout=60, stdin=subprocess.DEVNULL)
    results[name] = {'exit_code': result.returncode, 'passed': result.returncode == 0}
    if result.returncode:
        raise SystemExit(f'{name}: failed (exit {result.returncode}).')
write_json(state_dir() / 'verification/installed-launches.json', results)
print(json.dumps(results, indent=2))
