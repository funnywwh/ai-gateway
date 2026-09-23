#!/usr/bin/env python3
"""Multi-machine acceptance (M77): one control plane, one worker node, one tenant.

What it proves, in order:

  1. a tenant created with `--node <node>` is provisioned and run by the **node**, while the
     control plane's own process tree stays free of that worker;
  2. the control plane reads the tenant's state (running, worker port) over the control channel,
     and the port it records is the node's own;
  3. stop/start travel to the node and change what is running there;
  4. a node agent restart loses its workers and brings back the ones its own allocation table
     says should run — on the same worker port, so a live session's handshake authority stays
     valid;
  5. `node reconcile` repairs a control-plane record whose worker port no longer matches;
  6. a suspended tenant stays stopped across a node restart and across a reconcile;
  7. removal drops the control plane's entry and credential copy while the node's data stays.

Everything runs on one machine (two addresses, two state roots, two processes), which is what
makes this runnable in acceptance without a second host; the protocol, the registries and the
process boundaries are the real ones. The worker is a real `dsh web` inside bubblewrap — the
node's sandbox is not simulated — and aigw is a stub that answers `GET /v1/models`, because the
only thing the gateway needs from aigw here is a model list for a key.

Usage:
    scripts/dshgw_node_e2e.py [--keep] [--verbose]

Exit codes: 0 pass (or skip), 1 failure, 2 misconfiguration of this script's environment.
"""

import argparse
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DSHGW = os.path.join(ROOT, "bin", "dshgw")
NODE_BIN = os.environ.get("DSHGW_NODE", os.path.expanduser("~/.local/node-v22.23.1-linux-x64/bin/node"))
DSH_ROOT = os.environ.get("DSHGW_DSH_ROOT", os.path.expanduser("~/.local/dsh-0.1.2-rc.1"))
BWRAP = "/usr/bin/bwrap"

VERBOSE = False
STEPS = []


def log(message):
    print(message, flush=True)


def run(args, check=True, timeout=180, env=None):
    if VERBOSE:
        log("  $ " + " ".join(args))
    proc = subprocess.run(args, capture_output=True, text=True, timeout=timeout, env=env)
    if check and proc.returncode != 0:
        raise AssertionError(
            "%s failed (exit %d)\n--- stdout ---\n%s\n--- stderr ---\n%s"
            % (" ".join(args), proc.returncode, proc.stdout, proc.stderr)
        )
    return proc


def dshgw(config, *args, check=True):
    return run([DSHGW, "--config", config, *args], check=check)


def bwrap_can_create_namespaces():
    """Whether this host lets an unprivileged process build a namespace.

    Inside a DSH tenant sandbox the answer is no — the session already runs in one, and the
    kernel refuses to nest them. That is the same limitation `make dshgw-sandbox-test` reports,
    so this script skips with the same explicit reason instead of failing a deployment that is
    fine.
    """
    probe = subprocess.run([BWRAP, "--unshare-pid", "--dev", "/dev", "--proc", "/proc", "/bin/true"],
                           capture_output=True, text=True)
    return probe.returncode == 0, (probe.stderr or probe.stdout).strip()


def free_port():
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    return port


def step(name, ok, detail=""):
    STEPS.append((name, ok, detail))
    mark = "PASS" if ok else "FAIL"
    log("%-4s %s%s" % (mark, name, (" — " + detail) if detail else ""))
    if not ok:
        raise AssertionError("%s: %s" % (name, detail))


class Processes:
    def __init__(self):
        self.procs = {}
        self.logs = {}

    def start(self, name, args, log_path):
        log_file = open(log_path, "w")
        self.logs[name] = log_path
        proc = subprocess.Popen(args, stdout=log_file, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL, start_new_session=True)
        self.procs[name] = proc
        return proc

    def stop(self, name, grace=20):
        proc = self.procs.pop(name, None)
        if proc is None or proc.poll() is not None:
            return
        try:
            os.killpg(os.getpgid(proc.pid), signal.SIGTERM)
        except ProcessLookupError:
            return
        deadline = time.time() + grace
        while time.time() < deadline:
            if proc.poll() is not None:
                return
            time.sleep(0.2)
        try:
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        except ProcessLookupError:
            pass
        proc.wait(timeout=5)

    def stop_all(self):
        for name in list(self.procs):
            self.stop(name)

    def tail(self, name, lines=25):
        path = self.logs.get(name)
        if not path or not os.path.exists(path):
            return "(no log)"
        with open(path, "r", errors="replace") as handle:
            return "".join(handle.readlines()[-lines:])


def wait_for(predicate, timeout, what, interval=0.25):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            value = predicate()
            if value:
                return value
        except Exception as exc:  # noqa: BLE001 - the probe itself may fail while starting up
            last = exc
        time.sleep(interval)
    raise AssertionError("timed out waiting for %s%s" % (what, (": %s" % last) if last else ""))


def revision_of(binary):
    """The revision a dshgw binary reports (`dshgw --version` prints "revision <value>")."""
    out = subprocess.run([binary, "--version"], capture_output=True, text=True).stdout
    if "revision " in out:
        value = out.split("revision ", 1)[1]
        for separator in (")", ","):
            if separator in value:
                value = value.split(separator, 1)[0]
        return value.strip()
    return ""


def port_open(port):
    """Whether something is accepting connections on that port."""
    probe = socket.socket()
    probe.settimeout(1)
    try:
        probe.connect(("127.0.0.1", port))
        return True
    except OSError:
        return False
    finally:
        probe.close()


def http_call(host, port, method, path, headers=None, body=None):
    """One HTTP round trip with no redirect following and the headers verbatim.

    The portal answers 303 + Set-Cookie and the edge dispatches on Host, so a client that follows
    redirects or rewrites Host would hide exactly what these steps assert.
    """
    import http.client

    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=20)
    try:
        connection.request(method, path, body=body, headers=headers or {})
        response = connection.getresponse()
        payload = response.read().decode(errors="replace")
        return response.status, dict(response.getheaders()), payload
    finally:
        connection.close()


def http_json(url, token=None, data=None, method=None):
    request = urllib.request.Request(url, method=method or ("POST" if data else "GET"))
    if token:
        request.add_header("Authorization", "Bearer " + token)
    body = None
    if data is not None:
        body = json.dumps(data).encode()
        request.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(request, data=body, timeout=10) as response:
        return json.loads(response.read().decode() or "{}")


def write(path, content, mode=0o600):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as handle:
        handle.write(content)
    os.chmod(path, mode)


def dsh_worker_pids(port):
    """Processes holding the tenant's worker port, as /proc sees them."""
    pids = []
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        try:
            with open("/proc/%s/cmdline" % entry, "rb") as handle:
                cmdline = handle.read().decode(errors="replace").replace("\0", " ")
        except OSError:
            continue
        if "web --port %d" % port in cmdline or ("web" in cmdline and "--port %d" % port in cmdline):
            pids.append(int(entry))
    return pids


def main():
    global VERBOSE
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--keep", action="store_true", help="keep the throwaway deployment for inspection")
    parser.add_argument("--verbose", action="store_true", help="print every command the script runs")
    parser.add_argument("--passthrough-bwrap", action="store_true",
                        help="replace bubblewrap with a pass-through stand-in so the protocol and process path "
                             "can be exercised on a host that forbids nested namespaces; it proves nothing about "
                             "the sandbox, which is why the real run needs a host where bwrap works")
    args = parser.parse_args()
    VERBOSE = args.verbose

    missing = [path for path in (DSHGW, NODE_BIN, DSH_ROOT, BWRAP) if not os.path.exists(path)]
    if missing:
        log("SKIP: multi-machine acceptance needs %s (run `make dshgw-build` and set DSHGW_NODE/DSHGW_DSH_ROOT)" % ", ".join(missing))
        return 0
    if not os.access(BWRAP, os.X_OK):
        log("SKIP: %s is not executable on this host" % BWRAP)
        return 0
    namespaces, reason = bwrap_can_create_namespaces()
    if not namespaces and args.passthrough_bwrap:
        log("NOTE: bubblewrap cannot create a namespace here, so the sandbox is replaced by a pass-through")
        log("      stand-in. This run proves the control-plane↔node protocol and the process path; it does NOT")
        log("      prove the tenant sandbox. Run `make dshgw-node-e2e` on a host where bwrap works for that.")
    if not namespaces and not args.passthrough_bwrap:
        log("SKIP: this host does not allow a non-privileged namespace, so a node cannot start a real worker here.")
        log("      %s" % reason.splitlines()[0] if reason else "")
        log("      Run this on the gateway host (or any machine where `bwrap --unshare-pid /bin/true` works):")
        log("        make dshgw-node-e2e")
        return 0

    root = tempfile.mkdtemp(prefix="dshgw-node-e2e-")
    procs = Processes()
    aigw_port = free_port()
    node_port = free_port()
    control_listen = free_port()
    portal_port = free_port()
    tenant_port = free_port()
    token = "e2e-node-token-0123456789abcdef"
    key = "sk-e2e-node-acceptance-key"
    bwrap = BWRAP
    if not namespaces:
        # A stand-in that drops bubblewrap's own flags and runs the command after "--": the worker
        # is then a real `dsh web` process, just without a namespace around it.
        bwrap = os.path.join(root, "passthrough-bwrap.sh")
        write(bwrap, "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = \"--\" ]; then shift; exec \"$@\"; fi; shift; done\nexit 1\n", 0o755)

    log("multi-machine acceptance root: %s" % root)
    try:
        # ---------------------------------------------------------------- assets
        template = os.path.join(root, "template")
        write(os.path.join(root, "empty"), "", 0o600)
        env = dict(os.environ, HOME=template, DSH_HOME=template)
        os.makedirs(template, exist_ok=True)
        run([NODE_BIN, os.path.join(DSH_ROOT, "lib", "bin.js"), "--profile", "web", "--dump-config"], env=env)

        plugin = os.path.join(ROOT, "cmd", "dshgw", "plugin", "picker-clamp.js")
        node_dir = os.path.join(root, "node")
        control_dir = os.path.join(root, "control")
        os.makedirs(node_dir, exist_ok=True)
        os.makedirs(control_dir, exist_ok=True)
        write(os.path.join(root, "node-a.token"), token + "\n")
        write(os.path.join(root, "a.key"), key + "\n")

        write(os.path.join(root, "fake-aigw.py"), FAKE_AIGW_PY)
        procs.start("fake-aigw", [sys.executable, os.path.join(root, "fake-aigw.py"), str(aigw_port)], os.path.join(root, "fake-aigw.log"))
        wait_for(lambda: http_json("http://127.0.0.1:%d/v1/models" % aigw_port)["data"], 15, "the aigw stub")

        node_config = os.path.join(node_dir, "dshgw-node.yaml")
        os.makedirs(os.path.join(root, "shared"), exist_ok=True)
        write(node_config, node_yaml(root, template, plugin, node_port, aigw_port, bwrap))
        control_config = os.path.join(control_dir, "dshgw.yaml")
        write(control_config, control_yaml(root, template, plugin, control_listen, portal_port, tenant_port, node_port, aigw_port, bwrap))

        # ---------------------------------------------------------------- node up
        procs.start("node", [DSHGW, "--config", node_config, "node", "serve"], os.path.join(root, "node.log"))
        wait_for(lambda: http_json("http://127.0.0.1:%d/node/v1/health" % node_port, token=token).get("value"), 20, "the node agent")
        step("node agent serves its control surface", True)

        procs.start("control", [DSHGW, "--config", control_config, "serve"], os.path.join(root, "control.log"))
        wait_for(lambda: dshgw(control_config, "node", "list", check=False).returncode == 0, 20, "the control plane CLI")
        inventory = json.loads(dshgw(control_config, "node", "list").stdout)
        step("the control plane sees the node as reachable",
             bool(inventory) and inventory[0]["reachable"], json.dumps(inventory)[:200])

        # ---------------------------------------------------------------- create
        # Options before the positional name: dshgw's convention (the flag package stops at the
        # first positional argument).
        created = dshgw(control_config, "tenant", "create", "--node", "node-a",
                        "--key-file", os.path.join(root, "a.key"), "alice")
        step("tenant create on the node succeeded", "node node-a" in created.stdout, created.stdout.strip())

        listing = json.loads(dshgw(control_config, "tenant", "list", "--json").stdout)
        alice = next((row for row in listing if row["name"] == "alice"), None)
        step("the control plane records the tenant on the node", bool(alice) and alice["node"] == "node-a",
             json.dumps(alice)[:200] if alice else "missing")
        step("the tenant's worker is running on the node", bool(alice) and alice["running"])
        worker_port = alice["workerPort"] if "workerPort" in alice else alice.get("worker_port")
        worker_port_public = alice["public_port"]
        pids = wait_for(lambda: dsh_worker_pids(worker_port), 20, "the node's worker process")
        step("a real dsh worker runs on the node's worker port", bool(pids), "pids=%s" % pids)
        step("the node's paths are what the control plane recorded",
             alice["dsh_home"].startswith(node_dir) and alice["workspace"].startswith(node_dir),
             "dsh_home=%s" % alice["dsh_home"])
        step("the control plane's own state root holds no tenant data",
             not os.path.exists(os.path.join(control_dir, "state", "workspaces", "alice"))
             and not os.path.exists(os.path.join(control_dir, "state", "tenants", "alice")))

        # Node-side features (M77): the node renders the tenant plugins, materializes the host-share
        # target inside the tenant's workspace, and reports both in its feature list so the console
        # can say which machine offers what.
        patch_path = os.path.join(alice["dsh_home"], "profiles", "web", "cordis.patch.yml")
        with open(patch_path) as handle:
            patch = handle.read()
        step("the node renders the tenant-side web plugins into the tenant's profile",
             all(name in patch for name in ("web-tty", "workspace-files", "git-diff")),
             patch_path)
        share_target = os.path.join(alice["workspace"], "host", "shared")
        step("the node materializes the declared host-directory share inside the tenant's workspace",
             os.path.isdir(share_target), share_target)
        inventory = json.loads(dshgw(control_config, "node", "list").stdout)
        features = inventory[0].get("features", {})
        step("the node reports what it can serve",
             sorted(features.get("tenant_plugins", [])) == ["git-diff", "web-tty", "workspace-files"]
             and features.get("host_shares") == 1,
             json.dumps(features))

        # Node audit events reach the gateway's single audit stream (M77). The event is written by
        # hand: this asserts the pull, the cursor and the tagging, not a particular product event.
        node_audit = os.path.join(node_dir, "state", "gateway", "audit.jsonl")
        os.makedirs(os.path.dirname(node_audit), exist_ok=True)
        with open(node_audit, "a") as handle:
            handle.write(json.dumps({
                "time": "2026-09-23T01:00:00Z", "kind": "e2e-node-event", "tenant": "alice",
                "reason": "written by scripts/dshgw_node_e2e.py", "status": 403,
            }) + "\n")
        control_audit = os.path.join(control_dir, "state", "gateway", "audit.jsonl")

        def merged():
            if not os.path.exists(control_audit):
                return False
            with open(control_audit) as handle:
                for line in handle:
                    if "e2e-node-event" in line and '"node":"node-a"' in line.replace(" ", ""):
                        return True
            return False

        wait_for(merged, 60, "the node's audit event to reach the gateway's stream")
        step("node audit events are merged into the gateway's stream, tagged with the node", True)
        tail = dshgw(control_config, "node", "audit", "node-a")
        step("`node audit` reads a node's recent events", "e2e-node-event" in tail.stdout, tail.stdout.strip()[:120])

        # The running gateway picks up the CLI's registry write on its own (it watches the file),
        # and binds the tenant's public port as soon as it does.
        wait_for(lambda: port_open(worker_port_public), 15, "the gateway to bind the tenant's public port")
        step("the running gateway binds a tenant created by a separate CLI process", True)

        # ---------------------------------------------------------------- data plane
        # The tenant's traffic must reach the worker through the control plane: the browser talks to
        # the tenant's own public port here, the control plane authenticates the session, and the
        # request is forwarded to the node.
        import urllib.parse

        portal_host = "dshgw.local:%d" % portal_port
        login = http_call("127.0.0.1", portal_port, "POST", "/login",
                          {"Host": portal_host, "Origin": "http://" + portal_host,
                           "Content-Type": "application/x-www-form-urlencoded"},
                          urllib.parse.urlencode({"key": key}))
        cookies = login[1].get("Set-Cookie", "")
        session_token = ""
        for part in cookies.split(","):
            if part.strip().startswith("dshgw_s_alice="):
                session_token = part.strip().split(";")[0].split("=", 1)[1]
        step("portal login issues a session for the node-hosted tenant", login[0] in (302, 303) and bool(session_token),
             "status=%d cookie=%s" % (login[0], "yes" if session_token else "no"))

        tenant_host = "dshgw.local:%d" % worker_port_public
        page = http_call("127.0.0.1", worker_port_public, "GET", "/",
                         {"Host": tenant_host, "Cookie": "dshgw_s_alice=" + session_token})
        step("the tenant page is served through the control plane to the node's worker", page[0] == 200,
             "status=%d" % page[0])
        step("the worker's own auth cookie never reaches the browser",
             "dsh-auth-" not in page[1].get("Set-Cookie", ""), page[1].get("Set-Cookie", ""))

        # A forged node header must not survive: the control plane's own headers are the only ones
        # the node ever sees (it writes them after stripping the browser's).
        forged = http_call("127.0.0.1", worker_port_public, "GET", "/",
                           {"Host": tenant_host, "Cookie": "dshgw_s_alice=" + session_token,
                            "X-Dshgw-Tenant": "bob", "X-Dshgw-Protocol": "99", "Authorization": "Bearer forged"})
        step("a browser-forged node header is ignored", forged[0] == 200, "status=%d" % forged[0])

        # Stop the worker: the node refuses, and the control plane answers with its own wording
        # (the node's message names machines and internals).
        dshgw(control_config, "tenant", "stop", "alice")
        stopped_page = http_call("127.0.0.1", worker_port_public, "GET", "/",
                                 {"Host": tenant_host, "Cookie": "dshgw_s_alice=" + session_token})
        step("a stopped worker answers 503 through the control plane",
             stopped_page[0] == 503, "status=%d" % stopped_page[0])
        step("the node's internal wording is not shown to the browser",
             "node-a" not in stopped_page[2] and "worker" in stopped_page[2].lower(), stopped_page[2][:120])
        # The portal itself must not depend on any node being reachable.
        portal_page = http_call("127.0.0.1", portal_port, "GET", "/", {"Host": portal_host})
        step("the portal still answers while the tenant's worker is down", portal_page[0] == 200,
             "status=%d" % portal_page[0])
        dshgw(control_config, "tenant", "start", "alice")
        wait_for(lambda: dsh_worker_pids(worker_port), 20, "the worker to start again")

        # The node agent going away is a gateway-side outage: 503 with a different message.
        procs.stop("node")
        unreachable = http_call("127.0.0.1", worker_port_public, "GET", "/",
                                {"Host": tenant_host, "Cookie": "dshgw_s_alice=" + session_token})
        step("an unreachable node answers 503, not 502", unreachable[0] == 503, "status=%d" % unreachable[0])
        step("the unreachable message names the node role, not its host",
             "node is unreachable" in unreachable[2], unreachable[2][:120])
        session_still = http_call("127.0.0.1", portal_port, "GET", "/", {"Host": portal_host})
        step("the portal survives an unreachable node", session_still[0] == 200, "status=%d" % session_still[0])
        procs.start("node", [DSHGW, "--config", node_config, "node", "serve"], os.path.join(root, "node.log"))
        wait_for(lambda: http_json("http://127.0.0.1:%d/node/v1/health" % node_port, token=token).get("value"), 20, "the node agent")
        # The worker process appears before its socket accepts, so wait for the socket: the first
        # request after a node restart may otherwise race the bind.
        wait_for(lambda: port_open(worker_port), 30, "the worker's port to accept connections")
        # The session's cookie was handshaken with the worker that just died, so the first request
        # is expected to drive the re-handshake path (401 from the worker, one retry, then 200).
        recovered = None
        deadline = time.time() + 30
        while time.time() < deadline:
            recovered = http_call("127.0.0.1", worker_port_public, "GET", "/",
                                  {"Host": tenant_host, "Cookie": "dshgw_s_alice=" + session_token})
            if recovered[0] == 200:
                break
            time.sleep(0.5)
        step("the tenant works again after the node returns (re-handshake)", recovered[0] == 200,
             "status=%d" % recovered[0])

        # ---------------------------------------------------------------- stop/start
        dshgw(control_config, "tenant", "stop", "alice")
        wait_for(lambda: not dsh_worker_pids(worker_port), 20, "the worker to stop")
        stopped = json.loads(dshgw(control_config, "tenant", "list", "--json").stdout)[0]
        step("stop travels to the node and records the operator's intent",
             not stopped["running"] and stopped["suspended"], json.dumps(stopped)[:160])
        dshgw(control_config, "tenant", "start", "alice")
        wait_for(lambda: dsh_worker_pids(worker_port), 20, "the worker to start again")
        step("start brings the tenant back on the node", True)

        # ---------------------------------------------------------------- node restart
        procs.stop("node")
        step("a node agent restart takes its workers with it", not dsh_worker_pids(worker_port))
        procs.start("node", [DSHGW, "--config", node_config, "node", "serve"], os.path.join(root, "node.log"))
        wait_for(lambda: http_json("http://127.0.0.1:%d/node/v1/health" % node_port, token=token).get("value"), 20, "the node agent")
        wait_for(lambda: dsh_worker_pids(worker_port), 30, "the node to recover its worker")
        step("the node recovers its own tenants on the same worker port", bool(dsh_worker_pids(worker_port)))

        # ---------------------------------------------------------------- reconcile repairs drift
        registry_path = os.path.join(control_dir, "state", "registry.json")
        with open(registry_path) as handle:
            document = json.load(handle)
        for tenant in document["tenants"]:
            if tenant["name"] == "alice":
                tenant["worker_port"] = worker_port + 100
        write(registry_path, json.dumps(document), 0o600)
        reconciled = dshgw(control_config, "node", "reconcile", "node-a")
        with open(registry_path) as handle:
            repaired = json.load(handle)
        port_after = next(t["worker_port"] for t in repaired["tenants"] if t["name"] == "alice")
        step("reconcile records the node's real worker port", port_after == worker_port,
             "reconcile said: %s" % reconciled.stdout.strip())

        # ---------------------------------------------------------------- suspended stays stopped
        dshgw(control_config, "tenant", "stop", "alice")
        wait_for(lambda: not dsh_worker_pids(worker_port), 20, "the worker to stop")
        procs.stop("node")
        procs.start("node", [DSHGW, "--config", node_config, "node", "serve"], os.path.join(root, "node.log"))
        wait_for(lambda: http_json("http://127.0.0.1:%d/node/v1/health" % node_port, token=token).get("value"), 20, "the node agent")
        dshgw(control_config, "node", "reconcile", "node-a")
        time.sleep(1.5)
        step("a suspended tenant stays stopped across restart and reconcile", not dsh_worker_pids(worker_port))

        # ---------------------------------------------------------------- remove keeps node data
        removed = dshgw(control_config, "tenant", "remove", "alice")
        step("removal wrote a snapshot on the node", "snapshot: " in removed.stdout and removed.stdout.strip().split("snapshot: ")[-1].strip(),
             removed.stdout.strip())
        remaining = json.loads(dshgw(control_config, "tenant", "list", "--json").stdout)
        step("the control plane forgot the tenant", all(row["name"] != "alice" for row in remaining))
        step("the node's tenant data survived a removal without purge",
             os.path.isdir(os.path.join(node_dir, "state", "workspaces", "alice")))
        step("the control plane dropped its credential copy",
             not os.path.exists(os.path.join(control_dir, "state", "tenant-config", "alice")))

        log("")
        log("all %d steps passed" % len(STEPS))
        return 0
    except AssertionError as failure:
        log("")
        log("FAILED: %s" % failure)
        for name in procs.procs:
            log("--- %s log (tail) ---" % name)
            log(procs.tail(name))
        return 1
    finally:
        procs.stop_all()
        if args.keep:
            log("kept %s" % root)
        else:
            shutil.rmtree(root, ignore_errors=True)


FAKE_AIGW_PY = '''#!/usr/bin/env python3
"""A stub aigw: it answers the one endpoint dshgw reads while provisioning a tenant."""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

MODELS = {
    "data": [
        {
            "id": "deepseek-flash",
            "name": "deepseek-flash",
            "context_window": 1000000,
            "max_output_tokens": 65536,
            "capabilities": {"reasoning": True},
        }
    ]
}


class Handler(BaseHTTPRequestHandler):
    def _answer(self, payload):
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.rstrip("/") == "/version":
            self._answer({"version": "stub", "revision": "stub"})
        else:
            self._answer(MODELS)

    def do_POST(self):
        # The portal's login runs the account-level check before it issues a session; the stub
        # authorizes every key as the tenant under test.
        if self.path.rstrip("/") == "/v1/dshgw/authorize":
            self._answer({"allowed": True, "tenant": "alice", "account": "chen", "feishu_name": ""})
        else:
            self._answer({"ok": True})

    def log_message(self, *args):
        pass


HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
'''


def node_yaml(root, template, plugin, node_port, aigw_port, bwrap):
    return """# Generated by scripts/dshgw_node_e2e.py — a worker node.
state_dir: {root}/node/state
worker_port_lo: 32900
worker_port_hi: 32910
aigw_base_url: http://127.0.0.1:{aigw_port}
directory_picker: clamp
plugin_browser_fs: off
workspace_seed: [work]
node:
  name: node-a
  listen: 127.0.0.1:{node_port}
  token_file: {root}/node-a.token
deploy:
  plugin_path: {plugin}
  template_home: {template}
  bwrap_bin: {bwrap}
dsh:
  node_bin: {node_bin}
  bin_js: {bin_js}
  current_link: {dsh_root}
# The tenant-side web plugins and one host-directory share: both are node-side features (M77),
# so the node must render them and report them in its feature list.
tenant_plugins:
  web_tty: {{enabled: true}}
  workspace_files: {{enabled: true}}
  git_diff: {{enabled: true}}
host_shares:
  enabled: true
  subdir: host
  shares:
    - name: shared
      path: {root}/shared
      read_only: true
      tenants: [alice]
""".format(root=root, template=template, plugin=plugin, node_port=node_port, aigw_port=aigw_port,
           bwrap=bwrap, node_bin=NODE_BIN, bin_js=os.path.join(DSH_ROOT, "lib", "bin.js"), dsh_root=DSH_ROOT)


def control_yaml(root, template, plugin, listen_port, portal_port, tenant_port, node_port, aigw_port, bwrap):
    return """# Generated by scripts/dshgw_node_e2e.py — the control plane.
public_host: dshgw.local
public_scheme: http
portal_port: {portal_port}
tenant_port_lo: {tenant_port}
tenant_port_hi: {tenant_hi}
worker_port_lo: 32920
worker_port_hi: 32930
listen: 127.0.0.1:{listen_port}
aigw_base_url: http://127.0.0.1:{aigw_port}
directory_picker: clamp
plugin_browser_fs: off
workspace_seed: [work]
state_dir: {root}/control/state
default_node: node-a
nodes:
  - name: node-a
    url: http://127.0.0.1:{node_port}
    token_file: {root}/node-a.token
deploy:
  plugin_path: {plugin}
  template_home: {template}
  bwrap_bin: {bwrap}
dsh:
  node_bin: {node_bin}
  bin_js: {bin_js}
  current_link: {dsh_root}
tenant_plugins:
  web_tty: {{enabled: false}}
  workspace_files: {{enabled: false}}
  git_diff: {{enabled: false}}
""".format(root=root, template=template, plugin=plugin, listen_port=listen_port, portal_port=portal_port,
           tenant_port=tenant_port, tenant_hi=tenant_port + 9, node_port=node_port, aigw_port=aigw_port,
           bwrap=bwrap, node_bin=NODE_BIN, bin_js=os.path.join(DSH_ROOT, "lib", "bin.js"), dsh_root=DSH_ROOT)


if __name__ == "__main__":
    sys.exit(main())
