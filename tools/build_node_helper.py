"""Build a self-contained node zipapp with the terminal renderer included."""
import shutil
import importlib.util
import importlib.metadata
import tempfile
import zipapp
from pathlib import Path
root = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory() as temp:
    stage = Path(temp)
    shutil.copytree(root / "src/spark_launcher", stage / "spark_launcher", ignore=shutil.ignore_patterns("__pycache__"))
    for package in ('rich', 'pygments', 'markdown_it', 'mdurl'):
        spec = importlib.util.find_spec(package)
        if not spec or not spec.submodule_search_locations:
            raise SystemExit('Build with the project Python environment: uv run make node-helper')
        shutil.copytree(next(iter(spec.submodule_search_locations)), stage / package, ignore=shutil.ignore_patterns('__pycache__'))
    for name in ('rich', 'pygments', 'markdown-it-py', 'mdurl'):
        distribution = importlib.metadata.distribution(name)
        for entry in distribution.files or []:
            if entry.parts and entry.parts[0].endswith('.dist-info'):
                target = stage / str(entry)
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(distribution.locate_file(entry), target)
    (stage / "__main__.py").write_text('from spark_launcher.cli import main\nmain()\n')
    (root / "dist").mkdir(exist_ok=True)
    zipapp.create_archive(stage, root / "dist/sparky.pyz", interpreter="/usr/bin/env python3", compressed=True)
