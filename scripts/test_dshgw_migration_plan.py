#!/usr/bin/env python3
"""Check the migration script's dry-run plans against a synthetic old deployment.

The migration stops tenants and changes ownership, so its *plan* is the part worth
pinning: this test builds a fixture that looks like the root/systemd deployment and
asserts that `--apply --dry-run` and `--rollback --dry-run` print every step the
operator is about to approve. Neither run needs root or touches the host.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts/migrate_dshgw_to_supervised.sh"


def build_fixture(root: Path) -> dict[str, Path]:
    etc = root / "etc/dshgw"
    state = root / "var/lib/dshgw"
    workspaces = root / "srv/dsh"
    for path in (etc / "tenants/alice", etc / "tenants/bob",
                 state / "tenants/alice/.dsh", state / "tenants/bob/.dsh",
                 workspaces / "alice", workspaces / "bob", root / "backups"):
        path.mkdir(parents=True, exist_ok=True)
    old_config = etc / "config.yaml"
    old_config.write_text(
        f"""state_dir: {state}
tenant_root: {state}/tenants
workspace_root: {workspaces}
registry_path: {state}/registry.json
dsh:
  node_bin: /opt/dsh/node/bin/node
  bin_js: /opt/dsh/current/lib/bin.js
  current_link: /opt/dsh/current
deploy:
  tenant_config_root: {etc}/tenants
  backup_dir: {root}/backups
  worker_unit: dsh-worker@.service
  gateway_unit: dshgw.service
"""
    )
    registry = state / "registry.json"
    registry.write_text(json.dumps({"version": 1, "tenants": [
        {"name": "alice", "uid": 992, "public_port": 32601, "worker_port": 32100,
         "key_prefix": "sk-aaaaaaaaa", "dsh_home": str(state / "tenants/alice/.dsh"),
         "workspace": str(workspaces / "alice"), "created_at": "2026-01-01T00:00:00Z", "handshake": "ok"},
        {"name": "bob", "uid": 993, "public_port": 32602, "worker_port": 32101,
         "key_prefix": "sk-bbbbbbbbb", "dsh_home": str(state / "tenants/bob/.dsh"),
         "workspace": str(workspaces / "bob"), "created_at": "2026-01-01T00:00:00Z", "handshake": "ok"},
    ]}) + "\n")
    aigw_config = root / "aigw.yaml"
    aigw_config.write_text('server:\n  listen: 127.0.0.1:8088\n')
    return {"old_config": old_config, "old_registry": registry, "aigw_config": aigw_config,
            "backups": root / "backups", "state": state, "workspaces": workspaces, "etc": etc}


def run_plan(fixture: dict[str, Path], mode: str) -> str:
    args = [str(SCRIPT), mode, "--dry-run",
            "--old-config", str(fixture["old_config"]),
            "--old-registry", str(fixture["old_registry"]),
            "--aigw-config", str(fixture["aigw_config"]),
            "--backup-dir", str(fixture["backups"]),
            "--aigw-user", os.environ.get("USER", "nobody")]
    result = subprocess.run(args, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        raise SystemExit(f"FAIL: {mode} dry-run exited {result.returncode}:\n{result.stdout}\n{result.stderr}")
    return result.stdout


def require(plan: str, needle: str, label: str) -> None:
    if needle not in plan:
        raise SystemExit(f"FAIL: {label} plan is missing {needle!r}\n--- plan ---\n{plan}")


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="dshgw-migration-plan.") as tmp:
        fixture = build_fixture(Path(tmp))
        apply_plan = run_plan(fixture, "--apply")
        # The plan must name every consequential step: the backup, the old units it
        # stops, the ownership it changes, the registry rewrite, and the aigw config
        # it extends. A migration that quietly skips one of these is the failure
        # mode this test exists for.
        require(apply_plan, "PLAN  cp -a", "apply")                      # registry backup
        require(apply_plan, "systemctl disable --now dshgw.service", "apply")
        require(apply_plan, "systemctl disable --now dshgw-admin.service", "apply")
        require(apply_plan, f"chown -R {os.environ.get('USER', 'nobody')}", "apply")
        require(apply_plan, "python3 -", "apply")                        # registry rewrite
        require(apply_plan, "append", "apply")                            # aigw config block
        require(apply_plan, "tenants in registry: 2", "apply")

        rollback_plan = run_plan(fixture, "--rollback")
        require(rollback_plan, "systemctl --user disable --now aigw-local.service", "rollback")
        require(rollback_plan, "systemctl enable --now dshgw.service", "rollback")
        require(rollback_plan, "chown -R dsh-alice:dsh-alice", "rollback")
        require(rollback_plan, f"chown -R root:", "rollback")

        # Neither plan may execute anything.
        if "RUN " in apply_plan or "RUN " in rollback_plan:
            raise SystemExit("FAIL: a dry-run executed a command")

    print("PASS: dshgw migration plan (apply + rollback dry-runs)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
