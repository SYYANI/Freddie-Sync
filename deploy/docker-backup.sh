#!/bin/sh
# Run on the Docker host as root, for example from cron. Python 3 is used only
# on the host to retain the latest seven successful snapshots.
set -eu
cd "$(dirname "$0")/.."
backup_path="/state/backups/backup-$(date -u +%Y%m%dT%H%M%SZ)"
docker compose exec -T sync /usr/local/bin/freddie-sync backup --data /state/data --out "$backup_path"
python3 - <<'PY'
from pathlib import Path
import re
import shutil

root = Path('var/backups')
backups = sorted(p for p in root.iterdir()
                 if re.fullmatch(r'backup-\d{8}T\d{6}Z', p.name)
                 and p.is_dir() and not p.is_symlink())
for path in backups[:-7]:
    shutil.rmtree(path)
PY
