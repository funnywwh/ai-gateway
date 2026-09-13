# ai-gateway

类 OpenRouter 的 AI 网关：对外提供 OpenAI **Responses API** 兼容接口，对内把模型路由到多个供应商，
供应商以 **Go 插件（独立子进程 + stdio JSON 帧）** 提供，支持 API Key 分发与标签授权、完整计量与计费、
MCP 查询与后台操作、内容录制与 hooks、模型自由映射、数据库自动备份。

## 状态

计划中的里程碑 M0–M16 已全部落地并通过验证（M10 订阅后端参考适配器、M14 客户自助门户为计划内的可选项，未做）。
M17 完善了内置 `openai-chat` 供应商（思考模式、思考内容续接、DeepSeek 适配），不引入新插件。
M18 让控制台把「每个供应商类型支持哪些配置、密钥填哪一栏」直接讲清楚（内建 kind 自带字段说明，插件走 handshake），
并为无 node 环境补上真实浏览器里的控制台走查（`make ui-check`）。
M21 让 MCP 从"只读查询"变成"可执行全部后台接口"：令牌分 query / admin_read / admin 三档 scope，
agent 通过 admin_endpoints → admin_describe → admin_request 三个入口（渐进披露）驱动整张管理面。
M25 给请求日志补上写入兜底与保留期：内容写失败时退化为「无正文骨架行」（请求仍可见，失败/丢弃计数在
`/stats` 与 `/metrics`），`recording.retention_days` 从死配置变成真正的每日清理（手动端点
`POST /admin/api/v1/requests/prune`），存储响应与它同一个窗口到期。
M26 把审计写入搬出请求路径：存储响应与请求日志由后台线程成批提交（默认 250 ms / 256 请求一个事务），
写路径复用预编译语句，队列有界并在满时施加背压。实测在 8088 上每请求 CPU 2.57ms → 0.82ms、
p90 480ms → 17ms；旧实例上「1.2 MB 请求写失败退骨架行」的那类丢正文，修复后 42/42 完整落库。
M23 把「请求日志只留用户输入」定为默认口径：输入通道分 full / user / metadata / off 四档，默认 `user`
只保留用户自己写的输入（系统指令、工具定义与工具输出只留计数），思考与最终输出仍默认不落；同时把配额
策略的口径收敛成一个扁平形状并在写入时校验，`monthly_*` 如实标注为「已解析、未执行」。
M27 给请求日志补上身份维度与消耗度量：客户端（dsh/codex）、模型（请求名与路由名两个身份）、
工作区、会话、调用类型与会话标题在请求解析后立即提取并落列——与正文口径无关（`record_input=off` 也记），
因此不必再翻那份会截断、会过期的正文；token 与成本按 request_id 从计量表关联，与账单同源；
控制台可按这些维度筛选，并有 top-N 维度统计卡，三条 `(维度, created_at, id)` 索引让筛选查询
在 EXPLAIN 下仍无排序器（实测插入页写放大 1.13×）。
维度统计现已支持[小时汇总与实时补算](docs/design/request-dimension-rollups.md)：完整历史小时读取
汇总，当前小时、边界及失效区间读取明细；保留八维筛选与分页，请求数按请求去重，日志清理后立即生效。
M32 新增控制台的「智能问答」与「技能库」：在控制台里选模型（+ 计费账户/API Key）后与模型对话，
模型通过 MCP 的三个后台入口（admin_endpoints → admin_describe → admin_request）查询与操作网关；
每一步模型调用都是一条正常的计费请求（客户端记为 `console`），但**问答正文、技能内容、模型输出与
工具结果不进入全局请求日志/hooks**——技能按登录管理员隔离，而请求日志是全管理员可见的。
**智能问答本身就是一个 MCP 客户端**：会话绑定一个 MCP 令牌，工具面与可写范围完全由该令牌的 scope
决定（`query` 只读本账户、`admin_read` 后台只读、`admin` 等同管理员凭据），每次工具调用都走同一个
`POST /mcp`，撤销令牌后正在进行的会话立即失效。回答支持 Markdown、图表（`chart` JSON → 本地 SVG，
可导出 CSV/SVG）与 HTML5 预览（短时票据 + 强制沙箱，默认禁止外部资源与网络）。
M34 让那个 HTML5 页面**可以交互**：模型需要用户提供信息时构造一张表单，用户填写提交后事件以结构化
数据回到同一个会话（成为一条正常计费的提问），模型据此继续执行，回答边生成边回灌页面并**原地更新**
（已填内容不丢）。通道是 `MessageChannel` + 服务端注入的桥接脚本（每响应随机 token 与 CSP nonce），
票据分「只读 / 可交互」两种 scope，因此 `?bridge=1` 不能自行升权；沙箱未放松（无 `allow-same-origin`、
无 `allow-forms`、默认无网络），表单提交由脚本拦截而非导航。
M35 给"模型要问用户几个字段"这件最常见的事一条**更轻的路**：模型输出 `form` 代码块（一份字段规格
JSON），控制台**用自己的元素**把它渲染成对话气泡里的表单——就地填写、就地提交、回答就地原地更新，
不需要点「预览」、也不需要沙箱。模型只提供数据，标记与脚本一律不进控制台（`chat_form.js` 与桥接文件
受同一条无 `innerHTML` 禁令约束），凭据类字段被明确拒绝。提交复用既有的轮次端点，因此计费、幂等、
请求日志都是同一条路径、服务端零改动；沙箱那条链路保持不变，需要自由排版或页面脚本的模型仍走 `html`。
勾选技能之后，输入框**留空也能直接发送**：技能本身就是指令，那一轮会带着「按本会话已加载的技能执行」
发出（服务端对同一个动作另有兜底文案）；没有可执行的技能时空文本框点「发送」不发出任何请求，
只在界面上说明原因——一次点击就是一次计费调用。
M39 让「线上跑的是哪个版本」有一个能读的答案：版本号的真值是仓库根的 `VERSION`（`a.b.c`，不再是
`git describe` 给的 commit 前缀），构建时连同 revision 一起注入二进制，由 `GET /version` 与 `/healthz`
对外暴露，并在控制台左上角 `AI Gateway` 旁显示为 `v0.1.0 56df9b5`。同一里程碑修掉一个把供应商打挂的
缺陷：`openai-chat` 过去把 `config.response_format` **无条件**写进每个上游请求，于是声明 `json_object`
会把所有流量变成 JSON 模式，DeepSeek 对任何不含 "json" 的提示词直接 400（线上 DSH 选 `deepseek-flash`
首轮即失败）。现在档位只由请求的 `text.format` 决定，配置退回它文档里本来就写的"能力申报"语义，
且 `json_object` 与 `json_schema` 在路由层按级校验。
当前规模：约 4 万行 Go + 原生前端；26 个测试包、1 个分层断言包；真实二进制端到端走查覆盖数据面、计费、MCP、备份与控制台。

## 文档

流程约定：每个里程碑**先写设计文档并在对话中输出确认**，面向使用者的规格文档**同样先行**（spec first），
详见 `docs/PROCESS.md`。

| 文档 | 内容 | 状态 |
|---|---|---|
| `docs/PROCESS.md` | 实现流程约定（设计文档 / todo / 提交自检） | 生效中 |
| `docs/TODO.md` | 里程碑 → 任务 → 验收 的检查清单（随实现勾选） | 持续更新 |
| `docs/architecture.md` | 分层、模块边界与可替换扩展点 | 已落地（由 `internal/arch` 断言守护） |
| `docs/design/` | 每个里程碑的设计文档（接口、数据流、决策、异常、测试策略、实现差异） | M0–M39 全部产出（M28 的决策记在 `docs/api-providers.md` 与 `docs/TODO.md`） |
| `docs/plugin-protocol-v1.md` | 插件协议 v1（帧/方法/事件/取消/背压/错误分类） | **已实现（M2）** |
| `docs/api-responses.md` | Responses 兼容面（端点/字段/SSE 事件/错误封装/认证与限速） | **已实现（M5）** |
| `docs/api-providers.md` | openai-chat 供应商（配置开关、思考模式、用量维度、错误分类、DeepSeek 接入） | **已实现（M17）** |
| `docs/provider-ui.md` | 控制台如何展示供应商配置说明（字段语义、配置与凭据两条通道、密钥录入路径与优先级、排障） | **已实现（M18）** |
| `docs/routing.md` | 路由解析、候选过滤、负载均衡策略、熔断与冷却、**会话粘性** | **已实现（M3，+M38 粘性）** |
| `docs/pricing.md` | 计量维度 × 有序价格规则集（分时/分档/分维度）；多币种（模型级币种 + 汇率换算） | **已实现（M11a / M22）** |
| `docs/billing.md` | 计量、账本、在途额度、账单、充值、对账、赠送到期；账本币种与显示币种 | **已实现（M11/M12/M22）** |
| `docs/mcp.md` | MCP 服务（11 个查询工具 + 3 个后台工具、令牌 scope、渐进披露、审计、内容可见性、stdio） | **已实现（M6 + MCP-2 + M21）** |
| `docs/backup.md` | 一致点备份、保留、校验与两阶段恢复 | **已实现（M16）** |
| `docs/request-log.md` | 请求日志：录制通道矩阵、身份维度（客户端/模型/工作区/会话/标题 + 用户（账户）/API Key）、消耗口径、查询与统计（含列表底部的本页汇总行）、保留期与写入兜底 | **已实现（M27、M29、M30）** |
| `docs/chat.md` | 控制台智能问答与技能库：计费与隐私口径、按 MCP 令牌 scope 决定的权限边界、Markdown/图表/HTML5 预览、**内联表单（气泡内渲染 → 提交 → 模型原地更新）**、**可交互沙箱页面（生成表单 → 用户提交 → 模型继续执行）**、技能私有性、**勾选技能后留空直接发送**、中断恢复、配置与排障 | **已实现（M32 + M34 + M35）** |

## 构建

本仓库构建必须使用工作区本地工具链环境（`$HOME` 只读、无 CGO）：

```sh
source scripts/goenv.sh
make build      # 产出 bin/aigw
make test       # 单元测试
make verify     # vet + test + build
```

## 运行

```sh
./bin/aigw --version
./bin/aigw --config config.yaml
```

配置项见 `config.example.yaml`；管理控制台在 `http://<listen>/admin/ui/`（首次用 bootstrap 里的管理员账号登录）。

## 常用入口

| 命令 / 路径 | 用途 |
|---|---|
| `POST /v1/responses` | Responses API（流式 / 非流式） |
| `GET /v1/models` | 可用模型与**对客售价** |
| `GET /version` | 构建身份：`{"version":"a.b.c","revision":"<短 sha>"}`（公开，带 `base_path` 前缀） |
| `GET /healthz` | 存活探针（同样带 `version` 与 `revision`） |
| `POST /mcp` | MCP 服务（账户查询 + 按 scope 可执行后台接口，`aigw_mcp_` 令牌） |
| `bin/aigw mcp-serve --account <name>` | 本地 stdio MCP（复用同一套工具） |
| `/admin/api/v1/*` | 管理面：账户/Key/标签/供应商/模型/路由/映射/定价/账单/充值/对账/备份/审计 |
| `/admin/ui/*` | 内置控制台（零构建，随二进制发布） |
| `scripts/load.sh` | 一键压测（自建 `cmd/loadgen`，输出 rps 与分位延迟） |
| `scripts/deepseek-smoke.sh` | DeepSeek 接入走查（离线假上游；加 `--live` 与 `DEEPSEEK_API_KEY` 打真机） |
| `scripts/verify-m34.sh` | 可交互预览的自查（29 项，真实 HTTP、不产生模型费用、结束自动清理）；`GW_ADMIN_PASSWORD=… scripts/verify-m34.sh` |
| `scripts/ui-harness/run.sh`（`make ui-check`） | 控制台走查：API 快照 + headless firefox 渲染真实页面并断言（无 node 环境下的 UI 验证手段） |
| `scripts/format-smoke.sh` | `response_format` 语义走查：真实二进制 + 假 DeepSeek，逐条读上游收到的请求体（无网、无 key、不花钱） |
| `scripts/ui-badge-test.mjs`（`make ui-base`） | 左上角版本角标的 node 断言（三行 DOM shim，不需要浏览器）：两格内容、revision 为 `none` 时不显示、端点读不到时不报错 |
| `scripts/release.sh`（skill `release-version`） | 发版：升 `VERSION`（a.b.c）→ 提交打 tag → `make build`；用法 `scripts/release.sh patch/minor/major` |
| `make verify` | vet + 全量测试 + 控制台 node 断言 + 构建 |

## 压测基线（本机 i7-12700K，testecho 供应商）

```
32 并发 × 10s：619.6 rps，p50 1.73ms，p95 44.5ms，p99 1.73s
结算：usage 行与账本 charge 行逐条对齐，四条不变量 ok，无兜底文件
微基准：pricing.Evaluate 4.2µs，routing.Plan 4.7µs（并行 4.0µs）
```

细节与两处压测发现的缺陷见 `docs/design/m13-performance.md`。
