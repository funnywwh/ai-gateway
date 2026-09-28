#!/usr/bin/env python3
"""Desensitize this repository: find real environment identifiers and replace them.

Why this file exists
--------------------
The repository is also the operator's deployment log: real host aliases, LAN and public
addresses, real domains, personal names and mailboxes, a host-key fingerprint, API-key
prefixes and the operator's home directory all appear in code, fixtures, example configs,
scripts and docs. This script is the gate that keeps them out and the cleaner that takes
out the ones already there.

Two layers of rules, and the split is deliberate
------------------------------------------------
1. **Committed rules** (this file): generic classes that need no secret to state -- the Go
   module path's owner, private-range addresses, addresses outside the documentation
   ranges, mailboxes outside the allowed domains, real-shaped SSH host-key fingerprints,
   `/home/<user>` paths that are not placeholders, and real-looking `sk-` prefixes. A
   fresh clone therefore still fails on the things that actually hurt.
2. **The local table** (`.cache/desensitize/tokens.tsv`, gitignored, never in history):
   the exact real values and what replaces them, plus the paths that name one of them and
   the occurrence-level exemptions. It cannot be committed -- a table that lists the real
   values is itself the leak it is meant to prevent. `--require-table` (what the release
   gate passes) refuses to report a clean tree without it.

Consumers, one table (see docs/design/m89-code-desensitization.md):
  * working-tree scrub    -> `--apply`
  * release gate          -> `--check --require-table`   (scripts/release.sh calls it)
  * history rewrite       -> `--apply --root .` (tree-filter) + `--filter-message` (msg-filter)
  * post-rewrite audit    -> `--check-history --messages`

Deliberate exceptions
---------------------
  * `.dsh/skills/**` keeps real values: that is the operator's runbook and it has to name
    the real deployment host to be useful. `--strict` shows the residual.
  * The machine-local toolchain path family (the operator's `~/.local` and `~/.cache`,
    named by the table's `[exempt]` regexes) stays as it is: `Makefile`, the ops scripts and
    the docs explaining them all point at those paths, so a file-by-file exemption would
    put the docs out of step with the scripts.

Exit codes: 0 clean, 1 findings, 2 usage or environment error.
"""

from __future__ import annotations

import argparse
import ipaddress
import json
import os
import re
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterator

# The public identity this repository keeps: the module path must match *origin*, whose
# owner is already public. Every other owner is a leak (the old module path was exactly
# that). The owner is read from the remote rather than written here, so this file does not
# have to name the handle it protects.
MODULE_PATH = re.compile(rb"github\.com/([A-Za-z0-9_.-]+)/ai-gateway")
MODULE_TEMPLATE = "github.com/{owner}/ai-gateway"


_ORIGIN_OWNER: list[str | None] | None = None


def module_owner() -> str | None:
    """The owner of the origin remote: the one module path that is allowed to stay."""
    override = os.environ.get(MODULE_OWNER_ENV)
    if override:
        return override
    global _ORIGIN_OWNER
    if _ORIGIN_OWNER is not None:
        return _ORIGIN_OWNER[0]
    try:
        url = subprocess.run(["git", "remote", "get-url", "origin"], capture_output=True,
                             text=True, check=True).stdout.strip()
    except (OSError, subprocess.CalledProcessError):
        return None
    match = re.search(r"[:/]([^/:]+)/[^/]+?(?:\.git)?$", url)
    _ORIGIN_OWNER = [match.group(1) if match else None]
    return _ORIGIN_OWNER[0]

# Where the local (gitignored) table lives, and how to override it.
DEFAULT_TABLE = ".cache/desensitize/tokens.tsv"
TABLE_ENV = "GW_DESENSITIZE_TABLE"
# Tests and one-off runs pin the expected owner here; otherwise it comes from origin.
MODULE_OWNER_ENV = "GW_DESENSITIZE_MODULE_OWNER"

# Paths whose contents are deliberately left alone (the operator runbook).
EXEMPT_PATHS: tuple[str, ...] = (r"^\.dsh/skills/",)

# --------------------------------------------------------------------------
# Generic rules. Allow-lists are short and every entry has a reason.
# --------------------------------------------------------------------------
IPV4 = re.compile(rb"(?<![\d.])(?:\d{1,3}\.){3}\d{1,3}(?![\d.])")
PRIVATE_RANGES = (ipaddress.ip_network("192.168.0.0/16"), ipaddress.ip_network("172.16.0.0/12"))
ALLOWED_IPS = {
    "192.168.1.1",  # the SSRF guard's tests use a generic private address
    "172.16.3.4",   # same
    "93.184.216.34",  # example.com's real address, used as the "public" case
    "8.8.8.8", "1.1.1.1", "9.9.9.9", "7.7.7.7", "1.2.3.4",  # public literals in tests
    "192.168.0.0", "172.16.0.0",  # the network addresses of the ranges defined below
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

SK_PREFIX = re.compile(rb"sk-(?:gw-)?([0-9a-f]{8,})")

SKIP_DIRS = {".git", ".cache", "bin", "data", "node_modules", "__pycache__", "plugins"}
BINARY_SNIFF = 8192


# --------------------------------------------------------------------------
# The local table
# --------------------------------------------------------------------------
def literal_length(pattern: str) -> int:
    r"""How many literal characters a pattern insists on.

    Ordering by the raw pattern length would be wrong: it counts lookarounds and escapes,
    so `(?<![\w./:-])acct-owner(?![\w./:-])` (33) would run before `acct-owner@corp\.example`
    (19) and turn the mailbox into the bare account name. Stripping the regex machinery first
    gives the length of what the pattern actually has to match, which is what "longest
    first" means here.
    """
    stripped = re.sub(r"\(\?<?[!=][^)]*\)", "", pattern)  # lookarounds
    stripped = re.sub(r"\\[A-Za-z]", "", stripped)        # \b, \w, ...
    stripped = re.sub(r"[\[\](){}|?+*.^$\\]", "", stripped)
    return len(stripped)


@dataclass
class TokenRule:
    name: str
    pattern: str
    replacement: str


@dataclass
class Table:
    """Real values to remove, the paths that name one, and occurrence exemptions."""

    tokens: list[TokenRule] = field(default_factory=list)
    renames: list[tuple[str, str]] = field(default_factory=list)
    exempt: list[re.Pattern] = field(default_factory=list)
    path: Path | None = None
    _union: re.Pattern | None = field(default=None, init=False, repr=False)

    @property
    def loaded(self) -> bool:
        return bool(self.tokens or self.renames or self.exempt)

    def ordered(self) -> list[TokenRule]:
        return sorted(self.tokens, key=lambda rule: literal_length(rule.pattern), reverse=True)

    def union(self) -> re.Pattern | None:
        """One alternation for every token rule.

        Scanning a blob once instead of once per rule is what keeps `--check-history`
        (thousands of blobs) inside a couple of seconds. Python's alternation picks the
        first alternative that matches at the leftmost position, so ordering the group
        declarations by literal length still decides who wins.
        """
        if self._union is not None or not self.tokens:
            return self._union
        self._union = re.compile(b"|".join(
            b"(?P<" + rule.name.encode() + b">" + rule.pattern.encode() + b")"
            for rule in self.ordered()))
        return self._union

    def exempt_spans(self, data: bytes) -> list[tuple[int, int]]:
        """Merged spans the rules must not look at (the toolchain families)."""
        if not self.exempt:
            return []
        spans = []
        for pattern in self.exempt:
            spans.extend((match.start(), match.end()) for match in pattern.finditer(data))
        if not spans:
            return []
        spans.sort()
        merged = [list(spans[0])]
        for start, end in spans[1:]:
            if start <= merged[-1][1]:
                merged[-1][1] = max(merged[-1][1], end)
            else:
                merged.append([start, end])
        return [(start, end) for start, end in merged]


def load_table(path: Path | None) -> Table:
    """Parse the local table.

    Format: one rule per line, TAB-separated; `#` comments and blank lines are ignored.
    Section headers switch the following lines: `[token]` name/pattern/replacement,
    `[rename]` old/new, `[exempt]` a regex for spans the rules must skip.
    """
    table = Table(path=path)
    if path is None or not path.exists():
        return table
    section = "token"
    for lineno, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        stripped = line.strip()
        if stripped in ("[token]", "[rename]", "[exempt]"):
            section = stripped[1:-1]
            continue
        parts = line.split("\t")
        if section == "token":
            if len(parts) != 3:
                raise ValueError(f"{path}:{lineno}: token needs 3 tab-separated fields")
            table.tokens.append(TokenRule(parts[0].strip(), parts[1], parts[2]))
        elif section == "rename":
            if len(parts) != 2:
                raise ValueError(f"{path}:{lineno}: rename needs 2 tab-separated fields")
            table.renames.append((parts[0].strip(), parts[1].strip()))
        else:
            if len(parts) != 1:
                raise ValueError(f"{path}:{lineno}: exempt needs one regex")
            table.exempt.append(re.compile(parts[0].encode()))
    return table


def table_path(explicit: str | None) -> Path | None:
    if explicit:
        return Path(explicit)
    from_env = os.environ.get(TABLE_ENV)
    if from_env:
        return Path(from_env)
    root = repo_root()
    return (root / DEFAULT_TABLE) if root else Path(DEFAULT_TABLE)


# --------------------------------------------------------------------------
# Findings
# --------------------------------------------------------------------------
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


def line_of(data: bytes, offset: int) -> int:
    return data.count(b"\n", 0, offset) + 1


def snippet(raw: bytes, limit: int = 60) -> str:
    text = raw.decode("utf-8", "replace").replace("\n", "\\n")
    return text if len(text) <= limit else text[:limit] + "…"


def is_exempt(path: str) -> bool:
    return any(re.match(pattern, path) for pattern in EXEMPT_PATHS)


def _masked(text: str, keep: int = 12) -> str:
    """Shorten a matched value for reporting so the report itself stays shareable."""
    return text if len(text) <= keep else text[:keep] + "…"


def segments(data: bytes, table: Table) -> Iterator[tuple[int, bytes]]:
    """The parts of `data` the rules are allowed to look at, with their offsets."""
    pos = 0
    for start, end in table.exempt_spans(data):
        if start > pos:
            yield pos, data[pos:start]
        pos = max(pos, end)
    if pos < len(data):
        yield pos, data[pos:]


def table_hits(data: bytes, path: str, table: Table) -> list[tuple[Hit, int]]:
    union = table.union()
    if union is None:
        return []
    hits = []
    for offset, chunk in segments(data, table):
        for match in union.finditer(chunk):
            hits.append((Hit(match.lastgroup or "token", path,
                             line_of(data, offset + match.start()),
                             _masked(match.group(0).decode("utf-8", "replace"))),
                         offset + match.start()))
    return hits


def module_owner_hits(data: bytes, path: str) -> list[tuple[Hit, int]]:
    owner = module_owner()
    hits = []
    for match in MODULE_PATH.finditer(data):
        if match.group(1).decode() == owner:
            continue
        hits.append((Hit("module_path_owner", path, line_of(data, match.start()),
                         match.group(0).decode()), match.start()))
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
        user = match.group(1).decode()
        if user in ALLOWED_HOME_USERS:
            continue
        hits.append((Hit("home_path", path, line_of(data, match.start()), "/home/" + user),
                     match.start()))
    return hits


def warn_hits(data: bytes, path: str) -> list[tuple[Hit, int]]:
    """Warnings: real-looking key prefixes are worth a look, but they prove nothing
    (the repository is full of deliberately fake `sk-` fixtures)."""
    hits = []
    for match in SK_PREFIX.finditer(data):
        payload = match.group(1).decode()
        if len(set(payload)) == 1:
            continue  # sk-aaaaaaaaa and friends
        hits.append((Hit("sk_prefix_review", path, line_of(data, match.start()),
                         "sk-" + payload[:8] + "…", level="warn"), match.start()))
    return hits


GENERIC_CHECKS = (module_owner_hits, ip_hits, email_hits, fingerprint_hits, home_hits)


def scan(data: bytes, path: str, table: Table, strict: bool = False) -> list[Hit]:
    """Every finding for one file's bytes.

    A location the table already names is not reported a second time by the generic rules
    (a real private address would otherwise fire both).
    """
    if is_exempt(path) and not strict:
        return []
    hits: list[Hit] = []
    covered: set[int] = set()
    for hit, offset in table_hits(data, path, table):
        hits.append(hit)
        covered.add(offset)
    for offset, chunk in segments(data, table):
        for check in GENERIC_CHECKS:
            for hit, local in check(chunk, path):
                if (absolute := offset + local) not in covered:
                    hit.line = line_of(data, absolute)
                    hits.append(hit)
        for hit, local in warn_hits(chunk, path):
            if offset + local not in covered:  # a table hit already names this location
                hits.append(hit)
    return hits


def rewrite(data: bytes, path: str, table: Table, strict: bool = False) -> tuple[bytes, int]:
    """Apply the table (and the module-owner rule) to one file's bytes.

    Exempt spans are copied through verbatim: that is what keeps the toolchain path
    families working in `Makefile`, the ops scripts and the docs that name them.
    """
    if is_exempt(path) and not strict:
        return data, 0
    fired = 0
    union = table.union()
    if union is not None:
        by_name = {rule.name: rule for rule in table.tokens}

        def substitute(match: re.Match) -> bytes:
            return by_name[match.lastgroup or ""].replacement.encode()

        out: list[bytes] = []
        pos = 0
        changed = False
        for start, end in table.exempt_spans(data):
            chunk = _sub(data[pos:start], union, substitute)
            changed = changed or chunk != data[pos:start]
            out.append(chunk)
            out.append(data[start:end])
            pos = end
        tail = _sub(data[pos:], union, substitute)
        changed = changed or tail != data[pos:]
        out.append(tail)
        if changed:
            data = b"".join(out)
            fired += 1
    owner = module_owner()
    if owner and any(match.group(1).decode() != owner for match in MODULE_PATH.finditer(data)):
        data = MODULE_PATH.sub(lambda m: MODULE_TEMPLATE.format(owner=owner).encode(), data)
        fired += 1
    return data, fired


def _sub(chunk: bytes, union: re.Pattern, substitute) -> bytes:
    """Replace inside one non-exempt chunk (or hand it back untouched)."""
    return union.sub(substitute, chunk) if union.search(chunk) else chunk


def rewrite_message(text: str, table: Table) -> str:
    """Commit messages carry the same values (and filter-branch rewrites only trees)."""
    data, _fired = rewrite(text.encode(), "commit-message", table, strict=True)
    return data.decode()


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


def read_bytes(path: Path, rel: str, quiet: bool = False) -> bytes | None:
    if not path.exists():
        return None  # the index can lag the worktree right after --apply/rename
    try:
        data = path.read_bytes()
    except OSError as exc:
        if not quiet:
            print(f"desensitize: cannot read {rel}: {exc}", file=sys.stderr)
        return None
    if b"\x00" in data[:BINARY_SNIFF]:
        return None  # binary: the rules are text rules
    return data


# --------------------------------------------------------------------------
# Modes
# --------------------------------------------------------------------------
def table_note(table: Table, quiet: bool, required: bool) -> int | None:
    if table.loaded:
        if not quiet:
            print(f"desensitize: table {table.path} — {len(table.tokens)} tokens, "
                  f"{len(table.renames)} renames, {len(table.exempt)} exemptions",
                  file=sys.stderr)
        return None
    missing = (f"desensitize: no local table at {table.path}; only the generic rules ran "
               f"(set {TABLE_ENV} or pass --table)")
    if required:
        print(f"{missing} — refusing to report a clean tree without it", file=sys.stderr)
        return 2
    if not quiet:
        print(missing, file=sys.stderr)
    return None


def run_check(root: Path, files: list[str], table: Table, strict: bool, show_all: bool,
              limit: int = 40, quiet: bool = False) -> int:
    hits: list[Hit] = []
    scanned = 0
    for rel in files:
        data = read_bytes(root / rel, rel, quiet)
        if data is None:
            continue
        scanned += 1
        hits.extend(scan(data, rel, table, strict))
    errors = [hit for hit in hits if hit.level == "error"]
    warnings = [hit for hit in hits if hit.level != "error"]
    for hit in (hits if show_all else errors[:limit] + warnings):
        print(hit.render())
    if not show_all and len(errors) > limit:
        print(f"… and {len(errors) - limit} more (drop --quiet to see them all)")
    print(f"\ndesensitize: scanned {scanned} files, {len(errors)} findings, "
          f"{len(warnings)} warnings" + ("" if strict else " (exceptions excluded)"))
    return 1 if errors else 0


def run_inventory(root: Path, files: list[str], table: Table, strict: bool,
                  as_json: bool, quiet: bool) -> int:
    per_rule: dict[str, dict] = {}
    for rel in files:
        data = read_bytes(root / rel, rel, quiet)
        if data is None:
            continue
        for hit in scan(data, rel, table, strict):
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


def run_apply(root: Path, files: list[str], table: Table, strict: bool, quiet: bool) -> int:
    changed, total_hits = 0, 0
    for rel in files:
        target = root / rel
        data = read_bytes(target, rel, quiet)
        if data is None:
            continue
        new_data, fired = rewrite(data, rel, table, strict)
        if fired:
            if new_data != data:
                target.write_bytes(new_data)
                changed += 1
            total_hits += 1
    renames = 0
    for old, new in table.renames:
        source, dest = root / old, root / new
        if not source.exists():
            continue
        if dest.exists():
            print(f"desensitize: rename target already exists: {new}", file=sys.stderr)
            return 2
        dest.parent.mkdir(parents=True, exist_ok=True)
        source.rename(dest)
        renames += 1
    print(f"desensitize: {changed} files rewritten, {renames} renamed")
    return 0


def run_check_history(table: Table, strict: bool, messages: bool) -> int:
    """Scan every blob any ref can reach, using the path it lived at."""
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
            cache[sha] = scan(data, path, table, strict)
        for hit in cache[sha]:
            hits.append(Hit(hit.rule, path + f"@{sha[:9]}", hit.line, hit.text, hit.level))
    if messages:
        out = subprocess.run(["git", "log", "--all", "--format=%x00%H%x00%B"],
                             capture_output=True, check=True).stdout
        fields = out.split(b"\x00")
        for index in range(1, len(fields) - 1, 2):
            sha, message = fields[index].decode(), fields[index + 1]
            for hit in scan(message, f"commit-message@{sha[:9]}", table, strict):
                hits.append(hit)
    errors = [hit for hit in hits if hit.level == "error"]
    limit = 200
    for hit in errors[:limit]:
        print(hit.render())
    if len(errors) > limit:
        print(f"… and {len(errors) - limit} more")
    print(f"\ndesensitize: {len(blobs)} blobs across {len(pairs)} paths"
          f"{' + commit messages' if messages else ''}, {len(errors)} findings in history"
          + ("" if strict else " (exceptions excluded)"))
    return 1 if errors else 0


def _read_blobs(shas: list[str]) -> dict[str, bytes]:
    """Batch-read objects; non-blob objects are simply absent from the result."""
    if not shas:
        return {}
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


def run_filter_message(table: Table) -> int:
    """`git filter-branch --msg-filter` entry point: stdin in, rewritten message out."""
    sys.stdout.write(rewrite_message(sys.stdin.read(), table))
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="desensitize.py",
        description="扫描并替换仓库里的真实环境标识（脱敏口径的单一真源，见 "
                    "docs/design/m89-code-desensitization.md）")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--check", action="store_true",
                      help="扫 tracked 文件（默认）；有命中退出 1")
    mode.add_argument("--apply", action="store_true", help="就地替换并按表里的 renames 改名")
    mode.add_argument("--check-history", action="store_true", help="扫全部 ref 的全部 blob")
    mode.add_argument("--inventory", action="store_true", help="按规则汇总当前命中，用来补规则表")
    mode.add_argument("--filter-message", action="store_true",
                      help="从 stdin 读一条提交信息、写出脱敏后的版本（filter-branch --msg-filter）")
    parser.add_argument("--root", help="扫描目录（默认：仓库根；--apply 时也用它）")
    parser.add_argument("--table", help=f"局部规则表路径（默认 {DEFAULT_TABLE}，可用 {TABLE_ENV} 覆盖）")
    parser.add_argument("--require-table", action="store_true",
                        help="没有局部规则表就直接失败（发版门禁用）")
    parser.add_argument("--messages", action="store_true",
                        help="--check-history 时连提交信息一起扫")
    parser.add_argument("--strict", action="store_true",
                        help="把例外路径也当普通文件（审计刻意保留的残留）")
    parser.add_argument("--json", action="store_true", help="机器可读输出（--inventory）")
    parser.add_argument("--quiet", action="store_true", help="只打印汇总与少量样例")
    args = parser.parse_args(argv)

    try:
        table = load_table(table_path(args.table))
    except ValueError as exc:
        print(f"desensitize: {exc}", file=sys.stderr)
        return 2
    code = table_note(table, args.quiet, args.require_table)
    if code is not None:
        return code

    if args.filter_message:
        return run_filter_message(table)
    if args.check_history:
        return run_check_history(table, args.strict, args.messages)

    root = Path(args.root).resolve() if args.root else repo_root()
    if root is None:
        print("desensitize: --root is required outside a git repository", file=sys.stderr)
        return 2
    if not root.is_dir():
        print(f"desensitize: not a directory: {root}", file=sys.stderr)
        return 2
    files = walk_files(root) if args.root else tracked_files()

    if args.apply:
        return run_apply(root, files, table, args.strict, args.quiet)
    if args.inventory:
        return run_inventory(root, files, table, args.strict, args.json, args.quiet)
    return run_check(root, files, table, args.strict, show_all=not args.quiet, quiet=args.quiet)


if __name__ == "__main__":
    sys.exit(main())
