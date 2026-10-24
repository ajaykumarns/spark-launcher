"""Build a stdlib-only Python zipapp for node controls (not harness launches)."""
import shutil
import tempfile
import zipapp
from pathlib import Path
root = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory() as temp:
    stage = Path(temp)
    shutil.copytree(root / "src/spark_launcher", stage / "spark_launcher", ignore=shutil.ignore_patterns("__pycache__"))
    (stage / "__main__.py").write_text('from spark_launcher.cli import main\nmain()\n')
    (root / "dist").mkdir(exist_ok=True)
    zipapp.create_archive(stage, root / "dist/spark-node.pyz", interpreter="/usr/bin/env python3")
