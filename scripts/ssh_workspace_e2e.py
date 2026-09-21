#!/usr/bin/env python3
"""End-to-end acceptance for the M64 ssh workspace (docs/design/m64-ssh-workspace.md).

What it proves, on the gateway host itself:

  1. an account's mailbox request is consumed by dshgw, and the requested remote directory
     becomes a real sshfs mount inside that account's workspace;
  2. the account's worker profile binds that mount point, so the mount is visible *inside the
     sandbox* — the assumption the whole design rests on, because bubblewrap's --bind does not
     carry submounts and a tenant worker cannot mount anything itself;
  3. the mount stays private: the account's workspace is the only host tree in its view;
  4. removing the account detaches the mount again (mounts are kernel state, not files).

It runs a throwaway dshgw instance in its own state directory and port band, so the live
deployment is untouched. It skips itself (exit 0 with SKIP) wherever the pieces are missing:
sshfs, bwrap, a dsh runtime, or a non-interactive loopback ssh.

    python3 scripts/ssh_workspace_e2e.py [--root .cache/m64-ssh-e2e] [--keep]

Run it on the gateway host: inside a development sandbox the ssh server lives in another mount
namespace and cannot see the directory that would be mounted (the script says so and skips).
"""

from __future__ import annotations

import argparse
import json
import threading
import os
import pwd
import shutil
import signal
import socket
import subprocess
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent

# The port band is deliberately outside every live range (18xxx/32xxx) so a stray run can
# never collide with the deployment this repository serves.
LISTEN = "127.0.0.1:13099"
PORTAL_PORT = 13600
TENANT_PORT_LO, TENANT_PORT_HI = 13601, 13799
WORKER_PORT_LO, WORKER_PORT_HI = 13100, 13299


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


class StubAigw:
    """The two answers tenant creation needs from aigw: a model list, and nothing else.

    Tenant creation validates the key against aigw's /v1/models (the model list becomes the
    tenant's provider settings), so a stub is enough — and it keeps this acceptance away from
    the live gateway's keys, which must never be bound to a second tenant.
    """

    def __init__(self) -> None:
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:  # noqa: N802 - http.server's spelling
                if self.path.split("?")[0] != "/v1/models":
                    self.send_error(404)
                    return
                body = json.dumps({"data": [{"id": "ssh-e2e-model", "name": "ssh-e2e-model"}]}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

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


def locks_under(root: Path) -> list[str]:
    return mounts_under(root)


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


def config_document(args, root: Path, template_home: Path, aigw_base_url: str) -> dict:
    return {
        "public_host": "ssh-e2e.invalid",
        "listen": LISTEN,
        "portal_port": PORTAL_PORT,
        "tenant_port_lo": TENANT_PORT_LO,
        "tenant_port_hi": TENANT_PORT_HI,
        "worker_port_lo": WORKER_PORT_LO,
        "worker_port_hi": WORKER_PORT_HI,
        # A stub, so this acceptance never touches the live gateway's keys: tenant creation
        # validates the key against aigw /v1/models, and that list becomes the tenant's
        # provider settings. Nothing here needs a real upstream.
        "aigw_base_url": aigw_base_url,
        "key_revalidate": "off",
        "directory_picker": "clamp",
        "state_dir": str(root / "state"),
        "dsh": {
            "node_bin": args.node,
            # Resolved: the sandbox binds the release directory the launcher resolves to, so a
            # bin.js reached through the deployment's `current` symlink would be a path the
            # sandbox does not have (the worker then exits immediately).
            "bin_js": os.path.realpath(args.bin_js),
            "current_link": args.current_link,
        },
        "deploy": {
            "plugin_path": str(REPO / "cmd/dshgw/plugin/picker-clamp.js"),
            "template_home": str(template_home),
            "public_listen": "127.0.0.1",
            # Tenants run as the account that runs dshgw, which is how the deployed shape
            # works too; naming it keeps the default (a service account) out of the way.
            "gateway_user": pwd.getpwuid(os.geteuid()).pw_name,
            "worker_user": pwd.getpwuid(os.geteuid()).pw_name,
        },
        "ssh_workspaces": {
            "enabled": True,
            "mount_subdir": "ssh",
            "identity_source": str(Path(args.identity).expanduser()),
            # Per-account alias seeds: the only source of a tenant's aliases. The e2e proves the
            # alias a request names is resolved from THIS account's file (see the e2e-seed step)
            # and that the account's own list never picks up another account's seed.
            "ssh_config_dir": str(root / "ssh-configs"),
            "hosts": ["127.0.0.1", "e2e-seed"],
            "connect_timeout": "10s",
            "poll_interval": "1s",
            "max_entries": 100,
            "sshfs_options": ["reconnect", "ServerAliveInterval=15", "ServerAliveCountMax=3", "idmap=user"],
        },
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", default=str(REPO / ".cache/m64-ssh-e2e"))
    parser.add_argument("--dshgw", default=str(REPO / "bin/dshgw"))
    parser.add_argument("--node", default=os.environ.get("DSHGW_NODE", ""))
    parser.add_argument("--bin-js", default=os.environ.get("DSHGW_BIN_JS", ""))
    parser.add_argument("--current-link", default=os.environ.get("DSHGW_DSH_ROOT", ""))
    parser.add_argument("--template-home", default=str(REPO / "data/dshgw-verify/template-home"))
    parser.add_argument("--identity", default="~/.ssh/id_rsa")
    parser.add_argument("--keep", action="store_true", help="leave the throwaway deployment behind")
    parser.add_argument("--skip-teardown", action="store_true", help="stop before removing the account (debugging)")
    args = parser.parse_args()

    root = Path(args.root).resolve()
    tenant = "ssh-e2e"
    steps: list[str] = []

    def note(message: str) -> None:
        steps.append(message)
        print(f"  · {message}", flush=True)

    server: subprocess.Popen | None = None
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

        # The mount source, the ssh server and this process must share one mount namespace.
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
        # An interrupted earlier run leaves its gateway (and its mounts) behind; the pattern
        # names this run's own config file, so nothing else can match.
        subprocess.run(["pkill", "-f", f"{config_path} serve"], capture_output=True)
        host, port = LISTEN.split(":")
        if port_open(host, int(port)):
            raise Skip(f"{LISTEN} is already in use: stop the process holding it")
        for stale in locks_under(root):
            subprocess.run(["fusermount3", "-u", stale], capture_output=True)
            subprocess.run(["fusermount3", "-z", stale], capture_output=True)
        config_path.write_text(json.dumps(config_document(args, root, template_home, stub.base_url), indent=2) + "\n", encoding="utf-8")
        config_path.chmod(0o600)

        # Per-account alias seeds. `dsh-other` belongs to an account that does not exist here:
        # nothing of it may ever reach this account's config.
        me = pwd.getpwuid(os.geteuid()).pw_name
        seeds = root / "ssh-configs"
        seeds.mkdir(parents=True, exist_ok=True)
        (seeds / tenant).write_text(f"Host e2e-seed\n  HostName 127.0.0.1\n  User {me}\n", encoding="utf-8")
        (seeds / "dsh-other").write_text("Host other-seed\n  HostName 10.255.255.1\n", encoding="utf-8")

        # The removed host-wide key must be a loud, actionable failure — never a silent
        # fallback that would hand every account the operator's own ~/.ssh/config again.
        legacy = config_document(args, root, template_home, stub.base_url)
        legacy["ssh_workspaces"] = dict(legacy["ssh_workspaces"])
        legacy["ssh_workspaces"].pop("ssh_config_dir", None)
        legacy["ssh_workspaces"]["ssh_config_source"] = f"/home/{me}/.ssh/config"
        legacy_path = root / "legacy.yaml"
        legacy_path.write_text(json.dumps(legacy, indent=2) + "\n", encoding="utf-8")
        legacy_path.chmod(0o600)
        refused = run([args.dshgw, "--config", str(legacy_path), "tenant", "list"], cwd=str(REPO))
        if refused.returncode == 0 or "ssh_config_dir" not in (refused.stdout + refused.stderr):
            raise AssertionError(f"a config naming the removed ssh_config_source was accepted: {refused.stdout}{refused.stderr}")
        note("a configuration that still names ssh_config_source is refused, and the error names ssh_config_dir")

        # ── the throwaway gateway ────────────────────────────────────────────────────────
        log = open(root / "dshgw.log", "w", encoding="utf-8")
        server = subprocess.Popen(
            [args.dshgw, "--config", str(config_path), "serve"],
            stdout=log, stderr=subprocess.STDOUT, cwd=str(REPO),
        )
        host, port = LISTEN.split(":")
        wait_for(lambda: port_open(host, int(port)), 30, "the test gateway to listen")
        note(f"throwaway dshgw listening on {LISTEN} (state {root / 'state'})")

        key_file = root / "tenant.key"
        key_file.write_text("sk-ssh-e2e-0000000000\n", encoding="utf-8")
        key_file.chmod(0o600)
        # A tenant with an empty model list is fine here: this acceptance needs the worker
        # profile, not model access.
        created = run([
            args.dshgw, "--config", str(config_path), "tenant", "create",
            "-key-file", str(key_file), "-allow-empty-models", "-directory-picker", "clamp",
            # The browser-fs plugin is a separate feature with its own template pin; this
            # acceptance is about ssh mounts, and the live template may not pin it.
            "-browser-fs", "off", tenant,
        ], cwd=str(REPO))
        if created.returncode != 0:
            raise AssertionError(f"tenant create failed: {created.stdout}{created.stderr}")
        registry = json.loads((root / "state" / "registry.json").read_text(encoding="utf-8"))
        record = next(item for item in registry["tenants"] if item["name"] == tenant)
        workspace, dsh_home = Path(record["workspace"]), Path(record["dsh_home"])
        note(f"account {tenant}: workspace {workspace}, dsh home {dsh_home}")

        # ── the request the tenant plugin writes ─────────────────────────────────────────
        request_dir = dsh_home / "ssh-requests"
        request_dir.mkdir(parents=True, exist_ok=True)
        request = {"id": "open-e2e", "op": "open", "host": "127.0.0.1", "remote": str(remote),
                   "createdAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
        payload = request_dir / "open-e2e.json"
        payload.write_text(json.dumps(request), encoding="utf-8")
        payload.chmod(0o600)

        def reply() -> dict | None:
            answer = dsh_home / "ssh-replies" / "open-e2e.json"
            if not answer.exists():
                return None
            document = json.loads(answer.read_text(encoding="utf-8"))
            return document if document.get("ok") is True else None

        answer = wait_for(reply, 90, "the gateway to answer the mount request")
        if answer.get("mountpoint", "") == "":
            raise AssertionError(f"the gateway reported no mount point: {answer}")
        mountpoint = Path(answer["mountpoint"])
        note(f"gateway answered ok, mount point {mountpoint} (worker restarted: {answer.get('restarted')})")

        # The mount is real kernel state and lives inside the account's own workspace.
        fstype = wait_for(lambda: mount_type(str(mountpoint)) or None, 10, "the mount to appear in the table")
        assert fstype.startswith("fuse"), f"unexpected filesystem type {fstype}"
        if workspace not in mountpoint.parents:
            raise AssertionError(f"{mountpoint} is not inside {workspace}")
        content = (mountpoint / "marker.txt").read_text(encoding="utf-8")
        if content != "from the remote host\n":
            raise AssertionError(f"reading through the mount returned {content!r}")
        note(f"{mountpoint} is {fstype} and reads the remote file directly")

        # The account's own mirror is what its plugin reads to tell a mount from a parent
        # directory of the mirror layout.
        mirror = json.loads((dsh_home / "ssh-mounts.json").read_text(encoding="utf-8"))
        if [item["mountpoint"] for item in mirror["mounts"]] != [str(mountpoint)]:
            raise AssertionError(f"the account mirror does not name the mount: {mirror}")
        note("the account's mount mirror names exactly this mount")

        # ── the account's own alias list is what a mount is resolved with ────────────────
        config_text = (workspace / ".ssh" / "config").read_text(encoding="utf-8")
        if "Host e2e-seed" not in config_text or "127.0.0.1" not in config_text:
            raise AssertionError(f"the account config was not seeded from ssh_config_dir:\n{config_text}")
        if "other-seed" in config_text or "10.255.255.1" in config_text:
            raise AssertionError(f"another account's seed leaked into this account's config:\n{config_text}")
        note("the account config came from its own seed, and no other account's seed is in it")

        alias_request = {"id": "open-alias", "op": "open", "host": "e2e-seed", "remote": str(remote),
                         "createdAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
        (request_dir / "open-alias.json").write_text(json.dumps(alias_request), encoding="utf-8")
        (request_dir / "open-alias.json").chmod(0o600)

        def alias_reply() -> dict | None:
            answer_file = dsh_home / "ssh-replies" / "open-alias.json"
            if not answer_file.exists():
                return None
            document = json.loads(answer_file.read_text(encoding="utf-8"))
            return document if document.get("ok") is True else None

        alias_answer = wait_for(alias_reply, 90, "the gateway to answer the mount request by alias")
        alias_mountpoint = Path(alias_answer["mountpoint"])
        if alias_mountpoint.parts[-1] != mountpoint.parts[-1] or "e2e-seed" not in alias_mountpoint.parts:
            raise AssertionError(f"the alias mount landed in an unexpected place: {alias_mountpoint}")
        alias_fstype = wait_for(lambda: mount_type(str(alias_mountpoint)) or None, 10, "the alias mount to appear")
        if not alias_fstype.startswith("fuse"):
            raise AssertionError(f"unexpected filesystem type {alias_fstype}")
        if (alias_mountpoint / "marker.txt").read_text(encoding="utf-8") != "from the remote host\n":
            raise AssertionError("reading through the alias mount did not reach the remote file")
        note("an alias from the account's own config (e2e-seed) mounts the same remote directory")

        # ── the decisive check: is the mount visible INSIDE the sandbox? ──────────────────
        printed = run([args.dshgw, "--config", str(config_path), "sandbox-exec", "--print", tenant], cwd=str(REPO))
        if printed.returncode != 0:
            raise AssertionError(f"sandbox-exec --print failed: {printed.stdout}{printed.stderr}")
        argv = [line for line in printed.stdout.splitlines() if line != ""]
        separator = argv.index("--")
        # The scaffolding directories bwrap creates for the bind target do exist, so the
        # question is never "is /home empty" but "is anything of the host's home readable":
        # the operator's keys, the gateway's own configuration and the live data root are the
        # three that must not be.
        script = (
            f"cat {mountpoint}/marker.txt; echo ---; cat {alias_mountpoint}/marker.txt; echo ---; ls -1 {mountpoint}; echo ---; "
            "for probe in /home/%(user)s/.ssh /home/%(user)s/work/ai_gateway/config.yaml "
            "/home/%(user)s/work/ai_gateway/data; do "
            "[ -e \"$probe\" ] && echo \"LEAK $probe\"; done; echo ---; ls -A /home/%(user)s 2>&1"
        ) % {"user": me}
        inside = run([*argv[: separator + 1], "/bin/sh", "-c", script])
        if inside.returncode != 0:
            raise AssertionError(f"the sandbox run failed: {inside.stdout}{inside.stderr}")
        if inside.stdout.count("from the remote host") != 2:
            raise AssertionError(f"a mount is NOT visible inside the sandbox:\n{inside.stdout}{inside.stderr}")
        if "marker.txt" not in inside.stdout:
            raise AssertionError(f"the mount's contents are missing inside the sandbox:\n{inside.stdout}")
        note("both mounts (by address and by this account's alias) and their contents are visible inside the account's own sandbox")

        # …and the rest of the host is still hidden, so the new binding widened nothing.
        if "LEAK " in inside.stdout:
            raise AssertionError(f"the new binding exposed host paths:\n{inside.stdout}")
        note("the operator's keys, aigw's config and the live data root stay invisible inside the same sandbox")

        # ── teardown: removing the account detaches the mount ────────────────────────────
        if args.skip_teardown:
            print(f"PASS(partial): ssh workspace end-to-end, teardown skipped ({len(steps)} steps)")
            return 0
        removed = run([args.dshgw, "--config", str(config_path), "tenant", "remove", "-purge", "-yes", tenant], cwd=str(REPO))
        if removed.returncode != 0:
            raise AssertionError(f"tenant remove failed: {removed.stdout}{removed.stderr}")
        wait_for(lambda: (mount_type(str(mountpoint)) == "") or None, 15, "the mount to be detached")
        wait_for(lambda: (mount_type(str(alias_mountpoint)) == "") or None, 15, "the alias mount to be detached")
        if workspace.exists():
            raise AssertionError(f"the purged workspace survived: {workspace}")
        note("removing the account detached the mount and purged its state")

        print(f"PASS: ssh workspace end-to-end ({len(steps)} steps)")
        return 0
    except Skip as skip:
        print(f"SKIP: {skip}")
        return 0
    finally:
        if root.exists():
            # Best effort: an interrupted run can leave a mount behind, and mounts are kernel
            # state that outlives both the gateway and this script.
            subprocess.run([args.dshgw, "--config", str(root / "dshgw.yaml"), "tenant", "remove",
                            "-purge", "-yes", tenant], cwd=str(REPO), capture_output=True, timeout=60)
            for line in (mounts_under(root) if root.exists() else []):
                subprocess.run(["fusermount3", "-u", line], capture_output=True)
                subprocess.run(["fusermount3", "-z", line], capture_output=True)
        stub.__exit__()
        if server is not None and server.poll() is None:
            server.send_signal(signal.SIGTERM)
            try:
                server.wait(timeout=15)
            except subprocess.TimeoutExpired:
                server.kill()
        if not args.keep and root.exists():
            shutil.rmtree(root, ignore_errors=True)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except AssertionError as failure:
        print(f"FAIL: {failure}")
        sys.exit(1)
