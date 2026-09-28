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

* every `/admin/chat-artifact/…` request is answered with the rendered preview page and the
  artifact's own headers (`ARTIFACT_CSP`, `X-Aigw-Bridge`), byte for byte what
  `internal/httpapi/chat_artifact.go` sends for an interactive HTML preview. The console's
  preview iframe therefore loads a *real* document at the URL it built, and the script the
  server injects really runs — which is how the handshake between the console and the frame
  is tested at all (it used to be "only a human can check this", and it was broken the whole
  time). `TestHarnessPreviewCSPMatchesTheServer` pins this copy to the Go one.

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

#: The policy a preview payload is served with, byte for byte what `chatArtifactCSP` returns for
#: an HTML artifact on a deployment without `chat.artifact_allow_network` (`sandbox allow-scripts`
#: and no allow-same-origin — the frame is an opaque origin — plus `script-src 'unsafe-inline'`,
#: which is what authorizes both the model page's own scripts and the injected bridge).
#:
#: It matters that this is the deployed policy and not a convenient one: with `script-src` too
#: strict the injected script never runs, and the harness would report a broken channel that
#: users do not have. `TestHarnessPreviewCSPMatchesTheServer` (internal/httpapi) keeps the copy
#: honest, the same way style_csp_test.mjs pins the console's.
ARTIFACT_CSP = "sandbox allow-scripts; default-src 'none'; img-src data: blob:; media-src data: blob:; font-src data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'"

#: The path prefix the console builds its preview URL from, and the rendered page that answers it.
ARTIFACT_PREFIX = "/admin/chat-artifact/"
ARTIFACT_PAGE = "/preview_artifact.html"


class Handler(http.server.SimpleHTTPRequestHandler):
    #: Read once from the environment so every request agrees on the mode.
    gzip_sidecars = os.environ.get("UI_HARNESS_GZIP") == "1"

    def do_GET(self):
        # A preview URL carries a ticket query and an artifact id; neither means anything here.
        # What is being exercised is the *document* the console loads and the headers it arrives
        # with, so the path is rewritten before the static handler ever sees it.
        self.artifact_response = self.path.split("?", 1)[0].startswith(ARTIFACT_PREFIX)
        if self.artifact_response:
            self.path = ARTIFACT_PAGE
        super().do_GET()

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
        # …and only a preview artifact carries the artifact's policy: that page has no inline
        # script of its own beyond the model's, and running it under any other policy would test
        # a preview nobody has.
        if getattr(self, "artifact_response", False):
            self.send_header("Content-Security-Policy", ARTIFACT_CSP)
            self.send_header("X-Aigw-Bridge", "1")
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
