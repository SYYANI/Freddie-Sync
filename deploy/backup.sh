#!/bin/sh
set -eu
backup_path="/var/backups/freddie-sync/backup-$(date -u +%Y%m%dT%H%M%SZ)"
/opt/freddie-sync/current/freddie-sync backup --out "$backup_path"
python3 - <<'PY'
from pathlib import Path
import re
import shutil

root = Path('/var/backups/freddie-sync')
backups = sorted(p for p in root.iterdir()
                 if re.fullmatch(r'backup-\d{8}T\d{6}Z', p.name)
                 and p.is_dir() and not p.is_symlink())
for path in backups[:-7]:
    shutil.rmtree(path)
PY
