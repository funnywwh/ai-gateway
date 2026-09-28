#!/usr/bin/env python3
"""Check the desensitizer against fixtures, and against throwaway git repositories.

The tool is a gate: `scripts/release.sh` refuses to release when `--check` finds anything,
and the history rewrite runs the very same `--apply` over every commit. So what has to be
pinned is not "it replaced a host name once" but the properties the gate leans on:

  * the deliberate public identity survives (the module path whose owner is this repo's
    origin is left alone; any other owner is rewritten to it);
  * the two exception layers behave (`.dsh/skills/**` untouched, `[exempt]` spans untouched);
  * a longer literal wins over a shorter one it contains;
  * `--apply` is idempotent, skips binaries, renames the paths the table names, and refuses
    to sound clean when the local table is missing (`--require-table`);
  * the generic rules fire on *new* leakage -- an address, mailbox, fingerprint or home
    directory that is not in the table yet -- and stay quiet on the values the repository is
    allowed to use;
  * `--check-history` and `--filter-message` see what a tree-filter alone would not.

The fixtures are assembled at run time on purpose: this file is tracked, and the gate scans it
too, so it must not contain the very strings the rules are looking for.
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "desensitize.py"

sys.path.insert(0, str(ROOT / "scripts"))
import desensitize as d  # noqa: E402  (the module under test)

# Fixture values, built from pieces so this test file stays clean under the real gate.
FAKE_OWNER = "acme"
OTHER_OWNER = "somebody-" + "else"
MODULE_OK = "github.com/" + FAKE_OWNER + "/ai-gateway"
MODULE_BAD = "github.com/" + OTHER_OWNER + "/ai-gateway"
PRIVATE_IP = "192.168." + "77.7"
PUBLIC_IP = "47." + "44.9.1"
REAL_MAIL = "someone@" + "real-" + "corp.com"
FINGERPRINT = "SHA256:" + "Ab1+/" * 9 + "Ab"
SK_PREFIX = "sk-" + "1f2e" + "3d4c"
SK_UNKNOWN = "sk-" + "9a8b" + "7c6d"

TABLE = "\n".join([
    "[exempt]",
    r"/home/u/\.toolchain(?![\w-])",
    "",
    "[token]",
    "host_alpha\t\\bhost-alpha\\b\tgw-a",
    "host_alpha_fqdn\t\\bhost-alpha\\.corp\\.acme\\b\tgw-a.example.com",
    "alpha\t\\balpha\\b\talpha-renamed",
    "acct_mail\tlegacy@corp\\.example\towner@example.com",
    "acct_name\t(?<![A-Za-z0-9])ACCT-9\tacct-9",
    "home_u\t/home/u(?!/\\.toolchain)\t/home/operator",
    "sk_prefix\t" + SK_PREFIX + "\tsk-00000000",
    "",
    "[rename]",
    "docs/design/old-host-alpha.md\tdocs/design/old.md",
    "",
])


def write_table(root: Path) -> Path:
    path = root / "tokens.tsv"
    path.write_text(TABLE)
    return path


def load(table_file: Path) -> d.Table:
    return d.load_table(table_file)


def rewrite(text: str, table: d.Table, path: str = "docs/example.md") -> str:
    data, _fired = d.rewrite(text.encode(), path, table)
    return data.decode()


def rules_for(text: str, table: d.Table, path: str = "docs/example.md",
              strict: bool = False) -> list[str]:
    return [hit.rule for hit in d.scan(text.encode(), path, table, strict)]


def run_cli(*args: str, cwd: Path | None = None,
            table: Path | None = None) -> subprocess.CompletedProcess:
    command = [sys.executable, str(SCRIPT), *args]
    if table is not None:
        command += ["--table", str(table)]
    env = dict(os.environ, **{d.MODULE_OWNER_ENV: FAKE_OWNER})
    return subprocess.run(command, cwd=cwd, capture_output=True, text=True, env=env)


def git(repo: Path, *args: str) -> None:
    subprocess.run(["git", *args], cwd=repo, check=True, capture_output=True)


def main() -> int:
    failures: list[str] = []

    def expect(label: str, got: object, want: object) -> None:
        if got != want:
            failures.append(f"{label}: got {got!r}, want {want!r}")

    with tempfile.TemporaryDirectory() as tmp:
        home = Path(tmp)
        table_file = write_table(home)
        table = load(table_file)

        # ---- table parsing -------------------------------------------------
        expect("table loads tokens", len(table.tokens), 7)
        expect("table loads renames", table.renames,
               [("docs/design/old-host-alpha.md", "docs/design/old.md")])
        expect("table loads exemptions", len(table.exempt), 1)
        bad = home / "bad.tsv"
        bad.write_text("[token]\nonly-two\tfields\n")
        try:
            load(bad)
            failures.append("malformed table line was accepted")
        except ValueError:
            pass

        # ---- replacement, longest literal first, exemptions last -----------
        cases = [
            ("Host host-alpha\n", "Host gw-a\n"),
            ("host-alpha.corp.acme", "gw-a.example.com"),
            ("ssh host-alpha.corp.acme", "ssh gw-a.example.com"),
            ("mail: legacy@corp.example", "mail: owner@example.com"),
            ("name: ACCT-9_GROUP and dsh-acct-9-30", "name: acct-9_GROUP and dsh-acct-9-30"),
            ("/home/u/work", "/home/operator/work"),
            ("/home/u/.toolchain/node/bin/node", "/home/u/.toolchain/node/bin/node"),
            ("alpha and alphabet", "alpha-renamed and alphabet"),
        ]
        for before, after in cases:
            expect(f"rewrite {before!r}", rewrite(before, table), after)

        # ---- the module owner comes from origin, not from this file --------
        os.environ[d.MODULE_OWNER_ENV] = FAKE_OWNER
        try:
            expect("allowed owner stays", rewrite(MODULE_OK, table), MODULE_OK)
            expect("other owner is rewritten", rewrite(MODULE_BAD, table), MODULE_OK)
        finally:
            os.environ.pop(d.MODULE_OWNER_ENV, None)

        # ---- exception layers ---------------------------------------------
        skill = ".dsh/skills/release-version/SKILL.md"
        expect("skill file is exempt", rewrite("host-alpha", table, skill), "host-alpha")
        expect("skill file is visible with --strict",
               rules_for("host-alpha", table, skill, strict=True), ["host_alpha"])
        expect("skill file is silent without --strict", rules_for("host-alpha", table, skill), [])

        # ---- generic rules catch what the table does not know yet ----------
        expect("private address", rules_for("host: " + PRIVATE_IP, table), ["private_ipv4"])
        expect("public address", rules_for("host: " + PUBLIC_IP, table), ["public_ipv4"])
        expect("allowlisted private literals stay quiet",
               rules_for("192.168.1.1 and 172.16.3.4", table), [])
        expect("documentation ranges stay quiet",
               rules_for("192.0.2.101 198.51.100.101 203.0.113.9", table), [])
        expect("real mailbox", rules_for("mail: " + REAL_MAIL, table), ["email"])
        expect("git's own address stays quiet", rules_for("git@github.com:acme/repo.git", table), [])
        expect("real fingerprint", rules_for(FINGERPRINT, table), ["ssh_fingerprint"])
        expect("placeholder fingerprint stays quiet", rules_for("SHA256:" + "0" * 43, table), [])
        expect("unknown home directory", rules_for("/home/" + "somebody" + "/work", table), ["home_path"])
        expect("known home directories stay quiet",
               rules_for("/home/operator /home/dshgw /home/vscode", table), [])
        expect("a table-known key prefix is an error",
               [hit.level for hit in d.scan(("key: " + SK_PREFIX).encode(), "x.md", table)],
               ["error"])
        expect("an unknown real-looking key prefix warns only",
               [hit.level for hit in d.scan(("key: " + SK_UNKNOWN).encode(), "x.md", table)],
               ["warn"])

        # ---- apply: idempotent, binary-safe, renames -----------------------
        tree = home / "tree"
        (tree / "docs/design").mkdir(parents=True)
        (tree / "docs/notes.md").write_text("deploy to host-alpha, owner legacy@corp.example\n")
        (tree / "docs/design/old-host-alpha.md").write_text("see host-alpha\n")
        (tree / "assets.bin").write_bytes(b"\x00\x01host-alpha\x00")
        first = run_cli("--apply", "--root", str(tree), "--table", str(table_file))
        expect("apply exit code", first.returncode, 0)
        expect("apply rewrote the text file", (tree / "docs/notes.md").read_text(),
               "deploy to gw-a, owner owner@example.com\n")
        expect("apply renamed the host-named path",
               (tree / "docs/design/old.md").exists(), True)
        expect("apply left the binary alone",
               (tree / "assets.bin").read_bytes(), b"\x00\x01host-alpha\x00")
        second = run_cli("--apply", "--root", str(tree), "--table", str(table_file))
        expect("apply is idempotent",
               second.stdout.strip().endswith("0 files rewritten, 0 renamed"), True)
        expect("check on the scrubbed tree passes",
               run_cli("--check", "--root", str(tree), "--table", str(table_file)).returncode, 0)
        (tree / "docs/notes.md").write_text("deploy to " + PRIVATE_IP + "\n")
        expect("check fails on a fresh leak",
               run_cli("--check", "--root", str(tree), "--table", str(table_file)).returncode, 1)

        # ---- the gate refuses to sound clean without the table -------------
        clean_tree = home / "clean-tree"
        clean_tree.mkdir()
        (clean_tree / "notes.md").write_text("deploy to gw-a\n")
        missing = home / "nope.tsv"
        expect("missing table is fatal with --require-table",
               run_cli("--check", "--root", str(clean_tree), "--table", str(missing),
                       "--require-table").returncode, 2)
        expect("missing table still checks with a note",
               run_cli("--check", "--root", str(clean_tree), "--table", str(missing)).returncode, 0)

        # ---- history: blobs and commit messages ----------------------------
        repo = home / "repo"
        repo.mkdir()
        git(repo, "init", "-q")
        git(repo, "config", "user.email", "t@example.com")
        git(repo, "config", "user.name", "tester")
        git(repo, "remote", "add", "origin", "git@example.com:" + FAKE_OWNER + "/ai-gateway.git")
        (repo / "config.yaml").write_text("url: http://" + PRIVATE_IP + ":8088\n")
        git(repo, "add", "-A")
        git(repo, "commit", "-qm", "add real config for host-alpha")
        (repo / "config.yaml").write_text("url: http://aigw.internal:8088\n")
        git(repo, "add", "-A")
        git(repo, "commit", "-qm", "scrub the address")
        expect("tip is clean", run_cli("--check", cwd=repo, table=table_file).returncode, 0)
        history = run_cli("--check-history", cwd=repo, table=table_file)
        expect("history still has the blob leak", history.returncode, 1)
        expect("history reports the path", "config.yaml" in history.stdout, True)
        with_messages = run_cli("--check-history", "--messages", cwd=repo, table=table_file)
        expect("history sees the commit message too",
               "commit-message" in with_messages.stdout, True)

        git(repo, "checkout", "-q", "--orphan", "clean")
        git(repo, "add", "-A")
        git(repo, "commit", "-qm", "only clean history")
        git(repo, "branch", "-D", "master")
        expect("clean history passes",
               run_cli("--check-history", "--messages", cwd=repo, table=table_file).returncode, 0)

        # ---- filter-message: what the history rewrite pipes through --------
        piped = subprocess.run([sys.executable, str(SCRIPT), "--table", str(table_file),
                                "--filter-message"], input="deploy to host-alpha\n",
                               capture_output=True, text=True)
        expect("filter-message rewrites stdin", piped.stdout, "deploy to gw-a\n")

    if failures:
        for failure in failures:
            print(f"FAIL  {failure}", file=sys.stderr)
        return 1
    print("desensitize: table, exceptions, longest-first, idempotence, history and messages hold")
    return 0


if __name__ == "__main__":
    sys.exit(main())
