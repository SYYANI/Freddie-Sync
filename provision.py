#!/usr/bin/env python3
"""Create a device over SSH and write its one-time token to a private local file."""
import argparse
import json
import os
from pathlib import Path
import shlex
import subprocess


def admin(host, *args):
    command = shlex.join(['/opt/freddie-sync/current/freddie-sync', *args])
    result = subprocess.run(['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', host, command],
                            check=True, stdout=subprocess.PIPE, timeout=30)
    return json.loads(result.stdout) if result.stdout.strip() else None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host', required=True, help='SSH destination of the sync server')
    parser.add_argument('--label', required=True)
    parser.add_argument('--out', type=Path, required=True)
    library = parser.add_mutually_exclusive_group()
    library.add_argument('--library', metavar='ID', help='existing library; default is your personal library')
    library.add_argument('--new-library', metavar='NAME', help='create a separate library, for example for another person')
    args = parser.parse_args()
    args.out.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd = os.open(args.out, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        library_id = args.library or ''
        if args.new_library:
            library_id = admin(args.host, 'library-add', '--name', args.new_library)['id']
        credential = admin(args.host, 'device-add', '--label', args.label, '--library', library_id)
        if not credential.get('baseURL'):
            raise RuntimeError('The server returned a credential without baseURL; upgrade the server')
        with os.fdopen(fd, 'w') as stream:
            json.dump(credential, stream, indent=2)
            stream.write('\n')
    except BaseException:
        args.out.unlink(missing_ok=True)
        raise
    print(f'Device credential saved privately: {args.out} (library {credential["device"]["libraryID"]})')


if __name__ == '__main__':
    main()
