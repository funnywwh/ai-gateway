# M10c 设计：codex 健康探测改为真实流式补全

> 计划：M10c（已批准）。本文是设计记录，实现完成后回填第 8 节差异。
> 前置：`docs/design/m10-subscription-adapter.md`（适配器本体）、`docs/design/m10b-codex-egress-proxy.md`（出网代理）。

## 1. 目标与非目标

把 `examples/provider-codex` 的健康探测从 `GET {health_path}`（默认 `/me`）改为**发一次流式 "hi" 真实补全**，
并让探测失败的原因可读。同时修掉本次调研中实测确认、且会继续误导排障的三处缺陷
（`{"detail":…}` 正文被丢弃、Cloudflare 挑战被冒充成凭据失效、`max_output_tokens` 被透传导致 500）。

**为什么必须改**：当前业务请求全部正常，控制台却把该供应商显示为不健康。原因是 `/me` 在该出口被 Cloudflare
挑战（403 + `cf-mitigated: challenge` + HTML），被 `classifyResponse` 归为 `token_expired`——而凭据当时刚刚
刷新成功。一个"假红灯"比没有指示灯更糟：它会训练运维忽略告警。

非目标：不改网关的探测模式体系（仍是 `health|models|info`）；不把探测 deadline 做成配置项；
不透传/兼容 `max_output_tokens` 的开关；不改 `reasoning_effort` 的透传行为；不动内建 provider 的健康探测。

## 2. 实测依据

以下数据来自本次调研（真实代理 + 真实订阅账号 + 真实上游），全部可复现。

| # | 事实 | 影响 |
|---|---|---|
| E1 | `Health` **只在管理面点"测试"时按需调用**（唯一调用点 `internal/runtime/probe.go:101`；全仓库无周期轮询） | "每次探活花钱"不成立，探测可以是一次真实补全 |
| E2 | 探测 ctx deadline 是硬编码 **10s**（`internal/httpapi/admin_providers.go:467`，紧邻 `prober.Probe`） | 真实补全需要放宽 |
| E3 | 流式 "hi" 经代理实测：**TTFB 1.2–4.2s，总耗时 9.6–16.4s**（另有 18.8s 样本） | 等整条流会超出 10s |
| E4 | `max_output_tokens` 在该端点**任何取值都 400**：`{"detail":"Unsupported parameter: max_output_tokens"}` | 探测不能带它；D5 的依据 |
| E5 | 客户端带 `max_output_tokens` 经网关 → **HTTP 500**，正文 `upstream_error: the upstream returned 400 Bad Request`；不带则正常返回 `好` | 端到端确认的可达缺陷 |
| E6 | 上游这类错误用 `{"detail":"…"}`（FastAPI 形状），而 `upstreamMessage` 只认 `{"error":{"message"}}` 与 `{"message"}` | 唯一有诊断价值的正文被丢弃 |
| E7 | `/me` 在该出口被 Cloudflare 挑战（403 + `cf-mitigated: challenge` + HTML），而 `/responses` 不被挑战 | 当前"假不健康"的直接原因 |
| E8 | `/responses` **强制 `stream:true`**；插件 `Complete` 复用 `Stream`，恒以流式发送 | 探测天然满足该约束 |
| E9 | `reasoning.effort:"minimal"` 被上游 400 拒绝；`"low"` 可用但仍 14–17s 且 `reasoning_tokens:0` | 不能靠 effort 压延迟 |
| E10 | `Stream` 在 `io.EOF` 或 `done` 时均返回 nil，**流被中途截断也返回 nil** | 健康判据必须比 `Stream` 更严（D2） |
| E11 | `response.completed` / `response.incomplete` → translate 发 `pluginapi.EventUsage`（**仅当 usage 存在**） | 可观测的终态信号 |
| E12 | HTTP server 未设 `WriteTimeout`（`cmd/aigw/main.go:374-379` 只有 ReadHeader/Read） | 60s 量级探测不会被服务端截断 |

## 3. 关键决策

### D1 探测本体复用 `p.Stream`，不新写请求路径

探测请求 `pluginapi.Request{Model: <probe model>, Input: [user message "<health_prompt>"]}`，`emit` 只做观测。
一次探测即覆盖**代理 → 凭据刷新（含 401 后刷新重试一次）→ 请求构造 → 端点可达 → Cloudflare → 模型受理 →
SSE 解析与翻译**，与真实流量同一条路径；新增代码仅十几行。

**取舍**：探测会消耗极少 token（实测 "hi" 为 13 个输出 token）。因 E1 只在人工点击时发生，接受。

### D2 健康判据：`Stream` 无错 **且** 观测到终态 `EventUsage`

仅凭 `Stream` 是否返回 nil 不够——E10 表明**流被截断也算成功**。既然目标包含"验证流式收尾完整"，
就要求观测到 `pluginapi.EventUsage`（E11）。

未观测到终态 → 返回专用错误 `health_stream_incomplete`（fatal），而不是含糊失败。这样若某天上游完成事件
不带 usage，控制台会直说"流未正常收尾"，而不是又一个无法解释的红灯——**避免重新引入假不健康**。

**取舍**：比"首个事件即健康"慢（E3），但这是选定的深度；deadline 相应放宽（D4）。

### D3 删除 `health_path`，新增 `health_prompt` / `health_model`

端点探测正是 E7 的受害者（且 `/me` 对订阅鉴权是否真实存在都存疑）。**不保留**"便宜的端点模式"：
它无法区分"健康"与"被 Cloudflare 挑战"，留着就是把已知会骗人的路径留在代码里。

`json.Unmarshal` 忽略未知字段，因此删掉配置项不会让带旧键的配置报错（在 README 注明）。
`health_model` 默认取 `cfg.Models[0].ID`，由 `buildRequest` 完成 ID→上游模型映射；
`models` 为空且未设 `health_model` 时返回 `health_unconfigured`，指明是配置缺失而非上游故障。

### D4 探测 deadline 10s → 60s

依据 E3 最差样本 18.8s，再加插件冷启动与首次 token 刷新，60s 留约 3 倍余量；E12 确认服务端不会截断。
代价是控制台点"测试"需等十几秒，`latency_ms` 会如实显示。

不做成配置项：这是一次人工操作的等待，多一个旋钮就多一处自相矛盾的可能（配得比实测还小就回到假不健康）。

### D5 不再向上游透传 `max_output_tokens`

E4 表明该参数恒定被拒、E5 表明它经网关必然 500。改为在 `buildRequest` 丢弃，并在 README 与设计文档写明：
该订阅后端不支持此参数，客户端设的上限**不会**生效，但网关自身的在途额度
（`billing.reservation_mode: max_tokens` / `default_max_output_tokens`）照常计账。

**取舍**：不引入 `forward_max_output_tokens` 开关——本适配器本就专用于该订阅后端，加开关是为假想的可配置性付维护成本。

### D6 错误可读性（探测价值的前提）

- `upstreamMessage` 增加 `detail` 解析，优先级 `error.message` → `detail` → `message`。没有这条，
  探测即使遇到模型配错也只会说 `the upstream returned 400 Bad Request`（E5/E6 的现场）。
- `classifyResponse` 识别 Cloudflare 挑战：`401/403` 且响应头含 `cf-mitigated` 或正文以 HTML 开头 →
  返回 `upstream_challenge`（retryable, 403），**不再**归为 `token_expired`。挑战与凭据无关，
  冒充凭据错误会让人去重刷 token（本次就发生过）；retryable 允许路由在别的出口上故障切换。

### D7 保留 `proxyError()` 前置检查

非法代理仍应 fatal `proxy_invalid`（M10b 行为），探测不得掩盖它。

## 4. 接口

### 4.1 `examples/provider-codex/main.go`

```go
type config struct {
    ...                                          // 既有字段
    // HealthPath 删除
    HealthPrompt string `json:"health_prompt"`   // 新增，默认 "hi"
    HealthModel  string `json:"health_model"`    // 新增，默认 cfg.Models[0].ID
}
```

```go
// Health 发一次最小流式补全作为探活：与真实流量同一条路径，
// 且要求观测到终态 usage —— 被截断的流不算健康。
func (p *provider) Health(ctx context.Context) error

// healthModel 解析探活使用的模型（health_model 优先，否则第一个配置模型）。
func (p *provider) healthModel() (string, error)
```

判据小结：`Health` 返回 nil ⇔ 请求成功 **且** 流正常收尾。

其余改动：`buildRequest` 删除 `MaxOutputTokens` 转发（D5）；`upstreamMessage` 增加 `detail`（D6）；
`classifyResponse` 增加挑战判定（D6）；`configSchema` 删 `health_path`、加 `health_prompt`/`health_model`。

### 4.2 `internal/httpapi/admin_providers.go`

`10*time.Second` → `60*time.Second`（第 467 行，紧邻 `prober.Probe`），附注释引用 E3 的实测耗时。
与同文件其它超时（restart 3s、其它操作 30s/60s）互不影响。

## 5. 数据流

```
控制台点「测试」
  → POST /admin/api/v1/providers/{id}/test        (deadline 60s, D4)
  → dispatcher.Probe → plugin client.Health(ctx)
  → 插件 Health：proxyError 检查 → 选探活模型 → p.Stream(流式 "hi")
       → transport（可能经 proxy）→ 上游 /responses
       → SSE 翻译 → emit(EventUsage)  ← 观测到即视为收尾
  → ok=true，或带可读原因的错误
```

## 6. 异常与边界

1. **上游 400（模型配错等）**：错误带上 `detail` 正文 → 控制台显示可行动信息。
2. **Cloudflare 挑战**：`upstream_challenge`（retryable），不再冒充凭据问题。
3. **凭据真失效**：刷新重试后仍 401/403 → 仍为 `token_expired`（保持不变，这是正确分类）。
4. **流被截断**：`health_stream_incomplete`（fatal），正文说明"流未正常收尾"。
5. **未配置 models 且未指定 health_model**：`health_unconfigured`，指名配置缺失。
6. **代理不可达**：`token_endpoint_unreachable` / `upstream_unreachable`（retryable），保持既有分类。
7. **探测不写入网关的 usage/账本**：`Health` 走插件直连、不经过网关请求管线；上游侧会记极少 token（已接受）。

## 7. 测试策略

`examples/provider-codex/main_test.go`（`newTestProvider` 去掉 `HealthPath`）：

1. `TestHealthSendsAStreamingProbe` —— 探测是 `POST .../responses`、`stream:true`、prompt 为配置值、
   **不含** `max_output_tokens`；上游回 `happyFrames`（含 completed+usage）→ `Health` 返回 nil。
2. `TestHealthRejectsATruncatedStream` —— 上游只回 `response.created` 就关闭 → `health_stream_incomplete`
   （守住 D2，这正是只用 `Stream` 会漏掉的场景）。
3. `TestHealthSurfacesDetailMessage` —— 400 + `{"detail":"Unsupported parameter: max_output_tokens"}`
   → 错误正文包含该 `detail`，而不是 `the upstream returned 400`。
4. `TestHealthDoesNotMistakeAChallengeForCredentials` —— 403 + `cf-mitigated: challenge` + HTML
   → `upstream_challenge` 且可重试，**不是** `token_expired`。
5. `TestHealthWithoutModelsIsExplicit` —— `models` 为空 → `health_unconfigured`。
6. `TestMaxOutputTokensIsNotForwarded` —— `Request.MaxOutputTokens` 有值时上游收到的报文**没有**该字段，且调用成功。
7. `TestHealthStillReportsAnInvalidProxy` —— 非法代理时 `Health` 优先返回 `proxy_invalid`（回归 D7）。

回归：既有 15 个插件用例与全仓库测试保持全绿；`make verify`。

## 8. 实现与设计差异

1. **deadline 的必要性被实测坐实（不是预防性调整）**：新探测在真实环境返回 `ok=true`，`latency_ms = 16601`。
   16.6s > 旧的 10s deadline，所以旧配置下的新实现必然偶发超时——D4 的 60s 是必需项。
   探测从"恒假红（`ok=false` + `token_expired`）"变为 `ok=true` 且 `last_error` 清空。

2. **测试 7 例中只新增了 6 例**：计划里的第 7 例（非法代理时 `Health` 优先返回 `proxy_invalid`）已由 M10b 的
   `TestInvalidProxyIsRejectedAndFailsClosed` 覆盖（该用例本就断言 `Health` 返回 fatal `proxy_invalid`），
   故不重复新增，只确认其继续通过。

3. **`max_output_tokens` 的修复在真实环境验证**：同一请求（带 `max_output_tokens: 64`）修复前是 HTTP 500、
   修复后是 HTTP 200 且正文为 `好`；不带该参数与流式调用同时回归通过。这说明丢弃该参数不是"降级体验"，
   而是把一个必然失败的请求变回可用请求。

4. **`detail` 解析的价值在一次刻意配错中被直接展示**：把模型改成已知失效的 `gpt-5-codex` 后，探测错误为
   `upstream_error: The 'gpt-5-codex' model is not supported when using Codex with a ChatGPT account.`
   ——修复前这条只会是 `upstream_error: the upstream returned 400 Bad Request`。同一句话在本轮调研中
   曾需要手工 curl 才能拿到，现在运维在控制台就能读到。

5. **实测的隔离方式（重要，值得复用）**：验证新二进制不能直接在运行实例上做（它跑的是旧二进制），
   而简单再起一个实例会有真实风险——**refresh_token 轮换**：两个实例若共享同一个有效 refresh_token，
   任一侧刷新都会让另一侧手里的令牌失效。因此隔离实例采用：
   `GW_SERVER_LISTEN=:8099` + `GW_DATABASE_PATH=<库快照>` + `GW_PLUGINS_STATE_DIR=<独立状态目录>`，
   并把凭据换成 **`access_token`-only**（该模式永不调用 token 端点，从机制上排除轮换）。
   库快照用 sqlite 的备份 API 生成，对运行中的 WAL 库安全。

6. **顺带发现一个比本里程碑更严重的运维隐患（已记入 TODO，另立处理）**：
   真实部署里 refresh_token **已被轮换**，而**插件不会把轮换后的凭据回写到数据库**——有效值只存在于
   `$GW_PLUGIN_STATE_DIR/<instance>/session.json`，而数据库/控制台里显示"已设置"的那份是轮换前的、已失效的值。
   后果：状态目录一旦丢失（例如清 `data/`、换实例名、重装插件），供应商会以"凭据已配置"的姿态持续失败，
   而运维手上的凭据看起来是好的。协议里本就有 `notify`（设计用途正是"凭据回写"），插件没有使用它；
   这属于 M10 的设计缺口，不在 M10c 范围内。

7. **运行中的网关仍需重启才能生效**：deadline 改动在网关门关二进制里，沙箱内无法给宿主的实例发信号，
   因此该实例仍跑旧二进制（10s deadline + 旧探测）。重启命令：在宿主终端执行
   `./scripts/local-run.sh restart`。本里程碑的所有实测都在隔离实例（新二进制）上完成。
