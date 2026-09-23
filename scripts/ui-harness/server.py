#!/usr/bin/env python3
"""Static server for the console harness.

It serves the console's assets the way the binary does, with three deliberate differences:

* every response is `no-store`. The console sends `max-age=300` for its immutable assets,
  which is right in production and wrong here — a harness that runs yesterday's JavaScript
  reports failures that were already fixed and passes ones that were just introduced. The
  first version of this harness used `python3 -m http.server` and spent a debugging round
  chasing a stale module.

* with `UI_HARNESS_GZIP=1` it answers a client that accepts gzip with the `.gz` sidecar,
  exactly as internal/webui does. A release build embeds those sidecars, so a walkthrough
  that never sees one is exercising a code path no user takes — and would report
  "compression changed nothing" without anything having been compressed. See
  docs/design/m55-console-transfer-compression.md.

* `/csp.html` (the `csp` view) is sent with the console's own Content-Security-Policy, and
  every other page is not. Until that view existed the harness had no CSP at all, which is
  exactly how a console-wide breakage stayed invisible: `style-src 'self'` makes browsers
  discard style **attributes**, so the tree's indentation (`style="padding-left:…"`) was
  zero in production while the harness — with no policy to enforce — measured the indent
  and reported green. A console whose pages carry inline scripts (all the other harness
  pages do) cannot run under this policy; the `csp` view is therefore a page with no inline
  script or style, only a same-origin module.

Usage: server.py <port>
"""
import http.server
import os
import socketserver
import sys

#: The console's policy, byte for byte. internal/webui/embed.go is the source of truth (the
#: header it sets on the shell and on every `.html` asset), and
#: internal/webui/tests/style_csp_test.mjs pins the two copies equal — a harness measuring
#: another policy than the deployed one is worse than no harness, because it reports green.
CONSOLE_CSP = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'"


class Handler(http.server.SimpleHTTPRequestHandler):
    #: Read once from the environment so every request agrees on the mode.
    gzip_sidecars = os.environ.get("UI_HARNESS_GZIP") == "1"

    def send_head(self):
        if self.gzip_sidecars:
            asset = self.translate_path(self.path)
            sidecar = asset + ".gz"
            if os.path.isfile(sidecar):
                if "gzip" in self.headers.get("Accept-Encoding", ""):
                    try:
                        handle = open(sidecar, "rb")
                    except OSError:
                        return super().send_head()
                    size = os.fstat(handle.fileno()).st_size
                    self.send_response(200)
                    # The type belongs to the asset, not to the file on disk: the sidecar
                    # ends in ".gz" and the browser is receiving JavaScript.
                    self.send_header("Content-Type", self.guess_type(asset))
                    self.send_header("Content-Encoding", "gzip")
                    self.send_header("Vary", "Accept-Encoding")
                    self.sent_vary = True
                    self.send_header("Content-Length", str(size))
                    self.end_headers()
                    return handle
        return super().send_head()

    def end_headers(self):
        self.send_header("Cache-Control", "no-store, must-revalidate")
        # 只有 `csp` 视图那一页带策略：其余 harness 页有内联脚本/样式，在这条策略下会被整页拦掉，
        # 而报告发不出来时视图只会以 "no report" 失败（看起来像页面坏了，其实是策略在生效）。
        if self.path.split("?", 1)[0] == "/csp.html":
            self.send_header("Content-Security-Policy", CONSOLE_CSP)
        # A response that could have been answered two ways says so in both branches —
        # the compressed one above, and this one through the isfile check below — or a
        # shared cache could hand gzip to a client that never asked for it.
        if (
            self.gzip_sidecars
            and not getattr(self, "sent_vary", False)
            and os.path.isfile(self.translate_path(self.path) + ".gz")
        ):
            self.send_header("Vary", "Accept-Encoding")
        self.sent_vary = False
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
