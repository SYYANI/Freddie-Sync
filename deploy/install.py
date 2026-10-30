#!/usr/bin/env python3
"""Install a staged, locally built release on the authorized SSH host.

Touches only Freddie paths and adds one include to the existing HTTPS server
whose server_name is SERVER_NAME.
On failure, restore changed configuration and the previous running release.
"""
import hashlib
import os
from pathlib import Path
import pwd
import re
import shutil
import subprocess
import sys
import time
import urllib.request


HOSTNAME = re.compile(r'(?=.{1,253}$)([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?')


def run(*args):
    subprocess.run(args, check=True)


def main():
    if os.geteuid() != 0:
        raise RuntimeError('SSH installation requires root')
    if len(sys.argv) != 3:
        raise RuntimeError('usage: install.py STAGE_DIR SERVER_NAME')
    stage = Path(sys.argv[1]).resolve()
    server_name = sys.argv[2]
    if not HOSTNAME.fullmatch(server_name):
        raise RuntimeError('SERVER_NAME must be a bare hostname such as sync.example.com')
    binary = stage / 'freddie-sync'
    release_hash = hashlib.sha256(binary.read_bytes()).hexdigest()
    root = Path('/opt/freddie-sync')
    release = root / 'releases' / release_hash
    current = root / 'current'
    if current.exists() and not current.is_symlink():
        raise RuntimeError('Refusing to replace a non-symlink current path')
    previous = current.resolve() if current.is_symlink() else None
    nginx = Path('/etc/nginx/nginx.conf')
    original = nginx.read_bytes()
    include = 'include /etc/nginx/snippets/freddie-sync-location.conf;'
    text = original.decode()
    # The HTTPS server for SERVER_NAME must already exist; only one include is added to it.
    markers = list(re.finditer(r'^([ \t]*)server_name[ \t]+' + re.escape(server_name) + r'[ \t]*;[^\n]*$', text, re.M))
    if include not in text:
        if len(markers) != 1:
            raise RuntimeError(f'Expected exactly one nginx server with "server_name {server_name};" in {nginx}; no configuration changed')
        marker = markers[0]
        text = text[:marker.end()] + '\n' + marker.group(1) + include + text[marker.end():]
    location = (stage / 'deploy/nginx-location.conf').read_text().replace('@SERVER_NAME@', server_name)
    rendered = {
        Path('/etc/nginx/snippets/freddie-sync-location.conf'): location.encode(),
        Path('/etc/freddie-sync/serve.env'): f'FREDDIE_SYNC_HOST={server_name}\n'.encode(),
    }
    targets = {
        Path('/etc/nginx/conf.d/freddie-sync-limits.conf'): stage / 'deploy/nginx-limits.conf',
    }
    for name in ['freddie-sync.service', 'freddie-sync-backup.service', 'freddie-sync-backup.timer']:
        targets[Path('/etc/systemd/system') / name] = stage / 'deploy' / name
    saved = {path: path.read_bytes() if path.exists() else None for path in [*targets, *rendered]}
    backup_timer_was_enabled = subprocess.run(
        ['systemctl', 'is-enabled', '--quiet', 'freddie-sync-backup.timer'],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
    try:
        try:
            user = pwd.getpwnam('freddie-sync')
        except KeyError:
            run('useradd', '--system', '--home-dir', '/var/lib/freddie-sync',
                '--shell', '/usr/sbin/nologin', 'freddie-sync')
            user = pwd.getpwnam('freddie-sync')
        if user.pw_uid == 0:
            raise RuntimeError('Invalid service account')
        for path in ['/var/lib/freddie-sync', '/var/backups/freddie-sync']:
            directory = Path(path)
            directory.mkdir(mode=0o700, parents=True, exist_ok=True)
            os.chown(directory, user.pw_uid, user.pw_gid)
            os.chmod(directory, 0o700)
        release.mkdir(parents=True, exist_ok=True)
        shutil.copy2(binary, release / 'freddie-sync')
        shutil.copy2(stage / 'deploy/backup.sh', release / 'backup.sh')
        for name in ['freddie-sync', 'backup.sh']:
            os.chmod(release / name, 0o755)
        for target, content in [*((t, src.read_bytes()) for t, src in targets.items()), *rendered.items()]:
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(content)
            os.chmod(target, 0o644)
        # Keep a timestamped copy for manual recovery as well as the rollback below.
        archive = root / ('nginx.conf.before-' + time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()))
        archive.write_bytes(original)
        os.chmod(archive, 0o600)
        nginx.write_text(text)
        run('nginx', '-t')
        next_link = root / 'current.next'
        if next_link.is_symlink():
            next_link.unlink()
        next_link.symlink_to(release)
        next_link.replace(current)
        run('systemd-analyze', 'verify', '/etc/systemd/system/freddie-sync.service',
            '/etc/systemd/system/freddie-sync-backup.service',
            '/etc/systemd/system/freddie-sync-backup.timer')
        run('systemctl', 'daemon-reload')
        run('systemctl', 'enable', 'freddie-sync.service')
        run('systemctl', 'restart', 'freddie-sync.service')
        for attempt in range(30):
            try:
                with urllib.request.urlopen('http://127.0.0.1:17483/health', timeout=2) as response:
                    if response.status == 200:
                        break
            except OSError:
                time.sleep(0.2)
        else:
            raise RuntimeError('New service did not become healthy')
        run('systemctl', 'reload', 'nginx')
        run('systemctl', 'enable', '--now', 'freddie-sync-backup.timer')
        run('systemctl', 'start', 'freddie-sync-backup.service')
        print('Installed release ' + release_hash)
    except BaseException:
        nginx.write_bytes(original)
        for path, content in saved.items():
            if content is None:
                path.unlink(missing_ok=True)
            else:
                path.write_bytes(content)
        if not backup_timer_was_enabled:
            subprocess.run(['systemctl', 'disable', '--now', 'freddie-sync-backup.timer'])
        if previous:
            current.unlink(missing_ok=True)
            current.symlink_to(previous)
        subprocess.run(['systemctl', 'daemon-reload'])
        subprocess.run(['systemctl', 'restart' if previous else 'stop', 'freddie-sync.service'])
        subprocess.run(['nginx', '-t'])
        subprocess.run(['systemctl', 'reload', 'nginx'])
        raise


if __name__ == '__main__':
    main()
