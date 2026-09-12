#!/usr/bin/env python3
"""Capture console fixtures from a running gateway for scripts/ui-harness/run.sh.

Environment:
    GW_BASE            gateway base URL (default http://127.0.0.1:8088)
    GW_COOKIE          an existing aigw_admin session cookie value
    GW_ADMIN_USER      admin username (used to log in when GW_COOKIE is absent)
    GW_ADMIN_PASSWORD  admin password

Real endpoints are refreshed; anything else already in fixtures.json is preserved,
because the plugin scenarios (a provider whose handshake was or was not recorded yet)
are synthetic on purpose: they must not depend on a live plugin.
"""
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ.get("GW_BASE", "http://127.0.0.1:8088").rstrip("/")
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "fixtures.json")


def call(path, cookie=None, method="GET", payload=None):
    data = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if cookie:
        req.add_header("Cookie", "aigw_admin=" + cookie)
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read().decode())


def login():
    user = os.environ.get("GW_ADMIN_USER")
    password = os.environ.get("GW_ADMIN_PASSWORD")
    if not user or not password:
        return None
    req = urllib.request.Request(
        BASE + "/admin/api/v1/auth/login",
        data=json.dumps({"username": user, "password": password}).encode(),
        method="POST",
    )
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=30) as resp:
        for header, value in resp.getheaders():
            if header.lower() == "set-cookie" and value.startswith("aigw_admin="):
                return value.split(";", 1)[0].split("=", 1)[1]
    return None


def main():
    cookie = os.environ.get("GW_COOKIE") or login()
    if not cookie:
        print("no session: set GW_COOKIE or GW_ADMIN_USER/GW_ADMIN_PASSWORD", file=sys.stderr)
        return 2

    fixtures = {}
    if os.path.exists(OUT):
        fixtures = json.load(open(OUT, encoding="utf-8"))

    fixtures["/providers"] = call("/admin/api/v1/providers", cookie)
    # The key editor and the request log (keys.page.html). Kept in the capture so a
    # --refresh does not silently drop the fixtures those views depend on.
    fixtures["/keys"] = call("/admin/api/v1/keys", cookie)
    # The request log's 用户 (account) filter lists the accounts, like the key editor does.
    fixtures["/accounts"] = call("/admin/api/v1/accounts?limit=1000", cookie)
    fixtures["/requests"] = call("/admin/api/v1/requests?days=7&limit=50", cookie)
    # The dimension breakdown and the model filter list share this endpoint (M27), so a
    # --refresh has to capture it or the statistics card renders empty. The two credential
    # groupings (M30) are captured under their own keys because the harness answers them by
    # group_by — they are the ones whose names the card has to render.
    fixtures["/requests/dimensions"] = call(
        "/admin/api/v1/requests/dimensions?days=7&group_by=model&limit=20", cookie)
    for grouping in ("account", "api_key"):
        fixtures[f"/requests/dimensions?group_by={grouping}"] = call(
            f"/admin/api/v1/requests/dimensions?days=7&group_by={grouping}&limit=20", cookie)
    for row in fixtures["/requests"].get("data", [])[:1]:
        fixtures[f"/requests/{row['request_id']}"] = call(f"/admin/api/v1/requests/{row['request_id']}", cookie)
    fixtures["/provider-kinds"] = call("/admin/api/v1/provider-kinds", cookie)
    # The request-log page reads the retention/write-health block from /stats.
    fixtures["/stats"] = call("/admin/api/v1/stats", cookie)
    for row in fixtures["/providers"].get("data", []):
        pid = str(row["id"])
        fixtures[f"/providers/{pid}"] = call(f"/admin/api/v1/providers/{pid}", cookie)
        fixtures[f"/providers/{pid}/logs"] = call(f"/admin/api/v1/providers/{pid}/logs", cookie)
        fixtures[f"/providers/{pid}/actions"] = call(f"/admin/api/v1/providers/{pid}/actions", cookie)
        # The detail dialog's model-mapping section (M28) reads both of these: the
        # provider's own mappings, and every route so it can name the routes that point
        # here without a mapping (the row that makes a model silently unroutable).
        fixtures[f"/providers/{pid}/models"] = call(f"/admin/api/v1/providers/{pid}/models?limit=500", cookie)
    fixtures["/routes"] = call("/admin/api/v1/routes?limit=1000", cookie)

    json.dump(fixtures, open(OUT, "w", encoding="utf-8"), ensure_ascii=False, indent=1, sort_keys=True)
    print(f"wrote {OUT}: {len(fixtures)} endpoints")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except urllib.error.URLError as err:
        print(f"cannot reach {BASE}: {err}", file=sys.stderr)
        sys.exit(1)
