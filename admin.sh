#!/bin/sh
# Opens the administration page through an SSH forward of the server's 0600 admin
# socket. Nothing listens on a public port; the page lives only while this runs.
set -eu
if [ "$#" -lt 1 ] || [ "$#" -gt 3 ]; then echo 'usage: ./admin.sh SSH_DESTINATION [LOCAL_PORT] [REMOTE_SOCKET]' >&2; exit 2; fi
admin_target="$1"
admin_port="${2:-17484}"
admin_socket="${3:-/run/freddie-sync/admin.sock}"
case "$admin_target" in -*|*[!a-zA-Z0-9_.@-]*) echo 'Invalid SSH destination' >&2; exit 1;; esac
case "$admin_port" in ''|*[!0-9]*) echo 'Invalid local port' >&2; exit 1;; esac
case "$admin_socket" in *[!a-zA-Z0-9/._-]*) echo 'Invalid remote socket path' >&2; exit 1;; esac
case "$admin_socket" in /*) ;; *) echo 'Remote socket path must be absolute' >&2; exit 1;; esac
ssh -N -o BatchMode=yes -o ConnectTimeout=10 -o ExitOnForwardFailure=yes \
    -L "127.0.0.1:$admin_port:$admin_socket" "$admin_target" &
admin_tunnel=$!
trap 'kill "$admin_tunnel" 2>/dev/null || true' EXIT HUP INT TERM
admin_url="http://127.0.0.1:$admin_port/"
attempt=0
until curl -fsS -o /dev/null "$admin_url" 2>/dev/null; do
    attempt=$((attempt + 1))
    if [ "$attempt" -gt 100 ] || ! kill -0 "$admin_tunnel" 2>/dev/null; then
        echo 'The SSH forward did not start.' >&2
        exit 1
    fi
    sleep 0.1
done
if command -v open >/dev/null 2>&1; then open "$admin_url"; elif command -v xdg-open >/dev/null 2>&1; then xdg-open "$admin_url"; fi
echo "Administration page: $admin_url (Ctrl-C closes the tunnel)"
wait "$admin_tunnel"
