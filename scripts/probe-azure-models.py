#!/usr/bin/env python3
"""probe-azure-models: 判断 azure / Foundry 上游到底部署了哪些模型名。

    python3 probe-azure-models.py gpt-6-luna gpt-5.6-sol gpt-6-astra
    python3 probe-azure-models.py gpt-6-luna gpt-6-sol && python3 add-provider-models.py --provider azure --apply

为什么不能靠目录：该资源的目录接口没有可用信号——
  * `GET <base_url>/models` 返回的是 Foundry 市场目录（几百条，和实际部署无关）；
  * `/openai/deployments` 已下线；
  * 网关自带的 `models/refresh` 对它返回 0（见 provider 页的「刷新」）。
所以只能**真发一次** `POST <base_url>/responses`：**200 = 部署存在**，404 `DeploymentNotFound` = 没有。
（这与 docs/sub2api-migration.md 里 azure 可用性的判定口径一致。）

凭据来源二选一：
  * 默认：同机 sub2api 库里的上游账号（`accounts.credentials` 的 base_url + api_key），
    与 aigw 的 azure 供应商是同一条来源；只经 psql 的输出进内存，不打印、不落盘。
  * 或者直接给 `AZURE_BASE_URL` + `AZURE_API_KEY` 两个环境变量（不经 psql）。

环境变量：AZURE_BASE_URL、AZURE_API_KEY、AZURE_ACCOUNT（默认 10）、PG_CONTAINER（默认 sub2api-postgres）、
          PG_USER（默认 sub2api）、PG_DB（默认 sub2api）、PROBE_TIMEOUT（默认 180 秒）。

退出码：全部 200 → 0；有任何一条不是 200 → 1（方便 `&&` 串到 add-provider-models.py 前面）。

建议同时探一个**已知有部署**的和一个**已知没有部署**的名字作对照（例如 gpt-5.6-sol 与
gpt-6-astra）：只探一个名字时，探针本身写错（密钥、路径、形状）会伪装成「上游没有」。
"""
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

SECRET_HINT = "(密钥不打印)"


def credentials() -> tuple[str, str]:
    base_url, api_key = os.environ.get("AZURE_BASE_URL", ""), os.environ.get("AZURE_API_KEY", "")
    if base_url and api_key:
        return base_url, api_key
    container = os.environ.get("PG_CONTAINER", "sub2api-postgres")
    user = os.environ.get("PG_USER", "sub2api")
    database = os.environ.get("PG_DB", "sub2api")
    account = os.environ.get("AZURE_ACCOUNT", "10")
    sql = f"SELECT credentials::text FROM accounts WHERE id = {int(account)} AND deleted_at IS NULL;"
    out = subprocess.run(["docker", "exec", "-i", container, "psql", "-U", user, "-d", database, "-t", "-A", "-c", sql],
                         capture_output=True, text=True)
    if out.returncode != 0:
        sys.exit("probe-azure-models: psql 读取上游账号失败：" + out.stderr.strip()[:200])
    raw = out.stdout.strip()
    if not raw:
        sys.exit(f"probe-azure-models: sub2api 里没有上游账号 #{account}（或用 AZURE_BASE_URL/AZURE_API_KEY）")
    creds = json.loads(raw)
    return str(creds.get("base_url") or ""), str(creds.get("api_key") or "")


def probe(base_url: str, api_key: str, model: str) -> tuple[int, str]:
    body = json.dumps({
        "model": model,
        "input": [{"type": "message", "role": "user",
                   "content": [{"type": "input_text", "text": "hi"}]}],
        "stream": False,
    }).encode()
    req = urllib.request.Request(
        base_url.rstrip("/") + "/responses", data=body,
        headers={"Authorization": "Bearer " + api_key, "Content-Type": "application/json",
                 "Accept": "application/json"})
    timeout = float(os.environ.get("PROBE_TIMEOUT", "180"))
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            payload = json.loads(resp.read().decode("utf-8", "replace"))
            return resp.status, f"usage={json.dumps(payload.get('usage') or {})}"
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read(400).decode("utf-8", "replace").replace("\n", " ")[:260]
    except Exception as exc:  # noqa: BLE001
        return 0, f"{type(exc).__name__}: {str(exc)[:200]}"


def main() -> int:
    models = [m for m in sys.argv[1:] if m.strip()]
    if not models:
        sys.exit("用法：probe-azure-models.py <model-id> [model-id ...]（例如 gpt-6-luna gpt-6-sol）")
    base_url, api_key = credentials()
    if not base_url or not api_key:
        sys.exit("probe-azure-models: 缺少 base_url 或 api_key")
    print(f"上游 {base_url}（api_key 长度 {len(api_key)}，{SECRET_HINT}）")
    missing = []
    for model in models:
        status, detail = probe(base_url, api_key, model)
        if status != 200:
            missing.append(model)
        print(f"  {model:16s} HTTP {status}  {detail}")
    if missing:
        print("没有部署（不是网关侧配置问题）：" + "、".join(missing))
        return 1
    print("全部有部署：可以跑 add-provider-models.py --provider azure --models " + ",".join(models))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
