#!/bin/sh
set -eu
cd "$(dirname "$0")"
if [ "$#" -ne 2 ]; then echo 'usage: ./deploy.sh SSH_DESTINATION SERVER_NAME   (for example: ./deploy.sh root@vps sync.example.com)' >&2; exit 2; fi
sync_target="$1"
sync_server_name="$2"
case "$sync_target" in -*|*[!a-zA-Z0-9_.@-]*) echo 'Invalid SSH destination' >&2; exit 1;; esac
case "$sync_server_name" in -*|*[!a-zA-Z0-9.-]*|*.) echo 'Invalid server name' >&2; exit 1;; esac
sync_stage="$(mktemp -d "${TMPDIR:-/tmp}/freddie-sync-build.XXXXXX")"
trap 'rm -rf "$sync_stage"' EXIT HUP INT TERM
mkdir -p bin
sync_version="$(git rev-parse --short HEAD)-$(date -u +%Y%m%dT%H%M%SZ)"
GOTOOLCHAIN=go1.26.10 CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildVersion=$sync_version" -o "$sync_stage/freddie-sync" .
cp -R deploy "$sync_stage/deploy"
tar -czf "$sync_stage/release.tgz" -C "$sync_stage" freddie-sync deploy
sync_remote="$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$sync_target" 'umask 077; mktemp -d /tmp/freddie-sync-install.XXXXXX')"
case "$sync_remote" in *[!a-zA-Z0-9/._-]*) echo 'Invalid remote staging path' >&2; exit 1;; esac
case "$sync_remote" in /tmp/freddie-sync-install.[a-zA-Z0-9]*) ;; *) echo 'Unexpected staging path' >&2; exit 1;; esac
ssh -o BatchMode=yes -o ConnectTimeout=10 "$sync_target" "cat > '$sync_remote/release.tgz'" < "$sync_stage/release.tgz"
ssh -o BatchMode=yes -o ConnectTimeout=10 "$sync_target" "set -eu; tar -xzf '$sync_remote/release.tgz' -C '$sync_remote' --no-same-owner; python3 '$sync_remote/deploy/install.py' '$sync_remote' '$sync_server_name'"
ssh -o BatchMode=yes -o ConnectTimeout=10 "$sync_target" "rm -rf -- '$sync_remote'"
