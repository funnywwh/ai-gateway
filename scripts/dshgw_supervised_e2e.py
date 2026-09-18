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

    def __init__(self, models: list[str]) -> None:
        self.models = models
        self.calls = 0
        self.port = free_port()
        stub = self

        class Handler(http.server.BaseHTTPRequestHandler):
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
                body = json.dumps({"data": [{"id": m} for m in stub.models]}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

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


def http_probe(port: int, path: str = "/", timeout: float = 5.0) -> tuple[int, str]:
    """GET one loopback port; returns (status, location). 0 means "nothing bound".

    With no nginx in this shape, dshgw binds the portal port and every tenant's
    public port itself, so these ports are the user-facing surface.
    """
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=timeout) as conn:
            conn.sendall(f"GET {path} HTTP/1.0\r\nHost: localhost:{port}\r\n\r\n".encode())
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
                 memory_max: int, tasks_max: int, cpu_quota: int) -> None:
    doc = f"""server:
  listen: 127.0.0.1:{aigw_port}
  secret_key: supervised-e2e-secret
database:
  path: {state_dir}/aigw.db
credentials_key: "2jGxUI1VPSsMQgx0lbhv5OuGywKnCDUN"
log:
  level: debug
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
  worker_memory_max_bytes: {memory_max}
  worker_tasks_max: {tasks_max}
  worker_cpu_quota_percent: {cpu_quota}
"""
    path.write_text(doc)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--aigw-bin", required=True)
    parser.add_argument("--dshgw-bin", required=True)
    parser.add_argument("--node", default=os.environ.get("DSHGW_NODE", ""))
    parser.add_argument("--dsh-root", default=os.environ.get("DSHGW_DSH_ROOT", ""))
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

    stub = StubAigw(models=["e2e-model-a", "e2e-model-b"])
    aigw: subprocess.Popen | None = None
    aigw_log = None
    tenant = "e2e-supervised"
    try:
        template = prepare_template(node, dsh_root, work, check)
        write_config(
            config_path,
            aigw_port=free_port(),
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
        )
        stub.start()
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
        tenant_status, location = http_probe(ports["tenant_lo"])
        check.require(
            "tenant-port-redirects-to-portal",
            tenant_status == 302 and f":{ports['portal']}" in location,
            f"HTTP {tenant_status} location={location!r}",
        )

        # Resource limits: each worker gets its own cgroup v2 group with the
        # configured ceilings. A host that cannot provide one is reported as a
        # skip (the worker still runs), matching the degradation the runner logs.
        cgroup_note = check_worker_limits(check, process_tree(child_pid))

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
