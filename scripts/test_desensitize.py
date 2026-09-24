#!/usr/bin/env python3
"""Check the desensitizer's rules against fixtures, and against a throwaway git repo.

The tool is a gate: `scripts/release.sh` refuses to release when `--check` finds anything,
and the history rewrite runs the very same `--apply` over 479 commits. So what has to be
pinned is not "it replaced a host name once" but the properties the gate leans on:

  * the deliberate public identity survives the bare-word rules
    (`github.com/winger/ai-gateway` -> `github.com/funnywwh/ai-gateway`, and the origin URL
    `git@github.com:funnywwh/ai-gateway.git` is left exactly as it is);
  * the two exception layers behave (`.dsh/skills/**` untouched, toolchain paths untouched);
  * a longer literal wins over a shorter one it contains;
  * `--apply` is idempotent, skips binaries and renames the one path that names a host;
  * the generic rules fire on *new* leakage (an address, mailbox, fingerprint or home
    directory that is not in the table yet) and stay quiet on the values the repo is
    allowed to use;
  * `--check-history` looks at everything a ref can reach, not just the tip.

It runs on fixtures under a temporary directory: nothing here reads or writes the real tree.
"""

from __future__ import annotations

import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "desensitize.py"

sys.path.insert(0, str(ROOT / "scripts"))
import desensitize as d  # noqa: E402  (the module under test)


def rewrite(text: str, path: str = "docs/example.md") -> str:
    data, _fired = d.rewrite(text.encode(), path)
    return data.decode()


def rules_for(text: str, path: str = "docs/example.md", strict: bool = False) -> list[str]:
    return [hit.rule for hit in d.scan(text.encode(), path, strict)]


def run_cli(*args: str, cwd: Path | None = None) -> subprocess.CompletedProcess:
    return subprocess.run([sys.executable, str(SCRIPT), *args], cwd=cwd,
                          capture_output=True, text=True)


def main() -> int:
    failures: list[str] = []

    def expect(label: str, got: object, want: object) -> None:
        if got != want:
            failures.append(f"{label}: got {got!r}, want {want!r}")

    # ---- the table: real value -> placeholder -----------------------------
    cases = [
        ("gpt001:/opt/aigw/aigw", "gw-a:/opt/aigw/aigw"),
        ("Host gptjp\n  HostName gpt.tirisen.hk", "Host gw-b\n  HostName gw-b.example.net"),
        ("ssh rag-server", "ssh gw-c"),
        ("aipc 上的挂载", "gw-d 上的挂载"),
        ("chat.tirisen.hk", "chat.example.com"),
        ("https://mnl.iotalking.top/aigw/version", "https://gw-a.example.org/aigw/version"),
        ("user: winger", "user: operator"),
        ("/home/winger/work/ai_gateway", "/home/operator/work/ai_gateway"),
        ('Name: "lzhichao@lagenio.com",', 'Name: "lilei@example.com",'),
        ('"李智超(colin)" -> dsh-lizhichao-colin-8', '"李雷(alex)" -> dsh-lilei-alex-8'),
        ("codex-funnywwh", "codex-owner"),
        ("funnywwh@gmail.com", "owner@example.com"),
        ("蓝精灵1 / 智天成 / 电商 / 收纳", "测试标签1 / 客户组一 / 客户组二 / 客户组三"),
        ("E26Q_GROUP = 20; dsh-e26q-30", "K7QX_GROUP = 20; dsh-k7qx-30"),
        ("sk-f69aeca55", "sk-000000000"),
        ("sk-gw-981065ae", "sk-gw-00000000"),
        ("192.168.190.123", "192.0.2.106"),
        ("47.91.16.118", "198.51.100.101"),
    ]
    for before, after in cases:
        expect(f"rewrite {before!r}", rewrite(before), after)

    # ---- longest literal first, and the deliberate public identity --------
    expect("longest-first: url beats bare ip", rewrite("http://192.168.190.86:8088/admin/ui/"),
           "http://aigw.internal:8088/admin/ui/")
    expect("longest-first: fqdn beats bare host + domain", rewrite("gpt.lagenio.xyz"),
           "gw-b.example.com")
    expect("module path is rewritten to the origin name",
           rewrite("module github.com/winger/ai-gateway"),
           "module github.com/funnywwh/ai-gateway")
    expect("bare winger rule must not touch the module path",
           rewrite("import \"github.com/winger/ai-gateway/internal/store\""),
           "import \"github.com/funnywwh/ai-gateway/internal/store\"")
    expect("origin url is left alone",
           rewrite("git@github.com:funnywwh/ai-gateway.git"),
           "git@github.com:funnywwh/ai-gateway.git")
    expect("https origin url is left alone",
           rewrite("https://github.com/funnywwh/ai-gateway"), "https://github.com/funnywwh/ai-gateway")

    # ---- exception layers -------------------------------------------------
    skill = ".dsh/skills/release-version/SKILL.md"
    expect("skill file is exempt from rewriting",
           rewrite("scp bin/aigw gpt001:/opt/aigw/aigw  # /home/winger/ai-gateway", skill),
           "scp bin/aigw gpt001:/opt/aigw/aigw  # /home/winger/ai-gateway")
    expect("skill file is still visible with --strict", rules_for("gpt001", skill, strict=True),
           ["host_gpt001"])
    expect("skill file is silent without --strict", rules_for("gpt001", skill), [])
    expect("toolchain path family is exempt everywhere",
           rewrite("DSHGW_NODE ?= /home/winger/.local/node/bin/node"),
           "DSHGW_NODE ?= /home/winger/.local/node/bin/node")
    expect("toolchain path family is not reported",
           rules_for("chrome: /home/winger/.cache/ms-playwright/chrome"), [])
    expect("a non-toolchain home path is reported", "home_winger" in
           rules_for("/home/operator".replace("operator", "winger")), True)

    # ---- generic rules catch what the table does not know yet -------------
    expect("private address", rules_for("host: 192.168.44.9"), ["private_ipv4"])
    expect("public address", rules_for("host: 47.44.9.1"), ["public_ipv4"])
    expect("allowlisted private literals stay quiet",
           rules_for("192.168.1.1 and 172.16.3.4"), [])
    expect("documentation ranges stay quiet",
           rules_for("192.0.2.101 198.51.100.101 203.0.113.9"), [])
    expect("real mailbox", rules_for("mail: someone@real-corp.com"),
           ["email"])
    expect("git's own address stays quiet", rules_for("git@github.com:acme/repo.git"), [])
    expect("real fingerprint", "ssh_fingerprint" in
           rules_for("SHA256:" + "Ab1+/" * 9 + "Ab"), True)
    expect("placeholder fingerprint stays quiet", rules_for("SHA256:" + "0" * 43), [])
    expect("unknown home directory", rules_for("/home/somebody/work"), ["home_path"])
    expect("known home directories stay quiet",
           rules_for("/home/operator /home/dshgw /home/vscode"), [])

    # ---- apply is idempotent, skips binaries, renames the host-named path --
    with tempfile.TemporaryDirectory() as tmp:
        tree = Path(tmp) / "tree"
        (tree / "docs/design").mkdir(parents=True)
        (tree / "docs/notes.md").write_text("deploy to gpt001, user winger\n")
        (tree / "docs/design/m36-deploy-gpt001-prefix.md").write_text("see gpt001\n")
        (tree / "assets.bin").write_bytes(b"\x00\x01gpt001\x00")
        first = run_cli("--apply", "--root", str(tree))
        expect("apply exit code", first.returncode, 0)
        expect("apply rewrote the text file",
               (tree / "docs/notes.md").read_text(), "deploy to gw-a, user operator\n")
        expect("apply renamed the host-named path",
               (tree / "docs/design/m36-deploy-prefix.md").exists(), True)
        expect("apply left the binary alone",
               (tree / "assets.bin").read_bytes(), b"\x00\x01gpt001\x00")
        second = run_cli("--apply", "--root", str(tree), "--quiet")
        expect("apply is idempotent", second.stdout.strip().endswith("0 files rewritten, 0 renamed"),
               True)
        expect("check on the scrubbed tree passes", run_cli("--check", "--root", str(tree)).returncode, 0)
        (tree / "docs/notes.md").write_text("deploy to gptjp\n")
        expect("check fails on a fresh leak", run_cli("--check", "--root", str(tree)).returncode, 1)

    # ---- history mode sees what the tip no longer has ---------------------
    with tempfile.TemporaryDirectory() as tmp:
        repo = Path(tmp) / "repo"
        repo.mkdir()
        subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
        subprocess.run(["git", "config", "user.email", "t@example.com"], cwd=repo, check=True)
        subprocess.run(["git", "config", "user.name", "tester"], cwd=repo, check=True)
        (repo / "config.yaml").write_text("url: http://192.168.190.86:8088\n")
        subprocess.run(["git", "add", "-A"], cwd=repo, check=True)
        subprocess.run(["git", "commit", "-qm", "add real config"], cwd=repo, check=True)
        (repo / "config.yaml").write_text("url: http://aigw.internal:8088\n")
        subprocess.run(["git", "add", "-A"], cwd=repo, check=True)
        subprocess.run(["git", "commit", "-qm", "scrub"], cwd=repo, check=True)
        expect("tip is clean", run_cli("--check", cwd=repo).returncode, 0)
        history = run_cli("--check-history", cwd=repo)
        expect("history still has the leak", history.returncode, 1)
        expect("history reports the path", "config.yaml" in history.stdout, True)

        clean = Path(tmp) / "clean"
        shutil.copytree(repo, clean)
        subprocess.run(["git", "checkout", "-q", "--orphan", "clean"], cwd=clean, check=True)
        subprocess.run(["git", "add", "-A"], cwd=clean, check=True)
        subprocess.run(["git", "commit", "-qm", "only clean history"], cwd=clean, check=True)
        subprocess.run(["git", "branch", "-D", "master"], cwd=clean, check=True,
                       capture_output=True)
        expect("clean history passes", run_cli("--check-history", cwd=clean).returncode, 0)

    if failures:
        for failure in failures:
            print(f"FAIL  {failure}", file=sys.stderr)
        return 1
    print("desensitize: table, exceptions, longest-first, idempotence and history mode all hold")
    return 0


if __name__ == "__main__":
    sys.exit(main())
