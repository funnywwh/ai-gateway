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
- 入口：nginxWebUI 容器（`/root/nginx/nginx.conf`，容器内 `/home/nginxWebUI/nginx.conf`）
  新增 `gpt.iotalking.top` server 块，`location ^~ /aigw/ { proxy_pass http://127.0.0.1:8088; }`
  **不做 rewrite**，关缓冲（SSE），`proxy_read_timeout 3600s`。
  证书复用通配符 `*.iotalking.top`（ACMEdns 签发，含 `gpt.iotalking.top`）。
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
| 公开 IP 路径 `https://gpt.iotalking.top/aigw/...`（`--resolve` 到 47.80.68.113） | TLS 校验通过，全部 200 |
| `make test` / `go vet` / `make build` | 全绿（含新增 `basepath_test.go`） |
| `make ui-base`（node 推导挂载点） | 10 项通过 |

**未完成（域名侧，不在这台机器上）**：`gpt.iotalking.top` 的 A 记录不存在
（权威 NS `dns23/dns24.hichina.com` 返回 NXDOMAIN），因此真实域名还打不开。
域名持有者要在阿里云 DNS 加一条记录后即可用真实域名访问：

| 主机记录 | 类型 | 记录值 | TTL |
|---|---|---|---|
| `gpt` | A | `47.80.68.113` | 默认（10 分钟） |

证书不需要新签：现有的 `*.iotalking.top`（ACMEdns 签发，2026-10-06 到期）已覆盖该子域，
HTTP 的 80 块也已就位（301 跳 443）。

