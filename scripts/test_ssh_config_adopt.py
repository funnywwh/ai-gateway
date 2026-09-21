#!/usr/bin/env python3
"""Behaviour tests for scripts/ssh_config_adopt.sh.

The script is the one step of the ssh_config_dir migration that touches operator data, so its
three promises are pinned here: it adopts what each account has today, it never overwrites a
seed the operator already wrote, and it never touches an account workspace.

    python3 scripts/test_ssh_config_adopt.py
"""

from __future__ import annotations

import os
import stat
import subprocess
import sys
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
SCRIPT = REPO / "scripts" / "ssh_config_adopt.sh"

failures: list[str] = []
checks = 0


def check(condition: bool, message: str) -> None:
    global checks
    checks += 1
    if not condition:
        failures.append(message)


def run(*args: str, cwd: Path) -> subprocess.CompletedProcess:
    return subprocess.run(["sh", str(SCRIPT), *args], cwd=cwd, capture_output=True, text=True)


def make_account(workspaces: Path, name: str, config: str | None) -> None:
    home = workspaces / name / ".ssh"
    home.mkdir(parents=True, exist_ok=True)
    if config is not None:
        (home / "config").write_text(config, encoding="utf-8")


def main() -> int:
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        workspaces = root / "workspaces"
        dest = root / "ssh-configs"
        make_account(workspaces, "dsh-colin", "Host aipc\n  HostName 10.0.0.1\n")
        make_account(workspaces, "dsh-ran", "Host gpt001\n  HostName gpt001.example\n")
        make_account(workspaces, "dsh-empty", None)
        (workspaces / "not-an-account.txt").write_text("x\n", encoding="utf-8")

        # A dry run reports and writes nothing.
        dry = run("--workspaces", str(workspaces), "--dest", str(dest), "--dry-run", cwd=root)
        check(dry.returncode == 0, f"dry run failed: {dry.stderr}")
        check("would   dsh-colin" in dry.stdout, "the dry run names the account it would adopt")
        check(not dest.exists(), "a dry run writes nothing")
        check("not-an-account.txt" not in dry.stdout, "a non-directory entry is not an account")

        # A real run adopts each account that has a config, and skips one that has none.
        first = run("--workspaces", str(workspaces), "--dest", str(dest), cwd=root)
        check(first.returncode == 0, f"adopt failed: {first.stderr}")
        colin = dest / "dsh-colin"
        check(colin.read_text(encoding="utf-8").startswith("Host aipc"), "the account's own config became its seed")
        check((dest / "dsh-ran").exists(), "every account with a config is adopted")
        check(not (dest / "dsh-empty").exists(), "an account with no config gets no seed")
        check(stat.S_IMODE(colin.stat().st_mode) == 0o644, "a seed is written 0644")
        check("adopted=2" in first.stdout, f"the summary counts what it did: {first.stdout!r}")

        # Idempotent: an existing seed is kept, never overwritten, and nothing is lost.
        (dest / "dsh-colin").write_text("Host curated\n", encoding="utf-8")
        second = run("--workspaces", str(workspaces), "--dest", str(dest), cwd=root)
        check(second.returncode == 0, f"the second run failed: {second.stderr}")
        check((dest / "dsh-colin").read_text(encoding="utf-8") == "Host curated\n", "an existing seed is never overwritten")
        check("kept=2" in second.stdout, f"the kept seeds are reported: {second.stdout!r}")

        # The workspaces themselves are untouched: the live deployment keeps reading them.
        check((workspaces / "dsh-colin" / ".ssh" / "config").read_text(encoding="utf-8").startswith("Host aipc"),
              "an account workspace is not modified")
        check(sorted(p.name for p in (workspaces / "dsh-colin" / ".ssh").iterdir()) == ["config"],
              "no file is added to an account's .ssh")

        # A missing workspace root is an error, not a silent no-op.
        missing = run("--workspaces", str(root / "absent"), "--dest", str(dest), cwd=root)
        check(missing.returncode == 1, "a missing workspace root fails")

        # An empty deployment still gets the directory the configuration names.
        fresh_workspaces = root / "fresh"
        fresh_workspaces.mkdir()
        fresh_dest = root / "fresh-dest"
        fresh = run("--workspaces", str(fresh_workspaces), "--dest", str(fresh_dest), cwd=root)
        check(fresh.returncode == 0, f"an empty workspace root is fine: {fresh.stderr}")
        check(fresh_dest.is_dir(), "the configured seed directory is created even with nothing to adopt")

    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1
    print(f"ssh_config_adopt: {checks} assertions passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
