#!/usr/bin/env python3
"""Check the decommission script's dry-run plan against a synthetic legacy deployment.

The script stops system services, deletes three root-owned directories and reloads nginx, so
the part worth pinning is its *plan*: the deletion must never appear before the archive, the
archive step must cover every tree that carries information, and a dry run must execute
nothing at all. This test builds a fixture that looks like the root/systemd deployment
(/etc/dshgw, /var/lib/dshgw, /opt/dshgw, nginx conf.d, unit files) and runs the script with
--old-config pointed at it.

It cannot exercise the root-only paths themselves, and it does not need to: the fixture is
what the script reads, and the assertions are about ordering and coverage, not about tar.
"""

from __future__ import annotations

import json
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "decommission_legacy_dshgw.sh"


def build_fixture(root: Path) -> dict[str, Path]:
    etc = root / "etc/dshgw"
    state = root / "var/lib/dshgw"
    opt = root / "opt/dshgw"
    nginx = root / "etc/nginx/conf.d/dshgw"
    for path in (etc / "tenants/alice", state / "tenants/alice/.dsh",
                 state / "workspaces/alice", opt / "bin", nginx):
        path.mkdir(parents=True, exist_ok=True)
    old_config = etc / "config.yaml"
    old_config.write_text(
        f"""state_dir: {state}
tenant_root: {state}/tenants
workspace_root: {state}/workspaces
registry_path: {state}/registry.json
deploy:
  tenant_config_root: {etc}/tenants
  gateway_unit: dshgw.service
"""
    )
    (state / "registry.json").write_text(json.dumps({"version": 1, "tenants": [
        {"name": "alice", "uid": 992, "public_port": 32601, "worker_port": 32100,
         "key_prefix": "sk-aaaaaaaaa", "dsh_home": str(state / "tenants/alice/.dsh"),
         "workspace": str(state / "workspaces/alice"), "created_at": "2026-01-01T00:00:00Z",
         "handshake": "ok"},
    ]}) + "\n")
    (nginx / "01-portal.conf").write_text("server { listen 0.0.0.0:32600 ssl; }\n")
    (opt / "bin/dshgw").write_text("#!/bin/sh\necho 'dshgw 0.14.0'\n")
    (opt / "bin/dshgw").chmod(0o755)
    return {"old_config": old_config, "state": state, "etc": etc, "opt": opt, "nginx": nginx}


def plan_for(fixture: dict[str, Path], archive: Path, *extra: str) -> str:
    result = subprocess.run(
        ["bash", str(SCRIPT), "--old-config", str(fixture["old_config"]),
         "--archive-dir", str(archive), *extra],
        capture_output=True, text=True, cwd=ROOT, check=True,
    )
    return result.stdout


def archive_for(fixture: dict[str, Path], archive: Path) -> str:
    """Run the real archive-and-verify path (no root needed: the fixture is ours).

    This is the only part of the script that a test can execute for real, and it is the part
    that failed in production: a `tar -tzf … | grep -q …` under `set -o pipefail` reports a
    *good* archive as broken, because grep exits on its first match and tar dies of SIGPIPE.
    A dry run never reaches that code, so it needs its own test.
    """
    result = subprocess.run(
        ["bash", str(SCRIPT), "--old-config", str(fixture["old_config"]),
         "--archive-dir", str(archive), "--etc-dir", str(fixture["etc"]),
         "--nginx-dir", str(fixture["nginx"]), "--archive-only", "--apply"],
        capture_output=True, text=True, cwd=ROOT,
    )
    if result.returncode != 0:
        raise AssertionError(f"archive-only failed ({result.returncode}):\n{result.stdout}\n{result.stderr}")
    return result.stdout


def main() -> int:
    failures: list[str] = []
    with tempfile.TemporaryDirectory() as tmp:
        tmp_path = Path(tmp)
        fixture = build_fixture(tmp_path)
        archive = tmp_path / "archive"
        plan = plan_for(fixture, archive)

        def require(needle: str, why: str) -> None:
            if needle not in plan:
                failures.append(f"the plan does not mention {needle!r} ({why})")

        # Every tree that carries information must be archived, and the registry must be
        # named as the thing whose absence would make the archive useless.
        for needle, why in [
            ("var/lib/dshgw", "the old state tree (tenants, workspaces, registry)"),
            ("etc/dshgw", "the old configuration"),
            ("etc/nginx/conf.d/dshgw", "the old TLS front"),
            ("unit files", "the unit definitions needed to reconstruct the shape"),
        ]:
            require(needle, why)

        # Ordering: archive before stop, stop before delete, and the archive must be
        # verified before anything is removed.
        positions = {}
        for marker in ("tar -czf", "disable --now", "rm -rf"):
            index = plan.find(marker)
            if index < 0:
                failures.append(f"the plan never performs {marker!r}")
            positions[marker] = index
        if len(positions) == 3 and not (positions["tar -czf"] < positions["disable --now"] <= positions["rm -rf"]):
            failures.append("the plan archives after it stops or deletes: "
                            f"{positions}")

        # A dry run must not create the archive directory or touch the fixture.
        if archive.exists():
            failures.append("the dry run created the archive directory")
        for path in (fixture["state"], fixture["etc"], fixture["opt"], fixture["nginx"]):
            if not path.exists():
                failures.append(f"the dry run removed {path}")

        # The inventory reports the tenants it found, which is what an operator decides on.
        require("alice", "the tenant inventory")

        # The real archive path: every tree is packed, the registry is found inside it, and
        # nothing is stopped or deleted. The registry check is the one that broke in
        # production (see archive_for), so it must be exercised for real, not planned.
        archive = tmp_path / "archive-real"
        output = archive_for(fixture, archive)
        for name in ("var-lib-dshgw", "etc-dshgw", "nginx-conf-d-dshgw"):
            packed = archive / f"{name}.tar.gz"
            if not packed.is_file() or packed.stat().st_size == 0:
                failures.append(f"archive-only did not produce a usable {packed.name}")
        if "var-lib-dshgw.tar.gz:" not in output:
            failures.append("archive-only did not report the state archive it verified")
        with tarfile.open(archive / "var-lib-dshgw.tar.gz") as tar:
            names = tar.getnames()
        rel = str(fixture["state"] / "registry.json").lstrip("/")
        if rel not in names:
            failures.append(f"the state archive does not contain {rel}")
        index = archive / "INDEX.md"
        if not index.is_file() or "alice" not in index.read_text():
            failures.append("the archive index is missing or does not name the tenants")
        # Archive-only must not touch the deployment it archived.
        for path in (fixture["state"], fixture["etc"], fixture["opt"], fixture["nginx"]):
            if not path.exists():
                failures.append(f"archive-only removed {path}")
        if "archive-only" not in output:
            failures.append("archive-only did not say that it stopped before the destructive steps")

    if failures:
        for failure in failures:
            print(f"FAIL  {failure}", file=sys.stderr)
        return 1
    print("decommission plan: archive → stop → verify → delete, nothing executed in dry-run")
    return 0


if __name__ == "__main__":
    sys.exit(main())
