#!/usr/bin/env python3
"""codex-account: 在 sub2api 与 aigw 之间搬运 ChatGPT(Codex) 账号凭据。

为什么需要它：ChatGPT 的 OAuth **每次刷新都会轮换 refresh_token**，谁最后刷新谁持有唯一
可用的那份。aigw 的 provider-codex 插件刷新后会立刻把新令牌写进自己的
`$state_dir/session.json`，sub2api 则会写回 `accounts.credentials`。两边都定时刷新时，
对方手里那份就会在下一次刷新时报 invalid_grant。

因此这里的定位是「手动搬运工具」，不是同步服务：

  show              看两边现在各自用哪个 refresh_token（只比前缀，不打印明文）
  import            把 sub2api 某个账号的凭据推给 aigw 的某个供应商（并刷新一次验证）
  push-to-sub2api   把 aigw 当前最新的凭据写回 sub2api 的 accounts 行

凭据值只在进程内传递（stdin / psql 变量 / 临时文件 600），绝不打印到 stdout，
因此不会进入终端记录或对话记录。

用法：
  python3 codex_account.py show
  python3 codex_account.py import  --account 1 --provider codex-sub --model gpt-5.6-luna
  python3 codex_account.py push-to-sub2api --account 1 --provider codex-sub
"""
import argparse
import json
import os
import subprocess
import sys

GATEWAY = "http://127.0.0.1:8088/aigw"
PASSWORD_FILE = "/opt/aigw/.admin-password"
PSQL = ["docker", "exec", "-i", "sub2api-postgres", "psql", "-U", "sub2api", "-d", "sub2api"]
STATE = "/opt/aigw/data/plugin-state/{provider}/session.json"
COOKIES = "/tmp/.aigw-codex-account.cookies"


def psql(sql: str) -> str:
    out = subprocess.run(PSQL + ["-t", "-A", "-c", sql], capture_output=True, text=True)
    if out.returncode != 0:
        sys.exit(f"psql failed: {out.stderr.strip()[:400]}")
    return out.stdout.strip()


def psql_stdin(script: str) -> str:
    out = subprocess.run(PSQL + ["-t", "-A"], input=script, capture_output=True, text=True)
    if out.returncode != 0:
        sys.exit(f"psql failed: {out.stderr.strip()[:400]}")
    return out.stdout.strip()


def account_credentials(account_id: int) -> dict:
    raw = psql(f"SELECT credentials::text FROM accounts WHERE id = {int(account_id)} AND deleted_at IS NULL;")
    if not raw:
        sys.exit(f"sub2api 里没有 id={account_id} 的账号")
    return json.loads(raw)


def plugin_session(provider: str) -> dict:
    path = STATE.format(provider=provider)
    if not os.path.exists(path):
        return {}
    with open(path) as fh:
        return json.load(fh)


def login() -> None:
    with open(PASSWORD_FILE) as fh:
        password = fh.read().strip()
    body = json.dumps({"username": "admin", "password": password})
    subprocess.run(["curl", "-sS", "-c", COOKIES, "-o", "/dev/null", "-X", "POST",
                    "-H", "Content-Type: application/json", "-d", body,
                    f"{GATEWAY}/admin/api/v1/auth/login"], check=True)


def api(method: str, path: str, body: dict | None = None) -> dict:
    cmd = ["curl", "-sS", "-b", COOKIES, "-X", method, "-H", "Content-Type: application/json"]
    tmp = None
    if body is not None:
        tmp = "/tmp/.aigw-codex-account.body.json"
        with open(tmp, "w") as fh:
            json.dump(body, fh)
        os.chmod(tmp, 0o600)
        cmd += ["--data-binary", f"@{tmp}"]
    cmd.append(GATEWAY + path)
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, check=True).stdout
    finally:
        if tmp and os.path.exists(tmp):
            os.remove(tmp)
    try:
        return json.loads(out)
    except json.JSONDecodeError:
        sys.exit(f"网关返回了非 JSON：{out[:300]}")


def provider_id(name: str) -> int:
    listing = api("GET", "/admin/api/v1/providers?limit=200")
    for row in listing.get("data", []):
        if row.get("name") == name:
            return int(row["id"])
    sys.exit(f"aigw 里没有名为 {name} 的供应商")


def cmd_show(args) -> int:
    sub = account_credentials(args.account)
    plugin = plugin_session(args.provider)
    sub_prefix = (sub.get("refresh_token") or "")[:8]
    plugin_prefix = (plugin.get("refresh_token") or "")[:8]
    print(f"sub2api account #{args.account}: refresh={sub_prefix or '(无)'} access_len={len(sub.get('access_token') or '')}")
    print(f"aigw   {args.provider}: refresh={plugin_prefix or '(尚无 session.json)'} "
          f"last_refresh={plugin.get('last_refresh_at') or '-'} expires={plugin.get('expires_at') or '-'}")
    if sub_prefix and plugin_prefix:
        print("两边一致" if sub_prefix == plugin_prefix else "两边不一致：下一次刷新会作废其中一份")
    return 0


def cmd_import(args) -> int:
    creds = account_credentials(args.account)
    refresh = creds.get("refresh_token") or ""
    access = creds.get("access_token") or ""
    if not refresh:
        sys.exit(f"账号 #{args.account} 没有 refresh_token：它不是 OAuth(ChatGPT) 账号")
    sealed = {"refresh_token": refresh, "account_id": creds.get("chatgpt_account_id") or ""}
    if access:
        sealed["access_token"] = access
    if creds.get("client_id"):
        sealed["client_id"] = creds["client_id"]

    login()
    # 供应商写接口按 name upsert，`enabled` 省略即保持不变，因此这里只在显式要求时才带上它。
    enabled = {"keep": None, "true": True, "false": False}[args.enabled]
    body = {
        "name": args.provider,
        "kind": "plugin:provider-codex",
        "display_name": f"ChatGPT/Codex 订阅账号 {creds.get('email', '')} ({creds.get('plan_type', '')})".strip(),
        "priority": 50,
        "weight": 100,
        "config": {
            "base_url": "https://chatgpt.com/backend-api/codex",
            "store": False,
            "reasoning_effort": "medium",
            "health_prompt": "hi",
            "health_model": args.model,
            "models": [{"id": args.model, "upstream_model": args.model, "display_name": args.model,
                        "capabilities": {"stream": True, "tools": True}}],
        },
        "credentials": sealed,
        "meta": {"imported_from": f"sub2api account #{args.account}",
                 "source_email": creds.get("email", ""), "plan_type": creds.get("plan_type", "")},
    }
    if enabled is not None:
        body["enabled"] = enabled
    created = api("POST", "/admin/api/v1/providers", body)
    pid = created.get("id")
    print(f"供应商 {args.provider} (id={pid}) 已写入凭据：{created.get('credential_keys')}，enabled={created.get('enabled')}")

    # 让插件自己把这份凭据收进 state 并换一次 access_token：刷新成功才说明凭据真的可用。
    reset = api("POST", f"/admin/api/v1/providers/{pid}/actions/set_token", sealed)
    print("set_token:", json.dumps(reset.get("result", reset), ensure_ascii=False)[:300])
    print(f"再执行一次 `{os.path.basename(sys.argv[0])} show` 比对两边前缀；若 sub2api 那份已过期，"
          f"用 push-to-sub2api 把它同步过去。")
    return 0


def cmd_push(args) -> int:
    plugin = plugin_session(args.provider)
    refresh = plugin.get("refresh_token") or ""
    access = plugin.get("access_token") or ""
    if not refresh:
        sys.exit(f"aigw 的 {args.provider} 还没有 session.json：先 import 一次")
    # 值经过 psql 变量传入，不出现在命令行参数里。
    script = "\\set r '" + refresh.replace("'", "''") + "'\n"
    script += "\\set a '" + access.replace("'", "''") + "'\n"
    script += ("UPDATE accounts SET credentials = jsonb_set(jsonb_set(credentials, '{refresh_token}', "
               "to_jsonb(:'r'::text)), '{access_token}', to_jsonb(:'a'::text)), updated_at = now() "
               f"WHERE id = {int(args.account)} RETURNING id;\n")
    out = psql_stdin(script)
    print(f"已把 aigw 的凭据写回 sub2api account #{args.account}（{out.splitlines()[0] if out else '?'}）")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="cmd", required=True)

    show = sub.add_parser("show", help="比对两边的 refresh_token 前缀")
    show.add_argument("--account", type=int, default=1)
    show.add_argument("--provider", default="codex-sub")
    show.set_defaults(func=cmd_show)

    imp = sub.add_parser("import", help="sub2api -> aigw")
    imp.add_argument("--account", type=int, default=1)
    imp.add_argument("--provider", default="codex-sub")
    imp.add_argument("--model", default="gpt-5.6-luna")
    # Re-importing is how a stale token gets replaced, so the previous state is the
    # default: a provider that was serving traffic keeps serving it after a re-import.
    imp.add_argument("--enabled", choices=("keep", "true", "false"), default="keep",
                     help="导入后供应商的启用状态（默认沿用当前状态）")
    imp.set_defaults(func=cmd_import)

    push = sub.add_parser("push-to-sub2api", help="aigw -> sub2api")
    push.add_argument("--account", type=int, default=1)
    push.add_argument("--provider", default="codex-sub")
    push.set_defaults(func=cmd_push)

    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
