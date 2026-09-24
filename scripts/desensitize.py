#!/usr/bin/env python3
"""Desensitize this repository: find real environment identifiers and replace them.

Why this file exists
--------------------
The repository is also the operator's deployment log: real host aliases, LAN and
public addresses, real domains, personal names and mailboxes, a host-key
fingerprint, API-key prefixes and the operator's home directory all appear in
code, fixtures, example configs, scripts and docs. This script is the single
source of truth for "what counts as a real identifier" and for what replaces it.

Three consumers, one table (see docs/design/m89-code-desensitization.md):
  * the working tree scrub      -> `--apply`
  * the release gate            -> `--check`   (scripts/release.sh calls it)
  * the history rewrite         -> `--apply --root .` (git filter-branch tree-filter)
and a fourth for auditing:      -> `--check-history` (every blob in every ref).

Deliberate exceptions
---------------------
  * `.dsh/skills/**` keeps real values: that is the operator's runbook and it has
    to name the real deployment host to be useful (`--strict` shows the residual).
  * `/home/winger/.local/**` and `/home/winger/.cache/**` are the machine-local
    toolchain paths; they are referenced by `Makefile`, `scripts/**` *and* the
    docs that explain them, so the whole family stays as it is.

Exit codes: 0 clean, 1 findings, 2 usage or environment error.
"""

from __future__ import annotations

import argparse
import ipaddress
import json
import re
import subprocess
import sys
from pathlib import Path

# --------------------------------------------------------------------------
# Replacement table. Ordered at runtime by pattern length (longest first) so
# that a longer literal always wins over a shorter one it contains:
# `gpt.lagenio.xyz` before `lagenio.xyz`, `http://192.168.190.86:8088` before
# the bare IP, `github.com/winger/ai-gateway` before the bare `winger`.
# Bare word rules carry boundaries on purpose: `funnywwh` must not touch
# `github.com/funnywwh/ai-gateway` (the module path) nor
# `git@github.com:funnywwh/ai-gateway.git` (origin) -- both are the repo's
# deliberate public identity.
# --------------------------------------------------------------------------
TOKENS: list[tuple[str, str, str]] = [
    # module path: matches origin (git@github.com:funnywwh/ai-gateway.git)
    ("module_path", r"github\.com/winger/ai-gateway", "github.com/funnywwh/ai-gateway"),
    # the gateway URL default (longer than the bare address, so it wins)
    ("url_aigw_default", r"http://192\.168\.190\.86:8088", "http://aigw.internal:8088"),
    # domains
    ("dom_gpt001_iotalking", r"\bgpt001\.iotalking\.top\b", "gw-a.example.com"),
    ("dom_gpt_iotalking", r"\bgpt\.iotalking\.top\b", "gw-a.example.net"),
    ("dom_mnl_iotalking", r"\bmnl\.iotalking\.top\b", "gw-a.example.org"),
    ("dom_iotalking", r"\biotalking\.top\b", "example.org"),
    ("dom_gpt_lagenio", r"\bgpt\.lagenio\.xyz\b", "gw-b.example.com"),
    ("dom_gpt_tirisen", r"\bgpt\.tirisen\.hk\b", "gw-b.example.net"),
    ("dom_chat_tirisen", r"\bchat\.tirisen\.hk\b", "chat.example.com"),
    ("dom_tirisen", r"\btirisen\.hk\b", "example.net"),
    ("dom_lagenio_xyz", r"\blagenio\.xyz\b", "example.com"),
    ("dom_lagenio_com", r"\blagenio\.com\b", "example.com"),
    # host aliases
    ("host_gpt001", r"\bgpt001\b", "gw-a"),
    ("host_gptjp", r"\bgptjp\b", "gw-b"),
    ("host_rag_server", r"\brag-server\b", "gw-c"),
    ("host_aipc", r"\baipc\b", "gw-d"),
    # LAN addresses (real -> RFC 5737 documentation addresses, .101 upwards)
    ("ip_lan_86", r"\b192\.168\.190\.86\b", "192.0.2.101"),
    ("ip_lan_87", r"\b192\.168\.190\.87\b", "192.0.2.102"),
    ("ip_lan_88", r"\b192\.168\.190\.88\b", "192.0.2.103"),
    ("ip_lan_89", r"\b192\.168\.190\.89\b", "192.0.2.104"),
    ("ip_lan_90", r"\b192\.168\.190\.90\b", "192.0.2.105"),
    ("ip_lan_123", r"\b192\.168\.190\.123\b", "192.0.2.106"),
    ("ip_lan_222", r"\b192\.168\.190\.222\b", "192.0.2.107"),
    ("ip_lan_140_252", r"\b192\.168\.140\.252\b", "192.0.2.108"),
    # public addresses (real -> TEST-NET-2, .101 upwards)
    ("ip_wan_47_91", r"\b47\.91\.16\.118\b", "198.51.100.101"),
    ("ip_wan_47_80", r"\b47\.80\.68\.113\b", "198.51.100.102"),
    ("ip_wan_47_106", r"\b47\.106\.94\.177\b", "198.51.100.103"),
    ("ip_wan_8_211", r"\b8\.211\.157\.165\b", "198.51.100.104"),
    ("ip_wan_113_80", r"\b113\.80\.95\.12\b", "198.51.100.105"),
    ("ip_wan_103_59", r"\b103\.59\.145\.127\b", "198.51.100.106"),
    # people (name, pinyin slug and western alias travel together: the tenant
    # name is derived from the account name, and tests pin both halves)
    ("name_lizhichao", r"李智超", "李雷"),
    ("slug_lizhichao", r"\blizhichao\b", "lilei"),
    ("alias_colin", r"\bcolin\b", "alex"),
    ("name_chenjingfeng", r"陈景峰", "王强"),
    ("slug_chenjingfeng", r"\bchenjingfeng\b", "wangqiang"),
    ("name_yangmiao", r"杨妙", "刘洋"),
    ("slug_yangmiao", r"\byangmiao\b", "liuyang"),
    ("name_zhengxiaoting", r"郑晓婷", "孙倩"),
    ("slug_ranqiliang", r"\branqiliang\b", "acct-b"),
    ("slug_lianchangliang", r"\blianchangliang\b", "acct-c"),
    # mailboxes and the account/provider names built from them
    ("mail_lzhichao", r"lzhichao@lagenio\.com", "lilei@example.com"),
    ("mail_funnywwh", r"funnywwh@gmail\.com", "owner@example.com"),
    ("acct_codex_funnywwh", r"\bcodex-funnywwh\b", "codex-owner"),
    ("name_funnywwh", r"(?<![\w./:-])funnywwh(?![\w./:-])", "owner"),
    # business identifiers (upstream accounts, providers, key and tag names)
    ("corp_wisskys", r"\bwisskys\b", "corp-a"),
    ("user_liuhui", r"\bliuhui\b", "wangwu"),
    ("user_zhuyecheng", r"\bzhuyecheng\b", "zhangsan"),
    ("provider_libbygpt", r"\bLibbyGPT\b", "ProviderA"),
    ("provider_antigravity", r"\bantigravity\b", "ProviderB"),
    ("tag_lanjingling", r"蓝精灵", "测试标签"),
    ("tag_zhitiancheng", r"智天成", "客户组一"),
    ("tag_dianshang", r"电商", "客户组二"),
    ("tag_shouna", r"收纳", "客户组三"),
    ("keyname_e26q", r"(?<![A-Za-z0-9])E26Q", "K7QX"),
    ("keyname_e26q_lower", r"(?<![A-Za-z0-9])e26q", "k7qx"),
    # credential traces
    ("key_prefix_f69aeca55", r"sk-f69aeca55", "sk-000000000"),
    ("key_prefix_62e1a0b4c", r"sk-62e1a0b4c", "sk-000000000"),
    ("key_prefix_gw_981065ae", r"sk-gw-981065ae", "sk-gw-00000000"),
    ("host_key_fingerprint", r"SHA256:TG5FPCWIAAQEtNNBi7nTG7iNFrQUGgyQQDxRnuuIOgE", "SHA256:" + "0" * 43),
    # operator account and paths (the toolchain families in EXEMPT_PATTERNS stay)
    ("home_winger", r"/home/winger(?!/(?:\.local|\.cache)(?![\w-]))", "/home/operator"),
    ("user_winger", r"(?<![\w./-])winger(?![\w./-])", "operator"),
]

# File renames that follow from the table (a path may not name a real host).
RENAMES: dict[str, str] = {
    "docs/design/m36-deploy-gpt001-prefix.md": "docs/design/m36-deploy-prefix.md",
}

# Paths whose contents are deliberately left alone (the operator runbook).
EXEMPT_PATHS: tuple[str, ...] = (r"^\.dsh/skills/",)

# --------------------------------------------------------------------------
# Generic rules: these catch identifiers that are not in the table yet, which
# is the difference between "we cleaned the repo once" and "the repo stays
# clean". Allow-lists are short and every entry has a reason.
# --------------------------------------------------------------------------
IPV4 = re.compile(rb"(?<![\d.])(?:\d{1,3}\.){3}\d{1,3}(?![\d.])")
PRIVATE_RANGES = (ipaddress.ip_network("192.168.0.0/16"), ipaddress.ip_network("172.16.0.0/12"))
ALLOWED_IPS = {
    "192.168.1.1",  # the SSRF guard's tests use a generic private address
    "172.16.3.4",   # same
    "93.184.216.34",  # example.com's real address, used as the "public" case
    "8.8.8.8", "1.1.1.1", "9.9.9.9", "7.7.7.7", "1.2.3.4",  # public literals in tests
}
ALLOWED_IP_RANGES = [ipaddress.ip_network(n) for n in (
    "0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
    "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
    "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
)]

EMAIL = re.compile(rb"[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}")
ALLOWED_EMAIL_DOMAINS = ("example.com", "example.org", "example.net", "example.co.uk",
                         ".example", ".test", ".local", ".internal", ".service", "github.com")

FINGERPRINT = re.compile(rb"SHA256:([A-Za-z0-9+/=]{20,})")
ALLOWED_FINGERPRINTS = {"0" * 43}

HOME_PATH = re.compile(rb"/(?:home|Users)/([A-Za-z0-9._-]+)")
ALLOWED_HOME_USERS = {"account", "u", "nginxWebUI", "remote", "other", "dshgw", "ops",
                      "vscode", "operator"}
# The machine-local toolchain families (§2 D2): the `Makefile`, the ops scripts and
# the docs that explain them all point at the same paths, so the family is exempt
# as a whole rather than file by file.
TOOLCHAIN_PATH = re.compile(rb"/home/winger/\.(?:local|cache)(?![\w-])")

SK_PREFIX = re.compile(rb"sk-(?:gw-)?([0-9a-f]{8,})")

SKIP_DIRS = {".git", ".cache", "bin", "data", "node_modules", "__pycache__", "plugins"}
BINARY_SNIFF = 8192


class Hit:
    """One finding: which rule fired, where, and what it matched."""

    __slots__ = ("rule", "path", "line", "text", "level")

    def __init__(self, rule: str, path: str, line: int, text: str, level: str = "error"):
        self.rule, self.path, self.line, self.text, self.level = rule, path, line, text, level

    def as_dict(self) -> dict:
        return {"rule": self.rule, "path": self.path, "line": self.line,
                "text": self.text, "level": self.level}

    def render(self) -> str:
        return f"{self.path}:{self.line}: [{self.rule}] {self.text}"


def literal_length(pattern: str) -> int:
    r"""How many literal characters a pattern insists on.

    Ordering by the raw pattern length would be wrong: it counts lookarounds and
    escapes, so `(?<![\w./:-])funnywwh(?![\w./:-])` (33) would run before
    `funnywwh@gmail\\.com` (19) and turn the mailbox into `owner@gmail.com`.
    Stripping the regex machinery first gives the length of what the pattern
    actually has to match, which is what "longest first" means here.
    """
    stripped = re.sub(r"\(\?<?[!=][^)]*\)", "", pattern)  # lookarounds
    stripped = re.sub(r"\\[A-Za-z]", "", stripped)        # \b, \w, ...
    stripped = re.sub(r"[\[\](){}|?+*.^$\\]", "", stripped)
    return len(stripped)


def ordered_tokens() -> list[tuple[str, re.Pattern, bytes, str]]:
    """Compile TOKENS, longest literal first (see the note above the table)."""
    rules = [(name, re.compile(pattern.encode()), repl.encode(), pattern)
             for name, pattern, repl in TOKENS]
    rules.sort(key=lambda item: literal_length(item[3]), reverse=True)
    return rules


TOKEN_RULES = ordered_tokens()


def is_exempt(path: str) -> bool:
    return any(re.match(pattern, path) for pattern in EXEMPT_PATHS)


def line_of(data: bytes, offset: int) -> int:
    return data.count(b"\n", 0, offset) + 1


def snippet(raw: bytes, limit: int = 60) -> str:
    text = raw.decode("utf-8", "replace").replace("\n", "\\n")
    return text if len(text) <= limit else text[:limit] + "…"


def token_hits(data: bytes, path: str) -> list[tuple[Hit, int]]:
    hits = []
    for name, rx, _repl, _pattern in TOKEN_RULES:
        for match in rx.finditer(data):
            hits.append((Hit(name, path, line_of(data, match.start()), snippet(match.group(0))),
                         match.start()))
    return hits


def ip_hits(data: bytes, path: str) -> list[tuple[Hit, int]]:
    hits = []
    for match in IPV4.finditer(data):
        raw = match.group(0).decode()
        try:
            address = ipaddress.ip_address(raw)
        except ValueError:
            continue  # not an address after all (e.g. a version number)
        if raw in ALLOWED_IPS:
            continue
        if any(address in network for network in PRIVATE_RANGES):
            hits.append((Hit("private_ipv4", path, line_of(data, match.start()), raw), match.start()))
        elif not any(address in network for network in ALLOWED_IP_RANGES):
            hits.append((Hit("public_ipv4", path, line_of(data, match.start()), raw), match.start()))
    return hits


def email_hits(data: bytes, path: str) -> list[tuple[Hit, int]]:
    hits = []
    for match in EMAIL.finditer(data):
        raw = match.group(0).decode()
        domain = raw.rsplit("@", 1)[1].lower()
        if any(domain == allowed or domain.endswith(allowed) for allowed in ALLOWED_EMAIL_DOMAINS):
            continue
        hits.append((Hit("email", path, line_of(data, match.start()), raw), match.start()))
    return hits


def fingerprint_hits(data: bytes, path: str) -> list[tuple[Hit, int]]:
    hits = []
    for match in FINGERPRINT.finditer(data):
        if match.group(1).decode() in ALLOWED_FINGERPRINTS:
            continue
        hits.append((Hit("ssh_fingerprint", path, line_of(data, match.start()),
                         "SHA256:" + match.group(1).decode()[:12] + "…"), match.start()))
    return hits


def home_hits(data: bytes, path: str) -> list[tuple[Hit, int]]:
    hits = []
    for match in HOME_PATH.finditer(data):
        if TOOLCHAIN_PATH.match(data, match.start()):
            continue
        user = match.group(1).decode()
        if user in ALLOWED_HOME_USERS:
            continue
        hits.append((Hit("home_path", path, line_of(data, match.start()), "/home/" + user),
                     match.start()))
    return hits


def warn_hits(data: bytes, path: str) -> list[Hit]:
    """Warnings: real-looking key prefixes are worth a look, but they are not
    proof of anything (the repo is full of deliberately fake `sk-` fixtures)."""
    hits = []
    for match in SK_PREFIX.finditer(data):
        payload = match.group(1).decode()
        if len(set(payload)) == 1:
            continue  # sk-aaaaaaaaa and friends
        hits.append(Hit("sk_prefix_review", path, line_of(data, match.start()),
                        "sk-" + payload[:8] + "…", level="warn"))
    return hits





def scan(data: bytes, path: str, strict: bool = False) -> list[Hit]:
    """Every finding for one file's bytes, exceptions included.

    A location the explicit table already names is not reported a second time by
    the generic rules (both would fire on the same real address, e.g. an entry in
    TOKENS and the private-IPv4 sweep).
    """
    if is_exempt(path) and not strict:
        return []
    hits: list[Hit] = []
    covered: set[int] = set()
    for hit, offset in token_hits(data, path):
        hits.append(hit)
        covered.add(offset)
    for check in (ip_hits, email_hits, fingerprint_hits, home_hits):
        for hit, offset in check(data, path):
            if offset not in covered:
                hits.append(hit)
    hits.extend(warn_hits(data, path))
    return hits


def rewrite(data: bytes, path: str, strict: bool = False) -> tuple[bytes, int]:
    """Apply TOKENS to one file's bytes; returns (bytes, number of rules fired)."""
    if is_exempt(path) and not strict:
        return data, 0
    fired = 0
    for _name, rx, replacement, _pattern in TOKEN_RULES:
        if rx.search(data):
            fired += 1
            data = rx.sub(replacement, data)
    return data, fired


# --------------------------------------------------------------------------
# File discovery
# --------------------------------------------------------------------------
def repo_root() -> Path | None:
    try:
        out = subprocess.run(["git", "rev-parse", "--show-toplevel"], capture_output=True,
                             text=True, check=True)
    except (OSError, subprocess.CalledProcessError):
        return None
    return Path(out.stdout.strip())


def tracked_files() -> list[str]:
    out = subprocess.run(["git", "ls-files", "-z"], capture_output=True, check=True)
    return [name.decode("utf-8", "surrogateescape") for name in out.stdout.split(b"\x00") if name]


def walk_files(root: Path) -> list[str]:
    found = []
    for dirpath, dirnames, filenames in root.walk():
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS)
        for name in sorted(filenames):
            found.append(str((dirpath / name).relative_to(root)))
    return found


def read_bytes(path: Path, rel: str) -> bytes | None:
    try:
        data = path.read_bytes()
    except OSError as exc:
        print(f"desensitize: cannot read {rel}: {exc}", file=sys.stderr)
        return None
    if b"\x00" in data[:BINARY_SNIFF]:
        return None  # binary: rules are line-oriented text rules
    return data


# --------------------------------------------------------------------------
# Modes
# --------------------------------------------------------------------------
def run_check(root: Path, files: list[str], strict: bool, show_all: bool,
              limit: int = 40) -> int:
    hits: list[Hit] = []
    scanned = 0
    for rel in files:
        data = read_bytes(root / rel, rel)
        if data is None:
            continue
        scanned += 1
        hits.extend(scan(data, rel, strict))
    errors = [hit for hit in hits if hit.level == "error"]
    warnings = [hit for hit in hits if hit.level != "error"]
    for hit in (hits if show_all else errors[:limit] + warnings):
        print(hit.render())
    if not show_all and len(errors) > limit:
        print(f"… and {len(errors) - limit} more (drop --quiet to see them all)")
    print(f"\ndesensitize: scanned {scanned} files, {len(errors)} findings, "
          f"{len(warnings)} warnings" + ("" if strict else " (exceptions excluded)"))
    return 1 if errors else 0


def run_inventory(root: Path, files: list[str], strict: bool, as_json: bool) -> int:
    per_rule: dict[str, dict] = {}
    for rel in files:
        data = read_bytes(root / rel, rel)
        if data is None:
            continue
        for hit in scan(data, rel, strict):
            entry = per_rule.setdefault(hit.rule, {"count": 0, "files": set(), "sample": None})
            entry["count"] += 1
            entry["files"].add(hit.path)
            if entry["sample"] is None:
                entry["sample"] = hit.render()
    if as_json:
        print(json.dumps({rule: {"count": entry["count"], "files": sorted(entry["files"]),
                                 "sample": entry["sample"]}
                          for rule, entry in sorted(per_rule.items())},
                         ensure_ascii=False, indent=2))
    else:
        for rule, entry in sorted(per_rule.items(), key=lambda kv: -kv[1]["count"]):
            print(f"{entry['count']:5d}  {rule:24s} {len(entry['files']):3d} files  {entry['sample']}")
        print(f"\ndesensitize: {len(per_rule)} rules fired")
    return 0


def run_apply(root: Path, files: list[str], strict: bool) -> int:
    changed, renames = 0, 0
    total_hits = 0
    for rel in files:
        target = root / rel
        data = read_bytes(target, rel)
        if data is None:
            continue
        new_data, fired = rewrite(data, rel, strict)
        if fired:
            if new_data != data:
                target.write_bytes(new_data)
                changed += 1
            total_hits += 1
    for old, new in RENAMES.items():
        source, dest = root / old, root / new
        if source.exists():
            if dest.exists():
                print(f"desensitize: rename target already exists: {new}", file=sys.stderr)
                return 2
            dest.parent.mkdir(parents=True, exist_ok=True)
            source.rename(dest)
            renames += 1
    print(f"desensitize: {changed} files rewritten, {renames} renamed")
    return 0


def run_check_history(strict: bool) -> int:
    """Scan every blob that any ref can reach, using the path it lived at."""
    listing = subprocess.run(["git", "rev-list", "--objects", "--all"],
                             capture_output=True, check=True).stdout.split(b"\n")
    pairs: list[tuple[str, str]] = []
    for line in listing:
        if not line:
            continue
        parts = line.split(b" ", 1)
        if len(parts) == 2:
            pairs.append((parts[0].decode(), parts[1].decode("utf-8", "surrogateescape")))
    unique = sorted({sha for sha, _ in pairs})
    if not unique:
        print("desensitize: no history (not a git repository?)", file=sys.stderr)
        return 2
    blobs = _read_blobs(unique)
    cache: dict[str, list[Hit]] = {}
    hits: list[Hit] = []
    for sha, path in pairs:
        if sha not in blobs:
            continue  # tree or commit
        data = blobs[sha]
        if b"\x00" in data[:BINARY_SNIFF]:
            continue
        if sha not in cache:
            cache[sha] = scan(data, path, strict)
        for hit in cache[sha]:
            hits.append(Hit(hit.rule, path + f"@{sha[:9]}", hit.line, hit.text, hit.level))
    errors = [hit for hit in hits if hit.level == "error"]
    for hit in errors[:400]:
        print(hit.render())
    if len(errors) > 400:
        print(f"… and {len(errors) - 400} more")
    print(f"\ndesensitize: scanned {len(blobs)} blobs across {len({sha for sha, _ in pairs})} "
          f"paths, {len(errors)} findings in history" + ("" if strict else " (exceptions excluded)"))
    return 1 if errors else 0


def _read_blobs(shas: list[str]) -> dict[str, bytes]:
    """Batch-read objects; non-blob objects are simply absent from the result."""
    request = "".join(sha + "\n" for sha in shas).encode()
    out = subprocess.run(["git", "cat-file", "--batch"], input=request,
                         capture_output=True, check=True).stdout
    blobs: dict[str, bytes] = {}
    pos = 0
    while pos < len(out):
        end = out.find(b"\n", pos)
        if end < 0:
            break
        header = out[pos:end].split()
        pos = end + 1
        if len(header) < 3:
            continue  # "<sha> missing"
        sha, kind, size = header[0].decode(), header[1].decode(), int(header[2])
        payload = out[pos:pos + size]
        pos += size + 1  # trailing newline
        if kind == "blob":
            blobs[sha] = payload
    return blobs


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="desensitize.py",
        description="扫描并替换仓库里的真实环境标识（脱敏口径的单一真源，见 "
                    "docs/design/m89-code-desensitization.md）")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--check", action="store_true",
                      help="扫 tracked 文件（默认）；有命中退出 1")
    mode.add_argument("--apply", action="store_true", help="就地替换并按 RENAMES 改名")
    mode.add_argument("--check-history", action="store_true", help="扫全部 ref 的全部 blob")
    mode.add_argument("--inventory", action="store_true", help="按规则汇总当前命中，用来补规则表")
    parser.add_argument("--root", help="扫描目录（默认：仓库根；--apply 时也用它）")
    parser.add_argument("--strict", action="store_true",
                        help="把例外路径也当普通文件（审计刻意保留的残留）")
    parser.add_argument("--json", action="store_true", help="机器可读输出（--inventory）")
    parser.add_argument("--quiet", action="store_true", help="只打印汇总")
    args = parser.parse_args(argv)

    if args.check_history:
        return run_check_history(args.strict)

    root = Path(args.root).resolve() if args.root else repo_root()
    if root is None:
        print("desensitize: --root is required outside a git repository", file=sys.stderr)
        return 2
    if not root.is_dir():
        print(f"desensitize: not a directory: {root}", file=sys.stderr)
        return 2
    files = walk_files(root) if args.root else tracked_files()

    if args.apply:
        return run_apply(root, files, args.strict)
    if args.inventory:
        return run_inventory(root, files, args.strict, args.json)
    return run_check(root, files, args.strict, show_all=not args.quiet)


if __name__ == "__main__":
    sys.exit(main())
