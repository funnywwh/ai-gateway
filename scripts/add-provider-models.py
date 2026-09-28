#!/usr/bin/env python3
"""add-provider-models: 给实例上**一批**供应商补齐指定模型（目录项 + 供应商模型行 + 路由）。

    python3 add-provider-models.py                     # 干跑：全部 plugin:provider-codex，默认两个模型
    python3 add-provider-models.py --apply             # 写入
    python3 add-provider-models.py --provider azure    # 只动 azure（builtin 供应商，没有插件配置那一步）
    GW_BASE=http://127.0.0.1:8088/aigw python3 add-provider-models.py --apply

选供应商的两种方式（二选一）：
  * `--kind`（默认 `plugin:provider-codex`）：按 kind 选**全部**匹配的供应商；
  * `--provider NAME[,NAME]`：按名字选，忽略 kind；名字不存在直接报错。

环境变量：
  GW_BASE                 默认 http://127.0.0.1:8088（实例带 base_path 时要把前缀写进去，如 gw-b 的 /aigw）
  GW_ADMIN_USER/PASS      缺省从 config.yaml 的 bootstrap.admin 读
  GW_ADMIN_PASSWORD_FILE  直接给一份口令文件（gw-b 用 /opt/aigw/.admin-password）
  MODELS                  逗号分隔的模型 id，默认 gpt-6-sol,gpt-6-luna
  GW_KINDS / GW_PROVIDERS 与 --kind / --provider 等价

为什么需要它：scripts/official-pricing.sh 只给**已经存在**的供应商模型行写官方价，它不建行。
而「对客模型在目录里、路由也在、却漏了供应商模型行」正是 /v1/models 里看不到模型的经典成因
（internal/routing/routing.go 判 not_mapped：`pm == nil || !pm.Enabled` 都算没映射，所以**光有路由
是不生效的**）。所以补模型要做三件事：对客模型目录项、供应商模型行、路由；而**行必须当场带上
成本价**——先建行再补价，中间那段时间是按 0 成本计费的。

价格不在本脚本里维护：它调用 `official-pricing.sh --plan-out`（或读 --plan-file）拿到那张表的
原文，这里只有「把规则搬进请求体」的逻辑。调价只需改 official-pricing.sh 一处。

**上游得真有这个东西**（否则写出来的行会在请求时 404）：codex 那种订阅后端可以直接发一次请求探；
azure/Foundry 这类**没有可用目录接口**的供应商请先跑 `scripts/probe-azure-models.py`（真发一次
POST /responses，200 才算部署存在），确认了再跑本脚本。

本脚本刻意不猜外来的东西：
  * 只动 `--kind` / `--provider` 选中的供应商；
  * 已存在的行若 upstream_model 非空且与要补的模型不一致，报错并整批中止，不覆盖；
  * 插件供应商的 config.models 只在缺条目时追加，且提交的是**读回来的整份 config**
    （config 是整体替换，不是部分更新）；改它会让插件进程被停掉、下次请求懒启动
    （internal/runtime/probe.go 的 Dispatcher.Restart），所以输出里显式提示，--skip-config 可跳过；
    builtin 供应商（kind 不以 `plugin:` 开头）没有这一步。
"""
import argparse
import http.cookiejar
import json
import os
import re
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request

DEFAULT_KIND = "plugin:provider-codex"
DEFAULT_MODELS = "gpt-6-sol,gpt-6-luna"
# plugin: 前缀的供应商才有 config.models 这一步；builtin（azure/deepseek/…）只有行与路由。
PLUGIN_KIND_PREFIX = "plugin:"
# 与 examples/provider-codex 现有条目一致：codex 后端支持流式、工具与 reasoning。
CAPABILITIES = {"stream": True, "tools": True, "reasoning": True}
SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(SCRIPT_DIR)

# 只挡完整密钥，不挡 12 字符前缀（前缀是索引，不是秘密）。
SECRET_RE = re.compile(
    r"(sk-[A-Za-z0-9_\-]{13,}|rt\.[A-Za-z0-9._\-]{8,}|eyJ[A-Za-z0-9._\-]{16,}"
    r"|aigw_mcp_[A-Za-z0-9_\-]{12,}|[0-9a-f]{32,})")


def redact(text) -> str:
    return SECRET_RE.sub("<redacted>", str(text))


def die(message: str) -> None:
    sys.exit("add-provider-models: " + redact(message))


def note(message: str) -> None:
    print(redact(message), flush=True)


class GatewayError(Exception):
    pass


class Gateway:
    """管理接口的小客户端：会话 cookie 只在内存里。"""

    def __init__(self, base: str, username: str, password: str):
        self.base = base.rstrip("/")
        self.username = username
        self.password = password
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))

    def login(self) -> None:
        self.call("POST", "/admin/api/v1/auth/login",
                  {"username": self.username, "password": self.password})

    def call(self, method: str, path: str, body=None, timeout: int = 60) -> dict:
        data, headers = None, {}
        if body is not None:
            data = json.dumps(body, ensure_ascii=False).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(req, timeout=timeout) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                message = json.loads(raw).get("error", {}).get("message", raw[:200])
            except Exception:  # noqa: BLE001
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


# ---------------------------------------------------------------------------
# 定价计划（真值来自 official-pricing.sh 的价格表）
# ---------------------------------------------------------------------------


def load_plan(plan_file: str, models: list[str]) -> dict:
    """读定价计划：给了 --plan-file 就用它，否则现场找 official-pricing.sh 导出一份。

    每个要补的模型都必须在计划里有 **catch-all** 规则（when 为空）：没有的话，某些维度上的
    请求会取不到费率；而「白名单之外没有官方价的东西」根本不该被写进供应商行。
    """
    tmp = ""
    if not plan_file:
        # 仓库布局是 <根>/scripts/<两个脚本>；部署形态（/opt/aigw/<脚本>）里两个脚本同目录。
        candidates = [os.path.join(ROOT, "scripts", "official-pricing.sh"),
                      os.path.join(SCRIPT_DIR, "official-pricing.sh")]
        script = next((path for path in candidates if os.path.exists(path)), "")
        if not script:
            die("找不到 official-pricing.sh（或用 --plan-file 直接给定价计划）：找过 " +
                "、".join(candidates))
        fd, tmp = tempfile.mkstemp(prefix="codex-plan-", suffix=".json")
        os.close(fd)
        proc = subprocess.run(["bash", script, "--plan-out", tmp], capture_output=True, text=True)
        if proc.returncode != 0:
            die("official-pricing.sh --plan-out 失败：" +
                redact(proc.stderr.strip() or proc.stdout[-400:]))
        first = next((line for line in proc.stdout.splitlines() if line.strip()), "")
        if first:
            note(first)
        plan_file = tmp
    try:
        with open(plan_file) as fh:
            plan = json.load(fh)
    finally:
        if tmp and os.path.exists(tmp):
            os.remove(tmp)

    for model in models:
        ruleset = plan.get(model)
        if not ruleset:
            die(f"定价计划里没有 {model}：先把它加进 official-pricing.sh 的价格表（不许在这里猜价）")
        if not any(rule.get("when") == {} for rule in ruleset.get("rules") or []):
            die(f"定价计划里 {model} 没有 catch-all 规则：拒绝建行")
        if ruleset.get("currency") != "USD":
            die(f"定价计划里 {model} 的币种是 {ruleset.get('currency')}，预期 USD")
    return plan


def decide(row: dict | None, cfg_ids: set, route: dict | None, model: str, ruleset: dict,
           plugin: bool = True) -> dict:
    """一个 (供应商, 模型) 的动作。只判断，不写；main 里两条分支（干跑 / 写入）共用它。"""
    action = {"row": "create", "row_id": 0, "changes": [], "config": "n/a" if not plugin else "ok",
              "route": "ok", "route_id": 0, "fatal": ""}
    if row is not None:
        upstream = row.get("upstream_model") or ""
        action["row_id"] = row.get("id", 0)
        if upstream and upstream != model:
            action["row"] = "drift"
            action["fatal"] = (f"已有供应商模型行 {action['row_id']} 的 upstream_model={upstream!r}，"
                               f"与要补的 {model!r} 不一致：不覆盖，请人工确认后再跑")
        else:
            if not upstream:
                action["changes"].append("补 upstream")
            if row.get("pricing_rules") != ruleset:
                action["changes"].append("重写规则")
            if not row.get("enabled"):
                action["changes"].append("启用")
            action["row"] = "update" if action["changes"] else "ok"
    if plugin and model not in cfg_ids:
        action["config"] = "append"
    if route is not None:
        action["route_id"] = route.get("id", 0)
        action["route"] = "ok" if route.get("enabled") else "enable"
    else:
        action["route"] = "create"
    return action


def default_config() -> str:
    """CONFIG 环境变量优先；否则仓库布局读 <根>/config.yaml，部署形态读「与脚本同目录」的那份。"""
    if os.environ.get("CONFIG"):
        return os.environ["CONFIG"]
    for candidate in (os.path.join(ROOT, "config.yaml"), os.path.join(SCRIPT_DIR, "config.yaml")):
        if os.path.exists(candidate):
            return candidate
    return os.path.join(ROOT, "config.yaml")


def read_password(args) -> str:
    password = os.environ.get("GW_ADMIN_PASS", "")
    if password:
        return password
    if args.password_file:
        with open(args.password_file) as fh:
            return fh.read().strip()
    if os.path.exists(args.config):
        with open(args.config, encoding="utf-8", errors="replace") as fh:
            lines = fh.readlines()
        in_admin = False
        for line in lines:
            if re.match(r"^\s{2}admin:\s*$", line):
                in_admin = True
                continue
            if in_admin:
                found = re.match(r"^\s+password:\s*(\S+)", line)
                if found:
                    return found.group(1).strip('"')
                if re.match(r"^\S", line):
                    break
    die("缺少管理员口令：设 GW_ADMIN_PASS / --password-file，或让 config.yaml 里有 bootstrap.admin")
    return ""


def row_label(action: dict, skipped_config: bool) -> tuple[str, str]:
    row_text = action["row"] + (f"（{'/'.join(action['changes'])}）" if action["changes"] else "")
    if action["config"] == "n/a":
        config_text = "(不适用)"
    elif skipped_config:
        config_text = "(跳过)"
    else:
        config_text = action["config"]
    return row_text, config_text


def select_providers(gw: "Gateway", kinds: list[str], names: list[str]) -> list[dict]:
    """给了名字就按名字选（忽略 kind，名字缺失即报错）；否则按 kind 选全部匹配的。"""
    all_providers = gw.list_all("/admin/api/v1/providers")
    if names:
        by_name = {p.get("name"): p for p in all_providers}
        missing = [n for n in names if n not in by_name]
        if missing:
            die("实例上没有这些供应商：" + "、".join(missing))
        return [by_name[n] for n in names]
    return [p for p in all_providers if p.get("kind") in kinds]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--apply", action="store_true", help="真正写入（缺省只干跑）")
    parser.add_argument("--models", default=os.environ.get("MODELS", DEFAULT_MODELS),
                        help=f"逗号分隔的模型 id（默认 {DEFAULT_MODELS}）")
    parser.add_argument("--kind", default=os.environ.get("GW_KINDS", DEFAULT_KIND),
                        help=f"逗号分隔的供应商 kind（默认 {DEFAULT_KIND}）；给了 --provider 时忽略")
    parser.add_argument("--provider", default=os.environ.get("GW_PROVIDERS", ""),
                        help="逗号分隔的供应商名字；只动这些（按名字选，忽略 kind）")
    parser.add_argument("--plan-file", default=os.environ.get("GW_PLAN_FILE", ""),
                        help="official-pricing.sh --plan-out 导出的定价计划 JSON")
    parser.add_argument("--base", default=os.environ.get("GW_BASE", "http://127.0.0.1:8088"))
    parser.add_argument("--password-file", default=os.environ.get("GW_ADMIN_PASSWORD_FILE", ""))
    parser.add_argument("--config", default=default_config())
    parser.add_argument("--skip-config", action="store_true",
                        help="不碰插件配置（config.models）：跳过插件进程重启")
    args = parser.parse_args()

    models = [m.strip() for m in args.models.split(",") if m.strip()]
    if not models:
        die("没有要补的模型（--models/MODELS 为空）")
    kinds = [k.strip() for k in args.kind.split(",") if k.strip()]
    names = [n.strip() for n in args.provider.split(",") if n.strip()]

    plan = load_plan(args.plan_file, models)
    note(f"定价计划：{len(plan)} 个上游模型；本次要补：{', '.join(models)}")

    gw = Gateway(args.base, os.environ.get("GW_ADMIN_USER", "admin"), read_password(args))
    gw.login()
    note(f"已登录 {args.base}")

    providers = select_providers(gw, kinds, names)
    if not providers:
        die(f"实例上没有任何 kind 为 {','.join(kinds)} 的供应商")
    note("目标供应商 " + str(len(providers)) + " 个：" +
         "、".join(f"{p['id']} {p['name']}（{p['kind']}）" for p in providers))
    plugin_ids = {p["id"] for p in providers
                  if str(p.get("kind") or "").startswith(PLUGIN_KIND_PREFIX)}
    builtin = [f"{p['id']} {p['name']}" for p in providers if p["id"] not in plugin_ids]
    if builtin and not args.skip_config:
        note("  （builtin 供应商没有 config.models 这一步，只写行与路由：" + "、".join(builtin) + "）")

    catalog = {m.get("public_name") for m in gw.list_all("/admin/api/v1/models")}
    routes = gw.list_all("/admin/api/v1/routes")
    catalog_missing = [m for m in models if m not in catalog]

    work: list[dict] = []
    fatal: list[str] = []
    for provider in providers:
        pid = provider["id"]
        plugin = pid in plugin_ids
        detail = gw.call("GET", f"/admin/api/v1/providers/{pid}")
        detail = detail.get("data", detail)
        config = detail.get("config") or {}
        cfg_ids = {entry.get("id") for entry in (config.get("models") or [])}
        rows = {str(r.get("public_model")): r
                for r in gw.list_all(f"/admin/api/v1/providers/{pid}/models")}
        for model in models:
            route = next((r for r in routes
                          if r.get("model") == model and r.get("provider") == provider["name"]), None)
            action = decide(rows.get(model), cfg_ids, route, model, plan[model], plugin)
            action.update({"provider_id": pid, "provider_name": provider["name"], "plugin": plugin,
                           "model": model, "config_obj": config, "config_ids": cfg_ids})
            if action["fatal"]:
                fatal.append(action["fatal"])
            work.append(action)

    note("")
    note(f"{'供应商':30s} {'模型':12s} {'供应商模型行':26s} {'插件配置':8s} 路由")
    for action in work:
        row_text, config_text = row_label(action, args.skip_config)
        head = f"{action['provider_id']} {action['provider_name']}"
        note(f"{head:30s} {action['model']:12s} {row_text:26s} {config_text:8s} {action['route']}")
    if catalog_missing:
        note(f"对客模型目录：需新建 {', '.join(catalog_missing)}")
    touched = sorted({a["provider_name"] for a in work
                      if a["plugin"] and a["config"] != "ok"})
    if touched and not args.skip_config:
        note(f"注意：{len(touched)} 个供应商的插件配置会被改写（提交整份 config），"
             f"其插件进程会被停掉、下次请求懒启动：{'、'.join(touched)}")
    if fatal:
        for line in fatal:
            note("  ✗ " + line)
        die("有供应商模型行与目标模型冲突：整批中止，一行未写")

    if not args.apply:
        note("")
        note(f"干跑结束（未写入）。加 --apply 写入 {args.base}。")
        return 0

    note("")
    note("开始写入…")
    for model in catalog_missing:
        gw.call("POST", "/admin/api/v1/models",
                {"public_name": model, "display_name": model, "enabled": True})
        note(f"  目录项 {model} 已建")

    for action in work:
        pid, model = action["provider_id"], action["model"]
        if action["row"] == "create":
            gw.call("POST", f"/admin/api/v1/providers/{pid}/models", {
                "public_model": model, "upstream_model": model, "enabled": True,
                "priority": 100, "weight": 100, "context_window": 0, "max_output_tokens": 0,
                "capabilities": CAPABILITIES, "pricing_rules": plan[model]})
            note(f"  provider {pid} {model}：供应商模型行已建（含官方价）")
        elif action["row"] == "update":
            # POST 是部分更新：只有列出的字段会被改，权重/上下文/能力保持原值。
            body = {"public_model": model, "upstream_model": model, "enabled": True,
                    "pricing_rules": plan[model]}
            gw.call("POST", f"/admin/api/v1/providers/{pid}/models", body)
            note(f"  provider {pid} {model}：已更新（{'/'.join(action['changes'])}）")

    if not args.skip_config:
        for pid in sorted({a["provider_id"] for a in work if a["plugin"]}):
            changed = [a for a in work if a["provider_id"] == pid and a["config"] != "ok"]
            if not changed:
                continue
            config = dict(changed[0]["config_obj"])
            entries = list(config.get("models") or [])
            have = {entry.get("id") for entry in entries}
            added = [a["model"] for a in changed if a["model"] not in have]
            for model in added:
                entries.append({"id": model, "upstream_model": model,
                                "capabilities": CAPABILITIES})
            config["models"] = entries
            gw.call("PATCH", f"/admin/api/v1/providers/{pid}", {"config": config})
            note(f"  provider {pid}：config.models 追加 {'、'.join(added)}"
                 f"（插件进程已停，下次请求懒启动）")

    for action in work:
        if action["route"] == "create":
            gw.call("POST", "/admin/api/v1/routes", {
                "model": action["model"], "provider": action["provider_name"],
                "upstream_model": action["model"], "enabled": True, "priority": 100, "weight": 100})
            note(f"  provider {action['provider_id']} {action['model']}：路由已建")
        elif action["route"] == "enable":
            gw.call("PATCH", f"/admin/api/v1/routes/{action['route_id']}", {"enabled": True})
            note(f"  provider {action['provider_id']} {action['model']}：路由 {action['route_id']} 已启用")
    # ── 读回核对 ────────────────────────────────────────────────────────────
    note("")
    note("读回核对…")
    catalog = {m.get("public_name") for m in gw.list_all("/admin/api/v1/models")}
    routes = gw.list_all("/admin/api/v1/routes")
    bad = 0
    for model in models:
        if model not in catalog:
            note(f"  ✗ 对客模型目录里没有 {model}")
            bad += 1
    for provider in providers:
        pid = provider["id"]
        detail = gw.call("GET", f"/admin/api/v1/providers/{pid}")
        detail = detail.get("data", detail)
        cfg_ids = {entry.get("id") for entry in ((detail.get("config") or {}).get("models") or [])}
        rows = {str(r.get("public_model")): r
                for r in gw.list_all(f"/admin/api/v1/providers/{pid}/models")}
        for model in models:
            row = rows.get(model)
            if row is None:
                note(f"  ✗ provider {pid} {model}：行不存在")
                bad += 1
                continue
            problems = []
            if row.get("upstream_model") != model:
                problems.append(f"upstream={row.get('upstream_model')!r}")
            if not row.get("enabled"):
                problems.append("行未启用")
            if row.get("pricing_rules") != plan[model]:
                problems.append("规则与定价计划不一致")
            if pid in plugin_ids and not args.skip_config and model not in cfg_ids:
                problems.append("config.models 里没有它")
            route = next((r for r in routes
                          if r.get("model") == model and r.get("provider") == provider["name"]), None)
            if route is None:
                problems.append("路由不存在")
            elif not route.get("enabled"):
                problems.append("路由未启用")
            if problems:
                note(f"  ✗ provider {pid} {model}：" + "；".join(problems))
                bad += 1
            else:
                note(f"  ok provider {pid} {model}：行 {row.get('id')} + 路由 {route.get('id')} + "
                     f"{len(plan[model]['rules'])} 条规则")
    if bad:
        die(f"读回核对有 {bad} 项不符")
    note("")
    note("完成。价格核对与试算：`scripts/official-pricing.sh --apply`（同一张表：逐行读回 + 试算 + 体检）。")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except GatewayError as exc:  # 管理接口的错误是可读的，不必把栈打给运维看
        sys.exit("add-provider-models: " + redact(exc))
