#!/usr/bin/env python3
"""Write one harness page: a template with the captured fixtures embedded.

Usage: render_page.py TEMPLATE FIXTURES OUT
"""
import json
import sys


def main() -> int:
    template, fixtures, out = sys.argv[1], sys.argv[2], sys.argv[3]
    html = open(template, encoding="utf-8").read()
    data = json.load(open(fixtures, encoding="utf-8"))
    with open(out, "w", encoding="utf-8") as handle:
        handle.write(html.replace("__FIXTURES__", json.dumps(data, ensure_ascii=False)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
