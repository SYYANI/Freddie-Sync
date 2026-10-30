#!/bin/sh
# Apple container on macOS. Keep the database on a native Linux volume and
# forward the admin Unix socket separately from the sync HTTP port.
set -eu
cd "$(dirname "$0")"
umask 077

usage() {
    cat >&2 <<'USAGE'
usage: sh container.sh COMMAND [ARGS...]
  build                         Build the image
  up HOST [PUBLIC_URL]          Create the service and persistent volume
  start | stop | rm             Manage the service; rm keeps its volume
  status | logs                 Inspect the service
  exec COMMAND [FLAGS...]       Run a freddie-sync administration command
  admin [LOCAL_PORT]            Open the local administration page (Python 3)
  backup                        Create a verified online backup in the volume
USAGE
    exit 2
}

name="${FREDDIE_SYNC_CONTAINER_NAME:-freddie-sync}"
image="${FREDDIE_SYNC_CONTAINER_IMAGE:-freddie-sync-server:local}"
volume="${FREDDIE_SYNC_CONTAINER_VOLUME:-$name-state}"
init_image="${FREDDIE_SYNC_CONTAINER_INIT_IMAGE:-docker.io/library/busybox:1.37}"
for identifier in "$name" "$volume"; do
    case "$identifier" in ''|-*|*[!a-zA-Z0-9_.-]*) echo 'Invalid container or volume name' >&2; exit 2;; esac
done
admin_dir="$PWD/.local/container-$name"
admin_socket="$admin_dir/admin.sock"
guest_socket=/state/run/admin.sock
binary=/usr/local/bin/freddie-sync

[ "$#" -gt 0 ] || usage
command="$1"
shift
case "$command" in
    build)
        [ "$#" -eq 0 ] || usage
        exec container build --build-arg "VERSION=${FREDDIE_SYNC_VERSION:-container}" -t "$image" .
        ;;
    up)
        [ "$#" -ge 1 ] && [ "$#" -le 2 ] || usage
        host="$1"
        public_url="${2:-https://$host/freddie-sync/}"
        case "$host" in ''|-*|*[!a-zA-Z0-9.-]*|*.) echo 'Invalid public hostname' >&2; exit 2;; esac
        case "$public_url" in https://*) ;; *) echo 'Public URL must use HTTPS' >&2; exit 2;; esac
        if container inspect "$name" >/dev/null 2>&1; then
            echo "Container $name already exists. Use start, or stop and rm before recreating it." >&2
            exit 1
        fi
        if ! container volume inspect "$volume" >/dev/null 2>&1; then
            container volume create "$volume" >/dev/null
        fi
        # A fresh ext4 volume belongs to root. Initialize only its top-level
        # directories, without changing ownership of existing database files.
        container run --rm --user 0:0 --memory 128M --read-only \
            --cap-drop ALL --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE \
            --mount "type=volume,source=$volume,target=/state" \
            "$init_image" sh -ec '
                mkdir -p /state/data /state/backups /state/run
                chmod 0700 /state /state/data /state/backups /state/run
                chown 10001:10001 /state /state/data /state/backups /state/run
            '
        mkdir -p "$admin_dir"
        chmod 0700 "$admin_dir"
        exec container run --detach --name "$name" --user 10001:10001 \
            --memory 256M --cpus 2 --read-only --cap-drop ALL \
            --ulimit nofile=1024:1024 \
            --mount "type=volume,source=$volume,target=/state" \
            --publish 127.0.0.1:17483:17483 \
            --publish-socket "$admin_socket:$guest_socket" \
            "$image" serve --host "$host" --public-url "$public_url" \
            --data /state/data --socket "$guest_socket" \
            --listen 0.0.0.0:17483 --allow-non-loopback
        ;;
    start)
        [ "$#" -eq 0 ] || usage
        exec container start "$name"
        ;;
    stop)
        [ "$#" -eq 0 ] || usage
        exec container stop --time 40 "$name"
        ;;
    rm)
        [ "$#" -eq 0 ] || usage
        exec container delete "$name"
        ;;
    status)
        [ "$#" -eq 0 ] || usage
        exec container inspect "$name"
        ;;
    logs)
        [ "$#" -eq 0 ] || usage
        exec container logs -n 100 "$name"
        ;;
    exec)
        [ "$#" -gt 0 ] || usage
        task="$1"
        shift
        exec container exec --user 10001:10001 "$name" "$binary" "$task" --socket "$guest_socket" "$@"
        ;;
    admin)
        [ "$#" -le 1 ] || usage
        exec python3 deploy/container-admin.py --socket "$admin_socket" --port "${1:-17484}"
        ;;
    backup)
        [ "$#" -eq 0 ] || usage
        backup_path="/state/backups/backup-$(date -u +%Y%m%dT%H%M%SZ)"
        exec container exec --user 10001:10001 "$name" "$binary" backup \
            --socket "$guest_socket" --data /state/data --out "$backup_path"
        ;;
    *) usage ;;
esac
