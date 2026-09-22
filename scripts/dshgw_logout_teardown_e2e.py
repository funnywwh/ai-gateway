#!/usr/bin/env python3
"""End-to-end acceptance for M76: clicking 退出 force-detaches the mounts and force-stops the dsh.

What it proves, on the gateway host itself, with a throwaway dshgw in its own state directory and
port band (the live deployment is untouched):

  1. an account can have BOTH kinds of mount attached at once — a real browser-directory FUSE
     mount (driven here by a small stand-in for the browser side of the poll protocol, so no
     Chromium is needed) and a real sshfs mount over a loopback ssh;
  2. the browser mount is bound into the account's sandbox (activate), which is exactly the state
     in which a graceful unmount cannot succeed: the worker's own namespace holds the mount;
  3. `POST /dshgw/logout/` (the sidebar's 退出 button) takes BOTH mounts out of the kernel mount
     table and only THEN stops the dsh — the worker is gone, its port is closed, and the audit
     says `logout_mount_detach` + `logout_worker_stop` with no `logout_worker_stop_failed`;
  4. the ssh workspace survives as a RECORD: the next sign-in brings the same remote directory
     back at the same mount point, without another click.

It skips itself (exit 0 with SKIP) wherever the pieces are missing: sshfs, bwrap, a dsh runtime,
a prepared template, or a non-interactive loopback ssh. The busy/EBUSY half of the force ladder is
pinned by the Go test `TestRealFUSEForceUnmountTakesABusyMount` (browserworkspace), which needs no
gateway at all.

    python3 scripts/dshgw_logout_teardown_e2e.py [--root .cache/m76-logout-e2e] [--keep]
"""

from __future__ import annotations

import argparse
import json
import os
import pwd
import re
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent

# Deliberately outside every live range (18xxx/32xxx) and outside the other acceptance scripts'
# bands, so a stray run can never collide with a deployment or with another e2e.
LISTEN = "127.0.0.1:13097"
PORTAL_PORT = 13650
TENANT_PORT_LO, TENANT_PORT_HI = 13651, 13849
WORKER_PORT_LO, WORKER_PORT_HI = 13300, 13499

TENANT = "logout-e2e"
LOGIN_KEY = "sk-logout-e2e-0000000000"


class Skip(Exception):
    """A precondition is missing; this is not a failure of the feature."""


def run(cmd: list[str], **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, capture_output=True, text=True, **kwargs)


def need(binary: str) -> str:
    found = shutil.which(binary)
    if found is None:
        raise Skip(f"{binary} is not installed")
    return found


def wait_for(predicate, timeout: float, what: str):
    deadline = time.time() + timeout
    while time.time() < deadline:
        result = predicate()
        if result:
            return result
        time.sleep(0.3)
    raise AssertionError(f"timed out after {timeout:.0f}s waiting for {what}")


def port_open(host: str, port: int) -> bool:
    with socket.socket() as sock:
        sock.settimeout(0.5)
        return sock.connect_ex((host, port)) == 0


def mount_type(mountpoint: str) -> str:
    """The filesystem mounted exactly at mountpoint, or "" — read from /proc/self/mounts."""
    try:
        with open("/proc/self/mounts", encoding="utf-8") as handle:
            for line in handle:
                fields = line.split()
                if len(fields) >= 3 and fields[1].replace("\\040", " ") == mountpoint:
                    return fields[2]
    except OSError:
        return ""
    return ""


def mounts_under(root: Path) -> list[str]:
    """Every mount point at or below root, so an interrupted run can be cleaned up."""
    found = []
    prefix = str(root)
    try:
        with open("/proc/self/mounts", encoding="utf-8") as handle:
            for line in handle:
                fields = line.split()
                if len(fields) >= 3 and fields[1].replace("\\040", " ").startswith(prefix):
                    found.append(fields[1].replace("\\040", " "))
    except OSError:
        return []
    return found


class StubAigw:
    """The two answers the portal and tenant creation need: a model list and an admission.

    A stub keeps this acceptance away from the live gateway's keys, which must never be bound to
    a second tenant.
    """

    def __init__(self) -> None:
        class Handler(BaseHTTPRequestHandler):
            def _json(self, payload: dict, status: int = 200) -> None:
                body = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self) -> None:  # noqa: N802 - http.server's spelling
                if self.path.split("?")[0] != "/v1/models":
                    self.send_error(404)
                    return
                self._json({"data": [{"id": "logout-e2e-model", "name": "logout-e2e-model"}]})

            def do_POST(self) -> None:  # noqa: N802 - http.server's spelling
                if self.path.split("?")[0] != "/v1/dshgw/authorize":
                    self.send_error(404)
                    return
                self._json({"allowed": True, "tenant": TENANT, "account": "e2e", "feishu_name": "E2E"})

            def log_message(self, *_: object) -> None:
                return

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    @property
    def base_url(self) -> str:
        host, port = self.server.server_address[:2]
        return f"http://{host}:{port}"

    def __enter__(self) -> "StubAigw":
        self.thread.start()
        return self

    def __exit__(self, *_: object) -> None:
        self.server.shutdown()
        self.server.server_close()


class BrowserSide:
    """A stand-in for the browser half of the poll protocol, backed by a local directory.

    The real client is a Chromium page with a File System Access handle; what the gateway needs
    from it is exactly two things — the long-poll connection that IS the browser side of the
    mount, and answers to the file requests the kernel forwards. This class provides both from a
    temporary directory, which is what makes this acceptance runnable without a browser while
    still exercising the real FUSE mount, the real profile binding and the real teardown.
    """

    def __init__(self, origin: str, cookie: str, root: Path) -> None:
        self.origin, self.cookie, self.root = origin, cookie, root
        self.token = ""
        self.mountpoint = ""
        self.stop = threading.Event()
        self.thread: threading.Thread | None = None
        self.answered = 0
        self.error: Exception | None = None

    def call(self, op: str, body: dict, timeout: float = 30.0) -> dict:
        request = urllib.request.Request(
            f"{self.origin}/browser-workspace/{op}",
            data=json.dumps(body).encode(),
            headers={
                "Content-Type": "application/json",
                "Origin": self.origin,
                "Cookie": self.cookie,
            },
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=timeout) as response:
            document = json.loads(response.read().decode())
        if document.get("ok") is not True:
            raise AssertionError(f"browser-workspace {op} failed: {document}")
        return document.get("value") or {}

    def open(self, key: str) -> None:
        self.call("hello", {})
        self.call("allocate", {"name": key})
        value = self.call("open", {"name": key, "writable": True, "key": key})
        self.token, self.mountpoint = value["token"], value["mountpoint"]
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.thread.start()

    def activate(self) -> None:
        """Bind the mount into the account's sandbox (the worker restarts for it)."""
        self.call("activate", {"token": self.token}, timeout=120)

    def _serve(self) -> None:
        try:
            while not self.stop.is_set():
                try:
                    value = self.call("poll", {"token": self.token}, timeout=30)
                except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError, OSError):
                    # The mount was detached (or the page would have to reconnect): stop.
                    return
                for request in value.get("requests") or []:
                    self.call("respond", {"token": self.token, "id": request["id"], "result": self._answer(request)})
                    self.answered += 1
        except Exception as error:  # pragma: no cover - surfaced by the assertions
            self.error = error

    def _answer(self, request: dict) -> dict:
        """The file operations this stand-in supports: enough for the sandbox to stat and list."""
        op, rel = request.get("op"), request.get("path") or ""
        target = self.root / rel if rel else self.root
        try:
            if op == "stat":
                info = target.stat()
                kind = "directory" if target.is_dir() else "file"
                return {"ok": True, "value": {"kind": kind, "size": info.st_size, "lastModified": int(info.st_mtime * 1000)}}
            if op == "list":
                entries = [
                    {
                        "name": entry.name,
                        "kind": "directory" if entry.is_dir() else "file",
                        "size": entry.stat().st_size,
                    }
                    for entry in sorted(target.iterdir())
                ]
                return {"ok": True, "value": {"kind": "directory", "entries": entries}}
            if op == "read":
                with open(target, "rb") as handle:
                    handle.seek(request.get("offset") or 0)
                    data = handle.read(request.get("size") or 4096)
                import base64

                return {"ok": True, "value": {"data": base64.b64encode(data).decode(), "bytes": len(data)}}
            if op == "flush":
                target.stat()
                return {"ok": True, "value": {"kind": "file"}}
        except FileNotFoundError:
            return {"ok": False, "error": {"code": "ENOENT", "message": f"{rel} does not exist"}}
        except OSError as error:
            return {"ok": False, "error": {"code": "EIO", "message": str(error)}}
        return {"ok": False, "error": {"code": "ENOTSUP", "message": f"{op} is not supported by this stand-in"}}

    def close(self) -> None:
        self.stop.set()
        if self.token:
            try:
                self.call("close", {"token": self.token}, timeout=10)
            except Exception:
                pass


def config_document(args, root: Path, template_home: Path, aigw_base_url: str) -> dict:
    return {
        # Loopback HTTP: the portal and tenant origins are reached directly, and the Host header
        # has to match public_host — Dispatch picks portal or tenant from it.
        "public_host": "127.0.0.1",
        "public_scheme": "http",
        "listen": LISTEN,
        "portal_port": PORTAL_PORT,
        "tenant_port_lo": TENANT_PORT_LO,
        "tenant_port_hi": TENANT_PORT_HI,
        "worker_port_lo": WORKER_PORT_LO,
        "worker_port_hi": WORKER_PORT_HI,
        "aigw_base_url": aigw_base_url,
        "admin_socket": str(root / "state" / "admin.sock"),
        # The template this acceptance is given (the live deployment's) does not pin the legacy
        # dsh-browser-fs plugin, exactly like the deployment, which runs with it off. The M65
        # browser WORKSPACE is a different feature and stays enabled.
        "plugin_browser_fs": "off",
        "key_revalidate": "off",
        "directory_picker": "clamp",
        "state_dir": str(root / "state"),
        # Written where the assertions below read it (the default is state/gateway/audit.jsonl).
        "audit_path": str(root / "state" / "audit.jsonl"),
        "dsh": {
            "node_bin": args.node,
            "bin_js": os.path.realpath(args.bin_js),
            "current_link": args.current_link,
        },
        "deploy": {
            "plugin_path": str(REPO / "cmd/dshgw/plugin/picker-clamp.js"),
            "template_home": str(template_home),
            "public_listen": "127.0.0.1",
            "gateway_user": pwd.getpwuid(os.geteuid()).pw_name,
            "worker_user": pwd.getpwuid(os.geteuid()).pw_name,
        },
        "browser_workspaces": {"enabled": True},
        "account_card": {"enabled": True},
        "ssh_workspaces": {
            "enabled": True,
            "mount_subdir": "ssh",
            "identity_dir": str(root / "ssh-keys"),
            "ssh_config_dir": str(root / "ssh-configs"),
            "hosts": ["127.0.0.1"],
            "connect_timeout": "10s",
            "poll_interval": "1s",
            "max_entries": 100,
            "sshfs_options": ["reconnect", "ServerAliveInterval=15", "ServerAliveCountMax=3", "idmap=user"],
        },
    }


def login(portal_origin: str, key: str) -> str:
    """Sign in through the portal's key form and return the tenant's session cookie."""
    body = urllib.parse.urlencode({"key": key}).encode()
    request = urllib.request.Request(
        f"{portal_origin}/login",
        data=body,
        headers={"Content-Type": "application/x-www-form-urlencoded", "Origin": portal_origin},
        method="POST",
    )
    opener = urllib.request.build_opener(NoRedirect())
    try:
        with opener.open(request, timeout=60) as response:
            cookies = response.headers.get_all("Set-Cookie") or []
    except urllib.error.HTTPError as error:
        # A login answers 302 to the tenant origin; `NoRedirect` surfaces that as an HTTPError,
        # and the cookie is on the response either way.
        if error.code not in (301, 302, 303, 307, 308):
            raise
        cookies = error.headers.get_all("Set-Cookie") or []
    for cookie in cookies:
        if cookie.startswith("dshgw_s_"):
            return cookie.split(";", 1)[0]
    raise AssertionError(f"no tenant session cookie in {cookies}")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_: object, **__: object):  # noqa: D102 - see the class docstring
        return None


def admin_call(socket_path: Path, request: dict, timeout: float = 180) -> dict:
    """One newline-delimited JSON request on the daemon's own admin socket.

    The tenant is created through the RUNNING daemon, not the CLI: only this path tells the edge
    to bind the new tenant's public port, so a CLI-created tenant is not reachable from a browser
    (and this acceptance logs in through the portal) until the daemon restarts.
    """
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(timeout)
        conn.connect(str(socket_path))
        conn.sendall(json.dumps(request).encode() + b"\n")
        data = b""
        while not data.endswith(b"\n"):
            chunk = conn.recv(65536)
            if not chunk:
                break
            data += chunk
    if not data.strip():
        raise AssertionError(f"the admin channel answered nothing to {request['op']}")
    return json.loads(data.decode())


def audit_events(state: Path) -> list[dict]:
    path = state / "audit.jsonl"
    if not path.exists():
        return []
    events = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if line.strip():
            events.append(json.loads(line))
    return events


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", default=str(REPO / ".cache/m76-logout-e2e"))
    parser.add_argument("--dshgw", default=str(REPO / "bin/dshgw"))
    parser.add_argument("--node", default=os.environ.get("DSHGW_NODE", ""))
    parser.add_argument("--bin-js", default=os.environ.get("DSHGW_BIN_JS", ""))
    parser.add_argument("--current-link", default=os.environ.get("DSHGW_DSH_ROOT", ""))
    parser.add_argument("--template-home", default=str(REPO / "data/dshgw-verify/template-home"))
    parser.add_argument("--identity", default="~/.ssh/id_rsa")
    parser.add_argument("--keep", action="store_true", help="leave the throwaway deployment behind")
    args = parser.parse_args()

    root = Path(args.root).resolve()
    steps: list[str] = []

    def note(message: str) -> None:
        steps.append(message)
        print(f"  · {message}", flush=True)

    server: subprocess.Popen | None = None
    browser: BrowserSide | None = None
    stub = StubAigw()
    stub.__enter__()
    try:
        need("ssh")
        need("sshfs")
        need("bwrap")
        if not Path(args.dshgw).exists():
            raise Skip(f"{args.dshgw} is not built (make dshgw-build)")
        if not args.node or not args.bin_js or not args.current_link:
            raise Skip("set DSHGW_NODE, DSHGW_BIN_JS and DSHGW_DSH_ROOT to the installed dsh runtime")
        identity = Path(args.identity).expanduser()
        if not identity.exists():
            raise Skip(f"no ssh identity at {identity}")
        template_home = Path(args.template_home)
        if not (template_home / "profiles").exists():
            raise Skip(f"no prepared dsh template at {template_home} (deploy/dshgw/prepare-template.sh)")

        remote = root / "remote"
        if root.exists():
            shutil.rmtree(root)
        remote.mkdir(parents=True)
        (remote / "marker.txt").write_text("from the remote host\n", encoding="utf-8")
        probe = run([
            "ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new",
            "-o", "ConnectTimeout=5", "-i", str(identity), "--", "127.0.0.1", "test", "-d", str(remote),
        ])
        if probe.returncode != 0:
            raise Skip("the ssh server cannot see this directory: run this on the gateway host, not in a sandbox")

        config_path = root / "dshgw.yaml"
        config_path.parent.mkdir(parents=True, exist_ok=True)
        subprocess.run(["pkill", "-f", f"{config_path} serve"], capture_output=True)
        host, port = LISTEN.split(":")
        if port_open(host, int(port)):
            raise Skip(f"{LISTEN} is already in use: stop the process holding it")
        for stale in mounts_under(root):
            subprocess.run(["fusermount3", "-u", "-z", stale], capture_output=True)
        config_path.write_text(json.dumps(config_document(args, root, template_home, stub.base_url), indent=2) + "\n", encoding="utf-8")
        config_path.chmod(0o600)

        me = pwd.getpwuid(os.geteuid()).pw_name
        seeds = root / "ssh-configs"
        seeds.mkdir(parents=True, exist_ok=True)
        (seeds / TENANT).write_text(f"Host e2e-seed\n  HostName 127.0.0.1\n  User {me}\n", encoding="utf-8")
        keys = root / "ssh-keys"
        keys.mkdir(parents=True, exist_ok=True)
        keys.chmod(0o700)
        (keys / TENANT).write_bytes(identity.read_bytes())
        (keys / TENANT).chmod(0o600)

        # ── the throwaway gateway ────────────────────────────────────────────────────────
        log = open(root / "dshgw.log", "w", encoding="utf-8")
        server = subprocess.Popen(
            [args.dshgw, "--config", str(config_path), "serve"],
            stdout=log, stderr=subprocess.STDOUT, cwd=str(REPO),
        )
        wait_for(lambda: port_open(host, int(port)), 30, "the test gateway to listen")
        note(f"throwaway dshgw listening on {LISTEN} (state {root / 'state'})")

        admin_socket = root / "state" / "admin.sock"
        wait_for(lambda: admin_socket.exists() or None, 30, "the daemon's admin socket")
        created = admin_call(admin_socket, {
            "id": 1, "op": "tenant-create", "name": TENANT, "key": LOGIN_KEY,
            "allow_empty_models": True,
        })
        if not created.get("ok") or created.get("result", {}).get("name") != TENANT:
            raise AssertionError(f"tenant create failed: {created}")
        registry = json.loads((root / "state" / "registry.json").read_text(encoding="utf-8"))
        record = next(item for item in registry["tenants"] if item["name"] == TENANT)
        workspace, dsh_home = Path(record["workspace"]), Path(record["dsh_home"])
        worker_port = int(record["worker_port"])
        note(f"account {TENANT}: workspace {workspace}, worker port {worker_port}")

        portal_origin = f"http://127.0.0.1:{PORTAL_PORT}"
        tenant_origin = f"http://127.0.0.1:{record['public_port']}"
        cookie = login(portal_origin, LOGIN_KEY)
        wait_for(lambda: port_open("127.0.0.1", worker_port), 90, "the worker the login started")
        note("signed in through the portal; the tenant's dsh is up")

        # ── a real browser-directory mount, bound into the sandbox ───────────────────────
        local = root / "local"
        (local / "sub").mkdir(parents=True)
        (local / "sub" / "local.txt").write_text("from the browser side\n", encoding="utf-8")
        browser = BrowserSide(tenant_origin, cookie, local)
        browser.open("logout_e2e_dir")
        mountpoint = browser.mountpoint
        fstype = wait_for(lambda: mount_type(mountpoint) or None, 10, "the browser mount to appear")
        if fstype != "fuse.browser-workspace":
            raise AssertionError(f"{mountpoint} is {fstype}, want fuse.browser-workspace")
        note(f"{mountpoint} is {fstype}")

        # Activating binds it into the account's sandbox: from here on the worker's own namespace
        # holds the mount, which is why a graceful unmount cannot succeed and the forced ladder is
        # what takes it out (the shape the deployment hit on 2026-09-22).
        browser.activate()
        wait_for(lambda: port_open("127.0.0.1", worker_port), 90, "the worker the binding restart started")
        if mount_type(mountpoint) != "fuse.browser-workspace":
            raise AssertionError("the browser mount vanished while the worker restarted")
        note("the browser mount is bound into the account's sandbox")

        # ── and a real sshfs mount next to it ────────────────────────────────────────────
        request_dir = dsh_home / "ssh-requests"
        request_dir.mkdir(parents=True, exist_ok=True)
        request = {"id": "open-e2e", "op": "open", "host": "127.0.0.1", "remote": str(remote),
                   "createdAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
        payload = request_dir / "open-e2e.json"
        payload.write_text(json.dumps(request), encoding="utf-8")
        payload.chmod(0o600)

        def ssh_reply() -> dict | None:
            answer = dsh_home / "ssh-replies" / "open-e2e.json"
            if not answer.exists():
                return None
            document = json.loads(answer.read_text(encoding="utf-8"))
            return document if document.get("ok") is True else None

        answer = wait_for(ssh_reply, 120, "the gateway to answer the ssh mount request")
        ssh_mountpoint = answer["mountpoint"]
        ssh_fstype = wait_for(lambda: mount_type(ssh_mountpoint) or None, 15, "the sshfs mount to appear")
        if not ssh_fstype.startswith("fuse.sshfs"):
            raise AssertionError(f"{ssh_mountpoint} is {ssh_fstype}, want fuse.sshfs")
        note(f"{ssh_mountpoint} is {ssh_fstype}")

        # ── the click: 退出 ──────────────────────────────────────────────────────────────
        before = sorted(mounts_under(Path(workspace)))
        if len(before) != 2:
            raise AssertionError(f"expected the two mounts, found {before}")
        logout = urllib.request.Request(
            f"{tenant_origin}/dshgw/logout/",
            data=b"",
            headers={"Origin": tenant_origin, "Cookie": cookie},
            method="POST",
        )
        started = time.time()
        opener = urllib.request.build_opener(NoRedirect())
        try:
            opener.open(logout, timeout=120)
        except urllib.error.HTTPError as error:
            if error.code not in (301, 302, 303, 307, 308):
                raise AssertionError(f"logout answered {error.code}") from error
        took = time.time() - started
        note(f"POST /dshgw/logout/ answered in {took:.2f}s")

        # 1. the mounts are gone from the kernel's table.
        wait_for(lambda: not mounts_under(Path(workspace)) or None, 20, "both mounts to leave the mount table")
        left = mounts_under(Path(workspace))
        if left:
            raise AssertionError(f"mounts outlived the logout: {left}")
        note("both mounts left the kernel mount table")

        # 2. and the dsh is gone: no worker port, no worker process.
        wait_for(lambda: not port_open("127.0.0.1", worker_port) or None, 30, "the worker port to close")
        note("the tenant's dsh is stopped (worker port closed)")

        # 3. the audit says what happened, and does not claim a failure.
        events = [event for event in audit_events(root / "state") if event["kind"].startswith("logout")]
        kinds = [event["kind"] for event in events]
        if "logout_mount_detach" not in kinds:
            raise AssertionError(f"the audit does not record the mount detach: {events}")
        if "logout_worker_stop" not in kinds:
            raise AssertionError(f"the audit does not record the verified stop: {events}")
        if "logout_worker_stop_failed" in kinds or "logout_mount_leftover" in kinds:
            raise AssertionError(f"the audit reports a failed teardown: {events}")
        detach = next(event for event in events if event["kind"] == "logout_mount_detach")
        if "2 mount" not in detach["reason"]:
            raise AssertionError(f"the audit should count both mounts: {detach}")
        note(f"audit: {', '.join(kinds)}")

        # 4. the ssh workspace survives as a record (the browser mount does not: its folder is the
        #    page's, and a page that is gone leaves no mount behind).
        ssh_records = json.loads((root / "state" / "ssh-mounts.json").read_text(encoding="utf-8"))["mounts"]
        if [item["mountpoint"] for item in ssh_records] != [ssh_mountpoint]:
            raise AssertionError(f"the ssh record was not kept: {ssh_records}")
        if os.path.exists(ssh_mountpoint) is False:
            raise AssertionError(f"the ssh mount point {ssh_mountpoint} was removed")
        note("the ssh mount record and its mount point are kept")

        # ── signing in again puts the ssh workspace back, at the same path ───────────────
        cookie = login(portal_origin, LOGIN_KEY)
        wait_for(lambda: port_open("127.0.0.1", worker_port), 120, "the worker the second login started")
        restored = wait_for(lambda: mount_type(ssh_mountpoint) or None, 60, "the ssh mount to come back")
        if not restored.startswith("fuse.sshfs"):
            raise AssertionError(f"{ssh_mountpoint} came back as {restored}")
        content = Path(ssh_mountpoint, "marker.txt").read_text(encoding="utf-8")
        if content != "from the remote host\n":
            raise AssertionError(f"reading through the restored mount returned {content!r}")
        note(f"signing in again restored {ssh_mountpoint} at the same path ({restored})")

        print(f"\nPASS: {len(steps)} steps", flush=True)
        for index, step in enumerate(steps, 1):
            print(f"  {index:2d}. {step}", flush=True)
        return 0
    except Skip as skip:
        print(f"SKIP: {skip}", flush=True)
        return 0
    finally:
        if browser is not None:
            browser.close()
        if server is not None and server.poll() is None:
            server.terminate()
            try:
                server.wait(timeout=15)
            except subprocess.TimeoutExpired:
                server.kill()
        for stale in mounts_under(root):
            subprocess.run(["fusermount3", "-u", "-z", stale], capture_output=True)
        if not args.keep:
            shutil.rmtree(root, ignore_errors=True)
        stub.__exit__()


if __name__ == "__main__":
    sys.exit(main())
