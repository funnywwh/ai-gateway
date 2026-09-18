#!/usr/bin/env python3
"""Supervised-shape acceptance: aigw starts dshgw, dshgw runs a bwrap tenant worker.

This is the host-level counterpart to the Go tests. It exercises the shape that
M58 introduced and that no unit test can cover end to end:

  1. aigw generates the child's configuration and starts a sibling dshgw;
  2. the child reports readiness and opens its same-UID admin socket;
  3. a tenant created over that socket runs as a bubblewrap child of the child;
  4. the worker answers the gateway's unauthenticated /api probe with 401;
  5. the tenant's model list is refreshed from aigw before the worker starts;
  6. stopping/starting the tenant is durable (registry "suspended"), and a
     restarted aigw brings unsuspended tenants back by itself;
  7. stopping aigw leaves no orphaned worker processes behind.

Everything runs as the invoking (unprivileged) account, on its own ports and in
its own work directory, so it never touches a running deployment. It skips itself
when the host cannot provide the prerequisites (no bubblewrap, no dsh runtime).

Usage:
  scripts/dshgw_supervised_e2e.py --aigw-bin bin/aigw --dshgw-bin bin/dshgw
    [--node PATH] [--dsh-root PATH] [--workdir DIR] [--keep] [--verbose]
"""

from __future__ import annotations

import argparse
import http.server
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
from pathlib import Path

PROBE_TIMEOUT = 60.0
CHILD_TIMEOUT = 30.0

# The portal port dshgw runs with, needed by the front proxy's routing headers.
TENANT_PORT_FALLBACK: list[int] = [0]

# Per-worker limits the acceptance configures, then reads back from the kernel.
# They replace the systemd unit's MemoryMax/TasksMax, which the rootless shape has
# no unit to enforce.
MEMORY_MAX_BYTES = 2 * 1024 * 1024 * 1024
TASKS_MAX = 512
CPU_QUOTA_PERCENT = 200


class Failure(Exception):
    """A failed acceptance step: the message is the report."""


class Check:
    """Collects step results so one run reports every problem, not just the first."""

    def __init__(self, verbose: bool) -> None:
        self.verbose = verbose
        self.failures: list[str] = []
        self.passes = 0

    def ok(self, name: str, detail: str = "") -> None:
        self.passes += 1
        print(f"PASS {name}{(' — ' + detail) if detail else ''}")

    def fail(self, name: str, detail: str) -> None:
        self.failures.append(f"{name}: {detail}")
        print(f"FAIL {name} — {detail}")

    def require(self, name: str, condition: bool, detail: str = "") -> bool:
        if condition:
            self.ok(name, detail)
        else:
            self.fail(name, detail or "condition not met")
        return condition

    def note(self, message: str) -> None:
        if self.verbose:
            print(f"     {message}")


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


class StubAigw:
    """Minimal aigw: /v1/models answers any `sk-` key with a fixed model list.

    Using a stub keeps the acceptance free of real credentials and of aigw's own
    database, while still exercising the child's key validation and the model
    refresh that runs before a worker starts.
    """

    def __init__(self, models: list[str], tenant: str = "") -> None:
        self.models = models
        self.calls = 0
        self.port = free_port()
        # The entitlement answer POST /v1/dshgw/authorize gives, and a switch the acceptance
        # flips to prove that a revocation between login and redemption is honoured (M61).
        self.tenant = tenant
        self.deny = False
        self.authorize_calls = 0
        stub = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def _json(self, status: int, payload: dict) -> None:
                body = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self) -> None:  # noqa: N802 (http.server API)
                if self.path.rstrip("/") != "/v1/models":
                    self.send_response(404)
                    self.end_headers()
                    return
                stub.calls += 1
                if not self.headers.get("Authorization", "").startswith("Bearer sk-"):
                    self.send_response(401)
                    self.end_headers()
                    return
                self._json(200, {"data": [{"id": m} for m in stub.models]})

            def do_POST(self) -> None:  # noqa: N802 (http.server API)
                # What dshgw asks after redeeming a login ticket: may this tenant still use
                # the gateway? In production this is real aigw answering from the account row.
                if self.path.rstrip("/") != "/v1/dshgw/authorize":
                    self.send_response(404)
                    self.end_headers()
                    return
                stub.authorize_calls += 1
                if not self.headers.get("Authorization", "").startswith("Bearer "):
                    self._json(401, {"error": {"message": "missing key"}})
                    return
                if stub.deny:
                    self._json(403, {"allowed": False, "reason": "dsh_disabled"})
                    return
                self._json(200, {"allowed": True, "tenant": stub.tenant})

            def log_message(self, *args: object) -> None:  # silence the test output
                return

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", self.port), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def start(self) -> None:
        self.thread.start()

    def stop(self) -> None:
        self.server.shutdown()
        self.server.server_close()

    @property
    def base_url(self) -> str:
        return f"http://127.0.0.1:{self.port}"


class StubFeishu:
    """Minimal Feishu: the consent page immediately redirects back with a code.

    The acceptance needs the two server-side calls aigw makes (token exchange and user info)
    and one browser hop through the authorization page. Everything else — the app id, the
    secret, the registered redirect URL — is the same shape a real application has.
    """

    def __init__(self, open_id: str = "ou_e2e", name: str = "E2E 用户") -> None:
        self.open_id = open_id
        self.name = name
        self.port = free_port()
        self.authorizations = 0
        stub = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def _json(self, status: int, payload: dict) -> None:
                body = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self) -> None:  # noqa: N802 (http.server API)
                path, _, query = self.path.partition("?")
                if path == "/userinfo":
                    # aigw asks who this is with the user access token. open_id and the name
                    # are the only fields this feature reads, and neither needs a scope.
                    if not self.headers.get("Authorization", "").startswith("Bearer "):
                        self._json(200, {"code": 20005, "msg": "invalid token"})
                        return
                    self._json(200, {"code": 0, "msg": "success", "data": {
                        "open_id": stub.open_id, "union_id": "on_e2e", "name": stub.name}})
                    return
                if path != "/authorize":
                    self._json(404, {"code": 20001, "msg": "not found"})
                    return
                params = dict(part.split("=", 1) for part in query.split("&") if "=" in part)
                if "redirect_uri" not in params:
                    self._json(400, {"code": 20001, "msg": "redirect_uri is required"})
                    return
                stub.authorizations += 1
                # The user consents: back to the registered URL with a one-time code. The
                # redirect target is percent-encoded by whoever built the link.
                import urllib.parse
                target = urllib.parse.unquote(params["redirect_uri"])
                target += ("&" if "?" in target else "?") + "code=e2e-auth-code"
                if "state" in params:
                    target += "&state=" + params["state"]
                self.send_response(302)
                self.send_header("Location", target)
                self.end_headers()

            def do_POST(self) -> None:  # noqa: N802 (http.server API)
                if self.path != "/token":
                    self._json(404, {"code": 20001, "msg": "not found"})
                    return
                length = int(self.headers.get("Content-Length") or 0)
                form = self.rfile.read(length).decode()
                if "client_secret=secret" not in form or "grant_type=authorization_code" not in form:
                    self._json(400, {"code": 20002, "error": "invalid_client"})
                    return
                self._json(200, {"code": 0, "access_token": "u-e2e-token", "expires_in": 7200,
                                 "token_type": "Bearer"})

            def log_message(self, *args: object) -> None:  # silence the test output
                return

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", self.port), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def start(self) -> None:
        self.thread.start()

    def stop(self) -> None:
        self.server.shutdown()
        self.server.server_close()

    @property
    def base_url(self) -> str:
        return f"http://127.0.0.1:{self.port}"


class Browser:
    """A tiny cookie-keeping HTTP client that never follows a redirect.

    Every hop of the Feishu handoff is asserted, so following redirects automatically would
    hide which side produced which Location — and the ticket cookie has to be carried by hand
    anyway, since that is exactly the mechanism M61 relies on.
    """

    def __init__(self) -> None:
        # name -> (value, path). The path is not decoration: a cookie scoped to /admin is
        # NOT sent to /feishu/login, and a harness that ignores this would accept a flow no
        # browser can complete (it did, once — the bind step lost the admin session).
        self.cookies: dict[str, tuple[str, str]] = {}

    def request(self, method: str, url: str, body: dict | None = None, headers: dict | None = None,
                origin: str = "") -> tuple[int, dict, str]:
        import urllib.parse
        parsed = urllib.parse.urlsplit(url)
        host = parsed.hostname or "127.0.0.1"
        port = parsed.port or 80
        request_path = parsed.path or "/"
        path = request_path
        if parsed.query:
            path += "?" + parsed.query
        sent = dict(headers or {})
        payload = b""
        if body is not None:
            payload = json.dumps(body).encode()
            sent.setdefault("Content-Type", "application/json")
        cookie_header = "; ".join(
            f"{name}={value}" for name, (value, cookie_path) in self.cookies.items()
            if request_path.startswith(cookie_path))
        if cookie_header:
            sent["Cookie"] = cookie_header
        if origin:
            sent["Origin"] = origin
        with socket.create_connection((host, port), timeout=10) as conn:
            head = f"{method} {path} HTTP/1.1\r\nHost: {host}:{port}\r\nConnection: close\r\n"
            head += f"Content-Length: {len(payload)}\r\n"
            for name, value in sent.items():
                head += f"{name}: {value}\r\n"
            conn.sendall(head.encode() + b"\r\n" + payload)
            chunks = []
            while True:
                chunk = conn.recv(65536)
                if not chunk:
                    break
                chunks.append(chunk)
                if sum(len(c) for c in chunks) > 1 << 20:
                    break
        raw = b"".join(chunks)
        header, _, body_bytes = raw.partition(b"\r\n\r\n")
        lines = header.decode(errors="replace").splitlines()
        status = 0
        if lines:
            parts = lines[0].split()
            if len(parts) >= 2 and parts[1].isdigit():
                status = int(parts[1])
        response_headers: dict[str, str] = {}
        for line in lines[1:]:
            name, _, value = line.partition(":")
            key = name.strip().lower()
            if key == "set-cookie":
                attributes = [part.strip() for part in value.split(";")]
                cookie_name, _, cookie_value = attributes[0].partition("=")
                cookie_path = "/"
                for attribute in attributes[1:]:
                    if attribute.lower().startswith("path="):
                        cookie_path = attribute.split("=", 1)[1].strip() or "/"
                self.cookies[cookie_name.strip()] = (cookie_value.strip(), cookie_path)
                response_headers.setdefault(key, value.strip())
            else:
                response_headers[key] = value.strip()
        return status, response_headers, body_bytes.decode(errors="replace")


def portal_base(proxy_port: int) -> str:
    """The public origin this acceptance uses: the front proxy on its own port."""
    return f"http://localhost:{proxy_port}"


def admin_call(socket_path: Path, request: dict, timeout: float = 180.0) -> dict:
    """One newline-delimited JSON round trip with the child's admin channel."""
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(timeout)
        conn.connect(str(socket_path))
        conn.sendall((json.dumps(request) + "\n").encode())
        buffer = b""
        while b"\n" not in buffer:
            chunk = conn.recv(65536)
            if not chunk:
                break
            buffer += chunk
    if not buffer:
        raise Failure(f"admin channel returned no response for {request.get('op')}")
    return json.loads(buffer.decode().splitlines()[0])


def wait_for(description: str, predicate, timeout: float = 30.0, interval: float = 0.2):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            last = predicate()
        except Exception as exc:  # pragma: no cover - diagnostic only
            last = exc
        if last:
            return last
        time.sleep(interval)
    raise Failure(f"timed out waiting for {description} (last: {last!r})")


def http_status(port: int, path: str = "/api", timeout: float = 5.0) -> int:
    """Raw HTTP GET against a loopback port; 0 means "nothing listening"."""
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=timeout) as conn:
            conn.sendall(f"GET {path} HTTP/1.0\r\nHost: 127.0.0.1:{port}\r\n\r\n".encode())
            data = conn.recv(64)
    except OSError:
        return 0
    try:
        return int(data.split(b" ")[1])
    except (IndexError, ValueError):
        return 0


def http_probe(port: int, path: str = "/", timeout: float = 5.0, method: str = "GET",
               origin: str = "") -> tuple[int, str]:
    """GET one loopback port; returns (status, location). 0 means "nothing bound".

    With no nginx in this shape, dshgw binds the portal port and every tenant's
    public port itself, so these ports are the user-facing surface.
    """
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=timeout) as conn:
            # A browser always sends Origin on an unsafe method, and dshgw's CSRF
            # fence requires it: omitting it here would test a request no browser
            # makes, and a 403 would say nothing about the session contract.
            request = (
                f"{method} {path} HTTP/1.0\r\nHost: localhost:{port}\r\n"
                + (f"Origin: {origin}\r\n" if origin else "")
                + "Content-Length: 0\r\n\r\n"
            )
            conn.sendall(request.encode())
            chunks = []
            while True:
                chunk = conn.recv(65536)
                if not chunk:
                    break
                chunks.append(chunk)
                if len(b"".join(chunks)) > 65536:
                    break
    except OSError:
        return 0, ""
    raw = b"".join(chunks)
    header, _, _ = raw.partition(b"\r\n\r\n")
    lines = header.decode(errors="replace").splitlines()
    status = 0
    if lines:
        parts = lines[0].split()
        if len(parts) >= 2 and parts[1].isdigit():
            status = int(parts[1])
    location = ""
    for line in lines[1:]:
        if line.lower().startswith("location:"):
            location = line.split(":", 1)[1].strip()
    return status, location


def process_children(pid: int) -> list[int]:
    try:
        output = subprocess.run(
            ["ps", "-o", "pid=", "--ppid", str(pid)], capture_output=True, text=True, check=False
        ).stdout
    except OSError:
        return []
    return [int(line) for line in output.split() if line.strip().isdigit()]


def process_tree(pid: int) -> list[int]:
    seen, queue = [], [pid]
    while queue:
        current = queue.pop()
        for child in process_children(current):
            if child not in seen:
                seen.append(child)
                queue.append(child)
    return seen


def command_line(pid: int) -> str:
    try:
        return Path(f"/proc/{pid}/cmdline").read_bytes().replace(b"\0", b" ").decode(errors="replace")
    except OSError:
        return ""


def prepare_template(node: str, dsh_root: str, home: Path, check: Check) -> Path:
    """Produce a tenant template without network access.

    The production template (deploy/dshgw/prepare-template.sh) adds the
    dsh-browser-fs plugin, which needs the npm registry. This acceptance only
    needs a profile dsh can boot, so it asks dsh to write its own base profile;
    the deployment config then runs with plugin_browser_fs: off.
    """
    template = home / "template-home"
    profile = template / "profiles/web/package.json"
    if profile.exists():
        return template
    template.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, HOME=str(template), DSH_HOME=str(template))
    result = subprocess.run(
        [node, f"{dsh_root}/lib/bin.js", "--profile", "web", "--dump-config"],
        env=env,
        capture_output=True,
        text=True,
        timeout=180,
        check=False,
    )
    if not profile.exists():
        raise Failure(
            "dsh could not produce a base profile: "
            + (result.stderr or result.stdout or "no output").strip()[:400]
        )
    check.note(f"template prepared at {template} (browser-fs plugin not installed)")
    return template


def write_config(path: Path, *, aigw_port: int, stub_url: str, state_dir: Path, template: Path,
                 node: str, dsh_root: str, plugin_path: Path, ports: dict,
                 memory_max: int, tasks_max: int, cpu_quota: int, feishu_url: str,
                 public_base_url: str) -> None:
    doc = f"""server:
  listen: 127.0.0.1:{aigw_port}
  secret_key: supervised-e2e-secret
database:
  path: {state_dir}/aigw.db
credentials_key: "2jGxUI1VPSsMQgx0lbhv5OuGywKnCDUN"
bootstrap:
  mode: upsert
  admin:
    username: e2e-admin
    password: e2e-password
log:
  level: debug
feishu:
  enabled: true
  app_id: cli_e2e
  app_secret: secret
  callback_url: http://localhost:{aigw_port}/feishu/callback
  authorize_url: {feishu_url}/authorize
  token_url: {feishu_url}/token
  userinfo_url: {feishu_url}/userinfo
  dsh_login: true
  portal_url: ""
  state_ttl_s: 600
  ticket_ttl_s: 120
dshgw:
  enabled: true
  state_dir: {state_dir}
  public_host: localhost
  listen: 127.0.0.1:{ports['listen']}
  portal_port: {ports['portal']}
  tenant_port_lo: {ports['tenant_lo']}
  tenant_port_hi: {ports['tenant_hi']}
  worker_port_lo: {ports['worker_lo']}
  worker_port_hi: {ports['worker_hi']}
  aigw_base_url: {stub_url}
  node_bin: {node}
  current_link: {dsh_root}
  template_home: {template}
  plugin_path: {plugin_path}
  plugin_browser_fs: off
  public_base_url: {public_base_url}
  tenant_path_prefix: /t
  portal_path_prefix: /dshgw
  public_scheme: http
  worker_memory_max_bytes: {memory_max}
  worker_tasks_max: {tasks_max}
  worker_cpu_quota_percent: {cpu_quota}
"""
    path.write_text(doc)


def check_feishu_login(check: Check, aigw_port: int, proxy_port: int, tenant: str,
                       feishu: "StubFeishu", upstream: "StubAigw", database: Path) -> None:
    """Bind an API key to a Feishu account, then use it to sign in to the portal.

    This is the acceptance for the whole M60/M61 handoff, and it runs the real binaries: aigw
    performs the OAuth exchange against the stub consent page and mints a ticket, the child
    verifies that ticket with its injected secret, and the session it issues is the same one a
    key login produces. The two flags the console normally sets through the DSH provisioning
    channel (an account being opted in, and which tenant it maps to) are written directly,
    because that path belongs to the other milestone and would rotate this tenant's worker key
    against the stub.

    Only the binding and the login are asserted here; the binding's own rules (uniqueness,
    conflicts, role checks) are covered by the Go tests, which is the right place for them.
    """
    import sqlite3
    import urllib.parse

    admin = Browser()
    base = f"http://localhost:{aigw_port}"
    admin_origin = base
    status, _, _ = admin.request("POST", f"{base}/admin/api/v1/auth/login",
                                 body={"username": "e2e-admin", "password": "e2e-password"},
                                 origin=admin_origin)
    if not check.require("feishu-admin-login", status == 200, f"HTTP {status}"):
        return

    status, _, body = admin.request("POST", f"{base}/admin/api/v1/accounts",
                                    body={"name": "e2e-feishu", "billing_mode": "postpaid"},
                                    origin=admin_origin)
    if not check.require("feishu-account-created", status in (200, 201), f"HTTP {status} {body[:200]}"):
        return
    account_id = json.loads(body).get("id")
    status, _, body = admin.request("POST", f"{base}/admin/api/v1/keys",
                                    body={"name": "e2e-feishu-key", "account_id": account_id},
                                    origin=admin_origin)
    if not check.require("feishu-key-created", status in (200, 201), f"HTTP {status} {body[:200]}"):
        return
    key_id = json.loads(body).get("id")


    # --- binding, through the console and the consent page --------------------------------
    # One hop: the console's own route (where the admin cookie is scoped) mints the state and
    # sends the browser to Feishu. The harness carries cookie paths, so a second hop through a
    # public route would fail here exactly as it did in a browser.
    status, headers, _ = admin.request("GET", f"{base}/admin/api/v1/keys/{key_id}/feishu/bind", origin=admin_origin)
    authorize = headers.get("location", "")
    check.require("feishu-bind-entry", status == 302 and authorize.startswith(feishu.base_url),
                  f"HTTP {status} location={authorize!r}")
    parsed = urllib.parse.urlsplit(authorize)
    query = dict(part.split("=", 1) for part in parsed.query.split("&") if "=" in part)
    check.require("feishu-registered-callback",
                  urllib.parse.unquote(query.get("redirect_uri", "")) == f"http://localhost:{aigw_port}/feishu/callback",
                  f"redirect_uri={query.get('redirect_uri')!r}")
    # The consent page sends the browser back with a code.
    status, headers, _ = admin.request("GET", authorize)
    callback = headers.get("location", "")
    status, headers, _ = admin.request("GET", callback)
    bind_location = headers.get("location", "")
    check.require("feishu-bind-callback", status == 303 and "feishu=bound" in bind_location,
                  f"HTTP {status} location={bind_location!r}")
    # Binding also opts the account in to DSH, through the same local provisioning channel the
    # console's button uses, so the person can sign in immediately.
    check.require("feishu-bind-enables-dsh", "dsh=enabled" in bind_location, f"location={bind_location!r}")
    with sqlite3.connect(database) as conn:
        bound = conn.execute("SELECT feishu_open_id, feishu_name FROM api_keys WHERE id = ?", (key_id,)).fetchone()
        account_row = conn.execute("SELECT dsh_enabled, dsh_tenant FROM accounts WHERE id = ?", (account_id,)).fetchone()
    check.require("feishu-binding-stored", bound is not None and bound[0] == feishu.open_id, f"row={bound!r}")
    check.require("feishu-account-opted-in", account_row is not None and account_row[0] == 1 and account_row[1],
                  f"account={account_row!r}")
    # The tenant the binding created is the one the login will land in, so the rest of this
    # step follows the URL the binding itself reported.
    bound_tenant = account_row[1]
    check.require("feishu-bound-tenant-is-live",
                  any(t["name"] == bound_tenant
                      for t in json.loads((database.parent / "registry.json").read_text())["tenants"]),
                  f"tenant={bound_tenant!r}")

    # --- signing in to the portal with that identity --------------------------------------
    # The portal is reached through the front door, on the URL the child itself generates.
    portal = f"http://localhost:{proxy_port}/dshgw"
    visitor = Browser()
    status, _, page = visitor.request("GET", portal + "/")
    check.require("feishu-portal-offers-login",
                  status == 200 and "飞书登录" in page and f"http://localhost:{aigw_port}/feishu/login" in page,
                  f"HTTP {status}")

    status, headers, _ = visitor.request("GET", f"{base}/feishu/login")
    authorize = headers.get("location", "")
    if not check.require("feishu-login-authorize", status == 302 and authorize.startswith(feishu.base_url),
                         f"HTTP {status} location={authorize!r}"):
        return
    status, headers, _ = visitor.request("GET", authorize)
    callback = headers.get("location", "")
    status, headers, _ = visitor.request("GET", callback)
    location = headers.get("location", "")
    check.require("feishu-login-returns-to-portal",
                  status == 303 and location.startswith(portal + "/login/feishu"),
                  f"HTTP {status} location={location!r}")
    # The ticket travelled as a host-only cookie, which is what makes it reach the portal on
    # its own port; the URL must not carry it.
    check.require("feishu-ticket-not-in-url", "ticket=" not in location, location)

    status, headers, _ = visitor.request("GET", location)
    tenant_location = headers.get("location", "")
    check.require("feishu-login-starts-tenant-session",
                  status == 302 and tenant_location == f"{portal_base(proxy_port)}/t/{bound_tenant}/",
                  f"HTTP {status} location={tenant_location!r}")
    session = visitor.cookies.get(f"dshgw_s_{bound_tenant}", ("", ""))[0]
    check.require("feishu-session-issued", bool(session), f"cookies={sorted(visitor.cookies)}")
    # The front proxy keeps its own registry snapshot (reload ~2s), and this tenant was created
    # by the binding a moment ago, so the first fetch can legitimately answer "unknown tenant".
    # Waiting for the proxy to catch up is part of the assertion, not a workaround.
    tenant_ui = wait_for("the front proxy to see the new tenant",
                         lambda: (visitor.request("GET", tenant_location)[0] == 200),
                         timeout=30)
    status, _, page = visitor.request("GET", tenant_location)
    check.require("feishu-tenant-serves-ui", tenant_ui is True and status == 200, f"HTTP {status} body={page[:80]!r}")
    check.require("feishu-entitlement-rechecked", upstream.authorize_calls > 0,
                  f"POST /v1/dshgw/authorize calls: {upstream.authorize_calls}")

    # --- refusals: unbound, and revoked between issue and redemption ----------------------
    status, _, _ = admin.request("DELETE", f"{base}/admin/api/v1/keys/{key_id}/feishu", origin=admin_origin)
    check.require("feishu-unbind", status == 200, f"HTTP {status}")
    stranger = Browser()
    status, headers, _ = stranger.request("GET", f"{base}/feishu/login")
    status, headers, _ = stranger.request("GET", headers.get("location", ""))
    status, headers, _ = stranger.request("GET", headers.get("location", ""))
    refusal_target = headers.get("location", "")
    status, _, page = stranger.request("GET", refusal_target)
    check.require("feishu-unbound-refused",
                  "feishu/error?reason=unbound" in refusal_target and "尚未绑定" in page,
                  f"HTTP {status} location={refusal_target!r} body={page[:120]!r}")

    # Revocation after aigw issued the ticket: the child asks again, and an upstream that says
    # no must stop the login rather than let it through.
    with sqlite3.connect(database) as conn:
        conn.execute("UPDATE api_keys SET feishu_open_id = ?, feishu_name = ? WHERE id = ?",
                     (feishu.open_id, feishu.name, key_id))
    upstream.deny = True
    try:
        revoked = Browser()
        status, headers, _ = revoked.request("GET", f"{base}/feishu/login")
        status, headers, _ = revoked.request("GET", headers.get("location", ""))
        status, headers, _ = revoked.request("GET", headers.get("location", ""))
        status, _, page = revoked.request("GET", headers.get("location", ""))
        check.require("feishu-revocation-refused",
                      status == 403 and "未启用 dsh" in page,
                      f"HTTP {status} body={page[:120]!r}")
        check.require("feishu-revocation-issues-no-session",
                      f"dshgw_s_{bound_tenant}" not in revoked.cookies, f"cookies={sorted(revoked.cookies)}")
    finally:
        upstream.deny = False
    # ...and the same identity works again once the upstream allows it, which proves the
    # refusal above came from the check and not from a broken chain.
    restored = Browser()
    status, headers, _ = restored.request("GET", f"{base}/feishu/login")
    status, headers, _ = restored.request("GET", headers.get("location", ""))
    status, headers, _ = restored.request("GET", headers.get("location", ""))
    status, headers, _ = restored.request("GET", headers.get("location", ""))
    check.require("feishu-login-recovers", status == 302 and f"dshgw_s_{bound_tenant}" in restored.cookies,
                  f"HTTP {status} cookies={sorted(restored.cookies)}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--aigw-bin", required=True)
    parser.add_argument("--dshgw-bin", required=True)
    parser.add_argument("--node", default=os.environ.get("DSHGW_NODE", ""))
    parser.add_argument("--dsh-root", default=os.environ.get("DSHGW_DSH_ROOT", ""))
    parser.add_argument("--gwproxy-bin", default="bin/gwproxy",
                        help="front proxy binary for the single-domain path check")
    parser.add_argument("--workdir", default="")
    parser.add_argument("--keep", action="store_true", help="keep the work directory for inspection")
    parser.add_argument("--verbose", action="store_true")
    args = parser.parse_args()

    verbose = args.verbose
    check = Check(verbose)

    aigw_bin = Path(args.aigw_bin).resolve()
    dshgw_bin = Path(args.dshgw_bin).resolve()
    for binary in (aigw_bin, dshgw_bin):
        if not binary.is_file() or not os.access(binary, os.X_OK):
            print(f"SKIP: {binary} is not an executable (run make build dshgw-build)", file=sys.stderr)
            return 0
    # The sibling rule is part of the shape: dshgw is found next to aigw.
    sibling = aigw_bin.parent / dshgw_bin.name
    if sibling != dshgw_bin:
        print(f"SKIP: dshgw must sit next to aigw ({sibling}), got {dshgw_bin}", file=sys.stderr)
        return 0

    node, dsh_root = args.node, args.dsh_root
    if not node or not Path(node).is_file() or not dsh_root or not Path(f"{dsh_root}/lib/bin.js").is_file():
        print("SKIP: set DSHGW_NODE and DSHGW_DSH_ROOT to a staged Node and dsh release", file=sys.stderr)
        return 0
    if not Path("/usr/bin/bwrap").exists():
        print("SKIP: /usr/bin/bwrap is missing", file=sys.stderr)
        return 0

    plugin = Path(__file__).resolve().parent.parent / "cmd/dshgw/plugin/picker-clamp.js"
    if not plugin.is_file():
        print(f"SKIP: picker plugin not built at {plugin}", file=sys.stderr)
        return 0

    work = Path(args.workdir).resolve() if args.workdir else Path(
        subprocess.run(["mktemp", "-d", "/tmp/dshgw-supervised.XXXXXX"], capture_output=True, text=True, check=True).stdout.strip()
    )
    state_dir = work / "dshgw"
    state_dir.mkdir(parents=True, exist_ok=True)
    config_path = work / "aigw.yaml"
    log_path = work / "aigw.log"
    admin_socket = state_dir / "admin.sock"

    ports = {
        "listen": free_port(),
        "portal": free_port(),
        "tenant_lo": free_port(),
        "tenant_hi": 0,
        "worker_lo": free_port(),
        "worker_hi": 0,
    }
    ports["tenant_hi"] = ports["tenant_lo"] + 20
    ports["worker_hi"] = ports["worker_lo"] + 20
    tenant_worker_port = ports["worker_lo"]
    TENANT_PORT_FALLBACK[0] = ports["portal"]

    stub = StubAigw(models=["e2e-model-a", "e2e-model-b"], tenant="e2e-supervised")
    feishu_stub = StubFeishu()
    aigw_port = free_port()
    # The front proxy's port is part of the public URLs the child generates, so it is chosen
    # before the child is configured rather than inside the path-mode step.
    proxy_port = free_port()
    public_base_url = f"http://localhost:{proxy_port}"
    aigw: subprocess.Popen | None = None
    aigw_log = None
    tenant = "e2e-supervised"
    try:
        template = prepare_template(node, dsh_root, work, check)
        write_config(
            config_path,
            aigw_port=aigw_port,
            feishu_url=feishu_stub.base_url,
            stub_url=stub.base_url,
            state_dir=state_dir,
            template=template,
            node=node,
            dsh_root=dsh_root,
            plugin_path=plugin,
            ports=ports,
            memory_max=MEMORY_MAX_BYTES,
            tasks_max=TASKS_MAX,
            cpu_quota=CPU_QUOTA_PERCENT,
            public_base_url=public_base_url,
        )
        stub.start()
        feishu_stub.start()
        aigw_log = log_path.open("wb")
        aigw = subprocess.Popen(
            [str(aigw_bin), "--config", str(config_path)], stdout=aigw_log, stderr=subprocess.STDOUT,
            start_new_session=True,
        )
        check.require("aigw-starts", wait_for_aigw(aigw, state_dir, admin_socket, check, verbose), "child ready")
        check.require("admin-socket-same-uid", admin_socket.exists() and
                      (admin_socket.stat().st_mode & 0o777) == 0o600,
                      f"mode {oct(admin_socket.stat().st_mode & 0o777) if admin_socket.exists() else 'missing'}")

        ping = admin_call(admin_socket, {"id": 1, "op": "ping"})
        check.require("admin-ping", ping.get("ok") is True, json.dumps(ping))

        created = admin_call(admin_socket, {"id": 2, "op": "tenant-create", "name": tenant, "key": "sk-aaaaaaaaa-rest"})
        check.require("tenant-create", created.get("ok") is True, created.get("error", ""))
        if not created.get("ok"):
            raise Failure("tenant creation failed; later steps cannot run")

        check.require(
            "worker-no-os-account",
            subprocess.run(["getent", "passwd", f"dsh-{tenant}"], capture_output=True).returncode != 0,
            "no per-tenant OS account exists",
        )
        status = wait_for("worker /api 401", lambda: http_status(tenant_worker_port) == 401, timeout=PROBE_TIMEOUT)
        check.ok("worker-api-401", f"port {tenant_worker_port} answered 401")
        handshake = state_dir / "handshake" / f"{tenant}.url"
        check.require("handshake-written", handshake.exists() and (handshake.stat().st_mode & 0o777) == 0o600,
                      str(handshake))

        # The worker must be a bubblewrap child of the child process, as the
        # account that runs aigw — not a systemd unit and not another user.
        child_pid = int(wait_for("child pid", lambda: child_pid_of(aigw.pid, state_dir) or 0, timeout=20))
        tree = process_tree(child_pid)
        bwrap_children = [pid for pid in tree if "bwrap" in command_line(pid)]
        check.require("worker-is-bwrap-child", bool(bwrap_children),
                      f"descendants of pid {child_pid}: {len(tree)}")
        owners = {os.stat(f"/proc/{pid}").st_uid for pid in [child_pid, *bwrap_children] if Path(f"/proc/{pid}").exists()}
        check.require("single-unprivileged-uid", owners == {os.getuid()}, f"uids={owners}, expected {{{os.getuid()}}}")

        # The public surface: dshgw binds the portal port and this tenant's public
        # port itself (no nginx). A session-less tenant request must be sent back to
        # the portal, and the portal must serve the login page.
        portal_status, _ = http_probe(ports["portal"])
        check.require("portal-port-serves-login", portal_status == 200, f"HTTP {portal_status}")
        # The portal is reached either on its port (default mode) or at its path
        # (single-domain mode): both are correct, and which one appears is decided by
        # dshgw's public_base_url, not by this test.
        tenant_status, location = http_probe(ports["tenant_lo"])
        portal_target = f":{ports['portal']}" in location or "/dshgw/" in location
        check.require(
            "tenant-port-redirects-to-portal",
            tenant_status == 302 and portal_target,
            f"HTTP {tenant_status} location={location!r}",
        )

        # Resource limits: each worker gets its own cgroup v2 group with the
        # configured ceilings. A host that cannot provide one is reported as a
        # skip (the worker still runs), matching the degradation the runner logs.
        cgroup_note = check_worker_limits(check, process_tree(child_pid))

        # Single-domain mode: no subdomains, no ports — the front proxy serves the
        # portal and the tenant under path prefixes, while dshgw generates the URLs
        # (redirects, session cookie path) that make those paths work.
        # The Feishu identity chain (M60 + M61) runs against the real binaries while the front
        # door is up: bind a key through the console and the stub consent page, then sign in to
        # the portal with it and land inside the tenant.
        check_path_mode(
            check, Path(args.gwproxy_bin).resolve(), state_dir, ports["listen"],
            ports["tenant_lo"], tenant, "localhost", proxy_port, public_base_url,
            meanwhile=lambda port: check_feishu_login(
                check, aigw_port, port, tenant, feishu_stub, stub, state_dir / "aigw.db"))

        calls_before = stub.calls
        stopped = admin_call(admin_socket, {"id": 3, "op": "tenant-stop", "name": tenant})
        check.require("tenant-stop", stopped.get("ok") is True, json.dumps(stopped))
        check.require("stopped-port-closed", wait_for("worker port to close",
                      lambda: http_status(tenant_worker_port, timeout=1) == 0, timeout=30) is True, "port closed")

        started = admin_call(admin_socket, {"id": 4, "op": "tenant-start", "name": tenant})
        check.require("tenant-start", started.get("ok") is True, json.dumps(started))
        check.require("restarted-api-401", wait_for("worker /api 401 after restart",
                      lambda: http_status(tenant_worker_port, timeout=2) == 401, timeout=PROBE_TIMEOUT) is True, "")
        check.require("models-refreshed-before-start", stub.calls > calls_before,
                      f"/v1/models calls {calls_before} -> {stub.calls}")

        # A restarted aigw brings unsuspended tenants back with it (this replaced
        # systemd's "enabled units come back").
        stop_process(aigw, check)
        aigw_log.close()
        aigw_log = log_path.open("ab")
        aigw = subprocess.Popen(
            [str(aigw_bin), "--config", str(config_path)], stdout=aigw_log, stderr=subprocess.STDOUT,
            start_new_session=True,
        )
        check.require("restart-child-ready", wait_for_aigw(aigw, state_dir, admin_socket, check, verbose), "")
        check.require("tenant-restored-after-restart",
                      wait_for("worker /api 401 after aigw restart",
                               lambda: http_status(tenant_worker_port, timeout=2) == 401, timeout=PROBE_TIMEOUT) is True,
                      "unsuspended tenant came back")

        # Shutdown must take the whole tree with it.
        children_before = process_tree(aigw.pid)
        stop_process(aigw, check)
        aigw = None
        leftover = wait_for("no leftover processes",
                            lambda: [pid for pid in children_before if Path(f"/proc/{pid}").exists()] == [],
                            timeout=30)
        check.require("shutdown-leaves-no-orphans", leftover is True,
                      f"{len(children_before)} descendants checked")
    except Failure as failure:
        check.fail("harness", str(failure))
    finally:
        if aigw is not None:
            stop_process(aigw, check, quiet=True)
        stub.stop()
        feishu_stub.stop()
        if aigw_log is not None:
            aigw_log.close()
        if not args.keep:
            shutil.rmtree(work, ignore_errors=True)
        else:
            print(f"work directory kept at {work}")

    print()
    if check.failures:
        print(f"FAIL: {len(check.failures)} step(s) failed, {check.passes} passed")
        for failure in check.failures:
            print(f"  - {failure}")
        return 1
    print(f"PASS: supervised shape acceptance ({check.passes} steps)")
    return 0


def check_path_mode(check: Check, proxy_bin: Path, state_dir: Path, dshgw_listen: int,
                    tenant_port: int, tenant: str, public_host: str, proxy_port: int,
                    public_origin: str, meanwhile=None) -> None:
    """Drive the single-domain path chain: proxy -> dshgw -> worker.

    This is the shape to use when subdomains are not available: one domain, one
    port, and a path per tenant. It is the only test that proves dshgw's
    generated URLs agree with the proxy's prefixes.
    """
    if not proxy_bin.is_file():
        check.ok("path-mode-skipped", "gwproxy not built (make gwproxy-build)")
        return
    config = state_dir.parent / "gwproxy.yaml"
    config.write_text(
        f"""listen: 127.0.0.1:{proxy_port}
public_host: {public_host}
aigw_prefix: /aigw
aigw_upstream: http://127.0.0.1:1
portal_prefix: /dshgw
portal_upstream: http://127.0.0.1:{dshgw_listen}
portal_port: {TENANT_PORT_FALLBACK[0]}
tenant_prefix: /t
tenant_upstream: http://127.0.0.1:{dshgw_listen}
registry_path: {state_dir}/registry.json
"""
    )
    process = subprocess.Popen([str(proxy_bin), "--config", str(config)],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                               start_new_session=True)
    try:
        started = wait_for("front proxy port", lambda: http_probe(proxy_port)[0] != 0, timeout=15)
        check.require("path-mode-proxy-up", started is True, f"proxy on {proxy_port}")

        portal_status, _ = http_probe(proxy_port, "/dshgw/")
        check.require("path-mode-portal", portal_status == 200, f"HTTP {portal_status}")

        # Unauthenticated tenant access must land on the portal *path*, not on a
        # portal host:port — that redirect is generated by dshgw from public_base_url.
        status, location = http_probe(proxy_port, f"/t/{tenant}/")
        check.require("path-mode-tenant-redirects-to-portal-path",
                      status in (302, 303) and "/dshgw/" in location,
                      f"HTTP {status} location={location!r}")

        # An unauthenticated GET is a redirect by design; the 401 contract belongs to
        # non-GET requests (a browser navigation is not a data request).
        api, _ = http_probe(proxy_port, f"/t/{tenant}/api", method="POST",
                            origin=public_origin)
        check.require("path-mode-tenant-api-401", api == 401, f"HTTP {api}")

        # Anything that needs the front door runs here, while it is up: that is how a person
        # reaches the portal in this deployment.
        if meanwhile is not None:
            meanwhile(proxy_port)
    finally:
        stop_process(process, check, quiet=True)
        _ = tenant_port


def check_worker_limits(check: Check, descendants: list[int]) -> str:
    """Read the worker's cgroup back from the kernel and compare with the config."""
    worker = None
    for pid in descendants:
        line = command_line(pid)
        if "bin.js" in line and "web --port" in line:
            worker = pid
    if worker is None:
        worker = descendants[0] if descendants else None
    if worker is None:
        check.fail("worker-cgroup-limits", "no worker process found")
        return ""
    try:
        path = Path(f"/proc/{worker}/cgroup").read_text().splitlines()[0].split("::", 1)[1].strip()
    except (OSError, IndexError) as exc:
        check.fail("worker-cgroup-limits", f"cannot read the worker cgroup: {exc}")
        return ""
    base = Path("/sys/fs/cgroup") / path.lstrip("/")
    limits = {}
    for name in ("memory.max", "pids.max", "cpu.max"):
        try:
            limits[name] = (base / name).read_text().strip()
        except OSError:
            limits[name] = ""
    if not any(limits.values()):
        check.ok("worker-cgroup-limits", f"host has no usable worker cgroup (skipped), cgroup={path}")
        return path
    # systemd renders CPUQuota as a "quota period" pair over a 100 ms period.
    want_cpu = f"{CPU_QUOTA_PERCENT * 1000} 100000"
    check.require(
        "worker-cgroup-limits",
        limits["memory.max"] == str(MEMORY_MAX_BYTES)
        and limits["pids.max"] == str(TASKS_MAX)
        and limits["cpu.max"] == want_cpu,
        f"cgroup={path} memory.max={limits['memory.max']!r} pids.max={limits['pids.max']!r} cpu.max={limits['cpu.max']!r}",
    )
    return path


def child_pid_of(aigw_pid: int | None, state_dir: Path) -> int | None:
    """The dshgw child of aigw, found by its generated config path."""
    if aigw_pid is None:
        return None
    config = str(state_dir / "config.yaml")
    for pid in process_tree(aigw_pid):
        line = command_line(pid)
        if "dshgw" in line and config in line:
            return pid
    return None


def wait_for_aigw(aigw: subprocess.Popen | None, state_dir: Path, admin_socket: Path, check: Check,
                  verbose: bool) -> bool:
    """Wait for the child to be configured, started and ready."""
    if aigw is None:
        return False
    try:
        wait_for("admin socket", lambda: admin_socket.exists(), timeout=CHILD_TIMEOUT, interval=0.3)
        wait_for("child process", lambda: child_pid_of(aigw.pid, state_dir), timeout=CHILD_TIMEOUT, interval=0.3)
    except Failure:
        return False
    check.note(f"child pid {child_pid_of(aigw.pid, state_dir)} with admin socket {admin_socket}")
    return True


def stop_process(process: subprocess.Popen | None, check: Check, quiet: bool = False) -> None:
    if process is None or process.poll() is not None:
        return
    process.send_signal(signal.SIGTERM)
    try:
        process.wait(timeout=40)
    except subprocess.TimeoutExpired:
        if not quiet:
            check.note("aigw ignored SIGTERM; killing it")
        process.kill()
        process.wait(timeout=10)


if __name__ == "__main__":
    sys.exit(main())
