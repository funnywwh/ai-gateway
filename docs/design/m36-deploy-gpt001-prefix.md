# M36 部署：gpt001 上的 /aigw/ 前缀挂载

> 状态：已落地（2026-09-13）。目标：`https://gpt.iotalking.top/aigw/` 前缀下**完整可用**
> ——控制台、管理 API、OpenAI 兼容数据面、MCP、预览 iframe 全在同一个前缀里。

## 1. 问题

控制台与数据面的路径原先全部写死在根上：

| 位置 | 原值 | 前缀下的后果 |
|---|---|---|
| `internal/httpapi/server.go` 的注册表 | `POST /v1/responses`、`GET /admin/ui/` … | nginx 保留前缀转发时，全部 404 |
| 会话 Cookie 的 Path（`admin.go`） | `/admin` | 浏览器不会把 Cookie 发给 `/aigw/admin/api/v1/...`，登录「成功但立刻失效」 |
| 控制台 `js/api.js` 的 `BASE` | `/admin/api/v1` | 所有 XHR 打到前缀之外 |
| 预览 iframe 的 `src`（服务端返回的 `url`） | `/admin/chat-artifact/<id>` | 沙箱 frame 出了挂载点，404 |

反代侧有两条路：**rewrite 掉前缀**（`location /aigw/ { proxy_pass http://127.0.0.1:8088/; }`）或
**保留前缀**。rewrite 看似省事，但服务端生成的路径（Cookie Path、301、预览 URL）依然指向前缀之外，
必须在 nginx 里逐个 `proxy_cookie_path`/`sub_filter` 打补丁——那些补丁只在生产生效，
本地永远测不到，属于「同一份代码两条路径」。

## 2. 设计

**前缀是部署事实，不是请求头。** 服务端从配置读 `server.base_path`：

```
config.server.base_path: "/aigw"      # 空 = 挂在根上（默认，行为与改动前完全一致）
```

- `withBasePath` 在 handler 链最外层把前缀**剥掉一次**（只认段边界：`/aigw` 不吞 `/aigw-other`），
  因此 mux 里的 pattern 一律仍写成根路径，路由表不需要知道前缀存在。
- 剥完之后，Cookie Path、301 的 Location、预览 URL 都用 `s.url(path)` 拼回前缀。
- 前缀之外的请求在这里直接 404（能走到这个进程的只有「保留前缀转发」的代理，
  以及被它拒绝的旁路请求）。

**控制台从自身 URL 推导挂载点**（`internal/webui/static/js/base.js`）：
资产 URL 的形状恒为 `<mount>/admin/ui/js/<file>.js`，所以挂载点就是最后一个 `/js/` 之前的部分。
`api.js` 的 BASE、CSV 导出、备份下载、预览 iframe 全部改成 `consolePath()`/`apiRoot()`。
一份构建同时服务 `/admin/ui/` 与 `/aigw/admin/ui/`，浏览器不需要被告知前缀。

## 3. 部署形态（gpt001 = 47.80.68.113，Alibaba ECS ap-southeast-6）

- 二进制：`/opt/aigw/aigw`（`make build` 产物，`CGO_ENABLED=0` 静态）
- 配置：`/opt/aigw/config.yaml`（600，`secret_key`/`credentials_key` 由 `openssl rand` 在本机生成）
- 数据：`/opt/aigw/data/aigw.db`（WAL）、`data/backups/`（每日备份，默认 cron）
- 服务：`/etc/systemd/system/aigw.service`，`Restart=always`，只监听 `127.0.0.1:8088`
- 入口：nginxWebUI 容器（`/root/nginx/nginx.conf`，容器内 `/home/nginxWebUI/nginx.conf`）。
  `/aigw/` 的路由只写一份，放在 **`conf.d/aigw-location.conf`**，由需要它的 vhost `include`
  （`^~ /aigw/` 前缀匹配优先于各站自己的 `location /`，所以同名的其余路由原样保留）：

  ```nginx
  location = /aigw      { return 301 /aigw/; }              # 裸前缀
  location = /aigw/     { return 302 /aigw/admin/ui/; }
  location ^~ /aigw/ {                                       # 不做 rewrite
    proxy_pass http://127.0.0.1:8088;
    proxy_buffering off;          # SSE：/v1/responses?stream=true、控制台问答
    proxy_read_timeout 3600s;
    ...
  }
  ```

  两个 vhost 都 include 它：**`gpt.iotalking.top`**（本次新增的 server 块）与
  **`mnl.iotalking.top`**（这台机器上原本就有的站，`/` 仍走 sub2api:8080、
  `/nginxwebui/` 仍走 nginxWebUI:9000，`/aigw/` 之后归网关）。
  证书复用通配符 `*.iotalking.top`（ACMEdns 签发，覆盖这两个名字）。
- 裸前缀 `/aigw` → `/aigw/` → `/aigw/admin/ui/`。

## 4. 验收（2026-09-13，实测）

| 检查 | 结果 |
|---|---|
| `GET /aigw/healthz` | 200（`/healthz` 404：前缀之外不认） |
| `GET /aigw/admin/ui/`、`js/api.js`、`js/base.js` | 200 |
| 登录 `POST /aigw/admin/api/v1/auth/login` | 200，`Set-Cookie: aigw_admin=...; Path=/aigw/admin` |
| 带该 Cookie 的 `/aigw/admin/api/v1/auth/me`、`/stats`、资源 CRUD | 200 |
| `POST /aigw/v1/responses`（非流式 / `?stream=true`） | 200，SSE 逐帧到达 |
| `/aigw/v1/models` | 200（带 key）/ 401（不带） |
| 控制台会话 `POST /aigw/admin/api/v1/chat/sessions/{id}/turns` | SSE：turn → … → done |
| 预览附件 `POST .../artifacts` | 返回 `url=/aigw/admin/chat-artifact/<id>`，带 ticket 取回 200 |
| **真 DNS + 真证书**：`https://mnl.iotalking.top/aigw/...` | 控制台 200、登录 200（`Path=/aigw/admin`）、`/auth/me`、`/stats`、`/requests`、`/providers` 全 200、`/v1/models` 200、`/v1/responses` 返回正确文本、`ssl_verify=0` |
| `https://gpt.iotalking.top/aigw/...`（`--resolve` 到 47.80.68.113） | TLS 校验通过，全部 200（等 DNS 记录落地即可用真名访问） |
| 同机既有站的回归：`mnl.iotalking.top` 的 `/`、`/nginxwebui/`、`/hiddify`；`dsh`、`ng` 两个 vhost | 全部 200，未被 `/aigw/` 影响 |
| `make test` / `go vet` / `make build` | 全绿（含新增 `basepath_test.go`） |
| `make ui-base`（node 推导挂载点） | 10 项通过 |

**两个域名两条 DNS 记录，现状不同**（都在阿里云 DNS，权威 NS `dns23/dns24.hichina.com`）：

| 名字 | 当前 A 记录 | 状态 |
|---|---|---|
| `mnl.iotalking.top` | `47.80.68.113` | 已存在，实测可访问（真 DNS + 真证书） |
| `gpt.iotalking.top` | 无（NXDOMAIN） | **待域名持有者添加** `gpt → 47.80.68.113` |

证书不需要新签：现有的 `*.iotalking.top`（ACMEdns 签发，2026-10-06 到期）已覆盖两个名字，
HTTP 的 80 块也已就位（301 跳 443）。


## 5. 运维要点

- **改代码后怎么上线**：`make build` → `scp bin/aigw gpt001:/tmp/aigw.new` →
  `install -m0755 /tmp/aigw.new /opt/aigw/aigw` → `systemctl restart aigw`。
  重装前 `/opt/aigw/aigw` 会被复制成 `aigw.prev-<时间戳>`，回滚就是一次 `mv`。
- **前缀改回根路径**：把 `/opt/aigw/config.yaml` 的 `server.base_path` 改成 `""` 并重启，
  同时删掉 nginx 的 `location ^~ /aigw/`（服务端不会再把任何路径指向前缀）。
- **日志与排障**：`journalctl -u aigw -f`；数据面 `/aigw/healthz`、`/aigw/readyz`、`/aigw/metrics`。
- **备份**：`backup.enabled=true`，快照落在 `/opt/aigw/data/backups/`，控制台可下载/恢复。
- **证书风险（值得盯一眼）**：通配符 `*.iotalking.top` 用的是 ACMEdns（`auth.nginxwebui.cn`）
  验证，2026-10-06 到期，续期依赖那个第三方校验服务；它不是本仓库的一部分，
  如果续期失败，`gpt`/`dsh`/`ng` 三个 vhost 会一起到期 —— 到期前一周确认 acme.sh 续期日志。
- **本机 /etc/hosts 的坑**：这台工作站的 `/etc/hosts` 里有 `103.59.145.127 gpt001.iotalking.top`
  这条手工记录（那个 IP 已下线，公网 DNS 里 `gpt001.iotalking.top` 指向 47.80.68.113），
  会让 `curl gpt001.iotalking.top` 静默连到一台不存在的机器。

## 6. Codex 账号导入（sub2api → aigw）

**来源**：同一台机器上跑着的 sub2api（`/opt/sub2api`，docker compose + postgres）。
它的 `accounts` 表里存着 4 个 openai 账号：1 个 ChatGPT OAuth 订阅号
（`funnywwh@gmail.com`，plan `prolite`）、2 个已停用的 OAuth 号（同邮箱的第二份、另一个邮箱的失效号，
后者 401 `authentication token has been invalidated`）、1 个第三方中转的 apikey 号。这里只导入第 1 个。

**做法**：用官方示例插件 `examples/provider-codex`（订阅型 Responses 后端 + OAuth 刷新），
构建后放到 `/opt/aigw/plugins/provider-codex`，供应商 kind 写 `plugin:provider-codex`：

| 项 | 值 |
|---|---|
| 供应商 | `codex-sub`（id=2，priority 50，enabled） |
| 凭据（sealed） | `refresh_token` / `access_token` / `account_id` / `client_id` |
| 配置 | `base_url=https://chatgpt.com/backend-api/codex`、`store=false`、`reasoning_effort=medium`、模型 `gpt-5.6-luna` |
| 对客模型 | `gpt-5.6-luna`（id=5，enabled，售价 `cost_follow` + `markup_bp=0`） |
| 路由 | `gpt-5.6-luna` → `codex-sub`（id=2，priority 10） |
| 凭据状态 | `/opt/aigw/data/plugin-state/codex-sub/{credentials,session}.json`（0600） |

**实测**：`refresh_session` 真换到 access_token（有效期 2026-09-23）；健康探测（真实流式补全）
`ok=true`、latency 2.5s；`POST /aigw/v1/responses` 非流式与 `stream=true` 都返回正确文本与 usage；
公网域名 `https://mnl.iotalking.top/aigw/v1/responses` 同样通过。出网没有被墙：
这台机器直连 `chatgpt.com` 的 `/backend-api/*` 正常（首页 403 `cf-mitigated: challenge` 是 Cloudflare
对浏览器的挑战，接口不受影响）。

### 6.1 refresh_token 轮换：这是本方案唯一需要人工维护的点

ChatGPT 的 OAuth **每次刷新都轮换 refresh_token**，谁最后刷新谁持有唯一可用的那份：

- aigw 的插件刷新后写自己的 `session.json`；
- sub2api 刷新后写回 `accounts.credentials`。

两边独立刷新时，对方那份会在下一次刷新时报 `invalid_grant`。本次导入过程中真的发生了
（sub2api 原本是 `rt.1.AAD…`，插件刷成了 `rt.1.AAA…`），当时已把新令牌写回 sub2api 让两边一致。
**按用户决定不装定时同步**，因此约定为「坏了手动重导」。

工具：`scripts/codex-account.py`（服务器上 `/opt/aigw/codex_account.py`，700）。

```sh
python3 /opt/aigw/codex_account.py show                              # 比对两边 refresh_token 前缀
python3 /opt/aigw/codex_account.py import --account 1 --provider codex-sub   # sub2api → aigw（沿用启用状态）
python3 /opt/aigw/codex_account.py push-to-sub2api --account 1 --provider codex-sub  # aigw → sub2api
```

凭据值只在进程内传递（psql 变量 / 600 临时文件），不打印到 stdout，所以不会落到对话或日志里。
`import` 之后建议再跑一次 `push-to-sub2api`：`import` 里的 `set_token` 会真刷新一次，
刷新后的令牌以 aigw 这边为准。控制台里对应的入口是供应商详情页的
`whoami` / `refresh_session` / `set_token` 三个动作。

### 6.2 已知取舍

- **计费**：订阅号没有按 token 的公开价格，`cost_follow` 记下的是 0 成本；
  想让控制台按「订阅折算价」看账，得在该模型上配绝对规则（`pricing.md` 的口径）。
- **条款风险**：`examples/provider-codex` 的 README 已写明，这是非官方后端、
  可能随时失效、且可能与上游条款冲突——要留要撤由账号持有者判断。
- **同一账号被两个系统共用**：sub2api 的调度与 aigw 的请求会共享同一份订阅额度。
