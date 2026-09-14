#!/usr/bin/env python3
"""sub2api-migrate: 把 sub2api 的用户与 API Key 迁移到同机的 ai_gateway。

为什么需要它：sub2api 里那些 key 已经在用户的客户端配置里，换 key 就要逐人重配；而把明文
交给网关或运维脚本，意味着它会路过终端回显、shell 历史、临时文件或对话记录。ai_gateway 的
key 表只存 `key_prefix`（明文前 12 字符）与 `key_hash`（明文 SHA-256），管理接口
`POST /admin/api/v1/keys/import` 也只收这两个值。于是本脚本让**源库自己算哈希**：

    left(btrim(key),12)                                  -> key_prefix
    encode(sha256(convert_to(btrim(key),'UTF8')),'hex')   -> key_hash   （与 Go 的 sha256 一致）

脚本从不 `SELECT key`，所以明文既不出 postgres 进程，也不会出现在任何输出里。

子命令：

  plan       只读：打印数据集、标签分配、全部断言与模型名覆盖，不做任何写入
  snapshot   写迁移前的数据库快照（0600）并断言目标实例还没有别人的账户/key/请求日志
  apply      建账户 + 导入 key（幂等：账户按名 upsert、key 按前缀 upsert）
  verify     结构核对：逐把 key 比对 aigw 库里的 prefix/hash/tags 与源库重算值
  report     写 /opt/aigw/data/sub2api-migration-<ts>.json（0600，无密钥材料）

用法（服务器上，root）：

  python3 sub2api_migrate.py plan
  python3 sub2api_migrate.py snapshot
  python3 sub2api_migrate.py apply
  python3 sub2api_migrate.py verify
  python3 sub2api_migrate.py report

详见 docs/sub2api-migration.md；导入接口的设计见 docs/design/m43-api-key-hash-import.md。
"""
import argparse
import http.cookiejar
import json
import os
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field

GATEWAY = "http://127.0.0.1:8088/aigw"
PASSWORD_FILE = "/opt/aigw/.admin-password"
DB_PATH = "/opt/aigw/data/aigw.db"
PSQL = ["docker", "exec", "-i", "sub2api-postgres", "psql", "-U", "sub2api", "-d", "sub2api"]
DEFAULT_NOTES = "智天成"
DEFAULT_TAGS = ["蓝精灵1", "蓝精灵2", "蓝精灵3"]
# 源分组 -> 目标标签：这些分组各自只含一个「已迁入 aigw 的供应商账号」，因此代表用户的既有订阅
# 意图，先按它绑定，其余 key 再按均衡规则补齐。以数据校验，改不动就报错而不是猜。
DEFAULT_INTENT_GROUPS = {"21": "蓝精灵2", "22": "蓝精灵3", "23": "蓝精灵1"}
ACCOUNT_NOTE_PREFIX = "sub2api user #"
IMPORT_MARKER = "import:"
FIELD_SEP = "\x1f"


def die(message: str) -> None:
    sys.exit("sub2api-migrate: " + message)


def note_line(message: str) -> None:
    print(message, flush=True)


# ---------------------------------------------------------------------------
# 源库（只读）与目标网关
# ---------------------------------------------------------------------------


def psql(sql: str) -> list[list[str]]:
    """Run one read-only query and return rows of already-split fields."""
    result = subprocess.run(PSQL + ["-t", "-A", "-F", FIELD_SEP, "-v", "ON_ERROR_STOP=1", "-c", sql],
                            capture_output=True, text=True)
    if result.returncode != 0:
        die("psql 失败: " + result.stderr.strip()[:400])
    rows: list[list[str]] = []
    for line in result.stdout.splitlines():
        if line == "":
            continue
        rows.append(line.split(FIELD_SEP))
    return rows


def sql_literal(value: str) -> str:
    if FIELD_SEP in value or "|" in value:
        die("内部错误：取值里有分隔符")
    return "'" + value.replace("'", "''") + "'"


@dataclass
class SourceUser:
    id: int
    username: str
    email: str
    status: str


@dataclass
class SourceKey:
    id: int
    user_id: int
    name: str
    group_id: str
    prefix: str
    khash: str
    last_used: str = ""


@dataclass
class Source:
    users: list[SourceUser] = field(default_factory=list)
    keys: list[SourceKey] = field(default_factory=list)
    excluded: list[tuple] = field(default_factory=list)
    group_accounts: dict[str, list[int]] = field(default_factory=dict)
    shape_problems: list[str] = field(default_factory=list)


def load_source(notes: str) -> Source:
    src = Source()
    for row in psql(
        "SELECT id, username, email, status FROM users "
        f"WHERE notes = {sql_literal(notes)} AND deleted_at IS NULL ORDER BY id;"
    ):
        src.users.append(SourceUser(int(row[0]), row[1], row[2], row[3]))
    if not src.users:
        die(f"源库里没有备注为 {notes} 的用户")

    for row in psql(
        "SELECT k.id, k.user_id, k.name, COALESCE(k.group_id::text, ''), "
        "       left(btrim(k.key), 12), "
        "       encode(sha256(convert_to(btrim(k.key), 'UTF8')), 'hex'), "
        "       COALESCE(to_char(k.last_used_at, 'YYYY-MM-DD'), '') "
        "FROM users u JOIN api_keys k ON k.user_id = u.id "
        f"WHERE u.notes = {sql_literal(notes)} AND u.deleted_at IS NULL "
        "  AND k.deleted_at IS NULL AND k.status = 'active' ORDER BY k.id;"
    ):
        src.keys.append(SourceKey(int(row[0]), int(row[1]), row[2], row[3], row[4], row[5], row[6]))

    for row in psql(
        "SELECT k.id, k.user_id, k.name, k.status, (k.deleted_at IS NOT NULL)::text "
        "FROM users u JOIN api_keys k ON k.user_id = u.id "
        f"WHERE u.notes = {sql_literal(notes)} AND u.deleted_at IS NULL "
        "  AND (k.deleted_at IS NOT NULL OR k.status <> 'active') ORDER BY k.id;"
    ):
        src.excluded.append(tuple(row))

    for row in psql(
        "SELECT ag.group_id::text, a.id::text FROM account_groups ag "
        "JOIN accounts a ON a.id = ag.account_id ORDER BY 1, 2;"
    ):
        src.group_accounts.setdefault(row[0], []).append(int(row[1]))

    checks = psql(
        "SELECT count(*) FILTER (WHERE k.key <> btrim(k.key)), "
        "       count(*) FILTER (WHERE k.key !~ '^[!-~]+$'), "
        "       count(*) FILTER (WHERE length(k.key) < 12) "
        "FROM users u JOIN api_keys k ON k.user_id = u.id "
        f"WHERE u.notes = {sql_literal(notes)} AND u.deleted_at IS NULL "
        "  AND k.deleted_at IS NULL AND k.status = 'active';"
    )[0]
    for label, count in zip(("含首尾空白的 key", "含非可打印字符的 key", "短于 12 字符的 key"), checks):
        if int(count) > 0:
            src.shape_problems.append(f"{label}: {count} 把")
    return src


def usage_models(notes: str, days: int) -> list[tuple[str, int]]:
    """Model names these users actually asked for, so the operator can see coverage gaps."""
    rows = psql(
        "SELECT l.model, count(*) FROM usage_logs l JOIN users u ON u.id = l.user_id "
        f"WHERE u.notes = {sql_literal(notes)} AND l.created_at > now() - interval '{int(days)} days' "
        "GROUP BY 1 ORDER BY 2 DESC;"
    )
    return [(row[0], int(row[1])) for row in rows]


class Gateway:
    """Small admin-API client. The session cookie lives in memory only."""

    def __init__(self, base: str, password_file: str):
        self.base = base.rstrip("/")
        self.password_file = password_file
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))

    def login(self, username: str = "admin") -> None:
        with open(self.password_file) as fh:
            password = fh.read().strip()
        self.call("POST", "/admin/api/v1/auth/login", {"username": username, "password": password})

    def call(self, method: str, path: str, body: dict | None = None) -> dict:
        data = None
        headers = {}
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(req, timeout=60) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                payload = json.loads(raw)
                message = payload.get("error", {}).get("message", raw[:200])
            except Exception:
                message = raw[:200]
            die(f"{method} {path} -> HTTP {exc.code}: {message}")
        if not raw:
            return {}
        return json.loads(raw)

    def list_all(self, path: str) -> list[dict]:
        out: list[dict] = []
        offset = 0
        while True:
            sep = "&" if "?" in path else "?"
            page = self.call("GET", f"{path}{sep}limit=200&offset={offset}")
            rows = page.get("data") or []
            out.extend(rows)
            total = page.get("total")
            offset += len(rows)
            if not rows or (isinstance(total, int) and offset >= total):
                return out


@dataclass
class Target:
    tags: list[dict] = field(default_factory=list)
    providers: list[dict] = field(default_factory=list)
    accounts: list[dict] = field(default_factory=list)
    keys: list[dict] = field(default_factory=list)

    def tag_by_name(self, name: str) -> dict | None:
        for tag in self.tags:
            if tag.get("name") == name:
                return tag
        return None

    def account_by_name(self, name: str) -> dict | None:
        for account in self.accounts:
            if account.get("name") == name:
                return account
        return None

    def key_by_prefix(self, prefix: str) -> dict | None:
        for key in self.keys:
            if key.get("key_prefix") == prefix:
                return key
        return None

    def provider_source_account(self, provider_name: str) -> int | None:
        for provider in self.providers:
            if provider.get("name") != provider_name:
                continue
            meta = provider.get("meta") or {}
            if isinstance(meta, dict) and meta.get("source_account_id") is not None:
                return int(meta["source_account_id"])
        return None

    def tag_granting_provider(self, provider_name: str) -> str | None:
        for tag in self.tags:
            grants = tag.get("grants")
            if isinstance(grants, dict) and provider_name in (grants.get("providers") or []):
                return tag.get("name")
        return None


def load_target(gw: Gateway) -> Target:
    return Target(
        tags=gw.list_all("/admin/api/v1/tags"),
        providers=gw.list_all("/admin/api/v1/providers"),
        accounts=gw.list_all("/admin/api/v1/accounts"),
        keys=gw.list_all("/admin/api/v1/keys"),
    )


def read_target_db(path: str) -> tuple[list[dict], list[dict]]:
    """Read the aigw SQLite file read-only: the API never exposes key_hash."""
    if not os.path.exists(path):
        die(f"找不到目标数据库 {path}")
    conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    try:
        keys = [
            {"id": r[0], "account_id": r[1], "name": r[2], "key_prefix": r[3], "key_hash": r[4],
             "tags_json": r[5], "status": r[6], "created_by": r[7]}
            for r in conn.execute(
                "SELECT id, account_id, name, key_prefix, key_hash, tags_json, status, created_by "
                "FROM api_keys ORDER BY id")
        ]
        accounts = [
            {"id": r[0], "name": r[1], "tags_json": r[2], "note": r[3], "status": r[4]}
            for r in conn.execute("SELECT id, name, tags_json, note, status FROM accounts ORDER BY id")
        ]
        return keys, accounts
    finally:
        conn.close()


# ---------------------------------------------------------------------------
# 分配规则
# ---------------------------------------------------------------------------


def intent_mapping(src: Source, target: Target, intent_groups: dict[str, str]) -> dict[int, str]:
    """Expand 分组 -> 标签 into 源账号 id -> 标签, verifying it against the live data."""
    mapping: dict[int, str] = {}
    for group_id, tag_name in intent_groups.items():
        accounts = src.group_accounts.get(group_id) or []
        mapped: list[tuple[int, str]] = []
        for account_id in accounts:
            for provider in target.providers:
                meta = provider.get("meta") or {}
                if not isinstance(meta, dict) or meta.get("source_account_id") is None:
                    continue
                if int(meta["source_account_id"]) != account_id:
                    continue
                tag = target.tag_granting_provider(provider.get("name"))
                if tag:
                    mapped.append((account_id, tag))
        unique = sorted(set(mapped))
        if len(unique) != 1:
            die(f"分组 {group_id} 的意图不唯一（{unique}）：请改用 --intent-group 明确指定")
        account_id, derived = unique[0]
        if derived != tag_name:
            die(f"分组 {group_id} 派生出标签 {derived}，与预期的 {tag_name} 不一致：请核对 --intent-group")
        mapping[account_id] = tag_name
        note_line(f"意图：分组 {group_id} → 源账号 {account_id} → 标签 {tag_name}")
    return mapping


def assign(src: Source, target: Target, tags: list[str], intent_groups: dict[str, str],
           reissue: set[int]) -> list[dict]:
    """Return one assignment per key (reissued keys keep their slot but are not imported)."""
    account_intent = intent_mapping(src, target, intent_groups)
    for tag in tags:
        if target.tag_by_name(tag) is None:
            die(f"目标网关里没有标签 {tag}")

    # 分组 -> 成员账号只在 intent_mapping 里用来校验显式意图是否仍然成立。
    assignments: list[dict] = []
    assigned: set[int] = set()
    counts = {tag: 0 for tag in tags}
    per_user: dict[int, dict[str, int]] = {}

    def record(key: SourceKey, tag: str, reason: str) -> None:
        counts[tag] += 1
        per_user.setdefault(key.user_id, {})
        per_user[key.user_id][tag] = per_user[key.user_id].get(tag, 0) + 1
        assigned.add(key.id)
        assignments.append({"key": key, "tag": tag, "reason": reason,
                            "reissue": key.id in reissue})

    # 1. 先按既有分组意图绑定：只认显式列出的分组（它们的成员账号唯一，代表用户既有订阅）。
    #    不按「成员恰好只有一个」自动扩大范围——额度型分组（20刀/40刀/200刀）成员的多少
    #    是历史巧合，不是订阅意图。
    for key in src.keys:
        expected_tag = intent_groups.get(key.group_id or "")
        if expected_tag:
            record(key, expected_tag, f"分组 {key.group_id} 的既有订阅")
    # 2. 其余按 (全局最少, 该用户最少, 标签 id 最小) 均衡，顺序固定为源 key id 升序。
    for key in src.keys:
        if key.id in assigned:
            continue

        def score(tag: str, user_id: int = key.user_id) -> tuple:
            return (counts[tag], per_user.get(user_id, {}).get(tag, 0), tags.index(tag))

        record(key, min(tags, key=score), "均衡补齐")
    return assignments


def duplicate_prefixes(keys: list[SourceKey]) -> dict[str, list[SourceKey]]:
    seen: dict[str, list[SourceKey]] = {}
    for key in keys:
        seen.setdefault(key.prefix, []).append(key)
    return {prefix: group for prefix, group in seen.items() if len(group) > 1}


# ---------------------------------------------------------------------------
# 断言
# ---------------------------------------------------------------------------


def preflight(src: Source, target: Target, assignments: list[dict], tags: list[str],
              reissue: set[int], db_path: str) -> list[str]:
    problems: list[str] = []
    problems.extend(src.shape_problems)

    # 重签的 key 不导入，因此它的旧前缀不参与冲突判定：冲突正是它被重签的原因。
    duplicates = duplicate_prefixes([a["key"] for a in assignments if not a["reissue"]])
    for prefix, group in duplicates.items():
        ids = ", ".join(f"#{k.id}" for k in group)
        problems.append(
            f"前缀冲突 {prefix}（key {ids}）：网关只按 12 字符前缀查找且该列唯一，"
            "必须保留其中一把，另一把用 --reissue-key 在网关重签")

    names: dict[str, int] = {}
    for user in src.users:
        names[user.username] = names.get(user.username, 0) + 1
    for user in src.users:
        if names[user.username] > 1:
            problems.append(f"源用户名重复（{user.username}）：账户名唯一，需要人工定名")
        if len(user.username) > 64:
            problems.append(f"用户名超过 64 字符：{user.username}")
        if user.username.strip() == "":
            problems.append(f"源用户 {user.id} 没有用户名：无法命名账户")
        if user.status != "active":
            problems.append(f"源用户 {user.username} 不是 active（status={user.status}）")
        account = target.account_by_name(user.username)
        if account is not None and not str(account.get("note", "")).startswith(ACCOUNT_NOTE_PREFIX):
            problems.append(
                f"目标账户 {user.username} 已存在且不是本迁移创建的（note={account.get('note')!r}）")

    stored = {row["key_prefix"]: row for row in read_target_db(db_path)[0]}
    for assignment in assignments:
        key = assignment["key"]
        existing = stored.get(key.prefix)
        if existing is None:
            continue
        if existing["key_hash"] != key.khash and not str(existing["created_by"]).startswith(IMPORT_MARKER):
            problems.append(
                f"前缀 {key.prefix} 已被控制台签发的 key #{existing['id']!s} 占用（{existing['name']}）："
                "导入会返回 409")
        if assignment["reissue"] and not str(existing["created_by"]).startswith(IMPORT_MARKER):
            problems.append(f"重签占位的前缀 {key.prefix} 在目标网关里已被占用")

    for tag in tags:
        tag_row = target.tag_by_name(tag)
        if tag_row is None:
            problems.append(f"目标网关缺少标签 {tag}")
            continue
        grants = tag_row.get("grants")
        providers = (grants or {}).get("providers") if isinstance(grants, dict) else None
        if not providers:
            problems.append(f"标签 {tag} 没有任何供应商授权：绑定的 key 会回落到默认通配授权")
            continue
        for provider_name in providers:
            match = [p for p in target.providers if p.get("name") == provider_name]
            if not match:
                problems.append(f"标签 {tag} 授权的供应商 {provider_name} 不存在")
            elif not match[0].get("enabled", True):
                problems.append(f"标签 {tag} 授权的供应商 {provider_name} 未启用")
    return problems


# ---------------------------------------------------------------------------
# 输出
# ---------------------------------------------------------------------------


def print_plan(src: Source, target: Target, assignments: list[dict], tags: list[str],
               reissue: set[int]) -> None:
    users = {user.id: user for user in src.users}
    note_line("")
    note_line("源数据集")
    note_line(f"  用户 {len(src.users)} 个，待迁 key {len(src.keys)} 把"
              f"（排除 {len(src.excluded)} 把：已删除或配额耗尽）")
    for row in src.excluded:
        note_line(f"    排除 key #{row[0]}（用户 {row[1]}，{row[2]}，status={row[3]}，deleted={row[4]}）")

    note_line("")
    note_line(f"{'源用户':<18}{'key':<6}{'名称':<26}{'前缀':<15}{'标签':<10}{'说明'}")
    for assignment in assignments:
        key = assignment["key"]
        user = users.get(key.user_id)
        label = f"{user.username}({user.id})" if user else f"用户 {key.user_id}"
        mark = "重签" if assignment["reissue"] else assignment["reason"]
        note_line(f"{label:<18}{key.id:<6}{key.name[:24]:<26}{key.prefix:<15}"
                  f"{assignment['tag']:<10}{mark}")

    counts = {tag: 0 for tag in tags}
    for assignment in assignments:
        counts[assignment["tag"]] += 1
    note_line("")
    note_line("分配结果：" + " / ".join(f"{tag}={counts[tag]}" for tag in tags))
    if reissue:
        kept = {a["key"].prefix for a in assignments if not a["reissue"]}
        for assignment in assignments:
            key = assignment["key"]
            if assignment["reissue"] and key.prefix in kept:
                others = [a["key"].id for a in assignments
                          if not a["reissue"] and a["key"].prefix == key.prefix]
                note_line(f"前缀冲突：key #{key.id} 与 key {others} 同为 {key.prefix}，"
                          f"保留 {others}，key #{key.id} 在网关重签后绑 {assignment['tag']}")
        note_line(f"需在网关重签的 key：{sorted(reissue)}（明文由控制台取出后单独交付本人）")


def model_coverage(notes: str, days: int, db_path: str) -> list[tuple[str, int, bool]]:
    resolved = set()
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        for name, aliases in conn.execute("SELECT public_name, aliases_json FROM models"):
            resolved.add(name)
            try:
                for alias in json.loads(aliases or "[]"):
                    resolved.add(alias)
            except Exception:
                pass
    finally:
        conn.close()
    out = []
    for model, count in usage_models(notes, days):
        out.append((model, count, model in resolved))
    return out


# ---------------------------------------------------------------------------
# 子命令
# ---------------------------------------------------------------------------


def build_plan(args, gw: Gateway) -> tuple[Source, Target, list[dict]]:
    src = load_source(args.notes)
    target = load_target(gw)
    tags = [t.strip() for t in args.tags.split(",") if t.strip()]
    reissue = {int(v) for v in args.reissue_key}
    skip = {int(v) for v in args.skip_key}
    for key_id in reissue | skip:
        if not any(key.id == key_id for key in src.keys):
            die(f"--reissue-key/--skip-key 指定的 key #{key_id} 不在待迁集合里")
    if skip:
        src.keys = [key for key in src.keys if key.id not in skip]
    intent_groups = dict(args.intent_group_map)
    assignments = assign(src, target, tags, intent_groups, reissue)
    return src, target, assignments


def cmd_plan(args, gw: Gateway) -> int:
    src, target, assignments = build_plan(args, gw)
    tags = [t.strip() for t in args.tags.split(",") if t.strip()]
    print_plan(src, target, assignments, tags, {int(v) for v in args.reissue_key})
    problems = preflight(src, target, assignments, tags, {int(v) for v in args.reissue_key}, args.db)
    note_line("")
    if problems:
        note_line("预检未通过：")
        for problem in problems:
            note_line("  ✗ " + problem)
    else:
        note_line("预检全部通过。")

    coverage = model_coverage(args.notes, args.coverage_days, args.db)
    missing = [(model, count) for model, count, ok in coverage if not ok]
    note_line("")
    note_line(f"模型名覆盖（近 {args.coverage_days} 天源库用量，目标网关解析不了的名字）")
    if missing:
        for model, count in missing[:20]:
            note_line(f"  ✗ {model}: {count} 次")
        note_line("  → 这些名字在 aigw 里没有 models/别名/映射；迁移后请求会 404，需补配置或通知用户改名")
    else:
        note_line("  全部命中")
    return 1 if problems else 0


def cmd_snapshot(args, gw: Gateway) -> int:
    keys, accounts = read_target_db(args.db)
    foreign_keys = [k for k in keys if not str(k["created_by"]).startswith(IMPORT_MARKER)]
    foreign_accounts = [a for a in accounts if not str(a["note"]).startswith(ACCOUNT_NOTE_PREFIX)]
    if foreign_keys or foreign_accounts:
        die(f"目标实例上已有 {len(foreign_accounts)} 个非迁移账户、{len(foreign_keys)} 把非导入 key："
            "快照回滚会连带丢掉它们，请先确认或改用逐条回滚")

    stamp = time.strftime("%Y%m%d-%H%M%S")
    dest = f"{args.db}.pre-sub2api-{stamp}"
    src = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    dst = sqlite3.connect(dest)
    try:
        with dst:
            src.backup(dst)
        check = dst.execute("PRAGMA quick_check").fetchone()[0]
    finally:
        dst.close()
        src.close()
    os.chmod(dest, 0o600)
    if check != "ok":
        die(f"快照自检失败：{check}")
    note_line(f"快照：{dest}（0600，quick_check=ok，"
              f"迁移前 accounts={len(accounts)} keys={len(keys)}）")
    return 0


def cmd_apply(args, gw: Gateway) -> int:
    src, target, assignments = build_plan(args, gw)
    tags = [t.strip() for t in args.tags.split(",") if t.strip()]
    reissue = {int(v) for v in args.reissue_key}
    problems = preflight(src, target, assignments, tags, reissue, args.db)
    if problems:
        for problem in problems:
            note_line("  ✗ " + problem)
        die("预检未通过，未做任何写入")
    if args.limit:
        keep = {a["key"].id for a in assignments[:args.limit]}
        assignments = [a for a in assignments if a["key"].id in keep]

    gw.login()
    created_users = 0
    imported = 0
    updated = 0
    for user in src.users:
        if not any(a["key"].user_id == user.id for a in assignments):
            continue
        account = target.account_by_name(user.username)
        if account is None:
            body = {"name": user.username, "billing_mode": "postpaid", "tags": [],
                    "note": f"{ACCOUNT_NOTE_PREFIX}{user.id} · {user.email} · 备注:{args.notes}"}
            account = gw.call("POST", "/admin/api/v1/accounts", body)
            created_users += 1
        for assignment in assignments:
            key = assignment["key"]
            if key.user_id != user.id:
                continue
            if assignment["reissue"]:
                continue
            result = gw.call("POST", "/admin/api/v1/keys/import", {
                "account_id": account["id"], "name": key.name,
                "key_prefix": key.prefix, "key_hash": key.khash,
                "tags": [assignment["tag"]], "status": "active",
            })
            if result.get("created"):
                imported += 1
            else:
                updated += 1
    note_line(f"账户新增 {created_users} 个；key 新增 {imported} 把、更新 {updated} 把（重签占位 {len(reissue)} 把未导入）")
    return 0


def cmd_verify(args, gw: Gateway) -> int:
    src, target, assignments = build_plan(args, gw)
    tags = [t.strip() for t in args.tags.split(",") if t.strip()]
    reissue = {int(v) for v in args.reissue_key}
    keys, accounts = read_target_db(args.db)
    accounts_by_id = {a["id"]: a for a in accounts}
    keys_by_prefix = {k["key_prefix"]: k for k in keys}
    users = {user.id: user for user in src.users}

    failures: list[str] = []
    warnings: list[str] = []
    counts = {tag: 0 for tag in tags}
    for assignment in assignments:
        key = assignment["key"]
        user = users[key.user_id]
        account = target.account_by_name(user.username)
        if account is None:
            failures.append(f"账户缺失：{user.username}")
            continue
        row = accounts_by_id.get(account["id"])
        if row is None:
            failures.append(f"账户 {user.username} 不在数据库里")
            continue
        if row["tags_json"] not in ("", "[]"):
            failures.append(f"账户 {user.username} 带了标签 {row['tags_json']}：会与 key 标签取并集，必须为空")
        if assignment["reissue"]:
            candidates = [
                k for k in keys
                if k["account_id"] == account["id"]
                and json.loads(k["tags_json"] or "[]") == [assignment["tag"]]
                and k["key_prefix"] not in {a["key"].prefix for a in assignments}
            ]
            if len(candidates) == 1:
                counts[assignment["tag"]] += 1
                note_line(f"重签 key：源 key #{key.id} → aigw key #{candidates[0]['id']}"
                          f"（{candidates[0]['key_prefix']}，标签 {assignment['tag']}）")
            else:
                warnings.append(f"重签 key 未就位：源 key #{key.id}（用户 {user.username}）"
                                f"应在账户 {user.username} 下新建一把标签为 {assignment['tag']} 的 key")
            continue
        stored = keys_by_prefix.get(key.prefix)
        if stored is None:
            failures.append(f"key 缺失：前缀 {key.prefix}（源 key #{key.id}）")
            continue
        if stored["key_hash"] != key.khash:
            failures.append(f"哈希不一致：前缀 {key.prefix}（源 key #{key.id}）")
        if stored["status"] != "active":
            failures.append(f"状态不是 active：前缀 {key.prefix} status={stored['status']}")
        if not str(stored["created_by"]).startswith(IMPORT_MARKER):
            failures.append(f"created_by 不是导入标记：前缀 {key.prefix} created_by={stored['created_by']}")
        if json.loads(stored["tags_json"] or "[]") != [assignment["tag"]]:
            failures.append(f"标签不符：前缀 {key.prefix} 库里是 {stored['tags_json']}，"
                            f"预期 [\"{assignment['tag']}\"]")
        counts[assignment["tag"]] += 1

    # 生效标签要经管理接口再确认一次：它走的是注册表快照 + 数据面同一套解析。
    expected_by_prefix = {a["key"].prefix: a["tag"] for a in assignments if not a["reissue"]}
    for key_row in target.keys:
        prefix = key_row.get("key_prefix")
        expected = expected_by_prefix.get(prefix)
        if expected is None:
            continue
        if key_row.get("effective_tags") != [expected]:
            failures.append(f"生效标签不符：{prefix} effective_tags={key_row.get('effective_tags')}，"
                            f"预期 [\"{expected}\"]")

    tag_summary = " / ".join(f"{tag}={counts[tag]}" for tag in tags)
    note_line(f"核对结果：{tag_summary}")
    for warning in warnings:
        note_line("  ! " + warning)
    for failure in failures:
        note_line("  ✗ " + failure)
    if failures:
        note_line(f"未通过：{len(failures)} 项")
        return 1
    note_line("结构与授权核对全部通过。")
    return 0


def cmd_report(args, gw: Gateway) -> int:
    src, target, assignments = build_plan(args, gw)
    tags = [t.strip() for t in args.tags.split(",") if t.strip()]
    reissue = {int(v) for v in args.reissue_key}
    keys, accounts = read_target_db(args.db)
    keys_by_prefix = {k["key_prefix"]: k for k in keys}
    users = {user.id: user for user in src.users}
    accounts_by_name = {a["name"]: a for a in target.accounts}

    rows = []
    for assignment in assignments:
        key = assignment["key"]
        user = users[key.user_id]
        account = accounts_by_name.get(user.username) or {}
        stored = keys_by_prefix.get(key.prefix)
        rows.append({
            "source_user_id": user.id, "source_username": user.username, "source_email": user.email,
            "source_key_id": key.id, "source_key_name": key.name, "source_group_id": key.group_id,
            "key_prefix": key.prefix,
            "tag": assignment["tag"], "reason": assignment["reason"], "reissue": assignment["reissue"],
            "target_account_id": account.get("id"), "target_account_name": user.username,
            "target_key_id": None if stored is None else stored["id"],
        })
    counts = {tag: 0 for tag in tags}
    for row in rows:
        counts[row["tag"]] += 1
    payload = {
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "notes_filter": args.notes,
        "gateway": args.gateway,
        "accounts": len(src.users),
        "keys": len(rows),
        "tag_counts": counts,
        "assignments": rows,
        "excluded_keys": [
            {"key_id": int(row[0]), "user_id": int(row[1]), "name": row[2], "status": row[3],
             "deleted": row[4] == "true"} for row in src.excluded
        ],
        "model_coverage": [
            {"model": model, "requests": count, "resolved": ok}
            for model, count, ok in model_coverage(args.notes, args.coverage_days, args.db)
        ],
    }
    stamp = time.strftime("%Y%m%d-%H%M%S")
    path = f"/opt/aigw/data/sub2api-migration-{stamp}.json"
    with open(path, "w") as fh:
        json.dump(payload, fh, ensure_ascii=False, indent=2)
    os.chmod(path, 0o600)
    note_line(f"报告：{path}（0600，{len(rows)} 条映射，"
              + " / ".join(f"{tag}={counts[tag]}" for tag in tags) + "）")
    return 0


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def parse_intent_groups(raw: str) -> dict[str, str]:
    if not raw:
        return dict(DEFAULT_INTENT_GROUPS)
    out: dict[str, str] = {}
    for item in raw.split(","):
        if not item.strip():
            continue
        if "=" not in item:
            die(f"--intent-group 需要 group=标签 形式：{item}")
        group, tag = item.split("=", 1)
        out[group.strip()] = tag.strip()
    return out


def main() -> int:
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("cmd", choices=("plan", "snapshot", "apply", "verify", "report"))
    parser.add_argument("--gateway", default=GATEWAY)
    parser.add_argument("--password-file", default=PASSWORD_FILE)
    parser.add_argument("--db", default=DB_PATH, help="目标 ai_gateway 的 SQLite 路径（只读核对用）")
    parser.add_argument("--notes", default=DEFAULT_NOTES, help="源库 users.notes 过滤值")
    parser.add_argument("--tags", default=",".join(DEFAULT_TAGS), help="参与分配的目标标签，逗号分隔")
    parser.add_argument("--intent-group", default="", help="分组=标签,分组=标签（默认按 gptjp 的 1/2/3 研发帐号）")
    parser.add_argument("--reissue-key", action="append", default=[],
                        help="该源 key 不导入（前缀冲突或需换 key），但保留标签占位，由控制台重签")
    parser.add_argument("--skip-key", action="append", default=[], help="该源 key 完全跳过")
    parser.add_argument("--limit", type=int, default=0, help="apply 只处理前 N 把（分批）")
    parser.add_argument("--coverage-days", type=int, default=30, help="模型名覆盖统计的回溯天数")
    args = parser.parse_args()
    args.intent_group_map = parse_intent_groups(args.intent_group)

    gw = Gateway(args.gateway, args.password_file)
    if args.cmd in ("plan", "snapshot", "apply", "verify", "report"):
        # 只读命令也要登录：账户/key/标签的现状只能经管理接口读。plan 不写任何东西。
        gw.login()
    handlers = {"plan": cmd_plan, "snapshot": cmd_snapshot, "apply": cmd_apply,
                "verify": cmd_verify, "report": cmd_report}
    return handlers[args.cmd](args, gw)


if __name__ == "__main__":
    raise SystemExit(main())
