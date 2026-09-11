# M10 设计：订阅后端参考适配器（`examples/provider-codex`）

> 计划：第四版 §2（已批准）。本文是设计记录，实现完成后回填第 7 节差异。
> 定位：**参考实现**，非官方用法，默认禁用。
> 后续：出网代理（境外出口受地区限制时必需）见 `docs/design/m10b-codex-egress-proxy.md`。

## 1. 目标与非目标

把「订阅制后端」包装成一个可以被网关加载的**插件**：它用订阅凭据换 access token，再以 Responses 语义调用该后端；
网关侧无需任何核心改动，因为插件协议（`pkg/pluginapi`）已经覆盖流式、用量维度、取消与错误分类。

非目标：交互式登录（浏览器/设备码/验证码/MFA 不由网关代做）；绕过上游限流；使用任何未公开端点。

## 2. 为什么是插件而不是内建 provider

内建 provider 在进程内、随二进制发布；订阅后端**非官方、可能随时失效、且涉及服务条款风险**，
必须能独立部署、独立升级、一键停用。插件的子进程模型天然满足这些要求，也复用既有的凭据加密与进程管理。

## 3. 凭据：三种形态与自动刷新

优先级 `refresh_token > session_cookie > access_token`，全部经既有 AES-GCM 凭据通道（写后不回显、不进日志）。

| 模式 | 字段 | 取 token | 续期 |
|---|---|---|---|
| C | `refresh_token`（可选 `client_id`/`token_endpoint`/`token_file`） | POST `token_endpoint`（默认 `https://auth.openai.com/oauth/token`）`grant_type=refresh_token` | 自动 |
| A | `session_cookie`（`__Secure-next-auth.session-token`） | GET `https://chatgpt.com/api/auth/session` | cookie 有效期内自动 |
| B | `access_token` | 直接使用 | 无 |

**状态文件** `$GW_PLUGIN_STATE_DIR/session.json`（0600，临时文件 + rename 原子替换）保存：

```json
{ "mode":"refresh_token", "access_token":"…", "refresh_token":"…",
  "expires_at":"2026-01-01T00:00:00Z", "refresh_token_expires_at":null,
  "last_refresh_at":"…", "last_error":"" }
```

关键规则：
1. **轮换必须落盘**：token 端点可能返回新的 `refresh_token`（旋转），不写回就会在下一次刷新时 `invalid_grant` 断链；
2. **单飞刷新**：互斥锁 + in-flight 标记；N 个并发请求只发一次刷新请求，其余等待结果；
3. **提前量**：启动与每次取用时判断，若 `expires_at - now < 60s`（启动时用 5min）即刷新；收到 401/403 刷新后**只重试一次**；
4. **失败分两类**：`invalid_grant`/400/401 → `fatal`+`token_expired`（提示重新登录并更新凭据）；端点 5xx/网络 → `retryable`（保留旧 token，不判死）；
5. **`token_file` 一次性导入**：读本地文件（例如官方 CLI 的 `~/.codex/auth.json`）中的 `access_token`/`refresh_token`/`account_id`，转存到自己的状态文件后即与源文件解耦。

## 4. 请求与事件翻译

- `Stream`：POST `{base_url}/responses`（默认 `https://chatgpt.com/backend-api/codex/responses`），头：
  `Authorization: Bearer <token>`、`chatgpt-account-id`、`session_id`（有则带）、`Accept: text/event-stream`、`Content-Type: application/json`，
  再叠加 config 的 `headers` 覆写（给上游改协议时留后路）；
- body 由 `pluginapi.Request` 组装成 Responses 形状：`model`、`instructions`、`input`（消息/工具调用原样透传）、`max_output_tokens`、
  `reasoning.effort`（config 默认）、`store`（默认 false，该后端拒绝存储）、`stream: true`；
- 用 `providerkit.SSEReader` 解析，事件映射：

| 上游事件 | 插件事件 |
|---|---|
| `response.output_text.delta` | `text.delta` |
| `response.reasoning_summary_text.delta` / `response.reasoning_text.delta` | `reasoning.delta` |
| `response.output_item.added`（function_call） | `tool_call.start` |
| `response.function_call_arguments.delta` | `tool_call.arguments.delta` |
| `response.completed` / `response.incomplete` | `usage`（含 `input_tokens`/`output_tokens`/`reasoning_tokens`/缓存命中） |
| `response.failed` / `error` | 返回 `*pluginapi.Error`（按 4xx/5xx 分类） |

- `Complete`：**复用 `Stream`** 并在内部聚合（该后端只有流式），保证两条路径的事件翻译只有一份实现；
- 健康检查：~~`GET {health_path}`（默认 `/me`）~~ → **已由 M10c 改为发一次流式 "hi" 真实补全**
  （见 `docs/design/m10c-codex-health-probe.md`）。原端点探测在本机出口被 Cloudflare 挑战，
  会把"出口被拦"误报成 `token_expired`，即一个恒亮的假红灯。

## 5. 交互动作

| action | 入参 | 行为 |
|---|---|---|
| `refresh_session` | — | 强制刷新一次并回报 `{mode, expires_at, refresh_token_expires_at, last_refresh_at}` |
| `whoami` | — | 回报当前状态（不联网），含 `account_id`（来自 config 或 JWT payload）、`expires_at`、`last_error` |
| `set_token` | `{access_token?, session_cookie?, refresh_token?, account_id?}` | 手动替换凭据并落盘（供控制台粘贴） |

JWT 只做**不验签**的 payload 解码，用于取 `exp` 与账号字段；解析失败不影响主流程。

## 6. 错误分类与路由语义

| 上游 | 插件错误 | 路由行为 |
|---|---|---|
| 401/403（刷新后仍失败） | `fatal` + `token_expired` | 不故障切换（换供应商也一样失败是凭据问题） |
| 429 | `quota_exhausted` + `Retry-After`→`reset_at` | 冷却该路由直到 reset |
| 5xx / 网络错误 | `retryable` | 允许故障切换 |
| 400/422 | `fatal` 带上游 error code | 不切换，直接回客户端 |

## 7. 实现与设计差异

1. **`token_file` 是一次性导入**：首次调用 `ensureToken` 时若状态为空则读取该文件，把 `access_token`/`refresh_token`/`account_id`
   转存进 `session.json`，此后与源文件解耦（源文件被删或被 CLI 轮换都不影响）。
2. **静态 access token（模式 B）在 401 后不会静默重试**：`ensureToken(force)` 对模式 B 直接返回
   `token_expired` 并提示「提供 refresh_token 或 session_cookie 才能自动续期」，避免无意义的重试。
3. **`Complete` 只在拿不到上游 usage 时标 `estimated`**：从流里捕获 `response.completed.usage` 后即按真实用量上报，
   不再用字符估算覆盖。
4. **`Health` 的 URL 是 `base_url + health_path`**（默认 `/me`）：因此假后端必须在 base 路径下服务该端点；
   这一点写进了测试夹具注释（首次实测就是被它绊了一下：404 被如实报告为 `health_failed`，而不是假装健康）。
   **该设计已被 M10c 取代**（`health_path` 已删除，探测改为真实流式补全）——真实上游上这个端点既被
   Cloudflare 挑战、其存在性本身也存疑，属"设计时无法预见、只能靠真实调用暴露"的一类差异。
5. **usage 维度切分按设计执行**：缓存命中/未命中拆分、reasoning 单列且从 output 中扣除，实测
   `{input_cache_hit:60, input_cache_miss:40, output:30, reasoning:10}`，与计价引擎的维度口径一致。
6. **`set_token` 只改插件内存中的凭据与状态文件**：宿主下一次推送凭据仍以 Providers 页保存的值为准，
   这样「动作」不会与「配置」产生第二个真源。
7. **已知协议限制（非本插件引入）**：`pluginapi.Item` 没有自定义 JSON 编解码，`Extra` 字段（协议里为前向兼容保留）
   在两个方向上都不会被传递；因此上游 item 的未知字段会被丢弃。已在此记录，未在本轮修改协议。
8. **实测（真实子进程 + 假上游 + 真实网关）**：插件由宿主拉起并握手成功；`/providers/{id}/test` → `ok=true`；
   `whoami` 报 `mode=refresh_token` 与到期时间；流式经网关得到 2 个文本增量 + 1 个思考增量；非流式得到
   `hello from codex` 与完整 usage；账本 cost=116 / charge=174 微美元（1.5× 取整正确），余额 5,000,000 → 4,999,652；
   轮换后的 refresh_token 已落盘；网关无 ERROR 日志。