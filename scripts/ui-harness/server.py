#!/usr/bin/env python3
"""Static server for the console harness.

It serves the console's assets exactly like the binary does, with one deliberate difference:
every response is `no-store`. The console sends `max-age=300` for its immutable assets, which is
right in production and wrong here — a harness that runs yesterday's JavaScript reports failures
that were already fixed and passes ones that were just introduced. The first version of this
harness used `python3 -m http.server` and spent a debugging round chasing a stale module.

Usage: server.py <port>
"""
import http.server
import socketserver
import sys


class Handler(http.server.SimpleHTTPRequestHandler):
    def end_headers(self):
        self.send_header("Cache-Control", "no-store, must-revalidate")
        super().end_headers()

    def log_message(self, *args):
        # Same shape as http.server's own log, which run.sh reads for the report POSTs.
        sys.stderr.write("%s - %s\n" % (self.address_string(), self.requestline))


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8097
    socketserver.TCPServer.allow_reuse_address = True
    with socketserver.TCPServer(("127.0.0.1", port), Handler) as httpd:
        httpd.serve_forever()


if __name__ == "__main__":
    main()
