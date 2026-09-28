#!/usr/bin/env python3
"""Write one harness page: a template with the captured fixtures embedded.

Usage: render_page.py TEMPLATE FIXTURES OUT

Two placeholders:

* `__FIXTURES__` becomes the captured API fixtures as a JSON object.
* `__AIGW_BRIDGE__` becomes the `<script id="aigw-ui-bridge">…</script>` element the gateway
  injects into an interactive preview. It is taken from the fixtures
  (`/js/pages/chat_ui_bridge_script.js`), which `TestUIBridgeScriptDumpForTheHarness` rewrites
  from `uiBridgeScript()` on every `go test` — so the harness runs exactly what the server
  serves, in the same place (right after `<head>`) and with the same id.
"""
import json
import sys

#: The fixture key that carries the generated bridge script, and the marker the Go test writes
#: into fixtures.json. A page that asks for the script without the fixture present must fail
#: loudly: an empty <script> would look like a broken preview instead of a missing fixture.
BRIDGE_KEY = "/js/pages/chat_ui_bridge_script.js"
BRIDGE_PLACEHOLDER = "__AIGW_BRIDGE__"


def main() -> int:
    template, fixtures, out = sys.argv[1], sys.argv[2], sys.argv[3]
    html = open(template, encoding="utf-8").read()
    data = json.load(open(fixtures, encoding="utf-8"))
    if BRIDGE_PLACEHOLDER in html:
        # str.replace takes every occurrence, so a page that *describes* the placeholder would get
        # a second copy of the bridge injected into its prose. Refuse instead: the copy would sit
        # inside whatever context that was, and nothing downstream would look wrong.
        count = html.count(BRIDGE_PLACEHOLDER)
        if count != 1:
            print(f"{template} mentions {BRIDGE_PLACEHOLDER} {count} times; exactly one is expected",
                  file=sys.stderr)
            return 2
        script = data.get(BRIDGE_KEY)
        if not script:
            print(f"{template} needs {BRIDGE_PLACEHOLDER}, but {fixtures} has no {BRIDGE_KEY}", file=sys.stderr)
            return 2
        tag = '<script id="aigw-ui-bridge">' + script + "</script>"
        html = html.replace(BRIDGE_PLACEHOLDER, tag)
    with open(out, "w", encoding="utf-8") as handle:
        handle.write(html.replace("__FIXTURES__", json.dumps(data, ensure_ascii=False)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
