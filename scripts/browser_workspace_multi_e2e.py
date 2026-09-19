#!/usr/bin/env python3
"""Does ONE account really mount SEVERAL local directories, and does each keep its OWN path?

`browser_workspace_mount_e2e.py` proves a single directory mounts end-to-end.
`browser_workspace_reload_e2e.py` proves that mount survives a reload. This script asks the
question the folder list exists for, on the same kind of real fixture (throwaway dshgw on a
13xxx band, real Chromium, real OPFS handles, real FUSE, real bubblewrap):

  1. the row's right-hand icon opens the folder list (not the directory dialog);
  2. two local directories are added through that list, and each becomes its own kernel mount
     with its own path under `<workspace>/browser/`, both bound into the account's running
     worker, with independent I/O (a file in one is not visible in the other);
  3. DISCONNECT releases one folder's mount only — the other keeps serving, the gateway's
     record for the disconnected one is gone, and the account's worker is restarted with only
     the surviving mount bound;
  4. the disconnected folder keeps its STABLE virtual path: the empty mount point stays, the
     workspace entry is NOT deleted, and connecting again returns to the same path with the
     SAME workspace id (the mapping is a property of the local directory, not of a mount);
  5. DELETING a folder releases its path and its workspace entry, and disturbs nothing else;
  6. a page reload leaves the surviving folder resumable, and one click takes the same mount
     back (resume, not a second mount).

Everything measured here is measured on the host: the kernel table, the gateway's own state
records, the running worker's argv, DSH's workspace store, and I/O through the mounts (from
the host and from inside the account's sandbox). The browser side is a real headless Chromium
whose only replaced step is the OS directory chooser — replaced by a REAL OPFS
FileSystemDirectoryHandle, so the executor, reverse channel, FUSE and sandbox are production.

    go build -o /tmp/dshgw-multi ./cmd/dshgw
    python3 scripts/browser_workspace_multi_e2e.py --dshgw /tmp/dshgw-multi [--keep]
"""

from __future__ import annotations

import argparse
import contextlib
import importlib.util
import json
import os
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

# The one implementation of the throwaway fixture (gateway + portal + CDP + page probes) is
# imported, never copied: a probe fix must reach every run. Its main() does not run on import.
_spec = importlib.util.spec_from_file_location("bw_e2e", REPO / "scripts/browser_workspace_mount_e2e.py")
assert _spec and _spec.loader
e2e = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(e2e)

# Its own band, deliberately outside every live range (18xxx / 32xxx) and outside the other
# two acceptance scripts' bands, so the three can run beside each other.
e2e.LISTEN = "127.0.0.1:13839"
e2e.PORTAL_PORT = 13840
e2e.TENANT_PORT_LO, e2e.TENANT_PORT_HI = 13841, 13858
e2e.WORKER_PORT_LO, e2e.WORKER_PORT_HI = 13859, 13876
e2e.TENANT = "bw-multi"

KEY = "sk-bw-multi-0000000000"
TENANT = e2e.TENANT
DIR_A = "picked-a"
DIR_B = "picked-b"
FILE_A = "only-in-a.txt"
FILE_B = "only-in-b.txt"
TEXT_A = "directory A\n"
TEXT_B = "directory B\n"


def records(root: Path, state: str | None = None) -> list[dict]:
    """Every gateway state record for this tenant (one per mount), optionally by state."""
    directory = root / "state" / "browser-mounts"
    if not directory.is_dir():
        return []
    found = []
    for path in sorted(directory.glob("*.json")):
        with contextlib.suppress(Exception):
            document = json.loads(path.read_text(encoding="utf-8"))
            if document.get("Tenant") == TENANT and (state is None or document.get("State") == state):
                found.append(document)
    return found


def record_for_path(root: Path, mountpoint: Path) -> dict | None:
    return next((item for item in records(root) if item.get("Path") == str(mountpoint)), None)


def workspace_store(dsh_home: Path) -> dict:
    """DSH's own workspace registry: the ids, paths and titles it groups sessions by."""
    path = dsh_home / "storages" / "workspace.json"
    if not path.exists():
        return {}
    with contextlib.suppress(Exception):
        return json.loads(path.read_text(encoding="utf-8"))
    return {}


def browser_workspaces(dsh_home: Path, workspace: Path) -> dict[str, dict]:
    """The workspace entries that map to this account's browser mount container, by path."""
    store = workspace_store(dsh_home)
    prefix = str(workspace / "browser") + "/"
    out = {}
    for workspace_id in (store.get("global") or {}).get("workspaceIds") or []:
        record = ((store.get("tables") or {}).get("workspaces") or {}).get(workspace_id)
        if isinstance(record, dict) and str(record.get("path", "")).startswith(prefix):
            out[record["path"]] = {"id": workspace_id, "title": record.get("title"), "sessionIds": record.get("sessionIds") or []}
    return out


def worker_argv(root: Path, log_path: Path) -> list[str] | None:
    """The RUNNING worker's own argv: the profile that actually binds the mount points."""
    if not log_path.exists():
        return None
    starts = re.findall(r"tenant worker started tenant=\S+ pid=(\d+)", log_path.read_text(encoding="utf-8", errors="replace"))
    if not starts:
        return None
    raw = Path(f"/proc/{starts[-1]}/cmdline")
    if not raw.exists():
        return None
    return [part for part in raw.read_bytes().decode("utf-8", "replace").split("\0") if part]


def host_io(mountpoint: Path, filename: str, text: str, timeout: int = 25) -> tuple[bool, str]:
    """Read and write through one mount point, from the gateway host.

    Each operation runs in a child process with a hard timeout: a FUSE server whose browser is
    gone does not fail, it blocks until its own timeout, and a blocked `ls` here would hang the
    whole run instead of being reported.
    """
    probe = (
        "import sys,os\n"
        "try:\n"
        "    names=sorted(os.listdir(" + repr(str(mountpoint)) + "))\n"
        "except Exception as e:\n"
        "    print('LIST-FAILED', type(e).__name__, e); sys.exit(0)\n"
        "print('LIST', names)\n"
        "try:\n"
        "    open(" + repr(str(mountpoint / filename)) + ",'w').write(" + repr(text) + ")\n"
        "    print('WROTE', len(" + repr(text) + "))\n"
        "except Exception as e:\n"
        "    print('WRITE-FAILED', type(e).__name__, e); sys.exit(0)\n"
        "try:\n"
        "    print('READ', repr(open(" + repr(str(mountpoint / filename)) + ").read()))\n"
        "except Exception as e:\n"
        "    print('READ-FAILED', type(e).__name__, e)\n"
    )
    try:
        done = subprocess.run([sys.executable, "-c", probe], capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return False, f"I/O through {mountpoint} HUNG ({timeout}s: the mount answered nothing)"
    out = (done.stdout + done.stderr).strip().replace("\n", " ")
    ok = "WROTE" in done.stdout and "READ " in done.stdout and "FAILED" not in done.stdout
    return ok, f"rc={done.returncode} {out[:300]}"


def sandbox_io(argv: list[str], mountpoint: Path, filename: str, text: str, timeout: int = 40) -> tuple[bool, str]:
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
        return False, f"sandbox I/O through {filename} HUNG ({timeout}s)"
    out = (done.stdout + done.stderr).strip().replace("\n", " ")
    ok = "WROTE" in done.stdout and "WRITE-FAILED" not in done.stdout and text.strip() in done.stdout
    return ok, f"rc={done.returncode} {out[:300]}"


def point_for_row(cdp, name: str, action: str) -> dict:
    """A point on one button of one folder row in the open folder list."""
    return cdp.evaluate(f"({e2e.FOLDER_ACTION_POINT})({json.dumps(name)}, {json.dumps(action)})")


def point_for_dialog(cdp, label: str) -> dict:
    return cdp.evaluate(f"({e2e.DLG_BUTTON_POINT})({json.dumps(label)})")


def click(cdp, point: dict) -> None:
    for kind in ("mousePressed", "mouseReleased"):
        cdp.call("Input.dispatchMouseEvent", {"type": kind, "x": point["x"], "y": point["y"], "button": "left", "clickCount": 1})
    time.sleep(0.2)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", default=str(REPO / ".cache/m65-browser-multi"))
    parser.add_argument("--dshgw", default=str(REPO / "bin/dshgw"))
    parser.add_argument("--node", default=os.environ.get("DSHGW_NODE") or (e2e.LOCAL_NODE if Path(e2e.LOCAL_NODE).exists() else "node"))
    parser.add_argument("--bin-js", default=os.environ.get("DSHGW_BIN_JS") or str(Path(e2e.LOCAL_DSH) / "lib/bin.js"))
    parser.add_argument("--current-link", default=os.environ.get("DSHGW_DSH_ROOT") or e2e.LOCAL_DSH)
    parser.add_argument("--chrome", default=os.environ.get("CHROME_BIN") or (e2e.LOCAL_CHROME if Path(e2e.LOCAL_CHROME).exists() else "chromium"))
    parser.add_argument("--template-home", default=str(REPO / "data/dshgw-verify/template-home"))
    parser.add_argument("--keep", action="store_true", help="leave the throwaway deployment behind")
    args = parser.parse_args()

    root = Path(args.root).resolve()
    steps: list[str] = []

    def note(message: str) -> None:
        steps.append(message)
        print(f"  · {message}", flush=True)

    server = chrome = cdp = None
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
        log_path = root / "dshgw.log"
        log = open(log_path, "w", encoding="utf-8")
        server = subprocess.Popen([args.dshgw, "--config", str(config_path), "serve"],
                                  stdout=log, stderr=subprocess.STDOUT, cwd=str(REPO), env=env)
        e2e.wait_for(lambda: e2e.port_open(host, int(port)), 30, "the test gateway to listen")
        note(f"throwaway dshgw listening on {e2e.LISTEN} (state {root / 'state'})")

        admin_socket = root / "state" / "admin.sock"
        e2e.wait_for(lambda: admin_socket.exists() or None, 30, "the daemon's admin socket")
        created = e2e.admin_call(admin_socket, {"id": 1, "op": "tenant-create", "name": TENANT,
                                                "key": KEY, "allow_empty_models": True})
        if not created.get("ok"):
            raise AssertionError(f"tenant create failed: {created}")

        registry = json.loads((root / "state" / "registry.json").read_text(encoding="utf-8"))
        record = next(item for item in registry["tenants"] if item["name"] == TENANT)
        workspace, public_port = Path(record["workspace"]), record["public_port"]
        dsh_home = Path(record["dsh_home"])
        note(f"account {TENANT}: workspace {workspace}, tenant origin http://127.0.0.1:{public_port}")

        # ── real Chromium and the portal login ───────────────────────────────────────
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
        with urllib.request.urlopen(f"http://127.0.0.1:{debug_port}/json/list", timeout=10) as resp:
            target = next(p for p in json.load(resp) if p["type"] == "page")
        cdp = e2e.CDP(target["webSocketDebuggerUrl"])
        cdp.call("Page.enable")
        cdp.call("Runtime.enable")
        cdp.call("Log.enable")
        cdp.call("Network.enable")
        cdp.call("Page.addScriptToEvaluateOnNewDocument", {"source": e2e.PICKER_OVERRIDE})
        # Two different local directories for the two picks this run makes.
        cdp.call("Page.addScriptToEvaluateOnNewDocument", {"source": f"window.__bwPickNames = {json.dumps([DIR_A, DIR_B])};"})
        note("real Chromium started; the OS directory chooser is replaced by real OPFS handles")

        cdp.call("Page.navigate", {"url": f"http://127.0.0.1:{e2e.PORTAL_PORT}/"})
        form = e2e.wait_for(lambda: (lambda f: f if f.get("found") else None)(cdp.evaluate(e2e.LOGIN_FORM)), 30, "the portal login form")
        cdp.evaluate(f"document.querySelector('input[name=\"key\"]').value = {KEY!r}")
        click(cdp, form)
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

        def dismiss_dialogs(limit: int = 6) -> None:
            for _ in range(limit):
                value = cdp.evaluate(e2e.DIALOG_BUTTON)
                if not value.get("found"):
                    break
                click(cdp, value)
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

        # ── both rows still stack, and the icon is a hit target of its own ───────────
        rows = e2e.wait_for(lambda: (lambda v: v if v.get("found") else None)(cdp.evaluate(e2e.ROWS)), 20, "both sidebar rows")
        if not rows.get("stacked") or rows["browser"]["y"] >= rows["ssh"]["y"]:
            raise AssertionError(f"the browser and ssh workspace rows are not stacked: {rows}")
        layout = e2e.wait_for(lambda: (lambda v: v if v.get("found") else None)(cdp.evaluate(e2e.ROW_LAYOUT)), 20,
                              "the row's two controls to render")
        if not layout.get("sideBySide") or not layout.get("singleLine") or layout.get("direction") == "column":
            raise AssertionError(f"the folder icon is not beside the row body: {layout}")
        note(f"the browser row container ({rows['browser']}) sits above the ssh row, same left and width")
        note(f"inside the row the body and the folder icon share one line (body {layout['body']['w']}px + icon {layout['icon']['w']}px)")

        # Two local directories, each with a file only IT has: that is how "independent I/O"
        # is checked later.
        for directory, filename, text in ((DIR_A, FILE_A, TEXT_A), (DIR_B, FILE_B, TEXT_B)):
            written = cdp.evaluate(e2e.browser_write(filename, text, directory), await_promise=True)
            if written.get("written") != len(text):
                raise AssertionError(f"the browser could not seed {directory}/{filename}: {written}")
        note(f"the browser seeded {DIR_A}/{FILE_A} and {DIR_B}/{FILE_B} in its own two directories")

        # ── the folder icon opens the list (the row itself would mount) ──────────────
        icon = e2e.wait_for(lambda: (lambda v: v if v.get("found") else None)(cdp.evaluate(e2e.MANAGE_POINT)), 15,
                            "the folder icon to be clickable")
        click(cdp, icon)
        opened = e2e.wait_for(lambda: (lambda v: v if v.get("dialogOpen") else None)(cdp.evaluate(e2e.ROW)), 15,
                              "the folder list to open")
        if opened.get("folders"):
            raise AssertionError(f"the folder list is not empty on a fresh account: {opened['folders']}")
        if "添加文件夹" not in (opened.get("dialogButtons") or []):
            raise AssertionError(f"the folder list does not offer 添加文件夹: {opened.get('dialogButtons')}")
        note("the row's right-hand icon opened the folder list: empty, with 添加文件夹")

        def add_folder(directory: str, timeout: float = 240) -> dict:
            add = e2e.wait_for(lambda: (lambda v: v if v.get("found") else None)(point_for_dialog(cdp, "添加文件夹")), 15,
                               "the 添加文件夹 button")
            click(cdp, add)
            state = e2e.wait_for(
                lambda: (lambda v: v if any(directory in f.get("text", "") and f.get("state") == "connected" for f in v.get("folders") or []) else None)(cdp.evaluate(e2e.ROW)),
                timeout, f"{directory} to be connected in the list")
            return state

        # ── folder A ─────────────────────────────────────────────────────────────────
        state = add_folder(DIR_A)
        folder_a = next(f for f in state["folders"] if DIR_A in f.get("text", ""))
        key_a = folder_a["key"]
        path_a = workspace / "browser" / key_a
        e2e.wait_for(lambda: record_for_path(root, path_a), 60, "folder A's gateway record")
        if not e2e.mount_type(str(path_a)).startswith("fuse"):
            raise AssertionError(f"folder A is not a kernel FUSE mount: {e2e.mount_type(str(path_a))}")
        argv = e2e.wait_for(lambda: worker_argv(root, log_path), 60, "the worker's own command line")
        if str(path_a) not in argv:
            raise AssertionError("the worker profile does not bind folder A's mount point")
        ok, detail = host_io(path_a, "a-from-host.txt", "A\n")
        if not ok:
            raise AssertionError(f"host I/O through folder A failed: {detail}")
        back = cdp.evaluate(e2e.browser_read("a-from-host.txt", DIR_A), await_promise=True)
        if back.get("content") != "A\n":
            raise AssertionError(f"a file written through folder A did not reach the browser: {back}")
        note(f"folder A mounted at {path_a} (key {key_a}); host and browser I/O both round-trip")

        # ── folder B, beside A ───────────────────────────────────────────────────────
        state = add_folder(DIR_B)
        rows_by_key = {f["key"]: f for f in state["folders"]}
        if len(rows_by_key) != 2:
            raise AssertionError(f"the folder list does not hold two folders: {state['folders']}")
        key_b = next(key for key in rows_by_key if key != key_a)
        path_b = workspace / "browser" / key_b
        e2e.wait_for(lambda: record_for_path(root, path_b), 60, "folder B's gateway record")
        for path in (path_a, path_b):
            if not e2e.mount_type(str(path)).startswith("fuse"):
                raise AssertionError(f"{path} is not a kernel FUSE mount")
        ready = e2e.wait_for(lambda: records(root, "ready") if len(records(root, "ready")) == 2 else None, 30,
                             "two ready mount records")
        argv = e2e.wait_for(lambda: worker_argv(root, log_path), 60, "the worker's own command line")
        for path in (path_a, path_b):
            if str(path) not in argv:
                raise AssertionError(f"the worker profile does not bind {path}: {argv}")
        note(f"two mounts ({path_a.name}, {path_b.name}) are live and BOTH bound into the account's worker")

        # Independence: each mount shows its own directory, not a shared one.
        host_ok, host_detail = host_io(path_b, "b-from-host.txt", "B\n")
        if not host_ok:
            raise AssertionError(f"host I/O through folder B failed: {host_detail}")
        listing_a = sorted(p.name for p in path_a.iterdir())
        listing_b = sorted(p.name for p in path_b.iterdir())
        if FILE_A not in listing_a or FILE_A in listing_b:
            raise AssertionError(f"folder A's mount does not show its own directory: {listing_a} vs {listing_b}")
        if FILE_B not in listing_b or FILE_B in listing_a:
            raise AssertionError(f"folder B's mount does not show its own directory: {listing_b} vs {listing_a}")
        note("each mount serves its OWN local directory: neither listing carries the other's file")

        # DSH registered one workspace per mounted directory, titled after it.
        store = e2e.wait_for(lambda: (lambda s: s if len(browser_workspaces(dsh_home, workspace)) == 2 else None)(workspace_store(dsh_home)),
                             60, "two workspace entries in DSH's own store")
        entries = browser_workspaces(dsh_home, workspace)
        ids_before = {path: entry["id"] for path, entry in entries.items()}
        if set(entries) != {str(path_a), str(path_b)}:
            raise AssertionError(f"the workspace entries do not match the mount points: {sorted(entries)}")
        titles = {entry["title"] for entry in entries.values()}
        if titles != {f"本地: {DIR_A}", f"本地: {DIR_B}"}:
            raise AssertionError(f"the workspace titles do not name the local directories: {titles}")
        note(f"DSH holds exactly two workspaces for the two directories: {sorted(ids_before.values())}")

        # ── disconnect A only ───────────────────────────────────────────────────────
        disconnect = e2e.wait_for(lambda: (lambda v: v if v.get("found") else None)(point_for_row(cdp, DIR_A, "断开")), 15,
                                  "folder A's 断开 button")
        click(cdp, disconnect)
        e2e.wait_for(lambda: (lambda v: v if any(f.get("key") == key_a and f.get("state") == "disconnected" for f in v.get("folders") or []) else None)(cdp.evaluate(e2e.ROW)),
                     180, "folder A to report 已断开")
        e2e.wait_for(lambda: (e2e.mount_type(str(path_a)) == "") or None, 60, "A's kernel mount to detach")
        if path_a.is_dir() and any(path_a.iterdir()):
            raise AssertionError("A's mount point is not empty after the unmount")
        if not path_a.is_dir():
            raise AssertionError("A's stable mount point was removed by a plain disconnect")
        e2e.wait_for(lambda: (record_for_path(root, path_a) is None) or None, 60, "A's gateway record to go")
        # B is untouched: still mounted, still served, still bound into the worker.
        if not e2e.mount_type(str(path_b)).startswith("fuse"):
            raise AssertionError("disconnecting A detached B as well")
        host_ok, host_detail = host_io(path_b, "b-after-a-disconnect.txt", "B2\n")
        if not host_ok:
            raise AssertionError(f"folder B stopped serving when A disconnected: {host_detail}")
        argv = e2e.wait_for(lambda: worker_argv(root, log_path), 60, "the restarted worker")
        if str(path_b) not in argv or str(path_a) in argv:
            raise AssertionError("the restarted worker does not bind exactly the surviving mount")
        sandbox_ok, sandbox_detail = sandbox_io(argv, path_b, "b-in-sandbox.txt", "B3\n")
        if not sandbox_ok:
            raise AssertionError(f"the sandbox lost folder B when A disconnected: {sandbox_detail}")
        entries = browser_workspaces(dsh_home, workspace)
        if len(entries) != 2 or entries.get(str(path_a), {}).get("id") != ids_before[str(path_a)]:
            raise AssertionError(f"a disconnect disturbed the workspace mapping: {entries}")
        note("A's mount and record are gone, its empty mount point and workspace entry stay,")
        note("and B kept serving from both the host and the account's sandbox")

        # ── connect A again: the SAME path and the SAME workspace ────────────────────
        reconnect = e2e.wait_for(lambda: (lambda v: v if v.get("found") else None)(point_for_row(cdp, DIR_A, "连接")), 15,
                                 "folder A's 连接 button")
        click(cdp, reconnect)
        e2e.wait_for(lambda: (lambda v: v if any(f.get("key") == key_a and f.get("state") == "connected" for f in v.get("folders") or []) else None)(cdp.evaluate(e2e.ROW)),
                     240, "folder A to be connected again")
        record_a = e2e.wait_for(lambda: record_for_path(root, path_a), 60, "A's gateway record to come back")
        if record_a.get("Path") != str(path_a):
            raise AssertionError(f"A reconnected at {record_a.get('Path')} instead of {path_a}")
        if record_a.get("ID") != key_a:
            raise AssertionError(f"A reconnected with id {record_a.get('ID')} instead of its key {key_a}")
        if record_a.get("Persistent") is not True:
            raise AssertionError(f"A's mount is not marked persistent: {record_a}")
        if not e2e.mount_type(str(path_a)).startswith("fuse"):
            raise AssertionError("A did not come back as a real FUSE mount")
        host_ok, host_detail = host_io(path_a, "a-again.txt", "A2\n")
        if not host_ok:
            raise AssertionError(f"folder A did not serve again: {host_detail}")
        entries = e2e.wait_for(lambda: (lambda e: e if len(e) == 2 else None)(browser_workspaces(dsh_home, workspace)), 60,
                               "the two workspace entries")
        if entries.get(str(path_a), {}).get("id") != ids_before[str(path_a)]:
            raise AssertionError(f"reconnecting A changed its workspace: {entries}")
        note(f"A reconnected at the SAME path {path_a.name} with the SAME workspace id {ids_before[str(path_a)]}")

        # ── delete A only ───────────────────────────────────────────────────────────
        delete = e2e.wait_for(lambda: (lambda v: v if v.get("found") else None)(point_for_row(cdp, DIR_A, "删除")), 15,
                              "folder A's 删除 button")
        click(cdp, delete)
        confirm = e2e.wait_for(lambda: (lambda v: v if v.get("found") else None)(point_for_row(cdp, DIR_A, "确认删除")), 15,
                               "the confirmation on A's deletion")
        click(cdp, confirm)
        try:
            e2e.wait_for(lambda: (lambda v: v if len(v.get("folders") or []) == 1 else None)(cdp.evaluate(e2e.ROW)), 180,
                         "the list to hold only folder B")
        except AssertionError:
            listed = cdp.evaluate(e2e.ROW) or {}
            raise AssertionError("deleting A did not release it: " + json.dumps({
                "pageFolders": [{k: f.get(k) for k in ("key", "state", "text")} for f in listed.get("folders") or []],
                "browserWorkspaces": sorted(browser_workspaces(dsh_home, workspace)),
            }, ensure_ascii=False)[:900])
        e2e.wait_for(lambda: (e2e.mount_type(str(path_a)) == "") or None, 60, "A's mount to go")
        e2e.wait_for(lambda: (not path_a.exists()) or None, 60, "A's purged mount point to be removed")
        try:
            e2e.wait_for(lambda: (len(browser_workspaces(dsh_home, workspace)) == 1) or None, 60,
                         "A's workspace entry to be removed")
        except AssertionError:
            # The deletion travels over the tenant's own channel, not this plugin's endpoints,
            # so the recorded WebSocket frames are what says whether it was sent and answered.
            listed = cdp.evaluate(e2e.ROW) or {}
            raise AssertionError("A's workspace entry survived the folder deletion: " + json.dumps({
                "pageFolders": [{k: f.get(k) for k in ("key", "state", "text")} for f in listed.get("folders") or []],
                "browserWorkspaces": sorted(browser_workspaces(dsh_home, workspace)),
                "allWorkspaces": sorted(((workspace_store(dsh_home).get("tables") or {}).get("workspaces") or {})),
            }, ensure_ascii=False)[:1200])
        if not e2e.mount_type(str(path_b)).startswith("fuse"):
            raise AssertionError("deleting A detached B")
        note("deleting A released its mount, its mount point and its workspace entry; B is untouched")

        # ── a reload leaves B resumable, and one click takes it back ────────────────
        cdp.call("Page.navigate", {"url": tenant_url})
        time.sleep(3)
        resumable = e2e.wait_for(lambda: (lambda v: v if v.get("state") == "resumable" else None)(cdp.evaluate(e2e.ROW)), 90,
                                 "the reloaded page to offer the surviving folder")
        note(f"after the reload the row offers the survivor: {resumable.get('text')!r} (state={resumable.get('state')})")
        if not resumable.get("dialogOpen"):
            icon = e2e.wait_for(lambda: (lambda v: v if v.get("found") else None)(cdp.evaluate(e2e.MANAGE_POINT)), 15,
                                "the folder icon after the reload")
            click(cdp, icon)
        resume = e2e.wait_for(lambda: (lambda v: v if v.get("found") else None)(point_for_row(cdp, DIR_B, "连接")), 20,
                              "folder B's 连接 button after the reload")
        click(cdp, resume)
        e2e.wait_for(lambda: (lambda v: v if any(f.get("key") == key_b and f.get("state") == "connected" for f in v.get("folders") or []) else None)(cdp.evaluate(e2e.ROW)),
                     120, "folder B to be connected after the reload")
        if not e2e.mount_type(str(path_b)).startswith("fuse"):
            raise AssertionError("B's mount did not survive the reload")
        record_b = record_for_path(root, path_b)
        if not record_b or record_b.get("ID") != key_b:
            raise AssertionError(f"B's identity changed across the reload: {record_b}")
        host_ok, host_detail = host_io(path_b, "b-after-reload.txt", "B4\n")
        if not host_ok:
            raise AssertionError(f"folder B does not serve after the reload: {host_detail}")
        note(f"one click after the reload restored B's own mount ({path_b.name}, id {key_b})")

        # ── releasing the last folder, so the account can be removed ───────────────
        # `tenant remove` refuses while a browser mount is live (the offline guard is right
        # about that), so the last folder is deleted through the window first — which also
        # pins that deleting the LAST folder releases its path and its workspace entry.
        delete = e2e.wait_for(lambda: (lambda v: v if v.get("found") else None)(point_for_row(cdp, DIR_B, "删除")), 20,
                              "folder B's 删除 button")
        click(cdp, delete)
        confirm = e2e.wait_for(lambda: (lambda v: v if v.get("found") else None)(point_for_row(cdp, DIR_B, "确认删除")), 15,
                               "the confirmation on B's deletion")
        click(cdp, confirm)
        e2e.wait_for(lambda: (lambda v: v if not (v.get("folders") or []) else None)(cdp.evaluate(e2e.ROW)), 240,
                     "the folder list to empty")
        e2e.wait_for(lambda: (e2e.mount_type(str(path_b)) == "") or None, 60, "B's mount to go")
        e2e.wait_for(lambda: (not path_b.exists()) or None, 60, "B's purged mount point to be removed")
        e2e.wait_for(lambda: (len(browser_workspaces(dsh_home, workspace)) == 0) or None, 60,
                     "B's workspace entry to be removed")
        if records(root):
            raise AssertionError(f"a gateway record survived both deletions: {records(root)}")
        note("deleting the last folder released its mount, its path and its workspace entry")

        # ── the account is removed; nothing of this run survives ───────────────────
        removed = e2e.run([args.dshgw, "--config", str(config_path), "tenant", "remove", "-purge", "-yes", TENANT], cwd=str(REPO))
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
        note("no JavaScript exception and no failed browser-workspace request on the tenant page")

        print(f"PASS: several browser directories mount, disconnect, reconnect and delete independently ({len(steps)} steps)")
        return 0
    except e2e.Skip as skip:
        print(f"SKIP: {skip}")
        return 0
    except AssertionError as failure:
        print(f"FAIL: {failure}")
        return 1
    finally:
        if cdp is not None:
            with contextlib.suppress(Exception):
                cdp.sock.close()
        e2e.stop_group(chrome)
        if root.exists() and not args.keep:
            subprocess.run([args.dshgw, "--config", str(root / "dshgw.yaml"), "tenant", "remove",
                            "-purge", "-yes", TENANT], cwd=str(REPO), capture_output=True, timeout=60)
            for stale in (e2e.mounts_under(root) if root.exists() else []):
                subprocess.run(["fusermount3", "-u", stale], capture_output=True)
                subprocess.run(["fusermount3", "-z", stale], capture_output=True)
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
        else:
            print(f"kept: {root}")


if __name__ == "__main__":
    sys.exit(main())
