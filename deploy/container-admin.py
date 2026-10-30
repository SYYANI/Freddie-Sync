#!/usr/bin/env python3
"""Open the Apple container admin socket through a local loopback TCP relay."""
import argparse
from pathlib import Path
import select
import socket
import socketserver
import webbrowser


class Relay(socketserver.BaseRequestHandler):
    def handle(self):
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as upstream:
            upstream.settimeout(60)
            self.request.settimeout(60)
            try:
                upstream.connect(str(self.server.admin_socket))
                peers = {self.request: upstream, upstream: self.request}
                while True:
                    readable, _, _ = select.select(list(peers), [], [], 60)
                    if not readable:
                        return
                    for source in readable:
                        data = source.recv(65536)
                        if not data:
                            return
                        peers[source].sendall(data)
            except OSError:
                return


class LocalServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--socket', type=Path, required=True)
    parser.add_argument('--port', type=int, default=17484)
    args = parser.parse_args()
    if not 1 <= args.port <= 65535:
        parser.error('port must be between 1 and 65535')
    if not args.socket.is_socket():
        parser.error('admin socket is unavailable; start the container first')
    with LocalServer(('127.0.0.1', args.port), Relay) as server:
        server.admin_socket = args.socket
        url = f'http://127.0.0.1:{args.port}/'
        print(f'Administration page: {url} (Ctrl-C closes the relay)', flush=True)
        webbrowser.open(url)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass


if __name__ == '__main__':
    main()
