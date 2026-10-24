"""Install compatibility wrappers after the main launcher is installed."""
import os
import shutil
import subprocess
from datetime import datetime, timezone
from pathlib import Path

home = Path.home()
bin_dir = home / ".local/bin"
spark = bin_dir / "spark"
subprocess.run([str(spark), "--version"], check=True)
backup = home / ".local/state/spark/backups" / ("legacy-" + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ"))
backup.mkdir(mode=0o700, parents=True)
for name, command in (("spark-models", 'exec "$HOME/.local/bin/spark" "$@"'),
                      ("opencode-spark", 'exec "$HOME/.local/bin/spark" legacy-opencode "$@"')):
    path = bin_dir / name
    if path.exists():
        shutil.copyfile(path, backup / name)
        (backup / name).chmod(0o600)
    path.write_text("#!/bin/sh\n" + command + "\n")
    path.chmod(0o755)
print("Compatibility wrappers installed. Backup:", backup)
