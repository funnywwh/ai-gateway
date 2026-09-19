#!/usr/bin/env python3
"""End-to-end acceptance: does the browser workspace actually MOUNT?

What this proves, on the gateway host itself, through a real Chromium:

  1. the tenant GUI's 浏览器工作区 row hands a directory to the plugin and the gateway
     answers with a real mount point over the same-origin reverse channel;
  2. that path is real kernel state — `fuse.browser-workspace` in /proc/self/mounts —
     inside the account's own workspace, and the gateway's state record says ready;
  3. file I/O really travels to the browser: a file the browser wrote is readable
     through the mount, a file written through the mount is readable in the browser;
  4. the mount is visible INSIDE the account's bubblewrap sandbox (the assumption the
     whole design rests on), reads and writes both work there, and the rest of the
     host stays hidden;
  5. DSH registers the workspace and the row reports 已挂载;
  6. clicking the row again detaches the mount, removes the mount point, and leaves the
     browser's own directory intact.

The OS directory chooser is the ONE step no automation can drive (showDirectoryPicker
needs a real gesture and a system dialog). It is replaced by a REAL
FileSystemDirectoryHandle from the browser's origin-private filesystem, so every step
after the choice — permission query, FSA executor, reverse long-poll, FUSE, sandbox
binding, workspace registration — is the production path, and the click is a real CDP
input event whose transient user activation is asserted at picker time.

It runs a throwaway dshgw in its own state directory and port band (13xxx, outside the
live 18xxx/32xxx ranges), with a stub aigw, so the live deployment is untouched.
It skips itself (exit 0 with SKIP) wherever a precondition is missing.

    go build -o /tmp/dshgw-e2e ./cmd/dshgw
    python3 scripts/browser_workspace_mount_e2e.py --dshgw /tmp/dshgw-e2e [--keep]

Pass `--dshgw` a binary built from THIS tree: the committed `bin/dshgw` can predate the
fixes this script verifies. Run it on the gateway host — FUSE user mounts, bubblewrap and
the installed dsh runtime must all be available.
"""

from __future__ import annotations

import argparse
import contextlib
import json
import os
import pwd
import queue
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import websocket

REPO = Path(__file__).resolve().parent.parent

# Deliberately outside every live range (18xxx / 32xxx) so a stray run cannot collide
# with the deployment this repository serves.
LISTEN = "127.0.0.1:13899"
PORTAL_PORT = 13800
TENANT_PORT_LO, TENANT_PORT_HI = 13801, 13820
WORKER_PORT_LO, WORKER_PORT_HI = 13821, 13840
TENANT = "bw-e2e"
PICKED = "picked"
BROWSER_FILE = "from-browser.txt"
HOST_FILE = "from-host.txt"
SANDBOX_FILE = "from-sandbox.txt"
BROWSER_TEXT = "hello from the browser\n"
HOST_TEXT = "written on the gateway host\n"
SANDBOX_TEXT = "written inside the sandbox\n"

LOCAL_NODE = "/home/winger/.local/node-v22.23.1-linux-x64/bin/node"
LOCAL_DSH = "/home/winger/.local/dsh-0.1.2-rc.1"
LOCAL_CHROME = "/home/winger/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome"


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
        time.sleep(0.4)
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
    """The three answers dshgw needs from aigw, and nothing else.

    `/v1/models` backs key validation and tenant creation, `/v1/dshgw/authorize` is the
    account-level dsh entitlement check the portal login always performs. Pointing at a
    stub keeps this acceptance away from the live gateway's keys, which must never be
    bound to a second tenant.
    """

    def __init__(self) -> None:
        tenant = TENANT

        class Handler(BaseHTTPRequestHandler):
            def _json(self, status: int, payload: dict) -> None:
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
                self._json(200, {"data": [{"id": "bw-e2e-model", "name": "bw-e2e-model"}]})

            def do_POST(self) -> None:  # noqa: N802 - http.server's spelling
                if self.path.split("?")[0] != "/v1/dshgw/authorize":
                    self.send_error(404)
                    return
                if not self.headers.get("Authorization", "").startswith("Bearer "):
                    self.send_error(401)
                    return
                self._json(200, {"allowed": True, "tenant": tenant})

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


def config_document(args, root: Path, template_home: Path, aigw_base_url: str) -> dict:
    return {
        # Loopback HTTP: the browser reaches the tenant GUI directly, and 127.0.0.1 is a
        # secure context, which is what the plugin checks before offering the picker.
        "public_host": "127.0.0.1",
        "public_scheme": "http",
        "listen": LISTEN,
        "portal_port": PORTAL_PORT,
        "tenant_port_lo": TENANT_PORT_LO,
        "tenant_port_hi": TENANT_PORT_HI,
        "worker_port_lo": WORKER_PORT_LO,
        "worker_port_hi": WORKER_PORT_HI,
        "state_dir": str(root / "state"),
        "session_path": str(root / "state" / "sessions.json"),
        "audit_path": str(root / "state" / "audit.jsonl"),
        "activity_path": str(root / "state" / "activity.json"),
        "aigw_base_url": aigw_base_url,
        "key_revalidate": "off",
        "directory_picker": "clamp",
        # The live template pins no browser-fs plugin; the tenant-side filesystem surface
        # under test here is the browser workspace, not dsh-browser-fs.
        "plugin_browser_fs": "off",
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
    }


# ── browser side ─────────────────────────────────────────────────────────────────────

# Replaces the one step no automation can drive. The replacement is NOT a stub object:
# it is the browser's own origin-private filesystem handle, a real
# FileSystemDirectoryHandle with real createWritable/move/queryPermission behaviour, so
# the plugin's executor, the gateway's FUSE and the sandbox all run the production path.
PICKER_OVERRIDE = r'''(() => {
  window.__bwPicker = { replaced: true, calls: 0, mode: null, active: null, name: null,
    kind: null, isHandle: null, permission: null, error: null };
  // The gateway contract is observed from the page, not inferred: every same-origin
  // browser-workspace POST the plugin makes, with its status.
  window.__bwFetch = [];
  const realFetch = window.fetch.bind(window);
  window.fetch = (input, init) => {
    const url = typeof input === 'string' ? input : (input && input.url) || '';
    const record = { url: String(url), method: (init && init.method) || 'GET', status: null };
    if (record.url.includes('browser-workspace')) window.__bwFetch.push(record);
    return realFetch(input, init).then(
      response => { record.status = response.status; return response },
      error => { record.status = 'error'; throw error });
  };
  window.showDirectoryPicker = async (options) => {
    const spy = window.__bwPicker;
    spy.calls += 1;
    spy.mode = options && options.mode;
    spy.active = navigator.userActivation ? navigator.userActivation.isActive : null;
    try {
      const storageRoot = await navigator.storage.getDirectory();
      const dir = await storageRoot.getDirectoryHandle('picked', {create: true});
      spy.name = dir.name;
      spy.kind = dir.kind;
      spy.isHandle = dir instanceof FileSystemDirectoryHandle;
      spy.permission = dir.queryPermission ? await dir.queryPermission({mode: 'readwrite'}) : null;
      return dir;
    } catch (error) { spy.error = String(error); throw error; }
  };
  return true;
})()'''

# The row is one compact button like the ssh-workspace one: icon, label, state note.
ROW = r'''(() => {
  const buttons = [...document.querySelectorAll('button')].filter(
    b => b.textContent.includes('浏览器工作区'));
  const b = buttons.find(b => {
    const r = b.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && b.checkVisibility({checkOpacity:true, checkVisibilityCSS:true});
  });
  if (!b) return {found:false};
  const footer = b.closest('[class*="_footerActions"]');
  const box = b.getBoundingClientRect();
  return {found:!!footer, text:b.textContent.trim(), disabled:!!b.disabled,
    x: Math.round(box.left + box.width / 2), y: Math.round(box.top + box.height / 2)};
})()'''

# Where to click: the row itself must be the hit target, or a modal mask would swallow it.
HIT_POINT = r'''(() => {
  const button = [...document.querySelectorAll('button')].find(b => b.textContent.includes('浏览器工作区'));
  if (!button) return {found: false, blocker: 'no row'};
  const box = button.getBoundingClientRect();
  for (let fx = 0.5; fx >= 0.1; fx -= 0.2) {
    for (let fy = 0.5; fy >= 0.1; fy -= 0.2) {
      const x = box.left + box.width * fx, y = box.top + box.height * fy;
      const hit = document.elementFromPoint(x, y);
      if (hit === button || button.contains(hit)) return {found: true, x: Math.round(x), y: Math.round(y)};
    }
  }
  const blocker = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
  return {found: false, blocker: blocker ? blocker.tagName + '.' + String(blocker.className || '') : null};
})()'''

# DSH's own first-run notices are modal and cover the sidebar foot until dismissed.
DIALOG_BUTTON = r'''(() => {
  const mask = document.querySelector('[class*="_mask_"]');
  const dialog = mask && mask.parentElement;
  if (!dialog) return {found: false};
  const buttons = [...dialog.querySelectorAll('button')].filter(b => {
    const r = b.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  });
  const button = buttons.find(b => /(later|continue|ok|got it|知道了|继续|关闭|稍后)/i.test(b.textContent.trim()))
    || (buttons.length === 1 ? buttons[0] : null);
  if (!button) return {found: false};
  const box = button.getBoundingClientRect();
  return {found: true, label: button.textContent.trim(), x: Math.round(box.left + box.width / 2), y: Math.round(box.top + box.height / 2)};
})()'''

LOGIN_FORM = r'''(() => {
  const input = document.querySelector('input[name="key"]');
  if (!input) return {found: false};
  const form = input.closest('form');
  const submit = form && form.querySelector('button[type="submit"]');
  const box = submit && submit.getBoundingClientRect();
  return {found: true, hasButton: !!submit,
    x: box ? Math.round(box.left + box.width / 2) : 0,
    y: box ? Math.round(box.top + box.height / 2) : 0};
})()'''

PICKER_STATE = 'window.__bwPicker'


def browser_write(path: str, text: str) -> str:
    """Write a file through the browser's own FSA handle (what the executor would do)."""
    return (
        "(async () => {"
        " const root = await navigator.storage.getDirectory();"
        # create:true so the pre-mount seed file lands in the same directory the picker
        # will hand to the plugin.
        f" const dir = await root.getDirectoryHandle({PICKED!r}, {{create: true}});"
        f" const fh = await dir.getFileHandle({path!r}, {{create: true}});"
        " const w = await fh.createWritable();"
        f" await w.write({text!r}); await w.close();"
        " return {written: (await fh.getFile()).size};"
        "})()"
    )


def browser_read(path: str) -> str:
    """Read a file back through the browser's own FSA handle."""
    return (
        "(async () => {"
        " const root = await navigator.storage.getDirectory();"
        f" const dir = await root.getDirectoryHandle({PICKED!r});"
        " const names = [];"
        " for await (const [name] of dir.entries()) names.push(name);"
        " let content = null, error = null;"
        f" try {{ const fh = await dir.getFileHandle({path!r}); content = await (await fh.getFile()).text(); }}"
        " catch (e) { error = e.name + ': ' + e.message; }"
        " return {names, content, error};"
        "})()"
    )


class CDP:
    def __init__(self, url: str):
        self.sock = websocket.create_connection(url, timeout=60, suppress_origin=True)
        self.serial = 0
        self.exceptions: list = []
        self.console_errors: list = []
        # Transport-agnostic trace of everything that could carry a remote call: the
        # DSH client may use fetch, XHR or a WebSocket, and a hung call leaves no
        # response at all — which is exactly the case worth seeing.
        self.network: list = []

    def _event(self, kind: str, data: dict) -> None:
        if kind == "Network.requestWillBeSent":
            request = data.get("request", {})
            self.network.append({"kind": "request", "method": request.get("method"),
                                 "url": request.get("url"), "status": None})
        elif kind == "Network.responseReceived":
            response = data.get("response", {})
            self.network.append({"kind": "response", "url": response.get("url"),
                                 "status": response.get("status")})
        elif kind == "Network.loadingFailed":
            self.network.append({"kind": "failed", "url": data.get("requestId"),
                                 "status": data.get("errorText")})
        elif kind in ("Network.webSocketFrameSent", "Network.webSocketFrameReceived"):
            payload = data.get("response", {}).get("payloadData", "")
            self.network.append({"kind": kind.split("Frame")[1].lower(), "url": "ws",
                                 "status": payload[:600]})
        if len(self.network) > 400:
            del self.network[:200]

    def call(self, method: str, params: dict | None = None) -> dict:
        self.serial += 1
        self.sock.send(json.dumps({"id": self.serial, "method": method, "params": params or {}}))
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            try:
                message = json.loads(self.sock.recv())
            except websocket.WebSocketTimeoutException:
                raise RuntimeError("CDP response timed out: " + method) from None
            kind, data = message.get("method"), message.get("params", {})
            if kind == "Runtime.exceptionThrown":
                self.exceptions.append(data.get("exceptionDetails", {}))
            elif kind == "Runtime.consoleAPICalled" and data.get("type") in ("error", "assert"):
                self.console_errors.append(data)
            elif kind == "Log.entryAdded" and data.get("entry", {}).get("level") == "error":
                self.console_errors.append(data["entry"])
            elif kind and kind.startswith("Network."):
                self._event(kind, data)
            if message.get("id") == self.serial:
                if "error" in message:
                    raise RuntimeError("CDP command failed: " + method + ": " + json.dumps(message["error"]))
                return message.get("result", {})
        raise RuntimeError("CDP command timed out: " + method)

    def evaluate(self, expression: str, await_promise: bool = False):
        result = self.call("Runtime.evaluate", {"expression": expression, "returnByValue": True, "awaitPromise": await_promise})
        if "exceptionDetails" in result:
            raise AssertionError("page JavaScript raised: " + json.dumps(result["exceptionDetails"])[:400])
        return result.get("result", {}).get("value")

    def click(self, x: int, y: int) -> None:
        """A real input event, not a synthetic element.click()."""
        for kind in ("mousePressed", "mouseReleased"):
            self.call("Input.dispatchMouseEvent", {"type": kind, "x": x, "y": y, "button": "left", "clickCount": 1})
            time.sleep(0.05)

    def url(self) -> str:
        return self.evaluate("location.href") or ""


def diagnose(cdp, root: Path) -> str:
    """What the page and the gateway were doing when a wait gave up."""
    parts = []
    with contextlib.suppress(Exception):
        parts.append("page: " + json.dumps({
            "url": cdp.evaluate("location.href"),
            "title": cdp.evaluate("document.title"),
            "text": (cdp.evaluate("document.body ? document.body.innerText : null") or "")[:400],
        }))
    # The full trace goes to a file: it is far too large for a failure line, and it is the
    # only record of a remote call that never came back.
    with contextlib.suppress(Exception):
        (root / "diagnostics.json").write_text(json.dumps({
            "row": cdp.evaluate(ROW), "picker": cdp.evaluate(PICKER_STATE),
            "exceptions": cdp.exceptions, "console_errors": cdp.console_errors,
            "network": cdp.network,
        }, ensure_ascii=False, indent=2), encoding="utf-8")
        parts.append(f"full trace: {root / 'diagnostics.json'}")
    log = root / "dshgw.log"
    if log.exists():
        parts.append("gateway log tail: " + "\n".join(log.read_text(encoding="utf-8", errors="replace").splitlines()[-25:]))
    return " | ".join(parts)


def admin_call(socket_path: Path, request: dict, timeout: float = 120) -> dict:
    """One newline-delimited JSON request on the daemon's own admin socket.

    The tenant is created through the RUNNING daemon, not the CLI: only this path tells
    the edge to bind the new tenant's public port, so a CLI-created tenant is not
    reachable from a browser until the daemon restarts.
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


def stop_group(proc):
    """Stop the entire fixture process group, including Chromium descendants."""
    if proc is None:
        return
    with contextlib.suppress(ProcessLookupError):
        os.killpg(proc.pid, signal.SIGTERM)
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        pass
    with contextlib.suppress(ProcessLookupError):
        os.killpg(proc.pid, signal.SIGKILL)
    with contextlib.suppress(subprocess.TimeoutExpired):
        proc.wait(timeout=5)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", default=str(REPO / ".cache/m65-browser-e2e"))
    parser.add_argument("--dshgw", default=str(REPO / "bin/dshgw"))
    parser.add_argument("--node", default=os.environ.get("DSHGW_NODE") or (LOCAL_NODE if Path(LOCAL_NODE).exists() else "node"))
    parser.add_argument("--bin-js", default=os.environ.get("DSHGW_BIN_JS") or str(Path(LOCAL_DSH) / "lib/bin.js"))
    parser.add_argument("--current-link", default=os.environ.get("DSHGW_DSH_ROOT") or LOCAL_DSH)
    parser.add_argument("--chrome", default=os.environ.get("CHROME_BIN") or (LOCAL_CHROME if Path(LOCAL_CHROME).exists() else "chromium"))
    parser.add_argument("--template-home", default=str(REPO / "data/dshgw-verify/template-home"))
    parser.add_argument("--keep", action="store_true", help="leave the throwaway deployment behind")
    parser.add_argument("--skip-teardown", action="store_true", help="stop before the close/unmount step")
    args = parser.parse_args()

    root = Path(args.root).resolve()
    steps: list[str] = []

    def note(message: str) -> None:
        steps.append(message)
        print(f"  · {message}", flush=True)

    server = chrome = cdp = None
    stub = StubAigw()
    stub.__enter__()
    log = None
    chrome_log = None
    try:
        # ── preconditions ────────────────────────────────────────────────────────────
        need("bwrap")
        need("fusermount3")
        if not Path("/dev/fuse").exists():
            raise Skip("/dev/fuse is missing: browser workspaces need user FUSE mounts")
        if not Path(args.dshgw).exists():
            raise Skip(f"{args.dshgw} is not built (make dshgw-build)")
        if not Path(args.bin_js).exists() or not Path(args.node).exists():
            raise Skip("the installed dsh runtime (node + bin.js) was not found")
        if not Path(args.chrome).exists() and shutil.which(args.chrome) is None:
            raise Skip(f"no Chromium at {args.chrome}")
        template_home = Path(args.template_home)
        if not (template_home / "profiles").exists():
            raise Skip(f"no prepared dsh template at {template_home} (deploy/dshgw/prepare-template.sh)")

        if root.exists():
            subprocess.run(["pkill", "-f", f"{root}/dshgw.yaml serve"], capture_output=True)
            for stale in mounts_under(root):
                subprocess.run(["fusermount3", "-u", stale], capture_output=True)
                subprocess.run(["fusermount3", "-z", stale], capture_output=True)
            shutil.rmtree(root, ignore_errors=True)
        root.mkdir(parents=True)
        host, port = LISTEN.split(":")
        if port_open(host, int(port)):
            raise Skip(f"{LISTEN} is already in use: stop the process holding it")

        config_path = root / "dshgw.yaml"
        config_path.write_text(json.dumps(config_document(args, root, template_home, stub.base_url), indent=2) + "\n", encoding="utf-8")
        config_path.chmod(0o600)

        # The gateway must not inherit this session's DSH_* variables: its workers would.
        env = os.environ.copy()
        for name in tuple(env):
            if name.startswith("DSH_") or name in ("NODE_OPTIONS", "NODE_PATH"):
                env.pop(name)

        # ── the throwaway gateway ────────────────────────────────────────────────────
        log = open(root / "dshgw.log", "w", encoding="utf-8")
        server = subprocess.Popen([args.dshgw, "--config", str(config_path), "serve"],
                                  stdout=log, stderr=subprocess.STDOUT, cwd=str(REPO), env=env)
        wait_for(lambda: port_open(host, int(port)), 30, "the test gateway to listen")
        note(f"throwaway dshgw listening on {LISTEN} (state {root / 'state'})")

        key_file = root / "tenant.key"
        key_file.write_text("sk-bw-e2e-0000000000\n", encoding="utf-8")
        key_file.chmod(0o600)
        admin_socket = root / "state" / "admin.sock"
        wait_for(lambda: admin_socket.exists() or None, 30, "the daemon's admin socket")
        created = admin_call(admin_socket, {"id": 1, "op": "tenant-create", "name": TENANT,
                                            "key": "sk-bw-e2e-0000000000", "allow_empty_models": True})
        if not created.get("ok"):
            raise AssertionError(f"tenant create failed: {created}")
        if created.get("result", {}).get("name") != TENANT:
            raise AssertionError(f"unexpected tenant-create answer: {created}")

        registry = json.loads((root / "state" / "registry.json").read_text(encoding="utf-8"))
        record = next(item for item in registry["tenants"] if item["name"] == TENANT)
        workspace, public_port = Path(record["workspace"]), record["public_port"]
        patch = (Path(record["dsh_home"]) / "profiles" / "web" / "cordis.patch.yml").read_text(encoding="utf-8")
        if "browser-workspace" not in patch:
            raise AssertionError("the tenant profile patch does not carry the browser-workspace row")
        note(f"account {TENANT}: workspace {workspace}, tenant origin http://127.0.0.1:{public_port}")
        note("the tenant's rendered profile patch carries the browser-workspace row")

        # ── real Chromium ────────────────────────────────────────────────────────────
        profile = root / "chrome"
        profile.mkdir(mode=0o700)
        chrome_log = open(root / "chrome.log", "w")
        chrome = subprocess.Popen([args.chrome, "--headless=new", "--no-sandbox", "--disable-gpu",
                                   "--no-first-run", "--no-default-browser-check",
                                   "--disable-background-networking", "--window-size=1440,1000",
                                   "--remote-debugging-port=0", "--remote-allow-origins=*",
                                   "--user-data-dir=" + str(profile), "about:blank"],
                                  cwd=str(root), env=env, stdout=chrome_log, stderr=chrome_log,
                                  start_new_session=True)
        portfile = profile / "DevToolsActivePort"
        wait_for(lambda: portfile.exists() or None, 30, "Chromium to publish its debugging port")
        debug_port = int(portfile.read_text().splitlines()[0])
        with urllib.request.urlopen(f"http://127.0.0.1:{debug_port}/json/list", timeout=10) as resp:
            target = next(p for p in json.load(resp) if p["type"] == "page")
        cdp = CDP(target["webSocketDebuggerUrl"])
        cdp.call("Page.enable")
        cdp.call("Runtime.enable")
        cdp.call("Log.enable")
        cdp.call("Network.enable")
        cdp.call("Page.addScriptToEvaluateOnNewDocument", {"source": PICKER_OVERRIDE})
        note("real Chromium started; the OS directory chooser is replaced by a real OPFS handle")

        # ── login through the portal, like a person ──────────────────────────────────
        cdp.call("Page.navigate", {"url": f"http://127.0.0.1:{PORTAL_PORT}/"})
        form = wait_for(lambda: (lambda f: f if f.get("found") else None)(cdp.evaluate(LOGIN_FORM)), 30, "the portal login form")
        cdp.evaluate(f"document.querySelector('input[name=\"key\"]').value = {'sk-bw-e2e-0000000000'!r}")
        cdp.click(form["x"], form["y"])
        # The first tenant request starts the account's DSH worker, so the redirect only
        # commits once that worker is up: this is the slowest wait in the run.
        deadline = time.time() + 240
        while time.time() < deadline and f"127.0.0.1:{public_port}" not in cdp.url():
            time.sleep(2)
        if f"127.0.0.1:{public_port}" not in cdp.url():
            raise AssertionError("the tenant GUI never loaded: " + diagnose(cdp, root))
        note(f"portal login accepted; the browser is on the tenant GUI ({cdp.url()})")

        # ── the tenant GUI boots, then its sidebar row ───────────────────────────────
        def row():
            value = cdp.evaluate(ROW)
            return value if value.get("found") else None

        def dismiss_dialogs(limit=6):
            dismissed = []
            for _ in range(limit):
                value = cdp.evaluate(DIALOG_BUTTON)
                if not value.get("found"):
                    break
                dismissed.append(value["label"])
                cdp.click(value["x"], value["y"])
                time.sleep(0.6)
            return dismissed

        deadline = time.time() + 180
        while time.time() < deadline:
            dismiss_dialogs()
            if row():
                break
            time.sleep(1)
        else:
            raise AssertionError("the 浏览器工作区 row never appeared in the tenant GUI")
        note("the 浏览器工作区 row is present and enabled in the real tenant GUI")

        # A file the browser owns BEFORE the mount exists: its contents must appear
        # through the FUSE mount, which is what proves the I/O really round-trips.
        wrote = cdp.evaluate(browser_write(BROWSER_FILE, BROWSER_TEXT), await_promise=True)
        if wrote.get("written") != len(BROWSER_TEXT):
            raise AssertionError(f"the browser could not write its own file: {wrote}")
        note(f"the browser wrote {BROWSER_FILE} into its own directory before mounting")

        # ── one real click: picker → open → FUSE mount → worker restart → workspace ──
        point = wait_for(lambda: (lambda p: p if p.get("found") else None)(cdp.evaluate(HIT_POINT)), 15, "the row to be clickable")
        cdp.click(point["x"], point["y"])

        deadline = time.time() + 180
        row_state = None
        while time.time() < deadline:
            value = cdp.evaluate(ROW)
            text = value.get("text", "") if value.get("found") else ""
            if "已挂载" in text:
                row_state = value
                break
            if any(bad in text for bad in ("挂载失败", "需要 HTTPS", "已断线", "清理未确认")):
                raise AssertionError("the row refused the mount: " + text + " | " + diagnose(cdp, root))
            time.sleep(2)
        if row_state is None:
            raise AssertionError("the row never reported 已挂载: " + diagnose(cdp, root))
        picker = cdp.evaluate(PICKER_STATE)
        if picker.get("calls") != 1:
            raise AssertionError(f"the click did not reach the picker exactly once: {picker}")
        if picker.get("mode") != "readwrite":
            raise AssertionError(f"the picker was not asked for readwrite: {picker}")
        if picker.get("active") is not True:
            raise AssertionError(f"the click's transient activation was already spent: {picker}")
        if not picker.get("isHandle") or picker.get("permission") != "granted":
            raise AssertionError(f"the replaced handle is not a real granted FileSystemDirectoryHandle: {picker}")
        note(f"one real click reached the picker once with activation intact ({picker['mode']}, {picker['permission']})")
        note(f"the sidebar row reports: {row_state['text']}")
        calls = cdp.evaluate("window.__bwFetch") or []
        sequence = [f"{c['url'].split('/')[-1]}:{c['status']}" for c in calls]
        if not any(c.startswith("open:200") for c in sequence) or not any(c.startswith("activate:200") for c in sequence):
            raise AssertionError(f"the gateway contract was not completed: {sequence}")
        note("same-origin gateway calls from the page: " + ", ".join(sequence[:12]) + ("…" if len(sequence) > 12 else ""))

        # ── the gateway's own state, and the kernel's ────────────────────────────────
        records_dir = root / "state" / "browser-mounts"

        def ready_record():
            if not records_dir.is_dir():
                return None
            for path in records_dir.glob("*.json"):
                document = json.loads(path.read_text(encoding="utf-8"))
                if document.get("Tenant") == TENANT and document.get("State") == "ready":
                    return document
            return None

        document = wait_for(ready_record, 30, "the gateway's mount record to reach ready")
        mountpoint = Path(document["Path"])
        if workspace not in mountpoint.parents:
            raise AssertionError(f"{mountpoint} is not inside {workspace}")
        fstype = wait_for(lambda: mount_type(str(mountpoint)) or None, 15, "the mount to appear in the kernel table")
        if not fstype.startswith("fuse"):
            raise AssertionError(f"unexpected filesystem type {fstype}")
        note(f"gateway state record ready and the kernel mounts {mountpoint} as {fstype}")

        # ── browser → mount ──────────────────────────────────────────────────────────
        through = wait_for(lambda: (mountpoint / BROWSER_FILE).read_text(encoding="utf-8") if (mountpoint / BROWSER_FILE).exists() else None,
                           20, "the browser's file to be readable through the mount")
        if through != BROWSER_TEXT:
            raise AssertionError(f"reading through the mount returned {through!r}")
        note(f"{BROWSER_FILE} (written by the browser) reads back through the FUSE mount")

        # ── mount → browser ──────────────────────────────────────────────────────────
        (mountpoint / HOST_FILE).write_text(HOST_TEXT, encoding="utf-8")
        back = wait_for(lambda: (lambda v: v if v.get("content") == HOST_TEXT else None)(cdp.evaluate(browser_read(HOST_FILE), await_promise=True)),
                        20, "the host's file to be readable in the browser")
        if BROWSER_FILE not in back["names"]:
            raise AssertionError(f"the browser's directory listing lost its own file: {back}")
        note(f"{HOST_FILE} (written on the host) reads back inside the browser: {back['content']!r}")

        # ── the decisive check: is the mount visible INSIDE the sandbox? ─────────────
        # The profile that matters is the RUNNING worker's, not `sandbox-exec --print`:
        # that CLI process builds its own browser-mount service with no shares in it, so
        # it cannot know about live mounts. The real argv is on the worker's process.
        def worker_argv():
            log_text = (root / "dshgw.log").read_text(encoding="utf-8", errors="replace")
            starts = re.findall(r"tenant worker started tenant=\S+ pid=(\d+)", log_text)
            if not starts:
                return None
            raw = Path(f"/proc/{starts[-1]}/cmdline")
            if not raw.exists():
                return None
            return [part for part in raw.read_bytes().decode("utf-8", "replace").split("\0") if part]

        argv = wait_for(worker_argv, 20, "the restarted worker's own command line")
        if argv[0].endswith("bwrap") is False:
            raise AssertionError(f"the worker is not running under bubblewrap: {argv[0]}")
        separator = argv.index("--")
        bound = [i for i, part in enumerate(argv) if part == "--bind" and argv[i + 1] == str(mountpoint)]
        if not bound:
            raise AssertionError(f"the running worker's profile does not bind the mount point: {argv[:separator]}")
        note("the account's running worker profile binds the mount point read-write")
        user = pwd.getpwuid(os.geteuid()).pw_name
        script = (
            f"cat {mountpoint}/{BROWSER_FILE}; echo ---; ls -1 {mountpoint}; echo ---; "
            f"printf %s '{SANDBOX_TEXT}' > {mountpoint}/{SANDBOX_FILE} && echo WROTE || echo WRITE-FAILED; "
            f"echo ---; ls -1 {mountpoint}; echo ---; "
            f"for probe in /home/{user}/.ssh /home/{user}/work/ai_gateway/config.yaml "
            f"/home/{user}/work/ai_gateway/data; do "
            "[ -e \"$probe\" ] && echo \"LEAK $probe\"; done; echo ---; "
            f"ls -A /home/{user} 2>&1"
        )
        inside = run([*argv[: separator + 1], "/bin/sh", "-c", script])
        detail = f"rc={inside.returncode} stdout={inside.stdout!r} stderr={inside.stderr!r}"
        if inside.returncode != 0:
            raise AssertionError(f"the sandbox run failed: {detail}")
        if BROWSER_TEXT.strip() not in inside.stdout:
            raise AssertionError(f"the mount is NOT visible inside the sandbox: {detail}")
        if BROWSER_FILE not in inside.stdout or HOST_FILE not in inside.stdout:
            raise AssertionError(f"listing the mount inside the sandbox did not show both files: {detail}")
        if "LEAK " in inside.stdout:
            raise AssertionError(f"the mount binding exposed host paths: {detail}")
        if "WRITE-FAILED" in inside.stdout or SANDBOX_FILE not in inside.stdout:
            raise AssertionError(f"the sandbox could not write into the mount: {detail}")
        note("the mount is visible inside the account's own sandbox: it lists both files, reads")
        note("the browser's file, and writes its own file through the mount")
        note("the operator's keys, aigw's config and the live data root stay invisible in that sandbox")

        # ── sandbox → browser: the write really lands in the browser's directory ─────
        landed = wait_for(lambda: (lambda v: v if v.get("content") == SANDBOX_TEXT else None)(cdp.evaluate(browser_read(SANDBOX_FILE), await_promise=True)),
                          20, "the sandbox's write to appear in the browser")
        if SANDBOX_FILE not in landed["names"]:
            raise AssertionError(f"the browser's listing does not show the sandbox's file: {landed}")
        note(f"{SANDBOX_FILE} (written inside the sandbox) is readable in the browser: {landed['content']!r}")

        if args.skip_teardown:
            print(f"PASS(partial): browser workspace mounts end-to-end, unmount step skipped ({len(steps)} steps)")
            return 0

        # ── clicking the row again detaches the mount ────────────────────────────────
        point = wait_for(lambda: (lambda p: p if p.get("found") else None)(cdp.evaluate(HIT_POINT)), 15, "the row to be clickable again")
        cdp.click(point["x"], point["y"])
        wait_for(lambda: (lambda v: v if "已断开" in v.get("text", "") else None)(cdp.evaluate(ROW)), 90, "the row to report 已断开")
        wait_for(lambda: (mount_type(str(mountpoint)) == "") or None, 30, "the kernel to detach the mount")
        wait_for(lambda: (not mountpoint.exists()) or None, 30, "the gateway to remove the mount point")
        survived = cdp.evaluate(browser_read(BROWSER_FILE), await_promise=True)
        if survived.get("content") != BROWSER_TEXT:
            raise AssertionError(f"the browser's own directory did not survive the unmount: {survived}")
        note("the row reports 已断开, the kernel mount is gone, the mount point is removed")
        note("the browser's own directory and its files survived the unmount")

        # ── removing the account purges the rest ─────────────────────────────────────
        removed = run([args.dshgw, "--config", str(config_path), "tenant", "remove", "-purge", "-yes", TENANT], cwd=str(REPO))
        if removed.returncode != 0:
            raise AssertionError(f"tenant remove failed: {removed.stdout}{removed.stderr}")
        if workspace.exists():
            raise AssertionError(f"the purged workspace survived: {workspace}")
        note("removing the account purged its workspace")

        if cdp.exceptions:
            raise AssertionError(f"the tenant page raised JavaScript exceptions: {json.dumps(cdp.exceptions)[:600]}")
        endpoint_errors = [entry for entry in cdp.console_errors if "/browser-workspace/" in json.dumps(entry)]
        if endpoint_errors:
            raise AssertionError(f"a browser-workspace request failed: {json.dumps(endpoint_errors)[:600]}")
        # Expected noise, none of it this feature's:
        #  · a browser fetches <link rel=manifest> WITHOUT credentials, so the tenant proxy
        #    sees no session cookie and redirects it to the portal origin → CORS error;
        #  · mounting and unmounting restart the account's worker by design, so the page's
        #    WebSocket sees one 502 before reconnecting (the row's 已挂载 state above is
        #    asserted AFTER that reconnect).
        manifest = [entry for entry in cdp.console_errors if "manifest" in json.dumps(entry)]
        restarts = [entry for entry in cdp.console_errors if "502" in json.dumps(entry)]
        note("no JavaScript exception and no failed browser-workspace request on the tenant page")
        note(f"expected noise only: {len(manifest)} credential-less manifest/CORS error(s), "
             f"{len(restarts)} worker-restart 502(s)")

        print(f"PASS: browser workspace mounts end-to-end ({len(steps)} steps)")
        return 0
    except Skip as skip:
        print(f"SKIP: {skip}")
        return 0
    except AssertionError as failure:
        print(f"FAIL: {failure}")
        return 1
    finally:
        if cdp is not None:
            with contextlib.suppress(Exception):
                cdp.sock.close()
        stop_group(chrome)
        if root.exists() and not args.keep:
            subprocess.run([args.dshgw, "--config", str(root / "dshgw.yaml"), "tenant", "remove",
                            "-purge", "-yes", TENANT], cwd=str(REPO), capture_output=True, timeout=60)
            for line in (mounts_under(root) if root.exists() else []):
                subprocess.run(["fusermount3", "-u", line], capture_output=True)
                subprocess.run(["fusermount3", "-z", line], capture_output=True)
        stub.__exit__()
        if server is not None and server.poll() is None:
            server.send_signal(signal.SIGTERM)
            try:
                server.wait(timeout=20)
            except subprocess.TimeoutExpired:
                server.kill()
        if chrome_log is not None:
            chrome_log.close()
        if log is not None:
            log.close()
        if not args.keep and root.exists():
            shutil.rmtree(root, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
