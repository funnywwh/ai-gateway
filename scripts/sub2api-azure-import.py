#!/usr/bin/env python3
"""sub2api-azure-import: 把 gw-b sub2api 上游账号 #10「azureChatGPTkey」导入到一台 ai-gateway。

    python3 sub2api-azure-import.py plan     # 只读：探测上游部署 + 打印将要写入的每一行
    python3 sub2api-azure-import.py apply    # 真正写入（供应商 + 对客模型 + 供应商模型行 + 路由，行当场带价）
    python3 sub2api-azure-import.py verify   # 读回核对：路由能解析、价格与价表逐字一致、既有行未变

与 `scripts/sub2api-reimport.py` 的区别（刻意的，见 docs/sub2api-migration.md §9.5）：
  * 只导**一个**源上游账号，不建账户/Key/标签，不动业务数据；
  * **认 model_mapping**：reimport 的 probe_azure_upstream() 只读 base_url/api_key，把 public 名
    当 upstream 名写。本资源上 `gpt-4o` 实际由 `gpt-5.6-luna` 部署服务，照 public 名写会得到
    一个请求时 404 的映射——所以这里把 model_mapping 当权威的 public→deployment 表；
  * **部署存在性以真实请求为准**：azure 没有可用的目录接口（GET /models 是 Foundry 市场目录、
    /openai/deployments 已下线、网关 models/refresh 返回 0）。200 = 可用；400 `operation
    unsupported` = 存在（图像模型不接受文本请求）；404 `DeploymentNotFound` = 不存在，不建。

保密纪律（与 sub2api-migrate.py / sub2api-reimport.py 同一条）：
  * 明文 api_key 只经内存与 0600 临时文件传递，写完即碎；从不打印、不进报告；
  * 所有打印与异常过 redact()；报告里没有密钥材料。

环境变量：GW_BASE（默认 http://127.0.0.1:8088）、GW_ADMIN_USER / GW_ADMIN_PASS、
          SUB2API_PSQL（默认 docker exec -i sub2api-postgres psql -U sub2api -d sub2api）、
          AZURE_ACCOUNT（默认 10）、PLAN_FILE（official-pricing.sh --plan-out 的输出）、
          SRC_HOST（默认 gw-b，仅用于报告记录）。
"""
import argparse
import http.cookiejar
import json
import os
import re
import secrets
import shlex
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_BASE = "http://127.0.0.1:8088"
AZURE_PROVIDER = "azure"
AZURE_KIND = "openai-responses"
# 图像必须由**另一个供应商实例**承载：网关的 openai-responses 内建类型在代码层面就不能服务
# 图像模型（internal/runtime/dispatcher.go:291 的 "provider kind ... does not serve image models"
# ——它没有实现 pluginapi.ImageProvider）。docs/api-images.md §6.1 的口径是「一个供应商实例只讲
# 一门协议」，所以同一个 azure 资源开两个实例：azure 讲 /responses，azure-images 讲 /images。
AZURE_IMAGES_PROVIDER = "azure-images"
AZURE_IMAGES_KIND = "openai-images"
AZURE_ACCOUNT = int(os.environ.get("AZURE_ACCOUNT", "10"))
SRC_HOST = os.environ.get("SRC_HOST", "gw-b")
STAMP = time.strftime("%Y%m%d-%H%M%S")

# 文本模型：这个 azure 资源是 Responses 面，流式/工具都支持。
TEXT_CAPABILITIES = {"stream": True, "tools": True}
# 图像模型：image_generation 是**必须显式声明**的（docs/api-images.md §6.3）——图片端点要求声明，
# 未声明等于不参与，全部候选都不满足时客户端拿到 400 unsupported。image=true 表示接受参考图
# （/v1/images/edits 需要）。这两个键与 doc 里的映射行示例一致。
IMAGE_CAPABILITIES = {"image_generation": True, "image": True}

# 公开的图片模型名 -> 上游部署名。上游对文本探针回 400 `operation unsupported` 的那批就是图片模型；
# 这张表同时供 patch 步骤判断「哪一行该拿 IMAGE_CAPABILITIES」。
IMAGE_MODELS = {
    "gpt-image-1.5": "gpt-image-1.5",
    "gpt-image-2": "gpt-image-2",
    "gpt-image-2.5": "gpt-image-2.5-sunburst",
    "gpt-image-2.5-flare": "gpt-image-2.5-flare",
    "gpt-image-2.5-sunburst": "gpt-image-2.5-sunburst",
}

SECRET_RE = re.compile(
    r"(sk-[A-Za-z0-9_\-]{13,}|rt\.[A-Za-z0-9._\-]{8,}|eyJ[A-Za-z0-9._\-]{16,}"
    r"|aigw_mcp_[A-Za-z0-9_\-]{12,}|[0-9a-f]{32,})")

# ── 标签（tags 子命令）─────────────────────────────────────────────────────────
# 标签名与供应商名是两个独立命名空间：标签叫 `azure-image`（单数，按用户指定的名字），
# 但它 grants 里必须写供应商真名 `azure-images`（复数）。grants 存的是**原始 JSON 字符串**，
# 服务端不校验供应商是否存在（routing.go:151 只做 json.Unmarshal），写错只会静默不加权——
# 所以 verify-tags 的真实请求才是唯一证据。
TAG_SPECS = [
    ("azure", {"providers": [AZURE_PROVIDER], "models": ["*"]},
     "azure 文本供应商（openai-responses，/v1/responses）"),
    ("azure-image", {"providers": [AZURE_IMAGES_PROVIDER], "models": ["*"]},
     "azure-images 图像供应商（openai-images，/v1/images）"),
]
TAG_NAMES = [name for name, _, _ in TAG_SPECS]

# 要逐个真发请求验证的模型。
TEXT_MODELS_VERIFY = ["gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-4o"]
IMAGE_MODELS_VERIFY = ["gpt-image-1.5", "gpt-image-2", "gpt-image-2.5",
                       "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"]
# 验证 key 的落点：账户 `local`(1) 是 postpaid、余额≈$99,999，额度充足，让验证只反映授权，
# 不被 insufficient_quota 污染（图像每张按 8192 token 预留 ≈ $0.25，$1.00 的预付账户太紧）。
VERIFY_ACCOUNT = int(os.environ.get("VERIFY_ACCOUNT_ID", "1"))
VERIFY_KEY_NAME = "zz-tag-azure-verify"


def redact(text) -> str:
    return SECRET_RE.sub("<redacted>", str(text))


def note(message: str) -> None:
    print(redact(message), flush=True)


def die(message: str) -> None:
    sys.exit("sub2api-azure-import: " + redact(message))


class GatewayError(Exception):
    pass


class Gateway:
    def __init__(self, base: str, user: str, password: str):
        self.base = base.rstrip("/")
        self.user = user
        self.password = password
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))

    def login(self) -> None:
        body = json.dumps({"username": self.user, "password": self.password}).encode()
        req = urllib.request.Request(self.base + "/admin/api/v1/auth/login", data=body,
                                     headers={"Content-Type": "application/json"})
        try:
            with self.opener.open(req, timeout=20) as resp:
                json.loads(resp.read().decode("utf-8", "replace"))
        except urllib.error.HTTPError as exc:
            die(f"登录 {self.base} 失败：HTTP {exc.code} {redact(exc.read().decode('utf-8','replace'))[:200]}")
        except Exception as exc:  # noqa: BLE001
            die(f"登录 {self.base} 失败：{redact(exc)}")

    def call(self, method: str, path: str, body=None):
        data = None if body is None else json.dumps(body, ensure_ascii=False).encode()
        headers = {"Accept": "application/json"}
        if data is not None:
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(req, timeout=60) as resp:
                raw = resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", "replace")
            raise GatewayError(f"{method} {path} -> HTTP {exc.code} {redact(detail)[:300]}") from None
        except Exception as exc:  # noqa: BLE001
            raise GatewayError(f"{method} {path} -> {redact(exc)}") from None
        if not raw.strip():
            return {}
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            raise GatewayError(f"{method} {path} -> 非 JSON 响应") from None

    def list_all(self, path: str) -> list:
        payload = self.call("GET", path)
        if isinstance(payload, dict) and "data" in payload:
            return payload["data"] or []
        return payload if isinstance(payload, list) else []


# ── 源库读取（只经 psql 输出进内存）────────────────────────────────────────────
# 源库可能在**另一台机器**上：gw-c 自己也跑着一个 sub2api-postgres 容器，但那是另一个库
# （只有 5 个账号，没有 #10）。所以默认把 psql 走 ssh 到 SRC_SSH_HOST（默认 gw-b），而不是读本机
# 容器——否则会静默读到错误的库（账号不存在，或更糟：同名不同源）。
def psql(sql: str) -> list[list[str]]:
    inner = os.environ.get("SUB2API_PSQL", "docker exec -i sub2api-postgres psql -U sub2api -d sub2api")
    ssh_host = os.environ.get("SRC_SSH_HOST", "")
    if ssh_host:
        remote = inner + " -t -A -F '\x1f' -c " + shlex.quote(sql)
        argv = ["ssh", "-o", "BatchMode=yes", ssh_host, remote]
    else:
        argv = shlex.split(inner) + ["-t", "-A", "-F", "\x1f", "-c", sql]
    out = subprocess.run(argv, capture_output=True, text=True)
    if out.returncode != 0:
        die("读取源库失败：" + out.stderr.strip()[:300])
    rows = []
    for line in out.stdout.splitlines():
        if line.strip():
            rows.append(line.split("\x1f"))
    return rows


def source_account(account_id: int) -> dict:
    """Read the upstream account's credentials. The api_key stays in this process."""
    rows = psql(f"SELECT name, status, credentials::text FROM accounts "
                f"WHERE id = {int(account_id)} AND deleted_at IS NULL;")
    if not rows:
        die(f"源库里没有上游账号 #{account_id}（或已软删）")
    name, status, creds_raw = rows[0][0], rows[0][1], "\x1f".join(rows[0][2:])
    try:
        creds = json.loads(creds_raw)
    except json.JSONDecodeError:
        die("源账号凭据不是合法 JSON")
    return {"id": account_id, "name": name, "status": status,
            "base_url": str(creds.get("base_url") or "").rstrip("/"),
            "api_key": str(creds.get("api_key") or ""),
            "model_mapping": creds.get("model_mapping") or {}}


# ── 上游部署探测（真实请求，权威判据）──────────────────────────────────────────
def probe_deployment(base_url: str, api_key: str, model: str) -> tuple[int, str]:
    body = json.dumps({
        "model": model,
        "input": [{"type": "message", "role": "user",
                   "content": [{"type": "input_text", "text": "hi"}]}],
        "stream": False,
    }).encode()
    req = urllib.request.Request(
        base_url.rstrip("/") + "/responses", data=body,
        headers={"api-key": api_key, "Content-Type": "application/json",
                 "Accept": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=180) as resp:
            payload = json.loads(resp.read().decode("utf-8", "replace"))
            return resp.status, "usage=" + json.dumps(payload.get("usage") or {})
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read(500).decode("utf-8", "replace").replace("\n", " ")[:200]
    except Exception as exc:  # noqa: BLE001
        return 0, f"{type(exc).__name__}: {str(exc)[:160]}"


def classify(status: int, detail: str) -> str:
    """200 = available; 400 unsupported = exists (image models reject text); 404 = absent."""
    if status == 200:
        return "available"
    if status == 404 and "DeploymentNotFound" in detail:
        return "absent"
    if status == 400 and "unsupported" in detail.lower():
        return "exists"
    if status == 0:
        return "probe_error"
    return "unknown"


# ── 目标侧计划 ────────────────────────────────────────────────────────────────
def build_plan(gw: Gateway, account: dict, plan_prices: dict) -> dict:
    """Decide the exact rows to write. Public name -> upstream deployment name."""
    mapping = account["model_mapping"]
    # 反向：upstream 部署 -> 它服务的 public 名（可能多条，例如 gpt-5.6-luna 同时服务 gpt-4o）
    by_upstream: dict[str, list[str]] = {}
    for public, upstream in mapping.items():
        by_upstream.setdefault(str(upstream), []).append(str(public))

    providers = {p["name"]: p for p in gw.list_all("/admin/api/v1/providers")}
    catalog = {row["public_name"] for row in gw.list_all("/admin/api/v1/models")}
    routes = gw.list_all("/admin/api/v1/routes")
    existing_pm: dict[str, dict] = {}
    if AZURE_PROVIDER in providers:
        existing_pm = {str(r.get("public_model")): r
                       for r in gw.list_all(f"/admin/api/v1/providers/{providers[AZURE_PROVIDER]['id']}/models")}

    result = {"provider": AZURE_PROVIDER, "provider_exists": AZURE_PROVIDER in providers,
              "images_provider": AZURE_IMAGES_PROVIDER,
              "images_provider_exists": AZURE_IMAGES_PROVIDER in providers,
              "source_account": account["id"], "source_name": account["name"],
              "source_status": account["status"], "base_url": account["base_url"],
              "src_host": SRC_HOST, "probes": {}, "rows": [], "skipped": [],
              "catalog_new": [m for m in mapping if m not in catalog],
              "upstream_to_public": by_upstream}

    for public in sorted(mapping):
        upstream = str(mapping[public])
        status, detail = probe_deployment(account["base_url"], account["api_key"], upstream)
        verdict = classify(status, detail)
        result["probes"][upstream] = {"status": status, "verdict": verdict, "detail": redact(detail)}
        if verdict in ("absent", "probe_error", "unknown"):
            result["skipped"].append([public, upstream, verdict, redact(detail)[:120]])
            continue
        price = plan_prices.get(upstream)
        # 上游对文本探针回 400 `operation unsupported` 的那批就是图像模型；那是本次唯一能拿到
        # 「它是不是图片模型」的可靠信号。图像行走 azure-images 供应商（openai-images），
        # 文本行走 azure（openai-responses）——两者是不同 kind 的两个实例。
        is_image = verdict == "exists"
        caps = IMAGE_CAPABILITIES if is_image else TEXT_CAPABILITIES
        target = AZURE_IMAGES_PROVIDER if is_image else AZURE_PROVIDER
        row = {"public_model": public, "upstream_model": upstream, "verdict": verdict,
               "capabilities": caps, "provider": target,
               "needs_catalog": public not in catalog,
               "route_exists": any(r.get("model") == public and r.get("provider") == target
                                   for r in routes),
               "priced": price is not None}
        if price is None:
            result["skipped"].append([public, upstream, "no_official_price", "价表里没有这个 upstream id"])
            continue
        result["rows"].append(row)
    return result


def print_plan(plan: dict) -> None:
    note(f"源账号 #{plan['source_account']}「{plan['source_name']}」({plan['source_status']}) @ {plan['src_host']}")
    note(f"上游 base_url：{plan['base_url']}")
    note(f"目标实例已有供应商 {AZURE_PROVIDER}？{'是（将复用）' if plan['provider_exists'] else '否（将新建）'}")
    note("")
    note("上游部署探测（真实 POST /responses）：")
    for upstream, info in sorted(plan["probes"].items()):
        mark = {"available": "200 可用", "exists": "400 存在（图像模型，不接受文本请求）",
                "absent": "404 部署不存在", "probe_error": "探测失败", "unknown": "未知"}[info["verdict"]]
        note(f"  {upstream:26s} HTTP {info['status']:3d}  {mark}")
    note("")
    note(f"{'public 名':26s} {'上游部署':26s} {'目标供应商':14s} {'对客目录':8s} {'路由':8s} {'价格':6s}")
    for row in plan["rows"]:
        note(f"  {row['public_model']:26s} {row['upstream_model']:26s} {row['provider']:14s} "
             f"{'新建' if row['needs_catalog'] else '已有':8s} "
             f"{'已有' if row['route_exists'] else '新建':8s} "
             f"{'有' if row['priced'] else '缺':6s}")
    if plan["skipped"]:
        note("")
        note("跳过（不建行，理由如下）：")
        for public, upstream, why, detail in plan["skipped"]:
            note(f"  ✗ {public} -> {upstream}：{why}（{detail}）")


def ensure_provider(gw: Gateway, providers: dict, name: str, kind: str, account: dict) -> int:
    """Create the provider if absent; never touch an existing one (its creds/config stay as they are)."""
    if name in providers:
        pid = providers[name]["id"]
        note(f"供应商 {name} 已存在（id={pid}）：不复建，也不改它的凭据/config")
        return pid
    # 凭据直接走 POST body，明文不出本进程的内存。config 里**不写** api_key（那一栏是明文且
    # 管理面会回显），只写 base_url；凭据走 credentials 栏（AES-GCM 加密落库、永不回显）。
    body = {
        "name": name, "kind": kind,
        "display_name": _display_name(name, account),
        "enabled": True, "priority": 100, "weight": 100,
        "config": {"base_url": account["base_url"]},
        "credentials": {"api_key": account["api_key"]},
        "meta": {"source": f"{SRC_HOST}/sub2api", "source_account_id": account["id"],
                 "source_name": account["name"], "import": f"azureChatGPTkey-{STAMP}"},
    }
    result = gw.call("POST", "/admin/api/v1/providers", body)
    pid = result.get("id")
    note(f"供应商 {name} 已建：id={pid}，kind={kind}，"
         f"凭据字段={result.get('credential_keys')}，enabled={result.get('enabled')}")
    return pid


def _display_name(name: str, account: dict) -> str:
    suffix = "图像（/images）" if name == AZURE_IMAGES_PROVIDER else "（/responses）"
    return f"{name}{suffix}源账号 #{account['id']} {account['name']}"


def apply_plan(gw: Gateway, account: dict, plan: dict, plan_prices: dict) -> dict:
    """Write both providers, catalog rows, provider-model rows (with price in the same call) and routes."""
    providers = {p["name"]: p for p in gw.list_all("/admin/api/v1/providers")}
    pid_text = ensure_provider(gw, providers, AZURE_PROVIDER, AZURE_KIND, account)
    need_images = any(r["provider"] == AZURE_IMAGES_PROVIDER for r in plan["rows"])
    pid_images = (ensure_provider(gw, providers, AZURE_IMAGES_PROVIDER, AZURE_IMAGES_KIND, account)
                  if need_images else None)
    pid_of = {AZURE_PROVIDER: pid_text, AZURE_IMAGES_PROVIDER: pid_images}

    written = {"catalog": [], "provider_models": [], "routes": []}
    for row in plan["rows"]:
        public, upstream, target = row["public_model"], row["upstream_model"], row["provider"]
        pid = pid_of[target]
        if row["needs_catalog"]:
            gw.call("POST", "/admin/api/v1/models",
                    {"public_name": public, "display_name": public, "enabled": True})
            written["catalog"].append(public)
        # 行必须**当场带价**：先建行再补价，中间那段是按 0 成本计费的。
        # capabilities 也一起给：图像模型不声明 image_generation 就永远拿不到图片请求。
        body = {"public_model": public, "upstream_model": upstream, "enabled": True,
                "priority": 100, "weight": 100,
                "capabilities": row["capabilities"],
                "pricing_rules": plan_prices[upstream]}
        gw.call("POST", f"/admin/api/v1/providers/{pid}/models", body)
        written["provider_models"].append(f"{target}/{public}")
        if not row["route_exists"]:
            gw.call("POST", "/admin/api/v1/routes",
                    {"model": public, "provider": target, "upstream_model": upstream,
                     "enabled": True, "priority": 100, "weight": 100})
            written["routes"].append(public)
    return {"provider_id": pid, "written": written}


def cmd_plan(args, gw: Gateway) -> int:
    account = source_account(args.account)
    plan_prices = load_prices(args.plan_file)
    plan = build_plan(gw, account, plan_prices)
    print_plan(plan)
    out = os.path.join(args.data_dir, f"azure-import-plan-{STAMP}.json")
    dump_json(out, plan)
    note("")
    note(f"计划已落盘：{out}（0600）")
    return 0 if plan["rows"] else 1


def cmd_split(args, gw: Gateway) -> int:
    """Move the image rows off the responses provider onto a dedicated openai-images provider.

    Needed when an earlier apply put the gpt-image-* rows on `azure` (openai-responses): that kind
    cannot serve images at all (dispatcher.go:291), so those rows would answer every image request
    with 400. Prices and the text rows are left untouched.
    """
    account = source_account(args.account)
    plan_prices = load_prices(args.plan_file)
    providers = {p["name"]: p for p in gw.list_all("/admin/api/v1/providers")}
    if AZURE_PROVIDER not in providers:
        die(f"实例上没有供应商 {AZURE_PROVIDER}")
    pid_text = providers[AZURE_PROVIDER]["id"]

    rows = gw.list_all(f"/admin/api/v1/providers/{pid_text}/models")
    moving = [r for r in rows if str(r.get("public_model")) in IMAGE_MODELS]
    if not moving:
        note(f"{AZURE_PROVIDER} 上没有图像模型行：无需拆分")
        return 0
    note(f"将从 {AZURE_PROVIDER}(id={pid_text}) 迁出 {len(moving)} 条图像模型行："
         + "、".join(str(r.get("public_model")) for r in moving))

    pid_images = ensure_provider(gw, providers, AZURE_IMAGES_PROVIDER, AZURE_IMAGES_KIND, account)
    routes = gw.list_all("/admin/api/v1/routes")
    moved = []
    for row in moving:
        public = str(row["public_model"])
        upstream = str(row.get("upstream_model") or public)
        # 图像模型在 openai-images 上也要声明能力（kind 的 config.models 走的是同一组能力键）。
        gw.call("POST", f"/admin/api/v1/providers/{pid_images}/models",
                {"public_model": public, "upstream_model": upstream, "enabled": True,
                 "priority": 100, "weight": 100,
                 "capabilities": IMAGE_CAPABILITIES,
                 "pricing_rules": plan_prices.get(upstream)})
        # 旧路由指向 azure（responses），要改成 azure-images；先删后建，避免出现两条候选。
        for route in routes:
            if route.get("model") == public and route.get("provider") == AZURE_PROVIDER:
                gw.call("DELETE", f"/admin/api/v1/routes/{route['id']}")
        gw.call("POST", "/admin/api/v1/routes",
                {"model": public, "provider": AZURE_IMAGES_PROVIDER, "upstream_model": upstream,
                 "enabled": True, "priority": 100, "weight": 100})
        # 从 responses 供应商上删掉这一行，否则它仍是一条收不到图片请求的死行。
        gw.call("DELETE", f"/admin/api/v1/provider-models/{row['id']}")
        moved.append(public)
        note(f"  ✓ {public}: {AZURE_PROVIDER} -> {AZURE_IMAGES_PROVIDER}（行已迁、路由已改）")
    note(f"拆分完成：{len(moved)} 条图像模型行迁到 {AZURE_IMAGES_PROVIDER}(id={pid_images})")
    return 0


def cmd_apply(args, gw: Gateway) -> int:
    account = source_account(args.account)
    plan_prices = load_prices(args.plan_file)
    plan = build_plan(gw, account, plan_prices)
    print_plan(plan)
    if not plan["rows"]:
        die("没有任何行可写（全部被跳过）")
    note("")
    note("开始写入（供应商 → 对客模型 → 供应商模型行[带价] → 路由）：")
    result = apply_plan(gw, account, plan, plan_prices)
    w = result["written"]
    note(f"  对客模型目录新增 {len(w['catalog'])}：{'、'.join(w['catalog']) or '（无）'}")
    note(f"  供应商模型行写入 {len(w['provider_models'])}（每条都当场带官方价）")
    note(f"  路由新增 {len(w['routes'])}")
    report = {"plan": plan, "result": result, "stamp": STAMP}
    out = os.path.join(args.data_dir, f"azure-import-report-{STAMP}.json")
    dump_json(out, report)
    note(f"报告：{out}（0600，无密钥材料）")
    return 0


def cmd_verify(args, gw: Gateway) -> int:
    plan_prices = load_prices(args.plan_file)
    providers = {p["name"]: p for p in gw.list_all("/admin/api/v1/providers")}
    if AZURE_PROVIDER not in providers:
        die(f"实例上没有供应商 {AZURE_PROVIDER}")
    problems = []
    for pname, expected_kind in ((AZURE_PROVIDER, AZURE_KIND),
                                 (AZURE_IMAGES_PROVIDER, AZURE_IMAGES_KIND)):
        if pname not in providers:
            # 图像供应商只在有图像行时才该存在；缺了不是错误，但要看得见。
            note(f"供应商 {pname}：不存在（{expected_kind}）")
            continue
        pid = providers[pname]["id"]
        rows = gw.list_all(f"/admin/api/v1/providers/{pid}/models")
        note(f"供应商 {pname} (id={pid}, kind={providers[pname].get('kind')}) 的 {len(rows)} 条供应商模型行：")
        for row in sorted(rows, key=lambda r: str(r.get("public_model"))):
            public = str(row.get("public_model"))
            upstream = str(row.get("upstream_model") or "")
            stored = row.get("pricing_rules")
            if isinstance(stored, str):
                try:
                    stored = json.loads(stored)
                except json.JSONDecodeError:
                    stored = None
            expected = plan_prices.get(upstream)
            price_ok = stored == expected
            if not price_ok:
                problems.append(f"{pname}/{public}: 价格与价表不一致")
            caps = row.get("capabilities") or {}
            if isinstance(caps, str):
                try:
                    caps = json.loads(caps)
                except json.JSONDecodeError:
                    caps = {}
            # 路由候选：explain 用的是**合成 key**（没有标签授权），所以未授权时它一律回
            # `not_granted`，这对既有供应商（deepseek/replay-local）也一样——因此「候选=0」不是
            # 缺陷信号，「排除原因是 not_granted 而不是 not_mapped」才是我们要的结论：
            # 它证明路由/映射本身解析成功了。真正的授权由数据面的真实请求证明（selftest）。
            want = IMAGE_CAPABILITIES if public in IMAGE_MODELS else TEXT_CAPABILITIES
            cap_ok = all(caps.get(k) for k in want)
            if not cap_ok:
                problems.append(f"{pname}/{public}: 能力声明不全（有 {caps}，要 {want}）")
            query = (f"/admin/api/v1/router/explain?model={urllib.parse.quote(public)}"
                     f"&provider={urllib.parse.quote(pname)}")
            reasons, mapping_ok = [], False
            try:
                page = gw.call("GET", query)
                canonical = page.get("canonical")
                reasons = [str(e.get("reason")) for e in (page.get("excluded") or [])]
                mapping_ok = bool(canonical) and not any(
                    r in ("not_mapped", "unknown_model", "model_not_found", "no_candidate") for r in reasons)
            except GatewayError as exc:
                problems.append(f"{pname}/{public}: explain 失败 {redact(exc)}")
            if not mapping_ok:
                problems.append(f"{pname}/{public}: 映射没解析通（excluded={reasons}）")
            note(f"  {public:26s} -> {upstream:26s} 价格{'一致' if price_ok else '不一致'} "
                 f"能力={'全' if cap_ok else '缺'} 映射={'通' if mapping_ok else '不通'} "
                 f"排除={','.join(reasons) or '无'}")
    if problems:
        note("")
        for p in problems:
            note("  ✗ " + p)
        return 1
    note("")
    note("价格逐字一致，每条都能解析出 canonical 且映射通（授权由真实请求验证）。")
    return 0


def cmd_patch(args, gw: Gateway) -> int:
    """Repair pass: set capabilities on the provider-model rows the import already wrote.

    Needed because the first apply omitted `capabilities`, so the image rows declared nothing and
    image requests could never reach azure (docs/api-images.md §6.3 requires an explicit
    `image_generation`). Prices and mappings are left untouched.
    """
    providers = {p["name"]: p for p in gw.list_all("/admin/api/v1/providers")}
    if AZURE_PROVIDER not in providers:
        die(f"实例上没有供应商 {AZURE_PROVIDER}")
    pid = providers[AZURE_PROVIDER]["id"]
    rows = gw.list_all(f"/admin/api/v1/providers/{pid}/models")
    image_names = set()
    for public, upstream in IMAGE_MODELS.items():
        image_names.add(public)
    changed = []
    for row in rows:
        public = str(row.get("public_model") or "")
        want = IMAGE_CAPABILITIES if public in image_names else TEXT_CAPABILITIES
        caps = row.get("capabilities") or {}
        if isinstance(caps, str):
            try:
                caps = json.loads(caps)
            except json.JSONDecodeError:
                caps = {}
        if caps == want:
            continue
        gw.call("POST", f"/admin/api/v1/providers/{pid}/models",
                {"public_model": public, "capabilities": want})
        changed.append((public, sorted(k for k, v in want.items() if v)))
    for public, keys in changed:
        note(f"  {public:26s} capabilities={','.join(keys)}")
    note(f"修补 {len(changed)} 行 capabilities（价格与映射未动）")
    return 0


# ── 标签：创建与验证 ──────────────────────────────────────────────────────────
def data_request(base: str, token: str, method: str, path: str, body=None, timeout=300):
    """One data-plane call with a bearer key. Returns (status, parsed-or-text)."""
    data = None if body is None else json.dumps(body).encode()
    headers = {"Authorization": "Bearer " + token, "Accept": "application/json"}
    if data is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(base.rstrip("/") + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", "replace")
            try:
                return resp.status, json.loads(raw)
            except json.JSONDecodeError:
                return resp.status, raw
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", "replace")
        try:
            return exc.code, json.loads(raw)
        except json.JSONDecodeError:
            return exc.code, raw
    except Exception as exc:  # noqa: BLE001
        return 0, f"{type(exc).__name__}: {str(exc)[:160]}"


def error_code(payload) -> str:
    if isinstance(payload, dict):
        err = payload.get("error")
        if isinstance(err, dict):
            return str(err.get("code") or err.get("type") or "")
        if err:
            return str(err)
    return ""


def cmd_tags(args, gw: Gateway) -> int:
    """Create the two tags. Refuses to touch an existing same-named tag (POST is an upsert)."""
    existing = {t["name"]: t for t in gw.list_all("/admin/api/v1/tags")}
    clash = [n for n in TAG_NAMES if n in existing]
    if clash:
        die("同名标签已存在，拒绝覆盖（POST /tags 是按名 upsert）：" + "、".join(clash))

    created = []
    for name, grants, desc in TAG_SPECS:
        result = gw.call("POST", "/admin/api/v1/tags",
                         {"name": name, "description": desc, "grants": grants, "priority": 100})
        created.append({"name": name, "id": result.get("id"), "grants": grants})
        note(f"  标签 {name}（id={result.get('id')}）：grants={json.dumps(grants, ensure_ascii=False)}")

    # 读回确认（upsert 返回的体就是落库内容，但仍按读回的值为准）
    after = {t["name"]: t for t in gw.list_all("/admin/api/v1/tags")}
    for item in created:
        live = after.get(item["name"])
        if live is None:
            die(f"标签 {item['name']} 建完后读不回来")
        if live.get("grants") != item["grants"]:
            die(f"标签 {item['name']} 的 grants 与写入不一致：{live.get('grants')}")
        item["id"] = live["id"]
    note(f"标签就位：{len(created)} 个（grants 已读回核对）")
    out = os.path.join(args.data_dir, f"tag-create-{STAMP}.json")
    dump_json(out, {"tags": created, "stamp": STAMP})
    note(f"记录：{out}（0600）")
    return 0


def cmd_verify_tags(args, gw: Gateway) -> int:
    """Mint a key carrying ONLY these two tags, prove the tags are what grant access, then walk
    every model with a real request. The key is disabled and the plaintext file removed at the end
    even if a check fails."""
    tags = {t["name"]: t for t in gw.list_all("/admin/api/v1/tags")}
    missing = [n for n in TAG_NAMES if n not in tags]
    if missing:
        die("标签不存在，先跑 `tags`：" + "、".join(missing))

    providers = {p["name"]: p for p in gw.list_all("/admin/api/v1/providers")}
    for pname in (AZURE_PROVIDER, AZURE_IMAGES_PROVIDER):
        if pname not in providers:
            die(f"供应商 {pname} 不存在")
        if not providers[pname].get("enabled"):
            die(f"供应商 {pname} 未启用")

    plan = {"tags": {n: tags[n].get("grants") for n in TAG_NAMES}, "account_id": VERIFY_ACCOUNT,
            "key_name": VERIFY_KEY_NAME, "negative": {}, "models": [], "checks": [],
            "text_models": TEXT_MODELS_VERIFY, "image_models": IMAGE_MODELS_VERIFY}
    key_id = None
    secret_path = os.path.join(args.data_dir, f"verify-key-{STAMP}.txt")
    try:
        # 1) 只带账户、不带 tags/grants 的 key —— 用来做负向对照
        resp = gw.call("POST", "/admin/api/v1/keys",
                       {"name": VERIFY_KEY_NAME, "account_id": VERIFY_ACCOUNT})
        key_id = resp.get("id")
        token = ""
        for field in ("api_key", "key", "token", "plaintext"):
            if resp.get(field):
                token = str(resp[field])
                break
        if not token:
            die("签发响应里没有明文，无法做数据面验证")
        dump_json(secret_path, {"key_id": key_id, "api_key": token})
        note(f"验证 key 已签发：id={key_id}，account={VERIFY_ACCOUNT}，"
             f"初始 tags={json.dumps(resp.get('tags') or [])}（明文已写 {secret_path}，0600）")

        # 2) 负向对照：此刻它没有任何 grants，default_grant=none ⇒ 必须被拒
        status, payload = data_request(args.base, token, "POST", "/v1/responses",
                                       {"model": TEXT_MODELS_VERIFY[0],
                                        "input": "hi", "max_output_tokens": 16})
        plan["negative"] = {"status": status, "error_code": error_code(payload),
                            "detail": redact(json.dumps(payload, ensure_ascii=False))[:200]}
        note(f"负向对照（无标签）：HTTP {status} {error_code(payload)}")
        if status == 200:
            die(f"没有标签也能调用 {TEXT_MODELS_VERIFY[0]}：该账户/ key 另有授权，本次验证不成立")
        plan["checks"].append({"name": "negative_control", "ok": True})

        # 3) 打上两个标签，读回生效标签
        gw.call("PATCH", f"/admin/api/v1/keys/{key_id}",
                {"tags": TAG_NAMES})
        live = None
        for k in gw.list_all(f"/admin/api/v1/keys?account_id={VERIFY_ACCOUNT}"):
            if k.get("id") == key_id:
                live = k
                break
        if live is None:
            die("打完标签后读不回该 key")
        eff = list(live.get("effective_tags") or [])
        plan["effective_tags"] = eff
        note(f"打上标签后 effective_tags={eff}")
        if sorted(eff) != sorted(TAG_NAMES):
            die(f"生效标签不是预期的两个：{eff}")
        plan["checks"].append({"name": "effective_tags", "ok": True})

        # 4) 目录：9 个模型都应对该 key 可见
        status, payload = data_request(args.base, token, "GET", "/v1/models")
        visible = sorted(m.get("id") for m in (payload.get("data") or [])) if isinstance(payload, dict) else []
        wanted = sorted(TEXT_MODELS_VERIFY + IMAGE_MODELS_VERIFY)
        plan["visible_models"] = visible
        note(f"/v1/models 可见 {len(visible)} 个：{'、'.join(visible)}")
        if visible != wanted:
            die(f"可见模型与预期不符：缺 {sorted(set(wanted) - set(visible))}，"
                f"多 {sorted(set(visible) - set(wanted))}")
        plan["checks"].append({"name": "catalog_visible", "ok": True})

        # 5) 逐个真发请求
        failed = []
        for model in TEXT_MODELS_VERIFY:
            status, payload = data_request(args.base, token, "POST", "/v1/responses",
                                           {"model": model, "input": "reply with the single word: ok",
                                            "max_output_tokens": 32})
            usage = (payload.get("usage") or {}) if isinstance(payload, dict) else {}
            ok = status == 200
            plan["models"].append({"model": model, "provider": AZURE_PROVIDER, "endpoint": "/v1/responses",
                                   "status": status, "ok": ok, "usage": usage,
                                   "error": "" if ok else error_code(payload)})
            note(f"  文本 {model:24s} HTTP {status}  usage={json.dumps(usage)}"
                 + ("" if ok else f"  {error_code(payload)}"))
            if not ok:
                failed.append((model, status, error_code(payload)))

        for model in IMAGE_MODELS_VERIFY:
            status, payload = data_request(args.base, token, "POST", "/v1/images/generations",
                                           {"model": model, "prompt": "a red dot on white",
                                            "n": 1, "size": "1024x1024"})
            bytes_ = 0
            if isinstance(payload, dict) and payload.get("data"):
                item = payload["data"][0] or {}
                bytes_ = len(item.get("b64_json") or "") or len(item.get("url") or "")
            ok = status == 200
            plan["models"].append({"model": model, "provider": AZURE_IMAGES_PROVIDER,
                                   "endpoint": "/v1/images/generations", "status": status,
                                   "ok": ok, "payload_bytes": bytes_,
                                   "error": "" if ok else error_code(payload)})
            note(f"  图像 {model:24s} HTTP {status}  payload={bytes_} 字节"
                 + ("" if ok else f"  {error_code(payload)}"))
            if not ok:
                failed.append((model, status, error_code(payload)))

        plan["checks"].append({"name": "all_models_200", "ok": not failed})
        if failed:
            note("")
            for model, status, code in failed:
                note(f"  ✗ {model}: HTTP {status} {code}")
    finally:
        # 无论成败都要收回权限并抹掉明文
        if key_id is not None:
            try:
                gw.call("PATCH", f"/admin/api/v1/keys/{key_id}", {"status": "disabled"})
                note(f"验证 key {key_id} 已置 disabled")
            except GatewayError as exc:
                note(f"  ! 置 disabled 失败，请手工处理：{redact(exc)}")
        if os.path.exists(secret_path):
            with open(secret_path, "rb") as fh:
                blob = fh.read()
            with open(secret_path, "wb") as fh:
                fh.write(b"\0" * len(blob))
            os.remove(secret_path)
            note(f"明文文件已抹除：{secret_path}")

    out = os.path.join(args.data_dir, f"tag-verify-{STAMP}.json")
    dump_json(out, plan)
    note(f"报告：{out}（0600，无密钥材料）")
    return 0 if all(c["ok"] for c in plan["checks"]) else 1


def load_prices(path: str) -> dict:
    if not path:
        die("需要 --plan-file（official-pricing.sh --plan-out 的输出）")
    if not os.path.exists(path):
        die(f"价表文件不存在：{path}")
    with open(path) as fh:
        data = json.load(fh)
    if not isinstance(data, dict):
        die("价表文件格式不是对象")
    return data


def dump_json(path: str, payload) -> None:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as fh:
        json.dump(payload, fh, ensure_ascii=False, indent=2)


def read_password(args) -> str:
    if args.password:
        return args.password
    env = os.environ.get("GW_ADMIN_PASS")
    if env:
        return env
    if args.password_file:
        with open(args.password_file) as fh:
            value = fh.read().strip()
            if value:
                return value
    cfg = args.config
    if os.path.exists(cfg):
        with open(cfg) as fh:
            in_admin = False
            for line in fh:
                if line.startswith("  admin:"):
                    in_admin = True
                    continue
                if in_admin:
                    if line.strip().startswith("password:"):
                        return line.split("password:", 1)[1].strip().strip('"').strip("'")
                    if line and not line.startswith(("  ", "\t")) or line.strip().startswith(("accounts:", "mode:")):
                        break
    die("缺少管理员口令：--password / --password-file / GW_ADMIN_PASS / config.yaml 的 bootstrap.admin")


def main() -> int:
    parser = argparse.ArgumentParser(description="从 sub2api 的 azure 上游账号导入供应商/模型/价格")
    parser.add_argument("command",
                        choices=["plan", "apply", "verify", "patch", "split", "tags", "verify-tags"])
    parser.add_argument("--base", default=os.environ.get("GW_BASE", DEFAULT_BASE))
    parser.add_argument("--user", default=os.environ.get("GW_ADMIN_USER", "admin"))
    parser.add_argument("--password", default=os.environ.get("GW_ADMIN_PASS", ""))
    parser.add_argument("--password-file", default=os.environ.get("GW_ADMIN_PASSWORD_FILE", ""))
    parser.add_argument("--config", default="/home/operator/work/ai_gateway/config.yaml")
    parser.add_argument("--account", type=int, default=AZURE_ACCOUNT)
    parser.add_argument("--src-ssh", default=os.environ.get("SRC_SSH_HOST", ""),
                        help="源库所在的 ssh 主机（如 gw-b）；给了就把 psql 走 ssh 过去跑")
    parser.add_argument("--plan-file", default=os.environ.get("PLAN_FILE", ""))
    parser.add_argument("--data-dir", default="/home/operator/work/ai_gateway/data")
    args = parser.parse_args()
    if args.src_ssh:
        os.environ["SRC_SSH_HOST"] = args.src_ssh

    gw = Gateway(args.base, args.user, read_password(args))
    gw.login()
    note(f"已登录 {args.base}（{args.user}）")
    if args.command == "plan":
        return cmd_plan(args, gw)
    if args.command == "apply":
        return cmd_apply(args, gw)
    if args.command == "patch":
        return cmd_patch(args, gw)
    if args.command == "split":
        return cmd_split(args, gw)
    if args.command == "tags":
        return cmd_tags(args, gw)
    if args.command == "verify-tags":
        return cmd_verify_tags(args, gw)
    return cmd_verify(args, gw)


if __name__ == "__main__":
    raise SystemExit(main())
