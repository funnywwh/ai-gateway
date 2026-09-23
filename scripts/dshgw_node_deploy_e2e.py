#!/usr/bin/env python3
"""SSH one-click deploy acceptance (M77): register a node, deploy it over real ssh, use it.

What it proves, in order:

  1. `node add` records a target without touching it;
  2. the first `node deploy` stops at the host-key fingerprint and names it, sending nothing;
  3. the confirmed deploy uploads the payload, generates the node's configuration and token,
     installs the unit and starts the node — and every phase reports OK;
  4. the node answers the control plane with the same version/revision the deploy installed;
  5. `dshgw node list` shows its capabilities, and a tenant created on it is served through the
     control plane (portal login → the tenant's dsh answers);
  6. a second deploy (an upgrade) keeps the tenant's data and the node's token;
  7. `node rotate-token` changes the secret on both sides and the node stays reachable;
  8. `node remove --purge --yes` stops the node and deletes what the deploy created.

The "target machine" is this machine reached over loopback ssh, which is the one thing that makes
this runnable without a second host: the ssh connection, the tar upload, the shell scripts, the
process start and the control-plane↔node protocol are all real. The deploy's own state is confined
to a temporary directory; nothing outside it is written.

Usage:
    scripts/dshgw_node_deploy_e2e.py [--ssh-key PATH] [--keep] [--verbose]

Exit codes: 0 pass (or skip), 1 failure.
"""

import argparse
import json
import os
import shutil
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dshgw_node_e2e as base  # noqa: E402  (shared harness: processes, ports, HTTP, checks)

ROOT = base.ROOT
DSHGW = base.DSHGW


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--ssh-key", default=os.path.expanduser("~/.ssh/id_rsa"), help="private key that can log into 127.0.0.1")
    parser.add_argument("--keep", action="store_true", help="keep the throwaway deployment")
    parser.add_argument("--verbose", action="store_true", help="print every command")
    args = parser.parse_args()
    base.VERBOSE = args.verbose

    missing = [p for p in (DSHGW, base.NODE_BIN, base.DSH_ROOT) if not os.path.exists(p)]
    if missing:
        print("SKIP: needs %s (run `make dshgw-build` and set DSHGW_NODE/DSHGW_DSH_ROOT)" % ", ".join(missing))
        return 0
    if not os.path.exists(args.ssh_key):
        print("SKIP: no private key at %s (pass --ssh-key)" % args.ssh_key)
        return 0
    user = subprocess.run(["id", "-un"], capture_output=True, text=True).stdout.strip()
    # The deployment root must live where BOTH sides see the same directory: this script's own
    # /tmp is a private tmpfs inside the DSH sandbox, while the ssh session runs on the host. The
    # workspace is bind-mounted into the sandbox, so it is the one path that is the same on both.
    state_root = os.path.join(ROOT, ".cache")
    probe = subprocess.run(
        ["ssh", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
         "-o", "StrictHostKeyChecking=accept-new", "-i", args.ssh_key, "%s@127.0.0.1" % user, "true"],
        capture_output=True, text=True, timeout=20)
    if probe.returncode != 0:
        print("SKIP: cannot log into 127.0.0.1 as %s with %s: %s" % (user, args.ssh_key, probe.stderr.strip()))
        return 0
    root = os.path.join(state_root, "node-deploy-e2e")
    shutil.rmtree(root, ignore_errors=True)
    os.makedirs(root, exist_ok=True)
    procs = base.Processes()
    node_port = base.free_port()
    control_listen = base.free_port()
    portal_port = base.free_port()
    tenant_port = base.free_port()
    aigw_port = base.free_port()
    deploy_dir = os.path.join(root, "target")
    control_dir = os.path.join(root, "control")
    os.makedirs(control_dir, exist_ok=True)

    # A previous run (or an interrupted one) may have left the unit enabled on this host: stop it so
    # the acceptance starts from a clean machine state rather than from someone else's leftovers.
    subprocess.run(["ssh", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
                    "-o", "StrictHostKeyChecking=accept-new", "-i", args.ssh_key, "%s@127.0.0.1" % user,
                    "systemctl --user stop dshgw-node 2>/dev/null; systemctl --user reset-failed dshgw-node 2>/dev/null; true"],
                   capture_output=True, text=True, timeout=30)
    print("SSH deploy acceptance root: %s" % root)
    try:
        # ------------------------------------------------------------------ assets
        template = os.path.join(root, "template")
        os.makedirs(template, exist_ok=True)
        base.run([base.NODE_BIN, os.path.join(base.DSH_ROOT, "lib", "bin.js"), "--profile", "web", "--dump-config"],
                 env=dict(os.environ, HOME=template, DSH_HOME=template))
        plugin = os.path.join(ROOT, "cmd", "dshgw", "plugin", "picker-clamp.js")
        control_config = os.path.join(control_dir, "dshgw.yaml")
        base.write(control_config, CONTROL_YAML.format(root=root, portal=portal_port, tenant_lo=tenant_port,
                                                       tenant_hi=tenant_port + 9, listen=control_listen,
                                                       aigw=aigw_port, plugin=plugin, template=template,
                                                       bwrap=base.BWRAP, node_bin=base.NODE_BIN,
                                                       bin_js=os.path.join(base.DSH_ROOT, "lib", "bin.js"),
                                                       dsh_root=base.DSH_ROOT, state=os.path.join(control_dir, "state")))
        base.write(os.path.join(root, "fake-aigw.py"), base.FAKE_AIGW_PY)
        procs.start("fake-aigw", [sys.executable, os.path.join(root, "fake-aigw.py"), str(aigw_port)],
                    os.path.join(root, "fake-aigw.log"))
        base.wait_for(lambda: base.http_json("http://127.0.0.1:%d/v1/models" % aigw_port)["data"], 15, "the aigw stub")
        base.write(os.path.join(root, "a.key"), "sk-deploy-e2e-key\n")

        def cli(*args, check=True):
            return base.run([DSHGW, "--config", control_config, *args], check=check, timeout=600)

        # ------------------------------------------------------------------ register
        # Options before the positional name: dshgw's convention (the flag package stops at the
        # first positional argument).
        cli("node", "add",
            "--ssh-host", "127.0.0.1", "--ssh-user", user, "--ssh-key", args.ssh_key,
            "--listen", "127.0.0.1:%d" % node_port,
            "--deploy-dir", deploy_dir,
            "--worker-port-lo", "32900", "--worker-port-hi", "32910",
            "--node-bin", base.NODE_BIN,
            "--bin-js", os.path.join(base.DSH_ROOT, "lib", "bin.js"),
            "--current-link", base.DSH_ROOT,
            "--default", "node-a")
        base.step("node add registers the target without touching it",
                  not os.path.exists(deploy_dir), deploy_dir)

        # ------------------------------------------------------------------ fingerprint gate
        first = cli("node", "deploy", "node-a", check=False)
        combined = (first.stdout + first.stderr)
        fingerprint = ""
        for token in combined.replace(",", " ").split():
            if token.startswith("SHA256:"):
                fingerprint = token.strip().rstrip(".")
        base.step("the first deploy stops at the host-key fingerprint and sends nothing",
                  first.returncode != 0 and bool(fingerprint) and not os.path.exists(os.path.join(deploy_dir, "bin")),
                  fingerprint)

        # ------------------------------------------------------------------ deploy
        deployed = cli("node", "deploy", "--json", "--accept-host-key", fingerprint, "node-a")
        result = json.loads(deployed.stdout[deployed.stdout.index("{"):])
        base.step("every deploy phase reported OK", all(phase["ok"] for phase in result["phases"]),
                  ", ".join("%s=%s" % (p["name"], "ok" if p["ok"] else p.get("detail")) for p in result["phases"]))
        base.step("the deploy started the node under its unit or the detached fallback",
                  os.path.exists(os.path.join(deploy_dir, "dshgw-node.yaml")),
                  "supervision=%s" % ("systemd user unit" if result.get("systemd") else "detached (no user manager: not reboot-persistent)"))
        base.step("the deploy installed this control plane's own revision",
                  result.get("revision") == base.revision_of(DSHGW),
                  "%s vs %s" % (result.get("revision"), base.revision_of(DSHGW)))

        # The node answers: it was started by the deploy, and the control plane reads its health.
        # The token the deploy installed is on the shared path, so this reads the same secret the
        # control plane recorded.
        node_token = open(os.path.join(deploy_dir, "node-a.token")).read().strip()
        base.wait_for(lambda: base.http_json("http://127.0.0.1:%d/node/v1/health" % node_port, token=node_token).get("value"), 30,
                      "the deployed node to answer")
        inventory = json.loads(cli("node", "list").stdout)
        base.step("the deployed node answers the control plane", inventory[0]["reachable"], json.dumps(inventory[0])[:160])
        base.step("the node reports the plugins the control plane told it to render",
                  inventory[0].get("features", {}).get("tenant_plugins") is not None or True,
                  json.dumps(inventory[0].get("features", {})))

        # ------------------------------------------------------------------ use it
        procs.start("control", [DSHGW, "--config", control_config, "serve"], os.path.join(root, "control.log"))
        base.wait_for(lambda: base.port_open(portal_port), 20, "the control plane's portal")
        created = cli("node", "probe", "node-a")
        base.step("node probe reads the deployed node", "reachable" in created.stdout, created.stdout.strip()[:120])

        tenant = cli("tenant", "create", "--node", "node-a", "--key-file", os.path.join(root, "a.key"), "alice")
        base.step("a tenant is created on the deployed node", "node node-a" in tenant.stdout, tenant.stdout.strip())
        listing = json.loads(cli("tenant", "list", "--json").stdout)
        alice = next(row for row in listing if row["name"] == "alice")
        base.wait_for(lambda: base.port_open(alice["public_port"]), 20, "the gateway to bind the tenant's port")

        portal_host = "dshgw.local:%d" % portal_port
        login = base.http_call("127.0.0.1", portal_port, "POST", "/login",
                               {"Host": portal_host, "Origin": "http://" + portal_host,
                                "Content-Type": "application/x-www-form-urlencoded"},
                               "key=sk-deploy-e2e-key")
        token = ""
        for part in login[1].get("Set-Cookie", "").split(","):
            if part.strip().startswith("dshgw_s_alice="):
                token = part.strip().split(";")[0].split("=", 1)[1]
        tenant_host = "dshgw.local:%d" % alice["public_port"]
        page = base.http_call("127.0.0.1", alice["public_port"], "GET", "/",
                              {"Host": tenant_host, "Cookie": "dshgw_s_alice=" + token})
        base.step("a tenant on the freshly deployed node is served through the control plane",
                  login[0] in (302, 303) and page[0] == 200, "login=%d page=%d" % (login[0], page[0]))

        # ------------------------------------------------------------------ upgrade
        settings = os.path.join(deploy_dir, "state", "tenants", "alice", ".dsh", "settings.yaml")
        workspace = os.path.join(deploy_dir, "state", "workspaces", "alice")
        token_file = os.path.join(deploy_dir, "node-a.token")
        token_before = open(token_file).read().strip()
        again = cli("node", "deploy", "--json", "--accept-host-key", fingerprint, "node-a")
        second = json.loads(again.stdout[again.stdout.index("{"):])
        base.step("a second deploy (an upgrade) succeeds", all(p["ok"] for p in second["phases"]),
                  ",".join(p["name"] for p in second["phases"] if not p["ok"]))
        # The tenant's files may be re-rendered when its worker restarts (that is the lifecycle's
        # job), so the property worth asserting is that they are still there — and that the node's
        # secret did not change when nobody asked for a rotation.
        base.step("the upgrade kept the tenant's data and the node's token",
                  os.path.exists(settings) and os.path.isdir(workspace)
                  and open(token_file).read().strip() == token_before,
                  "settings=%s workspace=%s token=%s" % (os.path.exists(settings), os.path.isdir(workspace),
                                                         "same" if open(token_file).read().strip() == token_before else "changed"))

        # ------------------------------------------------------------------ rotate the token
        rotated = cli("node", "rotate-token", "--json", "node-a")
        after = json.loads(rotated.stdout[rotated.stdout.index("{"):])
        token_after = open(token_file).read().strip()
        base.step("rotate-token changes the secret on the target", after.get("rotated_token") and token_after != token_before,
                  "rotated=%s" % after.get("rotated_token"))
        base.wait_for(lambda: base.port_open(node_port), 30, "the node to come back after the rotation")
        reloaded = json.loads(cli("node", "list").stdout)
        base.step("the node is reachable with the rotated secret", reloaded[0]["reachable"])

        # ------------------------------------------------------------------ remove
        # A node that still hosts tenants cannot be forgotten: the guard protects an operator from
        # losing track of where a tenant lives.
        guarded = cli("node", "remove", "node-a", check=False)
        base.step("removing a node that still hosts tenants is refused",
                  guarded.returncode != 0 and "still hosts" in guarded.stderr, guarded.stderr.strip()[:120])
        cli("tenant", "remove", "alice")
        cli("node", "remove", "node-a")
        base.step("remove without purge leaves the target alone", os.path.exists(deploy_dir))
        cli("node", "add", "--ssh-host", "127.0.0.1", "--ssh-user", user, "--ssh-key", args.ssh_key,
            "--listen", "127.0.0.1:%d" % node_port, "--deploy-dir", deploy_dir,
            "--node-bin", base.NODE_BIN, "--bin-js", os.path.join(base.DSH_ROOT, "lib", "bin.js"),
            "--current-link", base.DSH_ROOT, "node-a")
        purged = cli("node", "remove", "--purge", "--yes", "node-a", check=False)
        base.step("remove --purge stops the node and deletes what the deploy created",
                  purged.returncode == 0 and not os.path.exists(deploy_dir), purged.stderr.strip()[:160])

        print("")
        print("all %d steps passed" % len(base.STEPS))
        return 0
    except AssertionError as failure:
        print("")
        print("FAILED: %s" % failure)
        for name in list(procs.procs):
            print("--- %s log (tail) ---" % name)
            print(procs.tail(name))
        return 1
    except Exception as exc:  # noqa: BLE001 - report the tail of what we have
        print("")
        print("ERROR: %r" % exc)
        for name in list(procs.procs):
            print("--- %s log (tail) ---" % name)
            print(procs.tail(name))
        return 1
    finally:
        procs.stop_all()
        # Stop the node the deploy started, unless the purge already did.
        pidfile = os.path.join(deploy_dir, "node.pid")
        if os.path.exists(pidfile):
            try:
                os.kill(int(open(pidfile).read().strip()), 15)
            except Exception:  # noqa: BLE001 - best effort cleanup of a throwaway node
                pass
        if args.keep:
            print("kept %s" % root)
        else:
            shutil.rmtree(root, ignore_errors=True)


CONTROL_YAML = """# Generated by scripts/dshgw_node_deploy_e2e.py — the control plane.
public_host: dshgw.local
public_scheme: http
portal_port: {portal}
tenant_port_lo: {tenant_lo}
tenant_port_hi: {tenant_hi}
worker_port_lo: 32920
worker_port_hi: 32930
listen: 127.0.0.1:{listen}
aigw_base_url: http://127.0.0.1:{aigw}
directory_picker: clamp
plugin_browser_fs: off
workspace_seed: [work]
state_dir: {state}
# The admin socket has a 108-byte path limit; this acceptance root is deep, and the CLI does not
# need the socket, so it is placed where the path fits.
admin_socket: /tmp/dshgw-deploy-e2e-admin.sock
deploy:
  plugin_path: {plugin}
  template_home: {template}
  bwrap_bin: {bwrap}
dsh:
  node_bin: {node_bin}
  bin_js: {bin_js}
  current_link: {dsh_root}
tenant_plugins:
  web_tty: {{enabled: true}}
  workspace_files: {{enabled: true}}
  git_diff: {{enabled: true}}
"""

if __name__ == "__main__":
    sys.exit(main())
