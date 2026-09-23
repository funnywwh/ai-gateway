#!/usr/bin/env python3
"""sub2api-reimport: 清空一个 ai_gateway 实例，再从同机 sub2api 全量重导。

与 `scripts/sub2api-migrate.py`（按 users.notes 过滤、增量搬用户与 key）互补：本脚本面向
「整库重建」——把目标实例变成 sub2api 的一个镜像：全部存活用户 → 账户，全部活跃 key →
导入的 key，sub2api 的上游账号 → 供应商，再按人群规则挂三个标签：

    智天成  → 只授权 deepseek
    电商    → 只授权 azure
    E26Q    → 只授权 azure

人群规则（可复算，先分组后备注）：有活跃 key 属于分组 20 的用户 → E26Q；否则 notes='智天成'
→ 智天成；其余 → 电商。

保密纪律（与 sub2api-migrate.py 同一条）：
  * key 的明文不出源库进程——前缀与哈希由源库 SQL 现算（`left(btrim(key),12)` 与
    `encode(sha256(convert_to(btrim(key),'UTF8')),'hex')`），脚本从不 `SELECT key`；
  * 供应商凭据（OAuth refresh_token / api_key）只在进程内与 0600 临时文件里出现，写后即碎，
    绝不打印；所有打印与异常都过一遍 redact()；
  * 报告文件（0600）里没有密钥材料。

子命令（除 inventory/snapshot 外都会写库，顺序即执行顺序）：

  inventory   只读：两侧盘点、人群与标签映射、前缀冲突、模型覆盖、全部断言
  snapshot    回滚点 + 重建素材：DB 快照、config 备份、rebuild-export-<ts>.json
  wipe        换空库：config 补 bootstrap.admin（否则清库后无法登录）→ 停服 → 移走 db → 起服 → 断言空
  providers   按 sub2api 上游账号建 10 个供应商（codex 插件 / openai-chat / openai-responses）
  models      恢复对客模型 + 供应商模型 + 路由，并发现 azure 真实部署、补齐缺口
  tags        建三个标签（智天成 / 电商 / E26Q）与授权
  accounts    建 110 个账户并导入 128 把 key（含冲突重签与 E26Q 替换密钥重导）
  selftest    三个标签各一把自检 key 跑真实请求 + 负向 401
  verify      结构与授权核对（逐把比 prefix/hash、生效标签、分桶计数）
  report      写 /opt/aigw/data/sub2api-reimport-<ts>.json（0600，无密钥材料）
"""
import argparse
import glob
import hashlib
import http.cookiejar
import json
import os
import re
import secrets
import shutil
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field

GATEWAY = "http://127.0.0.1:8088/aigw"
PASSWORD_FILE = "/opt/aigw/.admin-password"
DB_PATH = "/opt/aigw/data/aigw.db"
CONFIG_PATH = "/opt/aigw/config.yaml"
DATA_DIR = "/opt/aigw/data"
DEPLOY_DIR = "/opt/aigw"
E26Q_REISSUE_FILE = "/opt/aigw/data/E26Q-reissue-key.txt"
SERVICE = "aigw"
PSQL = ["docker", "exec", "-i", "sub2api-postgres", "psql", "-U", "sub2api", "-d", "sub2api"]
FIELD_SEP = "\x1f"

E26Q_GROUP = "20"
ZHITIANCHENG_NOTE = "智天成"
POP_TAGS = ["智天成", "电商", "E26Q"]
TAG_GRANTS = {
    "智天成": {"providers": ["deepseek"], "models": ["*"]},
    "电商": {"providers": ["azure"], "models": ["*"]},
    "E26Q": {"providers": ["azure"], "models": ["*"]},
}
CODEX = "plugin:provider-codex"
# (sub2api 上游账号 id, provider 名, kind, 源账号状态为 error 时是否建为 disabled)
PROVIDER_SPECS = [
    (2, "lzhichao-lagenio-3-expiry", CODEX),
    (3, "lizhichao-wisskys-3-expiry", CODEX),
    (9, "liuhui-wisskys-8-expiry", CODEX),
    (6, "codex-zhuyecheng", CODEX),
    (4, "codex-funnywwh", CODEX),
    (1, "codex-libbygpt", CODEX),
    (7, "deepseek", "openai-chat"),
    # 供应商名只允许 [A-Za-z0-9._-]（网关校验），所以「deepseek电商」用拼音落名。
    (8, "deepseek-dianshang", "openai-chat"),
    (10, "azure", "openai-responses"),
    (11, "azure-2", "openai-responses"),
]
# 明确不建：antigravity 不是 OpenAI 系上游，aigw 没有对应供应商类型。
SKIP_SOURCE_ACCOUNTS = {5: "platform=antigravity（Google Cloud Code 系），aigw 无对应 kind"}
ACCOUNT_NOTE_PREFIX = "sub2api user #"
IMPORT_MARKER = "import:"
SELFTEST_ACCOUNT = "zz-rebuild-selftest"
SELFTEST_NOTE = "selftest account created by sub2api-reimport.py; safe to close"
AZURE_SOURCE_ACCOUNT_ID = 10  # sub2api 上游账号：azure「ChatGPT 官 key」
AZURE_POP_MODELS_FALLBACK = ["gpt-5.6-luna", "gpt-4o"]
AZURE_MIN_REQUESTS = 5
CANONICAL_MODEL_RE = re.compile(r"^[a-z0-9][a-z0-9.\-]*$")
# 电商/E26Q 人群用得最多的几个模型：它们在 azure 上必须真实可用，其余只作记录。
AZURE_REQUIRED_MODELS = ["gpt-5.5", "gpt-5.6-sol", "gpt-5.6-luna", "gpt-6-astra", "gpt-5.6-terra"]

# 密钥材料的粗筛：只挡「完整密钥」，不挡 12 字符前缀——前缀是索引不是密钥（迁移报告里本来就要
# 逐把列出它），而 aigw 自己签发的 key 也是 12 字符前缀 + 更长的主体。完整密钥一定长于 12 字符。
SECRET_RE = re.compile(
    r"(sk-[A-Za-z0-9_\-]{13,}|rt\.[A-Za-z0-9._\-]{8,}|eyJ[A-Za-z0-9._\-]{16,}"
    r"|aigw_mcp_[A-Za-z0-9_\-]{12,}|[0-9a-f]{32,})")


def redact(text) -> str:
    return SECRET_RE.sub("<redacted>", str(text))


def die(message: str) -> None:
    sys.exit("sub2api-reimport: " + redact(message))


def note(message: str) -> None:
    print(redact(message), flush=True)


def stamp() -> str:
    return time.strftime("%Y%m%d-%H%M%S")


# ---------------------------------------------------------------------------
# 源库（只读）与目标库（只读直读 SQLite，管理接口读不到 key_hash）
# ---------------------------------------------------------------------------


def psql(sql: str) -> list[list[str]]:
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
    if FIELD_SEP in value:
        die("内部错误：取值里有分隔符")
    return "'" + value.replace("'", "''") + "'"


@dataclass
class SrcUser:
    id: int
    username: str
    email: str
    notes: str
    status: str


@dataclass
class SrcKey:
    id: int
    user_id: int
    name: str
    group_id: str
    prefix: str
    khash: str
    last_used: str = ""


@dataclass
class SrcAccount:
    id: int
    name: str
    platform: str
    atype: str
    status: str
    credentials: dict = field(default_factory=dict)


@dataclass
class Source:
    users: list[SrcUser] = field(default_factory=list)
    keys: list[SrcKey] = field(default_factory=list)
    excluded: list[tuple] = field(default_factory=list)
    accounts: list[SrcAccount] = field(default_factory=list)
    shape_problems: list[str] = field(default_factory=list)


def load_source(with_accounts: bool = False) -> Source:
    src = Source()
    for row in psql(
        "SELECT id, username, COALESCE(email,''), COALESCE(notes,''), status FROM users "
        "WHERE deleted_at IS NULL ORDER BY id;"
    ):
        src.users.append(SrcUser(int(row[0]), row[1], row[2], row[3], row[4]))
    if not src.users:
        die("源库里没有存活用户")

    for row in psql(
        "SELECT k.id, k.user_id, k.name, COALESCE(k.group_id::text, ''), "
        "       left(btrim(k.key), 12), "
        "       encode(sha256(convert_to(btrim(k.key), 'UTF8')), 'hex'), "
        "       COALESCE(to_char(k.last_used_at, 'YYYY-MM-DD'), '') "
        "FROM users u JOIN api_keys k ON k.user_id = u.id "
        "WHERE u.deleted_at IS NULL AND k.deleted_at IS NULL AND k.status = 'active' "
        "ORDER BY k.id;"
    ):
        src.keys.append(SrcKey(int(row[0]), int(row[1]), row[2], row[3], row[4], row[5], row[6]))

    for row in psql(
        "SELECT k.id, k.user_id, k.name, k.status, (k.deleted_at IS NOT NULL)::text "
        "FROM users u JOIN api_keys k ON k.user_id = u.id "
        "WHERE u.deleted_at IS NULL AND (k.deleted_at IS NOT NULL OR k.status <> 'active') "
        "ORDER BY k.id;"
    ):
        src.excluded.append(tuple(row))

    checks = psql(
        "SELECT count(*) FILTER (WHERE k.key <> btrim(k.key)), "
        "       count(*) FILTER (WHERE k.key !~ '^[!-~]+$'), "
        "       count(*) FILTER (WHERE length(k.key) < 12) "
        "FROM users u JOIN api_keys k ON k.user_id = u.id "
        "WHERE u.deleted_at IS NULL AND k.deleted_at IS NULL AND k.status = 'active';"
    )[0]
    for label, count in zip(("含首尾空白的 key", "含非可打印字符的 key", "短于 12 字符的 key"), checks):
        if int(count) > 0:
            src.shape_problems.append(f"{label}: {count} 把")

    if with_accounts:
        for row in psql(
            "SELECT id, name, platform, type, status, credentials::text FROM accounts "
            "WHERE deleted_at IS NULL ORDER BY id;"
        ):
            try:
                creds = json.loads(row[5]) if row[5] else {}
            except json.JSONDecodeError:
                creds = {}
            src.accounts.append(SrcAccount(int(row[0]), row[1], row[2], row[3], row[4], creds))
    return src


def usage_models_by_pop(days: int) -> dict[str, list[tuple[str, int]]]:
    """近 N 天两个人群实际请求过的模型名（智天成 与 其余=电商+E26Q）。"""
    rows = psql(
        "SELECT CASE WHEN u.notes = " + sql_literal(ZHITIANCHENG_NOTE) +
        " AND u.id NOT IN (SELECT DISTINCT user_id FROM api_keys WHERE group_id = " + E26Q_GROUP + ")"
        "   THEN 'zhitiancheng' ELSE 'other' END AS pop, l.model, count(*) "
        "FROM usage_logs l JOIN users u ON u.id = l.user_id "
        "WHERE u.deleted_at IS NULL AND l.created_at > now() - interval '" + str(int(days)) + " days' "
        "GROUP BY 1, 2 ORDER BY 1, 3 DESC;"
    )
    out: dict[str, list[tuple[str, int]]] = {"zhitiancheng": [], "other": []}
    for row in rows:
        out.setdefault(row[0], []).append((row[1], int(row[2])))
    return out


def read_target_db(path: str) -> dict:
    if not os.path.exists(path):
        return {"keys": [], "accounts": [], "providers": [], "models": [], "tags": [],
                "provider_models": [], "routes": [], "counts": {}, "schema": 0}
    conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    try:
        out = {
            "keys": [{"id": r[0], "account_id": r[1], "name": r[2], "key_prefix": r[3],
                      "key_hash": r[4], "tags_json": r[5], "status": r[6], "created_by": r[7]}
                     for r in conn.execute(
                         "SELECT id, account_id, name, key_prefix, key_hash, tags_json, status, "
                         "created_by FROM api_keys ORDER BY id")],
            "accounts": [{"id": r[0], "name": r[1], "tags_json": r[2], "note": r[3], "status": r[4]}
                         for r in conn.execute(
                             "SELECT id, name, tags_json, note, status FROM accounts ORDER BY id")],
            "providers": [{"id": r[0], "name": r[1], "kind": r[2], "enabled": r[3], "meta_json": r[4]}
                          for r in conn.execute(
                              "SELECT id, name, kind, enabled, meta_json FROM providers ORDER BY id")],
            "models": [{"public_name": r[0], "aliases_json": r[1], "enabled": r[2]}
                       for r in conn.execute(
                           "SELECT public_name, aliases_json, enabled FROM models ORDER BY id")],
            "tags": [{"id": r[0], "name": r[1], "grants_json": r[2]}
                     for r in conn.execute("SELECT id, name, grants_json FROM tags ORDER BY id")],
            "provider_models": [{"provider_id": r[0], "public_model": r[1], "upstream_model": r[2],
                                 "enabled": r[3], "pricing_rules_json": r[4], "capabilities_json": r[5],
                                 "priority": r[6], "weight": r[7], "context_window": r[8],
                                 "max_output_tokens": r[9]}
                                for r in conn.execute(
                                    "SELECT provider_id, public_model, upstream_model, enabled, "
                                    "pricing_rules_json, capabilities_json, priority, weight, "
                                    "context_window, max_output_tokens FROM provider_models ORDER BY id")],
            "routes": [{"model_id": r[0], "provider_id": r[1], "upstream_model": r[2], "enabled": r[3],
                        "priority": r[4], "weight": r[5], "policy_json": r[6]}
                       for r in conn.execute(
                           "SELECT model_id, provider_id, upstream_model, enabled, priority, weight, "
                           "policy_json FROM routes ORDER BY id")],
            "schema": conn.execute("SELECT COALESCE(max(version), 0) FROM schema_migrations").fetchone()[0],
        }
        counts = {}
        for table in ("accounts", "api_keys", "providers", "models", "provider_models", "routes",
                      "tags", "admin_users", "request_logs", "usage_records", "mcp_tokens"):
            try:
                counts[table] = conn.execute(f"SELECT count(*) FROM {table}").fetchone()[0]
            except sqlite3.Error:
                counts[table] = -1
        out["counts"] = counts
        return out
    finally:
        conn.close()


def table_names(conn: sqlite3.Connection) -> list[str]:
    return [r[0] for r in conn.execute("SELECT name FROM sqlite_master WHERE type='table'")]


# ---------------------------------------------------------------------------
# 管理接口客户端
# ---------------------------------------------------------------------------


class GatewayError(Exception):
    pass


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

    def call(self, method: str, path: str, body=None, body_file: str = "", timeout: int = 180) -> dict:
        data = None
        headers = {}
        if body_file:
            with open(body_file, "rb") as fh:
                data = fh.read()
            headers["Content-Type"] = "application/json"
        elif body is not None:
            data = json.dumps(body, ensure_ascii=False).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(req, timeout=timeout) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                payload = json.loads(raw)
                message = payload.get("error", {}).get("message", raw[:200])
            except Exception:
                message = raw[:200]
            raise GatewayError(f"{method} {path} -> HTTP {exc.code}: {redact(message)}") from None
        except Exception as exc:  # noqa: BLE001
            raise GatewayError(f"{method} {path} -> {redact(exc)}") from None
        if not raw:
            return {}
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            raise GatewayError(f"{method} {path} -> 非 JSON 响应（{len(raw)} 字节）") from None

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


def with_secret_body(path_body: dict, gateway: Gateway, path: str, tmp_dir: str = DATA_DIR) -> dict:
    """POST a body that carries credentials: it crosses the wire from a 0600 file that is
    shredded immediately, so the secret never lands in argv, a log line or a traceback."""
    tmp = os.path.join(tmp_dir, ".reimport-body-%s.json" % secrets.token_hex(6))
    with open(tmp, "w") as fh:
        json.dump(path_body, fh, ensure_ascii=False)
    os.chmod(tmp, 0o600)
    try:
        return gateway.call("POST", path, body_file=tmp)
    finally:
        shred(tmp)


def shred(path: str) -> None:
    if not path or not os.path.exists(path):
        return
    try:
        size = os.path.getsize(path)
        with open(path, "r+b") as fh:
            fh.write(b"\0" * size)
            fh.flush()
            os.fsync(fh.fileno())
    except OSError:
        pass
    try:
        os.remove(path)
    except OSError:
        pass


# ---------------------------------------------------------------------------
# 人群与标签
# ---------------------------------------------------------------------------


def assign_tags(src: Source) -> dict[int, str]:
    e26q_users = {key.user_id for key in src.keys if key.group_id == E26Q_GROUP}
    out: dict[int, str] = {}
    for user in src.users:
        if user.id in e26q_users:
            out[user.id] = "E26Q"
        elif user.notes == ZHITIANCHENG_NOTE:
            out[user.id] = POP_TAGS[0]
        else:
            out[user.id] = "电商"
    return out


def duplicate_prefixes(keys: list[SrcKey]) -> dict[str, list[SrcKey]]:
    seen: dict[str, list[SrcKey]] = {}
    for key in keys:
        seen.setdefault(key.prefix, []).append(key)
    return {prefix: group for prefix, group in seen.items() if len(group) > 1}


def plan_reissues(src: Source, target: dict, export: dict | None = None) -> tuple[dict[int, int], list[str]]:
    """Return (被重签的源 key id -> 保留者 id, 说明行).

    12 字符前缀是数据面的查找入口且唯一，撞车时只能有一把保留原明文。保留谁不是随便定的：
    要保留**这台实例原本就持有该前缀的那把**——它是生产里正在用的那一把，换它会打断真实客户端。
    换库之后目标库是空的，所以这时必须看换库前拍下的素材（export）而不是现库；两者都没有时，
    才退化成「最近使用过的优先」。
    """
    holder_by_prefix: dict[str, str] = {}
    if export:
        for key in export.get("keys", []):
            holder = str(key.get("account") or "")
            prefix = str(key.get("prefix") or "")
            if holder and prefix:
                holder_by_prefix[prefix] = holder
    if not holder_by_prefix and target["keys"]:
        account_by_id = {a["id"]: a["name"] for a in target["accounts"]}
        for key in target["keys"]:
            holder = account_by_id.get(key["account_id"])
            if holder:
                holder_by_prefix[key["key_prefix"]] = holder

    lines: list[str] = []
    reissue: dict[int, int] = {}
    user_by_id = {u.id: u for u in src.users}
    for prefix, group in duplicate_prefixes(src.keys).items():
        holder = holder_by_prefix.get(prefix, "")
        keep = None
        for key in group:
            if user_by_id[key.user_id].username == holder:
                keep = key
                break
        if keep is None:
            keep = sorted(group, key=lambda k: (k.last_used, k.id), reverse=True)[0]
        for key in group:
            if key.id == keep.id:
                continue
            reissue[key.id] = keep.id
        ids = ", ".join(f"#{k.id}({user_by_id[k.user_id].username})" for k in group)
        lines.append(f"前缀冲突 {prefix}：{ids} → 保留 #{keep.id}"
                     f"（{user_by_id[keep.user_id].username}），重签 {sorted(reissue)}"
                     + (f"；aigw 当前持有者={holder}" if holder else "；aigw 当前未持有该前缀"))
    return reissue, lines


def read_e26q_replacement(path: str) -> tuple[str, str, str] | None:
    """Read the plaintext of the key this instance minted for the E26Q user on 2026-09-14.

    It is the only user-facing key that exists *only* inside aigw (the source system never had
    it), so a rebuild would silently break that client. The value never leaves this process.
    """
    if not os.path.exists(path):
        return None
    with open(path) as fh:
        lines = fh.read().splitlines()
    token = ""
    for line in lines:
        candidate = line.strip()
        if candidate.startswith("sk-") and " " not in candidate:
            token = candidate
            break
    if not token:
        return None
    return token, token[:12], hashlib.sha256(token.encode("utf-8")).hexdigest()


# ---------------------------------------------------------------------------
# 素材（snapshot 产出，providers/models/tags 复用）
# ---------------------------------------------------------------------------


def latest_export(explicit: str) -> str:
    if explicit:
        return explicit
    files = sorted(glob.glob(os.path.join(DATA_DIR, "rebuild-export-*.json")))
    if not files:
        die("找不到重建素材 rebuild-export-*.json：请先跑 snapshot（或用 --export 指定）")
    return files[-1]


def load_export(path: str) -> dict:
    with open(path) as fh:
        return json.load(fh)


def optional_export(explicit: str) -> dict | None:
    """The material a snapshot produced, or None when there is none yet (inventory runs first)."""
    path = explicit or ""
    if not path:
        files = sorted(glob.glob(os.path.join(DATA_DIR, "rebuild-export-*.json")))
        path = files[-1] if files else ""
    if not path or not os.path.exists(path):
        return None
    try:
        return load_export(path)
    except (OSError, json.JSONDecodeError):
        return None


# ---------------------------------------------------------------------------
# 断言
# ---------------------------------------------------------------------------


def preflight(src: Source, tags_by_user: dict[int, str], reissue: dict[int, int],
              target: dict, export: dict | None) -> tuple[list[str], list[str]]:
    """Return (必须解决的问题, 只作提示的观察项)."""
    problems: list[str] = []
    notes: list[str] = []
    problems.extend(src.shape_problems)

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
        if user.id not in tags_by_user:
            problems.append(f"源用户 {user.id} 没有标签归属")
        # 库非空时，同名账户必须是本工具建的（note 前缀），否则 upsert 会覆盖别人的账户。
        # 整库重建（先 wipe 再导）时这条只是提示：换库后所有账户都不在了。
        account = next((a for a in target["accounts"] if a["name"] == user.username), None)
        if account is not None:
            note_text = str(account["note"])
            if not note_text.startswith(ACCOUNT_NOTE_PREFIX) and not note_text.startswith(SELFTEST_NOTE):
                notes.append(f"目标账户 {user.username} 已存在且不是本工具创建的（note={note_text!r}）："
                             f"整库重建会先换库，因此不阻塞；增量重导则要先处理")

    # 标签授权指向的供应商必须在本方案的建表里
    planned = {spec[1] for spec in PROVIDER_SPECS}
    for tag, grants in TAG_GRANTS.items():
        for provider in grants["providers"]:
            if provider not in planned:
                problems.append(f"标签 {tag} 授权的供应商 {provider} 不在本次要建的供应商里")
        if not grants.get("models"):
            problems.append(f"标签 {tag} 没有授权任何模型：绑定的 key 每个请求都会 403")

    return problems, notes


def model_coverage(pop_usage: dict[str, list[tuple[str, int]]], known: set[str]) -> list[tuple[str, str, int]]:
    out = []
    for pop, rows in pop_usage.items():
        for model, count in rows:
            if model not in known:
                out.append((pop, model, count))
    return out


# ---------------------------------------------------------------------------
# 子命令
# ---------------------------------------------------------------------------


def cmd_inventory(args, gw: Gateway) -> int:
    src = load_source(with_accounts=True)
    target = read_target_db(args.db)
    tags_by_user = assign_tags(src)
    export = optional_export(args.export)
    reissue, conflict_lines = plan_reissues(src, target, export)

    pop_usage = usage_models_by_pop(args.coverage_days)
    known: set[str] = set()
    source_of_models = export["models"] if export else target["models"]
    for row in source_of_models:
        known.add(row["public_name"])
        try:
            for alias in json.loads(row.get("aliases_json") or "[]"):
                known.add(alias)
        except Exception:  # noqa: BLE001
            pass
    known.discard("")
    if export is None:
        note("（还没有重建素材 rebuild-export-*.json：先跑 snapshot；providers/models 需要它）")

    user_by_id = {u.id: u for u in src.users}
    buckets: dict[str, dict[str, int]] = {tag: {"users": 0, "keys": 0} for tag in POP_TAGS}
    for user in src.users:
        buckets[tags_by_user[user.id]]["users"] += 1
    for key in src.keys:
        buckets[tags_by_user[key.user_id]]["keys"] += 1

    note("")
    note("源数据集（sub2api，只读）")
    note(f"  存活用户 {len(src.users)} 个；活跃 key {len(src.keys)} 把（排除 {len(src.excluded)} 把："
         f"已删除或非 active）")
    note(f"  上游账号 {len(src.accounts)} 个："
         + "、".join(f"#{a.id} {a.name}({a.atype},{a.status})" for a in src.accounts))
    note("")
    note("人群 → 标签")
    for tag in POP_TAGS:
        grants = TAG_GRANTS[tag]
        note(f"  {tag:<8} 用户 {buckets[tag]['users']:>4} 个 / key {buckets[tag]['keys']:>4} 把"
             f" → 供应商 {grants['providers']}，模型 {grants['models']}")

    note("")
    note("目标实例现状（aigw，只读）")
    counts = target["counts"]
    note(f"  schema={target['schema']} " + " ".join(f"{k}={v}" for k, v in counts.items()))
    note(f"  /version: {args.version_note}")
    note(f"  供应商：" + "、".join(f"#{p['id']} {p['name']}({p['kind']},enabled={p['enabled']})"
                                  for p in target["providers"]))
    note(f"  标签：" + "、".join(f"{t['name']}" for t in target["tags"]))

    note("")
    note("前缀冲突")
    for line in conflict_lines:
        note("  " + line)
    if not conflict_lines:
        note("  无")
    for key_id, keep_id in sorted(reissue.items()):
        key = next(k for k in src.keys if k.id == key_id)
        keep = next(k for k in src.keys if k.id == keep_id)
        note(f"  → 源 key #{key.id}（{user_by_id[key.user_id].username}，{key.name}）不导入，"
             f"改由控制台重签；保留 #{keep.id}（{user_by_id[keep.user_id].username}）")

    note("")
    e26q = read_e26q_replacement(E26Q_REISSUE_FILE)
    if e26q:
        note(f"E26Q 替换密钥：{E26Q_REISSUE_FILE} 存在（前缀 {e26q[1]}），会原样重导到账户 E26Q")
    else:
        note(f"E26Q 替换密钥：{E26Q_REISSUE_FILE} 不存在——该用户可能需要在控制台重新取一把 key")

    note("")
    note(f"模型覆盖（近 {args.coverage_days} 天源库用量，目标侧还没有的名字将按 §4.2 补齐）")
    gaps = model_coverage(pop_usage, known)
    for pop, model, count in gaps[:30]:
        note(f"  待补 {model}（{pop}，{count} 次）")
    if not gaps:
        note("  全部命中")
    note("  人群用量前几名：")
    for pop in ("zhitiancheng", "other"):
        top = ", ".join(f"{m}={c}" for m, c in pop_usage.get(pop, [])[:6])
        note(f"    {pop}: {top}")

    problems, observations = preflight(src, tags_by_user, reissue, target, export)
    # 环境断言
    free = shutil.disk_usage(DEPLOY_DIR).free
    if free < 3 * 1024 ** 3:
        problems.append(f"磁盘余量不足 3 GB（当前 {free / 1024 ** 3:.1f} GB）")
    live_specs = {a.id for a in src.accounts}
    for spec in PROVIDER_SPECS:
        if spec[0] not in live_specs:
            observations.append(f"源账号 #{spec[0]}（{spec[1]}）已删除：该供应商不建")
        if spec[2] != CODEX:
            continue
        state = os.path.join(DATA_DIR, "plugin-state", spec[1])
        if spec[1] in ("lzhichao-lagenio-3-expiry", "lizhichao-wisskys-3-expiry",
                       "liuhui-wisskys-8-expiry") \
                and spec[0] in live_specs and not os.path.isdir(state):
            problems.append(f"codex 供应商 {spec[1]} 的 plugin-state 目录缺失：{state}")
    for source_id in SKIP_SOURCE_ACCOUNTS:
        if source_id in live_specs:
            problems.append(f"源账号 #{source_id} 仍在源库里，但本方案明确不建它对应的供应商")
    if not os.path.exists(args.db):
        problems.append(f"找不到目标数据库 {args.db}")
    if target["schema"] and target["schema"] < 26:
        problems.append(f"目标库 schema={target['schema']}，低于预期（线上应为 27）")

    note("")
    for observation in observations:
        note("  ! " + observation)
    if problems:
        note("预检未通过：")
        for problem in problems:
            note("  ✗ " + problem)
    else:
        note("预检全部通过。")

    path = os.path.join(DATA_DIR, f"sub2api-reimport-plan-{stamp()}.json")
    payload = {
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "source": {"users": len(src.users), "keys": len(src.keys), "excluded": len(src.excluded),
                   "accounts": [{"id": a.id, "name": a.name, "type": a.atype, "status": a.status}
                                for a in src.accounts]},
        "buckets": buckets,
        "tags": {tag: TAG_GRANTS[tag] for tag in POP_TAGS},
        "providers_planned": [{"source_account_id": s[0], "name": s[1], "kind": s[2],
                               "source_present": s[0] in live_specs}
                              for s in PROVIDER_SPECS],
        "providers_skipped": SKIP_SOURCE_ACCOUNTS,
        "observations": observations,
        "problems": problems,
        "conflicts": conflict_lines,
        "reissue_source_key_ids": sorted(reissue),
        "target_before": {"schema": target["schema"], "counts": target["counts"]},
        "model_gaps": [{"pop": p, "model": m, "requests": c} for p, m, c in gaps],
        "assignments": [
            {"source_key_id": k.id, "source_user_id": k.user_id,
             "source_username": user_by_id[k.user_id].username, "key_name": k.name,
             "prefix": k.prefix, "tag": tags_by_user[k.user_id],
             "reissue": k.id in reissue} for k in src.keys],
    }
    with open(path, "w") as fh:
        json.dump(payload, fh, ensure_ascii=False, indent=2)
    os.chmod(path, 0o600)
    note(f"计划：{path}（0600）")
    return 1 if problems else 0


def cmd_snapshot(args, gw: Gateway) -> int:
    ts = stamp()
    dest_db = f"{args.db}.pre-rebuild-{ts}"
    dest_cfg = f"{args.config}.pre-rebuild-{ts}"
    src = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    dst = sqlite3.connect(dest_db)
    try:
        with dst:
            src.backup(dst)
        check = dst.execute("PRAGMA quick_check").fetchone()[0]
    finally:
        dst.close()
        src.close()
    os.chmod(dest_db, 0o600)
    if check != "ok":
        die(f"DB 快照自检失败：{check}")
    note(f"DB 快照：{dest_db}（0600，quick_check=ok）")

    if not os.path.exists(args.config):
        die(f"找不到 {args.config}")
    shutil.copy2(args.config, dest_cfg)
    os.chmod(dest_cfg, 0o600)
    note(f"配置备份：{dest_cfg}（0600）")

    conn = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    try:
        provider_name = {r[0]: r[1] for r in conn.execute("SELECT id, name FROM providers")}
        model_name = {r[0]: r[1] for r in conn.execute("SELECT id, public_name FROM models")}
        export = {
            "generated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
            "db": args.db,
            "schema": conn.execute("SELECT COALESCE(max(version),0) FROM schema_migrations").fetchone()[0],
            "providers": [
                {"name": r[0], "kind": r[1], "display_name": r[2], "config_json": r[3], "meta_json": r[4],
                 "enabled": r[5], "priority": r[6], "weight": r[7], "max_inflight": r[8],
                 "degradation": r[9]}
                for r in conn.execute(
                    "SELECT name, kind, display_name, config_json, meta_json, enabled, priority, "
                    "weight, max_inflight, degradation FROM providers ORDER BY id")],
            "models": [
                {"public_name": r[0], "display_name": r[1], "aliases_json": r[2], "enabled": r[3],
                 "sale_pricing_json": r[4], "policy_json": r[5], "reasoning_json": r[6]}
                for r in conn.execute(
                    "SELECT public_name, display_name, aliases_json, enabled, sale_pricing_json, "
                    "policy_json, reasoning_json FROM models ORDER BY id")],
            "provider_models": [
                {"provider": provider_name.get(r[0], ""), "public_model": r[1], "upstream_model": r[2],
                 "enabled": r[3], "priority": r[4], "weight": r[5], "context_window": r[6],
                 "max_output_tokens": r[7], "pricing_rules_json": r[8], "capabilities_json": r[9],
                 "capabilities_override": r[10]}
                for r in conn.execute(
                    "SELECT provider_id, public_model, upstream_model, enabled, priority, weight, "
                    "context_window, max_output_tokens, pricing_rules_json, capabilities_json, "
                    "capabilities_override FROM provider_models ORDER BY id")],
            "routes": [
                {"model": model_name.get(r[0], ""), "provider": provider_name.get(r[1], ""),
                 "upstream_model": r[2], "enabled": r[3], "priority": r[4], "weight": r[5],
                 "policy_json": r[6]}
                for r in conn.execute(
                    "SELECT model_id, provider_id, upstream_model, enabled, priority, weight, "
                    "policy_json FROM routes ORDER BY id")],
            "tags": [{"name": r[0], "grants_json": r[1], "policy_json": r[2], "priority": r[3]}
                     for r in conn.execute(
                         "SELECT name, grants_json, policy_json, priority FROM tags ORDER BY id")],
            "accounts": [{"name": r[0], "tags_json": r[1], "note": r[2], "status": r[3]}
                         for r in conn.execute("SELECT name, tags_json, note, status FROM accounts "
                                               "ORDER BY id")],
            "keys": [{"prefix": r[0], "account": r[1], "name": r[2], "status": r[3],
                      "created_by": r[4]}
                     for r in conn.execute(
                         "SELECT k.key_prefix, a.name, k.name, k.status, k.created_by "
                         "FROM api_keys k JOIN accounts a ON a.id = k.account_id ORDER BY k.id")],
        }
    finally:
        conn.close()
    export_path = os.path.join(DATA_DIR, f"rebuild-export-{ts}.json")
    with open(export_path, "w") as fh:
        json.dump(export, fh, ensure_ascii=False, indent=2)
    os.chmod(export_path, 0o600)
    note(f"重建素材：{export_path}（0600，"
         f"{len(export['models'])} 模型 / {len(export['provider_models'])} 供应商模型 / "
         f"{len(export['routes'])} 路由 / {len(export['tags'])} 标签 / {len(export['providers'])} 供应商）")
    note(f"回滚 = cp {dest_db} {args.db}（并删 -wal/-shm）+ systemctl restart {SERVICE}")
    return 0


def cmd_wipe(args, gw: Gateway) -> int:
    target = read_target_db(args.db)
    counts = target["counts"]
    if not args.force:
        blocking = {k: v for k, v in counts.items()
                    if k in ("accounts", "api_keys", "providers", "models", "routes", "tags") and v}
        if not blocking:
            note("库已是空的（没有账户/key/供应商/模型/路由/标签）：跳过换库，只核对服务状态")
            return check_after_wipe(gw)

    cfg_text = ""
    if os.path.exists(args.config):
        with open(args.config) as fh:
            cfg_text = fh.read()
    if args.config_bootstrap:
        with open(args.password_file) as fh:
            password = fh.read().strip()
        if password and not re.search(r"^bootstrap:", cfg_text, re.M):
            block = ("\n# 清库重建（sub2api-reimport）：清空数据库后管理员只能由 bootstrap.admin 种回，\n"
                     "# 否则管理接口无人可登录。口令与 .admin-password 同一份，文件模式保持 0600。\n"
                     "bootstrap:\n  mode: off\n  admin:\n    username: admin\n"
                     f"    password: {json.dumps(password)}\n")
            with open(args.config, "a") as fh:
                fh.write(block)
            note("配置已补 bootstrap.admin（口令沿用 /opt/aigw/.admin-password；mode=off 不种任何业务数据）")
        elif not password:
            die("找不到管理口令，无法补 bootstrap.admin")
        else:
            note("配置里已有 bootstrap 段：保持不变")
    else:
        note("--config-bootstrap=false：不动配置（仅在库里已有管理员时才安全）")

    note(f"停服务：systemctl stop {SERVICE}")
    subprocess.run(["systemctl", "stop", SERVICE], check=True)
    ts = stamp()
    moved = []
    for suffix in ("", "-wal", "-shm"):
        path = args.db + suffix
        if os.path.exists(path):
            dest = f"{path}.pre-rebuild-{ts}"
            os.rename(path, dest)
            moved.append(f"{os.path.basename(path)} -> {os.path.basename(dest)}")
    note("移走的库文件：" + "，".join(moved) if moved else "（原本就没有库文件）")

    subprocess.run(["systemctl", "start", SERVICE], check=True)
    note(f"起服务：systemctl start {SERVICE}")
    if not wait_ready(gw, seconds=args.wait):
        die("服务在 %ds 内没有就绪：请查 journalctl -u %s" % (args.wait, SERVICE))
    return check_after_wipe(gw)


def wait_ready(gw: Gateway, seconds: int = 90) -> bool:
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            page = gw.call("GET", "/version", timeout=5)
            if page.get("version"):
                return True
        except GatewayError:
            pass
        time.sleep(2)
    return False


def check_after_wipe(gw: Gateway) -> int:
    page = gw.call("GET", "/version")
    note(f"/version = {json.dumps(page, ensure_ascii=False)}")
    target = read_target_db(DB_PATH)
    counts = target["counts"]
    note(f"schema={target['schema']} " + " ".join(f"{k}={v}" for k, v in counts.items()))
    empties = {k: v for k, v in counts.items()
               if k in ("accounts", "api_keys", "providers", "models", "provider_models", "routes", "tags")}
    problems = []
    for table, value in empties.items():
        if value != 0:
            problems.append(f"{table}={value}，预期 0")
    if counts.get("admin_users", 0) < 1:
        problems.append("admin_users=0：没有管理员可登录，检查 config.yaml 的 bootstrap.admin")
    if target["schema"] != 27:
        problems.append(f"schema={target['schema']}，预期 27（迁移未跑完？）")
    if problems:
        for problem in problems:
            note("  ✗ " + problem)
        return 1
    note("空库断言通过：业务表全 0，管理员已就位。")
    return 0


def codex_config(export: dict) -> dict:
    for provider in export["providers"]:
        if provider["kind"] == CODEX:
            try:
                return json.loads(provider["config_json"] or "{}")
            except json.JSONDecodeError:
                break
    die("重建素材里没有 codex 供应商的 config：无法复用模型目录")


def chat_config(export: dict) -> dict:
    for provider in export["providers"]:
        if provider["kind"] == "openai-chat":
            try:
                return json.loads(provider["config_json"] or "{}")
            except json.JSONDecodeError:
                break
    die("重建素材里没有 openai-chat 供应商的 config")


def cmd_providers(args, gw: Gateway) -> int:
    export = load_export(latest_export(args.export))
    src = load_source(with_accounts=True)
    accounts = {a.id: a for a in src.accounts}
    codex_cfg = codex_config(export)
    chat_cfg = chat_config(export)

    created = []
    for source_id, name, kind in PROVIDER_SPECS:
        account = accounts.get(source_id)
        if account is None:
            note(f"  ! 源账号 #{source_id}（{name}）不存在或已删除：跳过")
            continue
        if kind == CODEX:
            creds = {k: v for k, v in {
                "refresh_token": account.credentials.get("refresh_token"),
                "access_token": account.credentials.get("access_token"),
                "account_id": account.credentials.get("chatgpt_account_id"),
                "client_id": account.credentials.get("client_id"),
            }.items() if v}
            config = dict(codex_cfg)
            display = (f"ChatGPT/Codex 订阅账号 {account.credentials.get('email', '')} "
                       f"({account.credentials.get('plan_type', '')})").strip()
            enabled = account.status != "error"
        elif kind == "openai-chat":
            creds = {"api_key": account.credentials.get("api_key", "")}
            config = dict(chat_cfg)
            config["base_url"] = account.credentials.get("base_url", config.get("base_url", ""))
            display = f"{account.name}（{account.credentials.get('base_url', '')}）"
            enabled = True
        else:
            creds = {"api_key": account.credentials.get("api_key", "")}
            config = {"base_url": account.credentials.get("base_url", "")}
            display = f"{account.name}（{account.credentials.get('base_url', '')}）"
            enabled = True
        if not creds or not all(creds.values()):
            note(f"  ! 源账号 #{source_id}（{name}）凭据不完整：跳过（{sorted(creds)}）")
            continue

        body = {
            "name": name, "kind": kind, "display_name": display, "enabled": enabled,
            "priority": 50, "weight": 100,
            "config": config,
            "credentials": creds,
            "meta": {"source": "gptjp/sub2api", "source_account_id": source_id,
                     "source_name": account.name, "source_status": account.status,
                     "source_type": account.atype, "reimport": time.strftime("%Y-%m-%d")},
        }
        result = with_secret_body(body, gw, "/admin/api/v1/providers")
        created.append((name, result.get("id"), result.get("credential_keys"), result.get("enabled")))
        note(f"  供应商 {name}（id={result.get('id')}，kind={kind}，enabled={result.get('enabled')}，"
             f"凭据字段={result.get('credential_keys')}）")

    for source_id, why in SKIP_SOURCE_ACCOUNTS.items():
        note(f"  跳过源账号 #{source_id}：{why}")
    note(f"供应商就位：{len(created)} 个（不调用 set_token，不主动刷新任何 OAuth 凭据）")
    return 0


def cmd_models(args, gw: Gateway) -> int:
    export = load_export(latest_export(args.export))
    providers = {p["name"]: p for p in gw.list_all("/admin/api/v1/providers")}
    provider_id = {name: row["id"] for name, row in providers.items()}

    # 先问上游有哪些部署：这一步必须在恢复素材之前做，否则读回来的目录里混着我们自己刚写的行，
    # 就分不清「上游真的有」和「我们刚写进去的」。
    azure_probe = probe_azure_upstream(gw, provider_id)
    note(f"azure 上游目录：发现 {len(azure_probe['discovered'])} 个部署名"
         + (f"，读取失败：{azure_probe.get('catalog_error')}" if azure_probe.get("catalog_error") else ""))

    models = {row["public_name"] for row in gw.list_all("/admin/api/v1/models")}
    created_models = 0
    for row in export["models"]:
        if row["public_name"] in models:
            continue
        body = {"public_name": row["public_name"], "display_name": row["display_name"],
                "enabled": bool(row["enabled"])}
        try:
            if row.get("aliases_json"):
                body["aliases"] = json.loads(row["aliases_json"])
        except json.JSONDecodeError:
            pass
        if row.get("sale_pricing_json"):
            try:
                body["sale_pricing"] = json.loads(row["sale_pricing_json"])
            except json.JSONDecodeError:
                pass
        if row.get("policy_json"):
            try:
                body["policy"] = json.loads(row["policy_json"])
            except json.JSONDecodeError:
                pass
        if row.get("reasoning_json"):
            try:
                body["reasoning"] = json.loads(row["reasoning_json"])
            except json.JSONDecodeError:
                pass
        gw.call("POST", "/admin/api/v1/models", body)
        models.add(row["public_name"])
        created_models += 1
    note(f"对客模型：新增 {created_models} 个，库内共 {len(models)} 个")

    pm_by_provider: dict[str, list[dict]] = {}
    for row in export["provider_models"]:
        pm_by_provider.setdefault(row["provider"], []).append(row)
    created_pm = skipped_pm = 0
    for name, rows in pm_by_provider.items():
        pid = provider_id.get(name)
        if pid is None:
            note(f"  ! 素材里的供应商 {name} 不在新库里：其 {len(rows)} 条供应商模型未恢复")
            continue
        for row in rows:
            body = {"public_model": row["public_model"], "upstream_model": row["upstream_model"],
                    "enabled": bool(row["enabled"]), "priority": row["priority"],
                    "weight": row["weight"], "context_window": row["context_window"],
                    "max_output_tokens": row["max_output_tokens"]}
            if row.get("capabilities_json"):
                try:
                    body["capabilities"] = json.loads(row["capabilities_json"])
                except json.JSONDecodeError:
                    pass
            if row.get("capabilities_override"):
                body["capabilities_override"] = row["capabilities_override"]
            if row.get("pricing_rules_json"):
                try:
                    body["pricing_rules"] = json.loads(row["pricing_rules_json"])
                except json.JSONDecodeError:
                    pass
            gw.call("POST", f"/admin/api/v1/providers/{pid}/models", body)
            created_pm += 1
    note(f"供应商模型：恢复 {created_pm} 条（跳过 {skipped_pm}）")

    created_routes = 0
    for row in export["routes"]:
        if row["model"] not in models or row["provider"] not in provider_id:
            continue
        body = {"model": row["model"], "provider": row["provider"],
                "upstream_model": row["upstream_model"], "enabled": bool(row["enabled"]),
                "priority": row["priority"], "weight": row["weight"]}
        if row.get("policy_json"):
            try:
                body["policy"] = json.loads(row["policy_json"])
            except json.JSONDecodeError:
                pass
        gw.call("POST", "/admin/api/v1/routes", body)
        created_routes += 1
    note(f"路由：恢复 {created_routes} 条")

    azure_gaps = map_azure_models(gw, args, provider_id, azure_probe)
    note(f"azure 覆盖：{len(azure_gaps['mapped'])} 个模型已映射，缺口 {len(azure_gaps['missing'])} 个"
         f"（明细见 report）")
    if azure_gaps.get("created_catalog"):
        note("  为它们补了对客模型目录项（定价随后由 official-pricing.sh 写）："
             + "、".join(azure_gaps["created_catalog"]))
    if azure_gaps.get("unverified"):
        note(f"  ! 上游目录不可得（{azure_gaps.get('catalog_error') or '目录为空'}），"
             f"以下 {len(azure_gaps['unverified'])} 个映射尚未被真实请求证实："
             + "、".join(azure_gaps["unverified"]))
    if azure_gaps.get("removed_stray"):
        note("  清理了误建的孤立上游模型行：" + "、".join(azure_gaps["removed_stray"]))
    for model in azure_gaps["mapped"]:
        note(f"  ✓ azure ← {model}")
    for model, reason in azure_gaps["missing"][:20]:
        note(f"  ✗ azure 缺 {model}：{reason}")
    path = os.path.join(DATA_DIR, f"azure-coverage-{stamp()}.json")
    with open(path, "w") as fh:
        json.dump(azure_gaps, fh, ensure_ascii=False, indent=2)
    os.chmod(path, 0o600)
    note(f"azure 覆盖明细：{path}（0600）")

    # 路由核对：每条「模型 + 供应商」都要能被路由解释器解析出至少一个候选。
    pairs = {(row["model"], row["provider"]) for row in export["routes"]}
    for model in azure_gaps["mapped"]:
        pairs.add((model, azure_gaps["provider"]))
    bad: list[tuple[str, str, str]] = []
    for model, provider in sorted(pairs):
        query = (f"/admin/api/v1/router/explain?model={urllib.parse.quote(model)}"
                 f"&provider={urllib.parse.quote(provider)}")
        try:
            page = gw.call("GET", query)
        except GatewayError as exc:
            bad.append((model, provider, redact(exc)))
            continue
        if not page.get("order"):
            failure = page.get("failure") or page.get("excluded") or "无候选"
            bad.append((model, provider, redact(json.dumps(failure, ensure_ascii=False))[:160]))
    if bad:
        note("以下「模型 + 供应商」当前解析不出候选：")
        for model, provider, why in bad[:30]:
            note(f"  - {model} @ {provider}: {why}")
    else:
        note(f"路由解释器：{len(pairs)} 条「模型 + 供应商」全部能解析出候选")
    return 0


def wanted_azure_models(args) -> list[str]:
    """The model names the 电商/E26Q population actually asks for, plus the two known-good ones.

    Three filters, all deliberate:
      * 音频转写端点（`*-transcribe`）不建：网关服务的是 /responses，不是转写接口；
      * 非规范 id（大写、Unicode 破折号等，通常是客户端拼错）不建；
      * 近 N 天少于 5 次的偶发名字不建——把偶发拼写变成目录项只会制造无路由的死名字。
    """
    pop_usage = usage_models_by_pop(args.coverage_days)
    wanted: list[str] = []
    for model, count in pop_usage.get("other", []):
        if count < AZURE_MIN_REQUESTS or model.endswith("transcribe") or not CANONICAL_MODEL_RE.match(model):
            continue
        if model not in wanted:
            wanted.append(model)
    for model in AZURE_POP_MODELS_FALLBACK:
        if model not in wanted:
            wanted.append(model)
    return wanted


def probe_azure_upstream(gw: Gateway, provider_id: dict[str, int]) -> dict:
    """Find out which deployments the azure upstream really has.

    The gateway's own `models/refresh` does not work for this provider (it answers 0), so the
    catalogue is read straight from the upstream with the same api key the provider uses:
    `GET <base_url>/models` with the `api-key` header. Read-only, and the key never leaves the
    process (it is never printed, and error text goes through redact()).
    """
    azure_name = TAG_GRANTS["电商"]["providers"][0]
    pid = provider_id.get(azure_name)
    result = {"provider": azure_name, "discovered": [], "canonical": {}, "refresh_error": "",
              "catalog_error": ""}
    if pid is None:
        result["catalog_error"] = f"供应商 {azure_name} 不存在"
        return result

    rows = psql("SELECT credentials::text FROM accounts WHERE id = " + str(AZURE_SOURCE_ACCOUNT_ID) +
                " AND deleted_at IS NULL;")
    if not rows:
        result["catalog_error"] = f"源库里没有上游账号 #{AZURE_SOURCE_ACCOUNT_ID}"
        return result
    try:
        creds = json.loads(rows[0][0])
    except json.JSONDecodeError:
        result["catalog_error"] = "源账号凭据不是合法 JSON"
        return result
    base_url = str(creds.get("base_url") or "").rstrip("/")
    api_key = str(creds.get("api_key") or "")
    if not base_url or not api_key:
        result["catalog_error"] = "源账号缺少 base_url 或 api_key"
        return result

    request = urllib.request.Request(base_url + "/models",
                                     headers={"api-key": api_key, "Accept": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=30) as resp:
            payload = json.loads(resp.read().decode("utf-8", "replace"))
        ids = sorted({str(item.get("id") or item.get("model") or "")
                      for item in (payload.get("data") or [])} - {""})
        result["discovered"] = ids
        result["canonical"] = {name.lower(): name for name in ids}
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", "replace")
        result["catalog_error"] = f"HTTP {exc.code}: {redact(body)[:200]}"
    except Exception as exc:  # noqa: BLE001
        result["catalog_error"] = redact(exc)
    return result


def map_azure_models(gw: Gateway, args, provider_id: dict[str, int], probe: dict) -> dict:
    """Map the models the 电商/E26Q population needs onto azure. Nothing is guessed: a name the
    upstream catalogue does not have is reported as a gap instead of being written as a route
    that would fail at request time. The upstream spelling from the catalogue wins, because that
    is the deployment name the request has to carry."""
    azure_name = TAG_GRANTS["电商"]["providers"][0]
    pid = provider_id.get(azure_name)
    wanted = wanted_azure_models(args)
    discovered = list(probe.get("discovered") or [])
    canonical = dict(probe.get("canonical") or {})
    result = {"provider": azure_name, "wanted": wanted, "discovered_count": len(discovered),
              "catalog_error": probe.get("catalog_error", ""), "mapped": [], "missing": [],
              "unverified": [], "upstream_names": {}}
    if pid is None:
        result["missing"] = [[m, f"供应商 {azure_name} 不存在"] for m in wanted]
        return result

    existing = set()
    try:
        existing = {str(r.get("public_model") or "")
                    for r in gw.list_all(f"/admin/api/v1/providers/{pid}/models")}
    except GatewayError as exc:
        result["list_error"] = redact(exc)

    catalog = {row["public_name"] for row in gw.list_all("/admin/api/v1/models")}
    created_catalog = []
    for model in wanted:
        upstream = canonical.get(model.lower(), "")
        if canonical and not upstream:
            result["missing"].append([model, "上游部署目录里没有这个名字"])
            continue
        if not canonical:
            upstream = model
            result["unverified"].append(model)
        try:
            # 路由要求对客模型已存在：这些名字是 sub2api 真在用、而 aigw 目录里没有的，先补目录项
            # （定价由 official-pricing.sh 按上游 id 随后写入，这里不猜价）。
            if model not in catalog:
                gw.call("POST", "/admin/api/v1/models",
                        {"public_name": model, "display_name": model, "enabled": True})
                catalog.add(model)
                created_catalog.append(model)
            if model not in existing:
                gw.call("POST", f"/admin/api/v1/providers/{pid}/models",
                        {"public_model": model, "upstream_model": upstream, "enabled": True,
                         "priority": 100, "weight": 100})
                existing.add(model)
            else:
                gw.call("POST", f"/admin/api/v1/providers/{pid}/models",
                        {"public_model": model, "upstream_model": upstream, "enabled": True})
            gw.call("POST", "/admin/api/v1/routes",
                    {"model": model, "provider": azure_name, "upstream_model": upstream,
                     "enabled": True, "priority": 100, "weight": 100})
            result["mapped"].append(model)
            result["upstream_names"][model] = upstream
        except GatewayError as exc:
            result["missing"].append([model, redact(exc)])
    result["created_catalog"] = created_catalog

    # 清掉「上游模型行还在、但这一轮没有为它建路由」的行（例如部署目录里根本没有的名字）：
    # 留着它只会让控制台显示一个永远用不了的上游模型。
    keep = set(result["mapped"])
    stray = []
    try:
        rows = gw.list_all(f"/admin/api/v1/providers/{pid}/models")
        for row in rows:
            name = str(row.get("public_model") or "")
            if name and name not in keep:
                gw.call("DELETE", f"/admin/api/v1/provider-models/{row.get('id')}")
                stray.append(name)
    except GatewayError as exc:
        result["cleanup_error"] = redact(exc)
    result["removed_stray"] = stray
    return result


def cmd_tags(args, gw: Gateway) -> int:
    for tag in POP_TAGS:
        result = gw.call("POST", "/admin/api/v1/tags",
                         {"name": tag, "description": "sub2api-reimport 人群标签",
                          "grants": TAG_GRANTS[tag], "priority": 100})
        note(f"  标签 {tag}：grants={json.dumps(TAG_GRANTS[tag], ensure_ascii=False)}"
             f"（id={result.get('id')}）")
    note(f"标签就位：{len(POP_TAGS)} 个")
    return 0


def cmd_accounts(args, gw: Gateway) -> int:
    src = load_source()
    tags_by_user = assign_tags(src)
    target = read_target_db(args.db)
    reissue, _lines = plan_reissues(src, target, optional_export(args.export))
    user_by_id = {u.id: u for u in src.users}
    tag_names = {tag["name"] for tag in gw.list_all("/admin/api/v1/tags")}
    for tag in POP_TAGS:
        if tag not in tag_names:
            die(f"标签 {tag} 不存在：先跑 tags")

    accounts = {a["name"]: a for a in gw.list_all("/admin/api/v1/accounts")}
    created = patched = 0
    for user in src.users:
        tag = tags_by_user[user.id]
        body = {"name": user.username, "billing_mode": "postpaid", "status": "active",
                "tags": [tag],
                "note": f"{ACCOUNT_NOTE_PREFIX}{user.id} · {user.email} · 备注:{user.notes}"}
        if user.username in accounts:
            gw.call("PATCH", f"/admin/api/v1/accounts/{accounts[user.username]['id']}",
                    {"tags": [tag], "status": "active", "note": body["note"],
                     "billing_mode": "postpaid"})
            patched += 1
        else:
            accounts[user.username] = gw.call("POST", "/admin/api/v1/accounts", body)
            created += 1
    note(f"账户：新建 {created} 个、更新 {patched} 个（共 {len(accounts)} 个）")

    imported = updated = 0
    for key in src.keys:
        user = user_by_id[key.user_id]
        account = accounts.get(user.username)
        if account is None:
            die(f"账户 {user.username} 未就位")
        if key.id in reissue:
            continue
        result = gw.call("POST", "/admin/api/v1/keys/import", {
            "account_id": account["id"], "name": key.name, "key_prefix": key.prefix,
            "key_hash": key.khash, "tags": [], "status": "active"})
        if result.get("created"):
            imported += 1
        else:
            updated += 1
    note(f"key：导入 {imported} 把、更新 {updated} 把（重签占位 {len(reissue)} 把未导入）")

    reissue_file = ""
    if reissue:
        lines = []
        for key_id, keep_id in sorted(reissue.items()):
            key = next(k for k in src.keys if k.id == key_id)
            keep = next(k for k in src.keys if k.id == keep_id)
            account = accounts[user_by_id[key.user_id].username]
            result = gw.call("POST", "/admin/api/v1/keys", {
                "name": f"{key.name} (替换原 sub2api key #{key.id})",
                "account_id": account["id"], "tags": [tags_by_user[key.user_id]]})
            lines.append({
                "source_key_id": key.id, "source_username": user_by_id[key.user_id].username,
                "source_key_name": key.name, "kept_source_key_id": keep.id,
                "kept_username": user_by_id[keep.user_id].username,
                "aigw_key_id": result.get("id"), "aigw_key_prefix": result.get("key_prefix"),
                "tag": tags_by_user[key.user_id],
            })
            lines[-1]["_plaintext"] = result.get("key", "")
        reissue_file = os.path.join(DATA_DIR, f"reissue-keys-{stamp()}.txt")
        with open(reissue_file, "w") as fh:
            fh.write("sub2api-reimport：以下 key 因 12 字符前缀冲突无法沿用原明文，已在 aigw 重签。\n"
                     "交付给本人后立即删除本文件：shred -u " + reissue_file + "\n\n")
            for row in lines:
                fh.write(f"源 key #{row['source_key_id']}（{row['source_username']}，"
                         f"{row['source_key_name']}）—— 前缀与保留的 key #{row['kept_source_key_id']}"
                         f"（{row['kept_username']}）撞车\n")
                fh.write(f"  aigw key #{row['aigw_key_id']}  标签 {row['tag']}\n")
                fh.write(f"  {row['_plaintext']}\n\n")
        os.chmod(reissue_file, 0o600)
        for row in lines:
            note(f"  重签：源 key #{row['source_key_id']}（{row['source_username']}）→ aigw key "
                 f"#{row['aigw_key_id']}（前缀 {row['aigw_key_prefix']}，标签 {row['tag']}）")
        note(f"重签明文：{reissue_file}（0600，交付后立刻 shred；未打印、未进报告）")

    e26q = read_e26q_replacement(E26Q_REISSUE_FILE)
    if e26q:
        token, prefix, khash = e26q
        account = accounts.get("E26Q")
        if account is None:
            note("  ! 没有名为 E26Q 的账户：E26Q 替换密钥未重导")
        else:
            result = gw.call("POST", "/admin/api/v1/keys/import", {
                "account_id": account["id"], "name": "图像 (替换原 sub2api key #24)",
                "key_prefix": prefix, "key_hash": khash, "tags": ["E26Q"], "status": "active"})
            note(f"  E26Q 替换密钥已重导：aigw key #{result.get('id')}（前缀 {prefix}，created="
                 f"{result.get('created')}）")
        del token
    return 0


def cmd_selftest(args, gw: Gateway) -> int:
    gateway = args.gateway
    accounts = {a["name"]: a for a in gw.list_all("/admin/api/v1/accounts")}
    account = accounts.get(SELFTEST_ACCOUNT)
    # 自检要真的把请求送出去，所以这个临时账户必须有一点授信：后付账户 credit_limit=0 时
    # 网关会在计费准入就拒掉（402 billing_hard_limit_reached），那测的就不是路由而是余额。
    selftest_credit = {"billing_mode": "postpaid", "credit_limit_micros": 5_000_000}
    if account is None:
        account = gw.call("POST", "/admin/api/v1/accounts", {
            "name": SELFTEST_ACCOUNT, "tags": [], "note": SELFTEST_NOTE, **selftest_credit})
    else:
        gw.call("PATCH", f"/admin/api/v1/accounts/{account['id']}",
                {"status": "active", **selftest_credit})

    azure_models = azure_selftest_models(gw)
    token_file = os.path.join(DATA_DIR, ".reimport-selftest-tokens.json")
    tokens = {}
    if os.path.exists(token_file):
        try:
            with open(token_file) as fh:
                tokens = json.load(fh)
        except json.JSONDecodeError:
            tokens = {}
    plan = {
        "智天成": ("deepseek-flash", ["智天成"]),
        "电商": (azure_models[0] if azure_models else "gpt-5.6-luna", ["电商"]),
        "E26Q": (azure_models[0] if azure_models else "gpt-5.6-luna", ["E26Q"]),
    }
    failures = 0
    created_keys = []
    token_by_label: dict[str, str] = {}
    for label, (model, tags) in plan.items():
        token = tokens.get(label, "")
        khash = hashlib.sha256(token.encode()).hexdigest() if token else ""
        if token:
            result = gw.call("POST", "/admin/api/v1/keys/import", {
                "account_id": account["id"], "name": f"selftest-{label}",
                "key_prefix": token[:12], "key_hash": khash, "tags": tags, "status": "active"})
            key_id = result.get("id")
            # 上一轮结尾会把这把 key 停用；重跑时要先恢复，否则 401 是上一轮的收尾而不是本次的结论。
            gw.call("PATCH", f"/admin/api/v1/keys/{key_id}", {"status": "active"})
        else:
            result = gw.call("POST", "/admin/api/v1/keys", {
                "name": f"selftest-{label}", "account_id": account["id"], "tags": tags})
            key_id = result.get("id")
            token = result["key"]
            tokens[label] = token
        token_by_label[label] = token
        created_keys.append(key_id)
        status, text = data_plane_call(gateway, token, {"model": model, "input": "ping"})
        if status == 200:
            note(f"  ✓ {label} → {model}: HTTP 200")
        else:
            failures += 1
            note(f"  ✗ {label} → {model}: HTTP {status} {redact(text)[:240]}")
        # 负向：前缀不是密钥
        bad_status, _ = data_plane_call(gateway, token[:12], {"model": model, "input": "ping"})
        if bad_status == 401:
            note(f"  ✓ 负向（{label} 仅前缀）: 401")
        else:
            failures += 1
            note(f"  ✗ 负向（{label} 仅前缀）: HTTP {bad_status}，预期 401")

    # azure 没有可用的部署目录接口（`models/refresh` 返回空，`/models` 给的是 Foundry 模型市场
    # 而非本资源的部署），所以「这个部署到底存不存在」只能用真实请求去证。判定分三类：
    #   upstream_404 部署不存在 → 证伪，关掉路由；
    #   upstream_400「operation unsupported」→ 探测方式不适用（例如图像模型不接受文本请求），
    #     不算证伪，保持开启并记为未证实；
    #   其它 → 记录原文，不改路由（避免把上游抖动当成结论）。
    probes = [m for m in azure_models if m not in (plan["电商"][0],)][:args.azure_probes]
    probed_ok: list[str] = []
    probed_bad: list[tuple[str, str]] = []
    probed_unverified: list[tuple[str, str]] = []
    if probes:
        token = token_by_label.get("电商", "")
        note(f"azure 逐模型真实请求（{len(probes)} 个）：")
        for model in probes:
            set_azure_route_enabled(gw, model, True)
            status, text = data_plane_call(gateway, token, {"model": model, "input": "ping"})
            reason = redact(text)[:200].replace("\n", " ")
            if status == 200:
                probed_ok.append(model)
                note(f"  ✓ azure {model}: HTTP 200")
            elif "does not exist" in text or "upstream_404" in text:
                probed_bad.append((model, reason))
                note(f"  ✗ azure {model}: 部署不存在（HTTP {status}）")
            elif "unsupported" in text:
                probed_unverified.append((model, reason))
                note(f"  ? azure {model}: 探测方式不适用，保持开启（HTTP {status} {reason[:120]}）")
            else:
                probed_unverified.append((model, reason))
                note(f"  ? azure {model}: 未定性，保持原状（HTTP {status} {reason[:120]}）")
        if probed_bad:
            disabled = set_azure_route_enabled(gw, [m for m, _ in probed_bad], False)
            if disabled:
                note(f"  已把 {len(disabled)} 条证伪（部署不存在）的 azure 路由置为 enabled=false："
                     + "、".join(disabled))
    missing_required = [m for m in AZURE_REQUIRED_MODELS
                        if m not in probed_ok and m != plan["电商"][0]]
    probes_path = os.path.join(DATA_DIR, f"azure-probes-{stamp()}.json")
    with open(probes_path, "w") as fh:
        json.dump({"generated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
                   "ok": probed_ok,
                   "deployment_missing": [{"model": m, "error": e} for m, e in probed_bad],
                   "unverified": [{"model": m, "error": e} for m, e in probed_unverified],
                   "high_usage_not_servable": missing_required},
                  fh, ensure_ascii=False, indent=2)
    os.chmod(probes_path, 0o600)
    note(f"azure 逐模型结果：{probes_path}（0600）")
    if missing_required:
        note(f"  ! 电商人群高频模型里有 {len(missing_required)} 个这台 azure 资源服务不了："
             + "、".join(missing_required)
             + "（要么在 azure 里建对应部署，要么把这些名字也授权给别的供应商）")

    with open(token_file, "w") as fh:
        json.dump(tokens, fh)
    os.chmod(token_file, 0o600)
    for key_id in created_keys:
        if key_id:
            gw.call("PATCH", f"/admin/api/v1/keys/{key_id}", {"status": "disabled"})
    gw.call("PATCH", f"/admin/api/v1/accounts/{account['id']}", {"status": "closed"})
    note(f"自检结束：{len(created_keys)} 把自检 key 已停用，账户 {SELFTEST_ACCOUNT} 已关闭"
         f"（网关无删除路由，保留为记录）")
    return 1 if failures else 0


def azure_selftest_models(gw: Gateway) -> list[str]:
    """Prefer the azure deployments the 电商 population actually requests."""
    try:
        providers = {p["name"]: p for p in gw.list_all("/admin/api/v1/providers")}
    except GatewayError:
        return []
    pid = (providers.get("azure") or {}).get("id")
    if pid is None:
        return []
    rows = gw.list_all(f"/admin/api/v1/providers/{pid}/models")
    names = [str(r.get("public_model") or "") for r in rows]
    preferred = ["gpt-5.6-sol", "gpt-5.6-luna", "gpt-5.5", "gpt-6-astra", "gpt-4o"]
    ordered = [m for m in preferred if m in names] + [m for m in names if m and m not in preferred]
    return [m for m in ordered if m]


def set_azure_route_enabled(gw: Gateway, models, enabled: bool) -> list[str]:
    """Turn one or many azure routes on/off. Used to keep every probe independent of whatever
    the previous run decided (a leftover disabled route would make the next probe report the
    gateway's own 403 instead of the upstream's answer), and to retire routes whose upstream
    deployment turned out not to exist."""
    azure_name = TAG_GRANTS["电商"]["providers"][0]
    targets = {models} if isinstance(models, str) else set(models)
    done: list[str] = []
    try:
        routes = gw.list_all("/admin/api/v1/routes")
    except GatewayError:
        return done
    for row in routes:
        if str(row.get("provider") or "") != azure_name:
            continue
        model = str(row.get("model") or "")
        if model not in targets or bool(row.get("enabled")) == enabled:
            continue
        try:
            gw.call("PATCH", f"/admin/api/v1/routes/{row.get('id')}", {"enabled": enabled})
            done.append(model)
        except GatewayError:
            continue
    return done


def data_plane_call(base: str, token: str, body: dict, timeout: int = 180) -> tuple[int, str]:
    request = urllib.request.Request(
        base.rstrip("/") + "/v1/responses",
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + token},
        method="POST")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace")
    except Exception as exc:  # noqa: BLE001
        return 0, str(exc)


def cmd_verify(args, gw: Gateway) -> int:
    src = load_source()
    tags_by_user = assign_tags(src)
    target = read_target_db(args.db)
    accounts_by_name = {a["name"]: a for a in target["accounts"]}
    keys_by_prefix = {k["key_prefix"]: k for k in target["keys"]}
    user_by_id = {u.id: u for u in src.users}
    reissue, _lines = plan_reissues(src, target, optional_export(args.export))

    failures: list[str] = []
    buckets: dict[str, int] = {tag: 0 for tag in POP_TAGS}
    for user in src.users:
        account = accounts_by_name.get(user.username)
        if account is None:
            failures.append(f"账户缺失：{user.username}")
            continue
        if not str(account["note"]).startswith(ACCOUNT_NOTE_PREFIX):
            failures.append(f"账户 {user.username} 的 note 不是迁移格式：{account['note']!r}")
        expected = tags_by_user[user.id]
        if json.loads(account["tags_json"] or "[]") != [expected]:
            failures.append(f"账户 {user.username} 标签={account['tags_json']}，预期 [\"{expected}\"]")

    for key in src.keys:
        user = user_by_id[key.user_id]
        if key.id in reissue:
            continue
        buckets[tags_by_user[key.user_id]] += 1
        stored = keys_by_prefix.get(key.prefix)
        if stored is None:
            failures.append(f"key 缺失：前缀 {key.prefix}（源 #{key.id}）")
            continue
        if stored["key_hash"] != key.khash:
            failures.append(f"哈希不一致：前缀 {key.prefix}（源 #{key.id}）")
        if stored["status"] != "active":
            failures.append(f"状态不是 active：前缀 {key.prefix}")
        if not str(stored["created_by"]).startswith(IMPORT_MARKER):
            failures.append(f"created_by 不是导入标记：前缀 {key.prefix}（{stored['created_by']}）")
        expected_account = accounts_by_name.get(user.username)
        if expected_account is not None and stored["account_id"] != expected_account["id"]:
            failures.append(f"账户挂错：前缀 {key.prefix} 属于账户 {stored['account_id']}，"
                            f"预期 {expected_account['id']}（{user.username}）")

    note("分桶：" + " / ".join(f"{tag}={buckets[tag]}" for tag in POP_TAGS))

    # 生效标签走管理接口（与数据面同一套解析）
    by_account: dict[int, list[dict]] = {}
    for row in gw.list_all("/admin/api/v1/keys"):
        by_account.setdefault(int(row.get("account_id") or 0), []).append(row)
    for user in src.users:
        expected = tags_by_user[user.id]
        account = accounts_by_name.get(user.username)
        if account is None:
            continue
        for row in by_account.get(account["id"], []):
            effective = row.get("effective_tags")
            if effective != [expected]:
                failures.append(f"生效标签不符：{user.username} 的 key {row.get('key_prefix')} "
                                f"effective_tags={effective}，预期 [\"{expected}\"]")

    tags = gw.list_all("/admin/api/v1/tags")
    names = sorted(t["name"] for t in tags)
    if names != sorted(POP_TAGS):
        failures.append(f"标签集合={names}，预期 {sorted(POP_TAGS)}")
    providers = {p["name"]: p for p in gw.list_all("/admin/api/v1/providers")}
    for tag in POP_TAGS:
        row = next((t for t in tags if t["name"] == tag), None)
        grants = (row or {}).get("grants") or {}
        if sorted(grants.get("providers") or []) != sorted(TAG_GRANTS[tag]["providers"]):
            failures.append(f"标签 {tag} grants.providers={grants.get('providers')}，"
                            f"预期 {TAG_GRANTS[tag]['providers']}")
        for provider in TAG_GRANTS[tag]["providers"]:
            if provider not in providers:
                failures.append(f"标签 {tag} 授权的供应商 {provider} 不存在")
            elif not providers[provider].get("enabled", True):
                failures.append(f"标签 {tag} 授权的供应商 {provider} 未启用")

    note(f"供应商：{len(providers)} 个（" + "、".join(f"{n}" for n in sorted(providers)) + "）")
    for failure in failures:
        note("  ✗ " + failure)
    if failures:
        note(f"核对未通过：{len(failures)} 项")
        return 1
    note("结构与授权核对全部通过。")
    return 0


def cmd_repair_prefix(args, gw: Gateway) -> int:
    """Re-import one source key over a prefix that another key currently occupies.

    Only needed when the survivor of a prefix collision was picked wrongly (e.g. the target had
    already been wiped, so the pre-wipe holder was unknown). The plaintext still never leaves the
    source database: prefix and hash are recomputed by SQL, and re-importing the same prefix
    replaces the row — that is the documented upsert behaviour of the import route.
    """
    src = load_source()
    user_by_id = {u.id: u for u in src.users}
    wanted = set(args.keep_source_key)
    keys = [k for k in src.keys if k.id in wanted]
    missing = wanted - {k.id for k in keys}
    if missing:
        die(f"源库里没有这些活跃 key：{sorted(missing)}")
    accounts = {a["name"]: a for a in gw.list_all("/admin/api/v1/accounts")}
    for key in keys:
        username = user_by_id[key.user_id].username
        account = accounts.get(username)
        if account is None:
            die(f"目标实例里没有账户 {username}")
        result = gw.call("POST", "/admin/api/v1/keys/import", {
            "account_id": account["id"], "name": key.name, "key_prefix": key.prefix,
            "key_hash": key.khash, "tags": [], "status": "active"})
        note(f"  前缀 {key.prefix} 现归属 {username}（源 key #{key.id} {key.name}，"
             f"aigw key #{result.get('id')}，created={result.get('created')}）")
    for key_id in args.disable_key:
        gw.call("PATCH", f"/admin/api/v1/keys/{key_id}", {"status": "disabled"})
        note(f"  停用多余的 key #{key_id}")
    return 0


def cmd_report(args, gw: Gateway) -> int:
    src = load_source(with_accounts=True)
    tags_by_user = assign_tags(src)
    target = read_target_db(args.db)
    reissue, lines = plan_reissues(src, target, optional_export(args.export))
    user_by_id = {u.id: u for u in src.users}
    accounts_by_name = {a["name"]: a for a in target["accounts"]}
    keys_by_prefix = {k["key_prefix"]: k for k in target["keys"]}

    rows = []
    for key in src.keys:
        user = user_by_id[key.user_id]
        account = accounts_by_name.get(user.username) or {}
        stored = keys_by_prefix.get(key.prefix) or {}
        rows.append({
            "source_key_id": key.id, "source_user_id": user.id, "source_username": user.username,
            "source_email": user.email, "source_key_name": key.name, "source_group_id": key.group_id,
            "source_key_prefix": key.prefix, "tag": tags_by_user[user.id],
            "reissue": key.id in reissue,
            "target_account_id": account.get("id"), "target_key_id": stored.get("id"),
        })
    buckets: dict[str, int] = {tag: 0 for tag in POP_TAGS}
    for row in rows:
        buckets[row["tag"]] += 1
    payload = {
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "gateway": args.gateway,
        "database": args.db,
        "source": {"users": len(src.users), "keys": len(src.keys), "excluded": len(src.excluded)},
        "tags": {tag: TAG_GRANTS[tag] for tag in POP_TAGS},
        "tag_counts": buckets,
        "conflicts": lines,
        "reissued": [{"source_key_id": key_id, "kept_source_key_id": keep_id}
                     for key_id, keep_id in sorted(reissue.items())],
        "providers": [{"name": p["name"], "kind": p["kind"], "enabled": p["enabled"]}
                      for p in target["providers"]],
        "providers_skipped": SKIP_SOURCE_ACCOUNTS,
        "counts_after": target["counts"],
        "schema_after": target["schema"],
        "assignments": rows,
    }
    for extra in sorted(glob.glob(os.path.join(DATA_DIR, "azure-coverage-*.json")))[-1:]:
        try:
            with open(extra) as fh:
                payload["azure_coverage"] = json.load(fh)
        except json.JSONDecodeError:
            pass
    for extra in sorted(glob.glob(os.path.join(DATA_DIR, "azure-probes-*.json")))[-1:]:
        try:
            with open(extra) as fh:
                payload["azure_probes"] = json.load(fh)
        except json.JSONDecodeError:
            pass
    path = os.path.join(DATA_DIR, f"sub2api-reimport-{stamp()}.json")
    with open(path, "w") as fh:
        json.dump(payload, fh, ensure_ascii=False, indent=2)
    os.chmod(path, 0o600)
    note(f"报告：{path}（0600，{len(rows)} 条映射，"
         + " / ".join(f"{tag}={buckets[tag]}" for tag in POP_TAGS) + "）")
    return 0


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def main() -> int:
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("cmd", choices=("inventory", "snapshot", "wipe", "providers", "models",
                                        "tags", "accounts", "selftest", "verify", "report",
                                        "repair-prefix"))
    parser.add_argument("--gateway", default=GATEWAY)
    parser.add_argument("--password-file", default=PASSWORD_FILE)
    parser.add_argument("--db", default=DB_PATH, help="目标实例的 SQLite 路径")
    parser.add_argument("--config", default=CONFIG_PATH, help="目标实例的 config.yaml")
    parser.add_argument("--export", default="", help="重建素材（默认取最新的 rebuild-export-*.json）")
    parser.add_argument("--coverage-days", type=int, default=30, help="人群模型用量的回溯天数")
    parser.add_argument("--version-note", default="", help="inventory 用来记录当前线上版本")
    parser.add_argument("--config-bootstrap", default=True,
                        help="wipe 时是否往 config.yaml 追加 bootstrap.admin（默认 true）")
    parser.add_argument("--force", action="store_true", help="wipe 在库非空时继续（危险）")
    parser.add_argument("--wait", type=int, default=90, help="wipe 后等待就绪的秒数")
    parser.add_argument("--azure-probes", type=int, default=8,
                        help="selftest 逐个真跑的 azure 模型数（0 = 只跑三个标签各一个）")
    parser.add_argument("--keep-source-key", action="append", type=int, default=[],
                        help="repair-prefix：要让其占用该前缀的源 key id（可重复）")
    parser.add_argument("--disable-key", action="append", type=int, default=[],
                        help="repair-prefix：顺带停用的 aigw key id（可重复）")
    args = parser.parse_args()
    if isinstance(args.config_bootstrap, str):
        args.config_bootstrap = args.config_bootstrap.lower() not in ("0", "false", "no", "off")

    gw = Gateway(args.gateway, args.password_file)
    if args.cmd != "wipe":
        gw.login()
    else:
        try:
            gw.login()
        except (GatewayError, OSError) as exc:
            note(f"清库前登录失败（将被重建，忽略）：{redact(exc)}")
    handlers = {"inventory": cmd_inventory, "snapshot": cmd_snapshot, "wipe": cmd_wipe,
                "providers": cmd_providers, "models": cmd_models, "tags": cmd_tags,
                "accounts": cmd_accounts, "selftest": cmd_selftest, "verify": cmd_verify,
                "report": cmd_report, "repair-prefix": cmd_repair_prefix}
    return handlers[args.cmd](args, gw)


if __name__ == "__main__":
    raise SystemExit(main())
