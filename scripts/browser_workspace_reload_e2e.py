#!/usr/bin/env python3
"""Does the browser workspace survive a page reload / a closed tab?

scripts/browser_workspace_mount_e2e.py proves the mount works while the page that
created it stays open. This script asks the next question, on the same real fixture
(throwaway dshgw on 13xxx, real Chromium, real OPFS handle, real FUSE, real bubblewrap):

  1. mount the picked directory and confirm read+write through the mount and inside the
     account's sandbox — the control;
  2. RELOAD the page (Page.navigate to the same URL, what F5 does), then measure:
       · is the kernel mount still there, and does the mount point still exist?
       · can the host still read and write through the mount point?
       · can the account's sandbox still read and write through it?
       · what do the row, the gateway's state record and the plugin's page say now?
  3. CLOSE the tab and open a fresh one on the same origin, then measure the same things;
  4. report every observation as a line, and fail only when a measurement contradicts
     itself — "the directory is gone after reload" is a finding, not a harness error.

Pass --expect{,-reload,-reopen} to turn the three headline questions into assertions:
  alive | dead | either      (default: either, so the run reports instead of deciding)

    go build -o /tmp/dshgw-reload ./cmd/dshgw
    python3 scripts/browser_workspace_reload_e2e.py --dshgw /tmp/dshgw-reload [--keep]
"""

from __future__ import annotations

import argparse
import contextlib
import importlib.util
import json
import os
import pwd
import re
import shlex
import shutil
import signal
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent

# The fixture harness (throwaway gateway, portal login, CDP client, page probes) is one
# implementation: this script imports it instead of copying it, so a probe fix cannot apply
# to only one of the two runs. Its main() never runs on import.
_spec = importlib.util.spec_from_file_location("bw_e2e", REPO / "scripts/browser_workspace_mount_e2e.py")
assert _spec and _spec.loader
e2e = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(e2e)

# The fixture band is deliberately outside every live range (18xxx / 32xxx). Reload runs
# beside the mount run, not instead of it, so it does not share the mount run's ports.
e2e.LISTEN = "127.0.0.1:13879"
e2e.PORTAL_PORT = 13860
e2e.TENANT_PORT_LO, e2e.TENANT_PORT_HI = 13861, 13878
e2e.WORKER_PORT_LO, e2e.WORKER_PORT_HI = 13881, 13898
e2e.TENANT = "bw-reload"

TENANT = e2e.TENANT
PICKED = e2e.PICKED
CONTROL_FILE = "control.txt"
CONTROL_TEXT = "written before the reload\n"
RELOAD_FILE = "after-reload.txt"
RELOAD_TEXT = "written after the reload\n"
REOPEN_FILE = "after-reopen.txt"
REOPEN_TEXT = "written after the closed tab was replaced\n"
RECONNECT_FILE = "after-reconnect.txt"
RECONNECT_TEXT = "written after the page reconnected on its own\n"
RESTORE_FILE = "after-restore.txt"
RESTORE_TEXT = "written under the mount the reloaded page restored\n"

# A request log that survives a reload. The plugin's own counter lives on `window`, so the
# record of what the gateway was asked BEFORE the navigation disappears with the page; a
# sessionStorage seed is the only way to keep one monotonic sequence across documents.
BWRAP_PROBE = r'''(() => {
  window.__bwFetch = [];
  window.__bwEpoch = Number(sessionStorage.getItem('bwEpoch') || '0') + 1;
  sessionStorage.setItem('bwEpoch', String(window.__bwEpoch));
  window.__bwNow = () => Math.round(performance.timeOrigin % 1e7);
  window.__bwJournal = window.__bwJournal || [];
  window.__bwLog = message => { try { window.__bwJournal.push(String(message)) } catch (e) {} };
  const realFetch = window.fetch.bind(window);
  window.fetch = (input, init) => {
    const url = typeof input === 'string' ? input : (input && input.url) || '';
    const record = { url: String(url), method: (init && init.method) || 'GET', status: null };
    if (record.url.includes('browser-workspace')) {
      record.at = (typeof performance !== 'undefined' && performance.now) ? Math.round(performance.now()) : null;
      window.__bwFetch.push(record);
    }
    return realFetch(input, init).then(
      response => { record.status = response.status; return response },
      error => { record.status = 'error'; record.why = String(error && (error.message || error)); throw error });
  };
  return true;
})()'''

# What the page says about the workspace after it comes back. `epoch` is this document's
# number: 1 is the mount, 2 the reload, 3 the reopened tab.
PAGE_STATE = r'''(() => {
  const out = { href: location.href, epoch: window.__bwEpoch || null,
    bw: window.__bwFetch || [], picker: window.__bwPicker || null,
    journal: window.__bwJournal || [],
    storage: null, hasWorkspaceAPI: null };
  try { out.storage = { epoch: sessionStorage.getItem('bwEpoch') }; } catch (e) { out.storage = String(e); }
  out.hasWorkspaceAPI = !!(window.__DSH_BOOT__ || document.querySelector('[class*="_workspace"]'));
  return out;
})()'''


class Finding:
    """One measured question: its evidence, and the state the question is really about.

    A phase can legitimately observe failure and success in the same run — a directory is
    unusable while the page is disconnected and usable again after it reconnects. The
    headline is therefore an explicit outcome, not whichever observation happened first.
    """

    def __init__(self, name: str):
        self.name = name
        self.outcome: bool | None = None
        self.details: list[str] = []

    def observe(self, alive: bool, detail: str) -> None:
        self.details.append(("ALIVE " if alive else "DEAD  ") + detail)
        print(f"      {'alive' if alive else 'dead '} · {detail}", flush=True)

    def conclude(self, alive: bool, detail: str = "") -> None:
        """What this phase is finally reporting. The last conclusion wins."""
        self.outcome = alive
        if detail:
            self.details.append(("ALIVE " if alive else "DEAD  ") + detail)

    def verdict(self) -> str:
        if self.outcome is None:
            return "UNKNOWN"
        return "ALIVE" if self.outcome else "DEAD"


def host_read_write(mountpoint: Path, filename: str, text: str, timeout: int = 25) -> tuple[bool, str]:
    """Read and write through the mount point, from the gateway host.

    Every operation runs in a child process with a hard timeout: a FUSE server whose browser
    is gone does not fail, it blocks until its own timeout, and a blocked `ls` here would
    hang the whole run instead of being reported.
    """
    probe = (
        "import sys,os\n"
        f"p={str(mountpoint / filename)!r}\n"
        "try:\n"
        "    names=sorted(os.listdir(" + repr(str(mountpoint)) + "))\n"
        "except Exception as e:\n"
        "    print('LIST-FAILED', type(e).__name__, e); sys.exit(0)\n"
        "print('LIST', names)\n"
        "try:\n"
        "    open(p,'w').write(" + repr(text) + ")\n"
        "    print('WROTE', len(" + repr(text) + "))\n"
        "except Exception as e:\n"
        "    print('WRITE-FAILED', type(e).__name__, e)\n"
        "try:\n"
        "    print('READ', repr(open(p).read()))\n"
        "except Exception as e:\n"
        "    print('READ-FAILED', type(e).__name__, e)\n"
    )
    try:
        done = subprocess.run([sys.executable, "-c", probe], capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return False, f"host read/write through {filename} HUNG (no answer in {timeout}s: FUSE had no browser to ask)"
    out = (done.stdout + done.stderr).strip().replace("\n", " | ")
    ok = "WROTE" in done.stdout and "READ" in done.stdout and "FAILED" not in done.stdout
    return ok, out


def sandbox_read_write(argv: list[str], mountpoint: Path, filename: str, text: str, timeout: int = 40) -> tuple[bool, str]:
    """The same read/write from INSIDE the account's sandbox — what the AI's tools see."""
    separator = argv.index("--")
    script = (
        f"ls -1 {shlex.quote(str(mountpoint))} 2>&1 | tr '\\n' ',' ; echo '|LIST-END'; "
        f"printf %s {shlex.quote(text)} > {shlex.quote(str(mountpoint / filename))} 2>&1 && echo WROTE || echo WRITE-FAILED; "
        f"cat {shlex.quote(str(mountpoint / filename))} 2>&1 | head -c 200; echo '|READ-END'"
    )
    try:
        done = subprocess.run([*argv[: separator + 1], "/bin/sh", "-c", script],
                              capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return False, f"sandbox read/write through {filename} HUNG ({timeout}s: the mount answered nothing)"
    out = (done.stdout + done.stderr).strip().replace("\n", " ")
    ok = "WROTE" in done.stdout and "WRITE-FAILED" not in done.stdout and text.strip() in done.stdout
    return ok, f"rc={done.returncode} {out[:400]}"


def worker_argv(root: Path) -> list[str] | None:
    """The RUNNING worker's own argv: the profile that actually binds the mount point."""
    log_text = (root / "dshgw.log").read_text(encoding="utf-8", errors="replace")
    starts = re.findall(r"tenant worker started tenant=\S+ pid=(\d+)", log_text)
    if not starts:
        return None
    raw = Path(f"/proc/{starts[-1]}/cmdline")
    if not raw.exists():
        return None
    return [part for part in raw.read_bytes().decode("utf-8", "replace").split("\0") if part]


def record_for(root: Path, expected: str | None = None) -> dict | None:
    records = root / "state" / "browser-mounts"
    if not records.is_dir():
        return None
    for path in sorted(records.glob("*.json")):
        with contextlib.suppress(Exception):
            document = json.loads(path.read_text(encoding="utf-8"))
            if document.get("Tenant") == TENANT and (expected is None or document.get("State") == expected):
                return document
    return None


def mount_identity(root: Path) -> dict:
    """The gateway's own record for this tenant's mount: identity and state, not just paths."""
    document = record_for(root)
    if not document:
        return {"id": None, "state": None, "path": None}
    return {"id": document.get("ID"), "state": document.get("State"), "path": document.get("Path")}


def gateway_counters(root: Path) -> str:
    """How many live shares the gateway still holds, straight from its own log/state."""
    records = list((root / "state" / "browser-mounts").glob("*.json")) if (root / "state" / "browser-mounts").is_dir() else []
    states = []
    for path in records:
        with contextlib.suppress(Exception):
            document = json.loads(path.read_text(encoding="utf-8"))
            states.append(f"{document.get('ID', '?')[:8]}:{document.get('State')}")
    return f"records={states or 'none'}"


def new_tab(debug_port: int, url: str) -> tuple[object, str]:
    """Open a REAL new tab on the same origin and attach to it.

    /json/new is PUT-only in every modern Chromium; a GET answers 405.
    """
    request = urllib.request.Request(f"http://127.0.0.1:{debug_port}/json/new?{url}", method="PUT")
    with urllib.request.urlopen(request, timeout=15) as resp:
        target = json.load(resp)
    cdp = e2e.CDP(target["webSocketDebuggerUrl"])
    cdp.call("Page.enable")
    cdp.call("Runtime.enable")
    cdp.call("Log.enable")
    cdp.call("Network.enable")
    cdp.call("Page.addScriptToEvaluateOnNewDocument", {"source": e2e.PICKER_OVERRIDE})
    cdp.call("Page.addScriptToEvaluateOnNewDocument", {"source": BWRAP_PROBE})
    return cdp, target["id"]


def page_list(debug_port: int) -> list[dict]:
    with urllib.request.urlopen(f"http://127.0.0.1:{debug_port}/json/list", timeout=15) as resp:
        return json.load(resp)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", default=str(REPO / ".cache/m65-browser-reload"))
    parser.add_argument("--dshgw", default=str(REPO / "bin/dshgw"))
    parser.add_argument("--node", default=os.environ.get("DSHGW_NODE") or (e2e.LOCAL_NODE if Path(e2e.LOCAL_NODE).exists() else "node"))
    parser.add_argument("--bin-js", default=os.environ.get("DSHGW_BIN_JS") or str(Path(e2e.LOCAL_DSH) / "lib/bin.js"))
    parser.add_argument("--current-link", default=os.environ.get("DSHGW_DSH_ROOT") or e2e.LOCAL_DSH)
    parser.add_argument("--chrome", default=os.environ.get("CHROME_BIN") or (e2e.LOCAL_CHROME if Path(e2e.LOCAL_CHROME).exists() else "chromium"))
    parser.add_argument("--template-home", default=str(REPO / "data/dshgw-verify/template-home"))
    parser.add_argument("--expect", choices=("alive", "dead", "either"), default="either",
                        help="the control mount while its own page is open (should be alive)")
    parser.add_argument("--expect-reload", choices=("alive", "dead", "either"), default="either",
                        help="read/write after a page reload, before the operator clicks 恢复")
    parser.add_argument("--expect-reopen", choices=("alive", "dead", "either"), default="either",
                        help="read/write in a new tab, before the operator clicks 恢复")
    parser.add_argument("--expect-restore", choices=("alive", "dead", "either"), default="either",
                        help="read/write after clicking the row in the reloaded page")
    parser.add_argument("--expect-reconnect", choices=("alive", "dead", "either"), default="either",
                        help="read/write after the page lost its network and got it back")
    parser.add_argument("--keep", action="store_true", help="leave the throwaway deployment behind")
    args = parser.parse_args()

    root = Path(args.root).resolve()
    steps: list[str] = []
    summary: dict[str, str] = {}

    def note(message: str) -> None:
        steps.append(message)
        print(f"  · {message}", flush=True)

    server = chrome = cdp = cdp2 = None
    stub = e2e.StubAigw()
    stub.__enter__()
    log = chrome_log = None
    try:
        # ── preconditions ────────────────────────────────────────────────────────────
        e2e.need("bwrap")
        e2e.need("fusermount3")
        if not Path("/dev/fuse").exists():
            raise e2e.Skip("/dev/fuse is missing: browser workspaces need user FUSE mounts")
        if not Path(args.dshgw).exists():
            raise e2e.Skip(f"{args.dshgw} is not built (make dshgw-build)")
        if not Path(args.bin_js).exists() or not Path(args.node).exists():
            raise e2e.Skip("the installed dsh runtime (node + bin.js) was not found")
        if not Path(args.chrome).exists() and shutil.which(args.chrome) is None:
            raise e2e.Skip(f"no Chromium at {args.chrome}")
        template_home = Path(args.template_home)
        if not (template_home / "profiles").exists():
            raise e2e.Skip(f"no prepared dsh template at {template_home} (deploy/dshgw/prepare-template.sh)")

        if root.exists():
            subprocess.run(["pkill", "-f", f"{root}/dshgw.yaml serve"], capture_output=True)
            for stale in e2e.mounts_under(root):
                subprocess.run(["fusermount3", "-u", stale], capture_output=True)
                subprocess.run(["fusermount3", "-z", stale], capture_output=True)
            shutil.rmtree(root, ignore_errors=True)
        root.mkdir(parents=True)
        host, port = e2e.LISTEN.split(":")
        if e2e.port_open(host, int(port)):
            raise e2e.Skip(f"{e2e.LISTEN} is already in use: stop the process holding it")

        config_path = root / "dshgw.yaml"
        config_path.write_text(json.dumps(e2e.config_document(args, root, template_home, stub.base_url), indent=2) + "\n", encoding="utf-8")
        config_path.chmod(0o600)

        env = os.environ.copy()
        for name in tuple(env):
            if name.startswith("DSH_") or name in ("NODE_OPTIONS", "NODE_PATH"):
                env.pop(name)

        # ── the throwaway gateway and the account ────────────────────────────────────
        log = open(root / "dshgw.log", "w", encoding="utf-8")
        server = subprocess.Popen([args.dshgw, "--config", str(config_path), "serve"],
                                  stdout=log, stderr=subprocess.STDOUT, cwd=str(REPO), env=env)
        e2e.wait_for(lambda: e2e.port_open(host, int(port)), 30, "the test gateway to listen")
        note(f"throwaway dshgw listening on {e2e.LISTEN} (state {root / 'state'})")

        admin_socket = root / "state" / "admin.sock"
        e2e.wait_for(lambda: admin_socket.exists() or None, 30, "the daemon's admin socket")
        created = e2e.admin_call(admin_socket, {"id": 1, "op": "tenant-create", "name": TENANT,
                                                "key": "sk-bw-reload-0000000000", "allow_empty_models": True})
        if not created.get("ok"):
            raise AssertionError(f"tenant create failed: {created}")

        registry = json.loads((root / "state" / "registry.json").read_text(encoding="utf-8"))
        record = next(item for item in registry["tenants"] if item["name"] == TENANT)
        workspace, public_port = Path(record["workspace"]), record["public_port"]
        note(f"account {TENANT}: workspace {workspace}, tenant origin http://127.0.0.1:{public_port}")

        # ── real Chromium, portal login ──────────────────────────────────────────────
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
        e2e.wait_for(lambda: portfile.exists() or None, 30, "Chromium to publish its debugging port")
        debug_port = int(portfile.read_text().splitlines()[0])
        target = next(p for p in page_list(debug_port) if p["type"] == "page")
        cdp = e2e.CDP(target["webSocketDebuggerUrl"])
        cdp.call("Page.enable")
        cdp.call("Runtime.enable")
        cdp.call("Log.enable")
        cdp.call("Network.enable")
        cdp.call("Page.addScriptToEvaluateOnNewDocument", {"source": e2e.PICKER_OVERRIDE})
        cdp.call("Page.addScriptToEvaluateOnNewDocument", {"source": BWRAP_PROBE})
        note("real Chromium started; the OS directory chooser is replaced by a real OPFS handle")
        note("a request log seeded in sessionStorage is what keeps one sequence across reloads")

        cdp.call("Page.navigate", {"url": f"http://127.0.0.1:{e2e.PORTAL_PORT}/"})
        form = e2e.wait_for(lambda: (lambda f: f if f.get("found") else None)(cdp.evaluate(e2e.LOGIN_FORM)), 30, "the portal login form")
        cdp.evaluate("document.querySelector('input[name=\"key\"]').value = 'sk-bw-reload-0000000000'")
        cdp.click(form["x"], form["y"])
        deadline = time.time() + 240
        while time.time() < deadline and f"127.0.0.1:{public_port}" not in cdp.url():
            time.sleep(2)
        if f"127.0.0.1:{public_port}" not in cdp.url():
            raise AssertionError("the tenant GUI never loaded: " + e2e.diagnose(cdp, root))
        tenant_url = cdp.url()
        note(f"portal login accepted; the browser is on the tenant GUI ({tenant_url})")

        def row():
            value = cdp.evaluate(e2e.ROW)
            return value if value.get("found") else None

        def dismiss_dialogs(limit=6):
            for _ in range(limit):
                value = cdp.evaluate(e2e.DIALOG_BUTTON)
                if not value.get("found"):
                    break
                cdp.click(value["x"], value["y"])
                time.sleep(0.6)

        deadline = time.time() + 180
        while time.time() < deadline:
            dismiss_dialogs()
            if row():
                break
            time.sleep(1)
        else:
            raise AssertionError("the 浏览器工作区 row never appeared in the tenant GUI")
        note("the 浏览器工作区 row is present in the real tenant GUI")

        seeded = cdp.evaluate(e2e.browser_write(CONTROL_FILE, CONTROL_TEXT), await_promise=True)
        if seeded.get("written") != len(CONTROL_TEXT):
            raise AssertionError(f"the browser could not seed its own file: {seeded}")
        note(f"the browser wrote {CONTROL_FILE} into its own directory before mounting")

        # ── mount ────────────────────────────────────────────────────────────────────
        point = e2e.wait_for(lambda: (lambda p: p if p.get("found") else None)(cdp.evaluate(e2e.HIT_POINT)), 15, "the row to be clickable")
        cdp.click(point["x"], point["y"])
        deadline, mounted_text = time.time() + 180, ""
        while time.time() < deadline:
            value = cdp.evaluate(e2e.ROW)
            text = value.get("text", "") if value.get("found") else ""
            if "已挂载" in text:
                mounted_text = text
                break
            if any(bad in text for bad in ("挂载失败", "需要 HTTPS", "已断线", "清理未确认")):
                raise AssertionError("the row refused the mount: " + text + " | " + e2e.diagnose(cdp, root))
            time.sleep(0.5)
        if not mounted_text:
            raise AssertionError("the row never reported 已挂载: " + e2e.diagnose(cdp, root))
        document = e2e.wait_for(lambda: record_for(root, "ready"), 30, "the gateway's mount record to reach ready")
        mountpoint = Path(document["Path"])
        e2e.wait_for(lambda: e2e.mount_type(str(mountpoint)) or None, 15, "the mount to appear in the kernel table")
        note(f"mounted {mountpoint} as {e2e.mount_type(str(mountpoint))}; the row says: {mounted_text}")

        argv = e2e.wait_for(lambda: worker_argv(root), 30, "the worker's own command line")
        separator = argv.index("--")
        bound = [i for i, part in enumerate(argv) if part == "--bind" and argv[i + 1] == str(mountpoint)]
        if not bound:
            raise AssertionError(f"the running worker's profile does not bind the mount point: {argv[:separator]}")
        note("the account's running worker profile binds the mount point read-write")

        # ── the control: read/write while the page owns the mount ────────────────────
        control = Finding("control (page still open)")
        host_ok, host_detail = host_read_write(mountpoint, CONTROL_FILE, CONTROL_TEXT)
        control.observe(host_ok, "host: " + host_detail)
        sandbox_ok, sandbox_detail = sandbox_read_write(argv, mountpoint, CONTROL_FILE, CONTROL_TEXT)
        control.observe(sandbox_ok, "sandbox: " + sandbox_detail)
        control.conclude(host_ok and sandbox_ok)
        summary["control"] = control.verdict()
        if control.verdict() != "ALIVE":
            raise AssertionError("the control failed: the mount is not usable even before any reload | " + " | ".join(control.details))

        baseline = mount_identity(root)
        note(f"baseline mount identity: id={(baseline['id'] or '')[:16]}… path={baseline['path']}")

        # ── IN-PAGE RECONNECT: the network dies, then comes back ─────────────────────
        # This is the case that needs no reload and no click: the page is still there, its
        # serving loop is what broke. DSH keeps the mount for its own grace window, so the
        # client's own retry is what turns a blip into nothing the operator ever sees.
        print("\n== the page's own transport breaks and comes back (no reload) ==", flush=True)
        reconnect_finding = Finding("reconnect")
        # The failure is injected at the ONLY place that matters — this plugin's own
        # endpoints — instead of taking the whole browser offline. A page-wide offline also
        # kills DSH's own WebSocket and asset loads, which is a different scenario (the whole
        # GUI reconnecting) and it hides the behaviour under test behind the GUI's own
        # recovery. Network.setBlockedURLs fails these requests outright, exactly like a
        # dropped connection to the gateway.
        cdp.call("Network.setBlockedURLs", {"urls": ["*/browser-workspace/*"]})
        try:
            e2e.wait_for(lambda: (lambda v: v if v.get("found") and v.get("state") == "resuming" else None)(
                cdp.evaluate(e2e.ROW)), 40, "the row to report that it is reconnecting")
            note("the row reports 正在重连 while its transport is broken — the page noticed by itself")
            host_ok, host_detail = host_read_write(mountpoint, "while-broken.txt", "broken\n", timeout=12)
            reconnect_finding.observe(host_ok, "host while the page cannot reach the gateway: " + host_detail)
        finally:
            cdp.call("Network.setBlockedURLs", {"urls": []})
        try:
            e2e.wait_for(lambda: (lambda v: v if v.get("found") and v.get("state") == "mounted" else None)(
                cdp.evaluate(e2e.ROW)), 60, "the row to report a live mount again")
        except AssertionError:
            state = cdp.evaluate(PAGE_STATE)
            raise AssertionError("the page never reconnected. journal: "
                                 + json.dumps(state.get("journal"))[:1200] + " | row="
                                 + json.dumps(cdp.evaluate(e2e.ROW))[:200] + " | " + e2e.diagnose(cdp, root))
        note("the page reconnected on its own: the row is back to 已挂载, with no reload and no click")
        host_ok, host_detail = host_read_write(mountpoint, RECONNECT_FILE, RECONNECT_TEXT)
        reconnect_finding.observe(host_ok, "host: " + host_detail)
        sandbox_ok, sandbox_detail = sandbox_read_write(argv, mountpoint, RECONNECT_FILE, RECONNECT_TEXT)
        reconnect_finding.observe(sandbox_ok, "sandbox: " + sandbox_detail)
        after = mount_identity(root)
        reconnect_finding.conclude(after["id"] == baseline["id"] and host_ok and sandbox_ok,
                                   "the page recovered the same mount in place, with no reload and no click")
        summary["reconnect"] = reconnect_finding.verdict()

        # ── RELOAD ───────────────────────────────────────────────────────────────────
        print("\n== page reload (Page.navigate to the same URL: what F5 does) ==", flush=True)
        before_state = cdp.evaluate(PAGE_STATE)
        cdp.call("Page.navigate", {"url": tenant_url})
        time.sleep(3)
        e2e.wait_for(lambda: (lambda v: v if v and v.get("epoch") == 2 else None)(cdp.evaluate(PAGE_STATE)), 60,
                     "the reloaded document to boot")
        reload_state = cdp.evaluate(PAGE_STATE)
        note(f"the page reloaded: document epoch {before_state.get('epoch')} → {reload_state.get('epoch')}, "
             f"{len(reload_state.get('bw') or [])} browser-workspace call(s) so far in the new document")

        reload_finding = Finding("reload (before the click)")
        try:
            resumable = e2e.wait_for(lambda: (lambda v: v if v.get("found") and v.get("state") == "resumable" else None)(
                cdp.evaluate(e2e.ROW)), 60, "the row to offer 恢复")
            note(f"the reloaded page offers to restore: {resumable.get('text')!r} (state={resumable.get('state')})")
        except AssertionError:
            raise AssertionError("the reloaded page did not offer to restore the mount: "
                                 + json.dumps(cdp.evaluate(e2e.ROW)) + " | " + e2e.diagnose(cdp, root))
        # Before the click the mount is nobody's browser side: the gateway excluded it from
        # every worker profile the moment the poll died, so reads and writes fail rather than
        # block. That is the state the click is about to leave.
        host_ok, host_detail = host_read_write(mountpoint, RELOAD_FILE, RELOAD_TEXT, timeout=15)
        reload_finding.observe(host_ok, "host before the click: " + host_detail)
        note(f"browser-workspace calls in the new document so far: {json.dumps(reload_state.get('bw') or [])[:200]}")
        reload_finding.conclude(host_ok)  # the reloaded page cannot use the mount until it is restored
        summary["reload"] = reload_finding.verdict()

        # ── RESTORE BY CLICK ─────────────────────────────────────────────────────────
        print("\n== one click restores the mount that was already there ==", flush=True)
        restore_finding = Finding("restore")
        point = e2e.wait_for(lambda: (lambda p: p if p.get("found") else None)(cdp.evaluate(e2e.HIT_POINT)), 20,
                             "the row to be clickable after the reload")
        cdp.click(point["x"], point["y"])
        mounted = e2e.wait_for(lambda: (lambda v: v if v.get("found") and v.get("state") == "mounted" else None)(
            cdp.evaluate(e2e.ROW)), 60, "the restored mount")
        note(f"after one click the row says: {mounted.get('text')!r} (state={mounted.get('state')})")
        calls = cdp.evaluate(PAGE_STATE).get("bw") or []
        endpoints = {str(c.get("url", "")).split("/")[-1] for c in calls if c.get("status") is not None}
        note(f"the click asked the gateway for: {endpoints}")
        if "resume" not in endpoints:
            raise AssertionError(f"the click did not resume the existing mount: {endpoints}")
        if "open" in endpoints:
            raise AssertionError(f"the click opened a NEW mount instead of restoring: {endpoints}")
        restored = mount_identity(root)
        restore_finding.observe(restored["id"] == baseline["id"] and restored["path"] == baseline["path"],
                                f"the same mount is serving again: id={(restored['id'] or '')[:16]}… path={restored['path']}")
        host_ok, host_detail = host_read_write(mountpoint, RESTORE_FILE, RESTORE_TEXT)
        restore_finding.observe(host_ok, "host after the click: " + host_detail)
        sandbox_ok, sandbox_detail = sandbox_read_write(argv, mountpoint, RESTORE_FILE, RESTORE_TEXT)
        restore_finding.observe(sandbox_ok, "sandbox after the click: " + sandbox_detail)
        restore_finding.conclude(host_ok and sandbox_ok)
        summary["restore"] = restore_finding.verdict()

        # ── CLOSE THE TAB, OPEN A NEW ONE ────────────────────────────────────────────
        print("\n== closed tab, then a brand-new tab on the same origin ==", flush=True)
        with contextlib.suppress(Exception):
            cdp.call("Page.close")
        cdp = None
        time.sleep(5)
        # The new tab is created on about:blank on purpose: the two probe scripts are
        # registered with the target BEFORE the first real navigation, and
        # addScriptToEvaluateOnNewDocument only applies to documents that start after it was
        # registered. Created directly on the tenant URL, the tab would load without the
        # request log AND without the picker override — which also breaks the page, because
        # the override is what replaces window.fetch with the counting wrapper.
        cdp2, target_id = new_tab(debug_port, "about:blank")
        cdp = cdp2
        cdp.call("Page.navigate", {"url": tenant_url})
        # A tab that just opened has empty sessionStorage, so its epoch is 1 — the probe's
        # own presence (picker.replaced, an epoch, and the row) is what says it booted.
        def booted():
            value = cdp.evaluate(PAGE_STATE)
            if not value or not value.get("picker") or not value.get("picker", {}).get("replaced"):
                return None
            if f"127.0.0.1:{public_port}" not in (value.get("href") or ""):
                return None
            return value

        try:
            reopen_state = e2e.wait_for(booted, 120, "the new tab to boot")
        except AssertionError:
            raise AssertionError(f"the new tab never booted: {json.dumps(cdp.evaluate(PAGE_STATE))[:400]} | {e2e.diagnose(cdp, root)}")
        reopen_state = cdp.evaluate(PAGE_STATE)
        note(f"a new tab is open ({target_id[:8]}…): document epoch {reopen_state.get('epoch')}")
        tabs = [p for p in page_list(debug_port) if p["type"] == "page"]
        note(f"tabs open in the browser now: {len(tabs)}")

        reopen_finding = Finding("reopen (before the click)")
        host_ok, host_detail = host_read_write(mountpoint, REOPEN_FILE, REOPEN_TEXT, timeout=15)
        reopen_finding.observe(host_ok, "host before the click: " + host_detail)
        try:
            deadline = time.time() + 60
            reopen_row = None
            while time.time() < deadline:
                dismiss_dialogs()
                reopen_row = cdp.evaluate(e2e.ROW)
                if reopen_row.get("found"):
                    break
                time.sleep(1)
            note(f"the row in the new tab says: {reopen_row.get('text')!r} (state={reopen_row.get('state')})")
            deadline = time.time() + 30
            while time.time() < deadline:
                value = cdp.evaluate(e2e.ROW)
                if value.get("found"):
                    break
                time.sleep(1)
            reopened = cdp.evaluate(e2e.browser_read(CONTROL_FILE), await_promise=True)
            note(f"the browser's own directory in the new tab still lists {reopened.get('names')} "
                 f"and reads {CONTROL_FILE}: {reopened.get('content')!r}")
        except Exception as error:
            note(f"the new tab could not be inspected: {error}")
        note(f"browser-workspace calls in the new tab: {json.dumps(reopen_state.get('bw') or [])[:300]}")
        summary["reopen"] = reopen_finding.verdict()

        reopen_before_click_ok = host_ok

        # ── the same click in the new tab ────────────────────────────────────────────
        print("\n== one click in the new tab restores the same mount ==", flush=True)
        try:
            point = e2e.wait_for(lambda: (lambda p: p if p.get("found") else None)(cdp.evaluate(e2e.HIT_POINT)), 30,
                                 "the row to be clickable in the new tab")
            cdp.click(point["x"], point["y"])
            mounted = e2e.wait_for(lambda: (lambda v: v if v.get("found") and v.get("state") == "mounted" else None)(
                cdp.evaluate(e2e.ROW)), 60, "the restored mount in the new tab")
            note(f"after one click the new tab's row says: {mounted.get('text')!r}")
            endpoints = {str(c.get("url", "")).split("/")[-1] for c in (cdp.evaluate(PAGE_STATE).get("bw") or [])
                         if c.get("status") is not None}
            note(f"the click asked the gateway for: {endpoints}")
            if "resume" not in endpoints or "open" in endpoints:
                raise AssertionError(f"the new tab did not restore the existing mount: {endpoints}")
            restored = mount_identity(root)
            reopen_finding.observe(restored["id"] == baseline["id"],
                                   f"the same mount is serving again: id={(restored['id'] or '')[:16]}… path={restored['path']}")
            host_ok, host_detail = host_read_write(mountpoint, REOPEN_FILE, REOPEN_TEXT)
            reopen_finding.observe(host_ok, "host after the click: " + host_detail)
            sandbox_ok, sandbox_detail = sandbox_read_write(argv, mountpoint, REOPEN_FILE, REOPEN_TEXT)
            reopen_finding.observe(sandbox_ok, "sandbox after the click: " + sandbox_detail)
            survived = cdp.evaluate(e2e.browser_read(CONTROL_FILE), await_promise=True)
            note(f"the browser's own directory survived everything: lists {survived.get('names')}, "
                 f"{CONTROL_FILE}={survived.get('content')!r}")
            reopen_finding.conclude(host_ok and sandbox_ok, "the new tab restored the same mount by one click")
            summary["recovery"] = "restored the same mount by one click"
        except Exception as error:
            reopen_finding.conclude(False, f"the new tab could not restore the mount: {error}")
            summary["recovery"] = f"restore in the new tab failed: {error}"
            note(summary["recovery"])
        note(f"browser-workspace calls in the new tab: {json.dumps(reopen_state.get('bw') or [])[:300]}")
        summary["reopen"] = reopen_finding.verdict()

        # ── verdict ──────────────────────────────────────────────────────────────────
        print("\n== findings ==", flush=True)
        for name, finding in (("control", control), ("reconnect", reconnect_finding),
                              ("reload", reload_finding), ("restore", restore_finding),
                              ("reopen", reopen_finding)):
            print(f"  {name:8s} {finding.verdict()}", flush=True)
            for detail in finding.details:
                print(f"           {detail}", flush=True)

        problems = []
        if args.expect != "either" and control.verdict() != args.expect.upper():
            problems.append(f"control={control.verdict()} want {args.expect.upper()}")
        if args.expect_reload != "either" and reload_finding.verdict() != args.expect_reload.upper():
            problems.append(f"reload={reload_finding.verdict()} want {args.expect_reload.upper()}")
        if args.expect_reopen != "either" and reopen_finding.verdict() != args.expect_reopen.upper():
            problems.append(f"reopen={reopen_finding.verdict()} want {args.expect_reopen.upper()}")
        if args.expect_restore != "either" and restore_finding.verdict() != args.expect_restore.upper():
            problems.append(f"restore={restore_finding.verdict()} want {args.expect_restore.upper()}")
        if args.expect_reconnect != "either" and reconnect_finding.verdict() != args.expect_reconnect.upper():
            problems.append(f"reconnect={reconnect_finding.verdict()} want {args.expect_reconnect.upper()}")

        payload = {"control": control.verdict(), "reconnect": reconnect_finding.verdict(),
                   "reload": reload_finding.verdict(), "restore": restore_finding.verdict(),
                   "reopen": reopen_finding.verdict(), "recovery": summary.get("recovery", "not attempted"),
                   "details": {f.name: f.details for f in (control, reconnect_finding, reload_finding,
                                                           restore_finding, reopen_finding)},
                   "steps": steps, "mountpoint": str(mountpoint)}
        (root / "reload-verdict.json").write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
        print(f"\n  wrote {root / 'reload-verdict.json'}", flush=True)

        if problems:
            print("FAIL: " + "; ".join(problems))
            return 1
        print(f"PASS: control={control.verdict()} reconnect={reconnect_finding.verdict()} "
              f"reload={reload_finding.verdict()} restore={restore_finding.verdict()} reopen={reopen_finding.verdict()}")
        return 0

    except e2e.Skip as skip:
        print(f"SKIP: {skip}")
        return 0
    except AssertionError as failure:
        print(f"FAIL: {failure}")
        return 1
    finally:
        for connection in (cdp, cdp2):
            if connection is not None:
                with contextlib.suppress(Exception):
                    connection.sock.close()
        e2e.stop_group(chrome)
        if root.exists() and not args.keep:
            subprocess.run([args.dshgw, "--config", str(root / "dshgw.yaml"), "tenant", "remove",
                            "-purge", "-yes", TENANT], cwd=str(REPO), capture_output=True, timeout=60)
            for line in (e2e.mounts_under(root) if root.exists() else []):
                subprocess.run(["fusermount3", "-u", line], capture_output=True)
                subprocess.run(["fusermount3", "-z", line], capture_output=True)
        stub.__exit__()
        if server is not None and server.poll() is None:
            server.send_signal(signal.SIGTERM)
            try:
                server.wait(timeout=20)
            except subprocess.TimeoutExpired:
                server.kill()
        for handle in (chrome_log, log):
            if handle is not None:
                handle.close()
        if not args.keep and root.exists():
            shutil.rmtree(root, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
