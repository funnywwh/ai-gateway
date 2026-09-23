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
M81–M83 把 `user` 档收到最紧：正文就是**最新一条「人说的话」的纯文本**——从末尾往回找，跳过客户端
**样板**（`Current runtime context.` 快照、`<environment_context>`、两个 agent 自己的系统提示、
`<system-reminder>`、`<skills_instructions>`、标题调用的提示词）与空消息，取第一条不超过
`recording.input_max_chars`（默认 2000，只是防呆上限）的消息，**整条原样**保留；一条都没有该行就没有正文。
样板必须靠**标记**而不是长度来判定：真实提问常常比短的样板长、比长的样板短——2026-09-23 一次现场就是某浏览器
DSH 租户的提问 117–521 字符、末尾 runtime 快照 542 字符，在「100 字符才留」的口径下它的请求日志**每条都是空的**。
系统指令、工具定义、工具输出、历史 assistant 轮次与图片一概不落库；**要保留全部**（整份原样 JSON 正文，
不受任何限制）就把该 Key 的输入录制切成 `full`。`metadata`/`off` 与控制台流量不受影响，历史行不重填；
控制台详情对没有正文的行会写明「未保留：只有样板或超长用户消息」。
M27 给请求日志补上身份维度与消耗度量：客户端（dsh/codex）、模型（请求名与路由名两个身份）、
工作区、会话、调用类型与会话标题在请求解析后立即提取并落列——与正文口径无关（`record_input=off` 也记），
因此不必再翻那份会截断、会过期的正文；token 与成本按 request_id 从计量表关联，与账单同源；
控制台可按这些维度筛选，并有 top-N 维度统计卡，三条 `(维度, created_at, id)` 索引让筛选查询
在 EXPLAIN 下仍无排序器（实测插入页写放大 1.13×）。
维度统计现已支持[小时汇总与实时补算](docs/design/request-dimension-rollups.md)：完整历史小时读取
汇总，当前小时、边界及失效区间读取明细；保留八维筛选与分页，请求数按请求去重，日志清理后立即生效。
M53 补上**供应商维度**：同一模型挂多家供应商时各自计价，成本按计量行的 `provider_id` 归属到实际服务的
那家（故障转移的请求在两家各计一次，故各分组请求数之和可能大于窗口总数）；列表/详情带供应商名、
可按供应商筛选，控制台与 MCP 都能按供应商统计成本——该维度恒读原始计量行，因为小时汇总按请求预聚合、
已经丢掉了供应商（见[设计](docs/design/m53-request-provider-dimension.md)）。
M56 让这份"按供应商的成本"从只能事后看，变成**能设上限、能复位、会被路由执行**：
每个供应商可设累计成本上限（账本微单位，`0` = 不限）与统计周期（不限/每天/每月），
达到上限后该供应商**从候选中被剔除**（`excluded` 里给出 `cost_cap_reached`），
请求按既有策略故障转移到别家；全部候选都超限时客户端拿到 **503 `provider_cost_capped`**
（不是 502：上游没坏；也不是 429：重试不会变好）。读数由进程内后台每 5 秒按计量表重读
（热路径零查询，最坏超额 = 这 5 秒的流量），控制台显示"已用 / 上限 / 已超上限"并给出
**「复位成本」**按钮——复位只把统计起算点挪到当前时刻，不修改任何计量行
（见[设计](docs/design/m56-provider-cost-cap.md)、[路由](docs/routing.md) §4.6）。
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
| `docs/TODO.md` | 未完成项（`[ ]` / `[~]`）及其所在小节的引言 | 持续更新 |
| `docs/todo_done.md` | 已完成记录归档（各里程碑清单、发布记录、实测数据） | 持续追加（只增不改） |
| `docs/architecture.md` | 分层、模块边界与可替换扩展点 | 已落地（由 `internal/arch` 断言守护） |
| `docs/design/` | 每个里程碑的设计文档（接口、数据流、决策、异常、测试策略、实现差异） | M0–M39 全部产出（M28 的决策记在 `docs/api-providers.md` 与 `docs/todo_done.md`） |
| `docs/plugin-protocol-v1.md` | 插件协议 v1（帧/方法/事件/取消/背压/错误分类） | **已实现（M2）** |
| `docs/api-responses.md` | Responses 兼容面（端点/字段/SSE 事件/错误封装/认证与限速）、`GET /v1/models` 的能力扩展字段 | **已实现（M5，+M68 能力扩展字段）** |
| `docs/api-providers.md` | openai-chat 供应商（配置开关、思考模式、用量维度、错误分类、DeepSeek 接入） | **已实现（M17）** |
| `docs/provider-ui.md` | 控制台如何展示供应商配置说明（字段语义、配置与凭据两条通道、密钥录入路径与优先级、排障） | **已实现（M18）** |
| `docs/routing.md` | 路由解析、候选过滤、负载均衡策略、熔断与冷却、**会话粘性**、**供应商并发上限与排队**、**供应商成本上限与复位**、**图片（`image`）能力** | **已实现（M3，+M38 粘性，+M44 并发排队，+M56 成本上限，+M68 image）** |
| `docs/pricing.md` | 计量维度 × 有序价格规则集（分时/分档/分维度）；多币种（模型级币种 + 汇率换算） | **已实现（M11a / M22）** |
| `docs/billing.md` | 计量、账本、在途额度、账单、充值、对账、赠送到期；账本币种与显示币种 | **已实现（M11/M12/M22）** |
| `docs/mcp.md` | MCP 服务（11 个查询工具 + 3 个后台工具、令牌 scope、渐进披露、审计、内容可见性、stdio、**Key 批量导入与归属查询（`admin_import_keys` / `admin_lookup_key`，含明文形式的口径与边界）**） | **已实现（M6 + MCP-2 + M21 + M80 批量导入/归属查询）** |
| `docs/backup.md` | 一致点备份、保留、校验与两阶段恢复 | **已实现（M16）** |
| `docs/request-log.md` | 请求日志：录制通道矩阵（含 `user` 档「只留最新一条人说的话的纯文本、样板按标记跳过」与 `full`=保留全部）、身份维度（客户端/模型/工作区/会话/标题 + 用户（账户）/API Key + 供应商/上游模型/路由路线）、消耗口径、查询与统计（含列表底部的本页汇总行）、保留期与写入兜底 | **已实现（M27、M29、M30、M53、M78、M81、M83）** |
| `docs/chat.md` | 控制台智能问答与技能库：计费与隐私口径、按 MCP 令牌 scope 决定的权限边界、Markdown/图表/HTML5 预览、**内联表单（气泡内渲染 → 提交 → 模型原地更新）**、**可交互沙箱页面（生成表单 → 用户提交 → 模型继续执行）**、技能私有性、**勾选技能后留空直接发送**、**联网（web_search / web_fetch：部署级 + 会话级双层开关、SSRF 防护、来源引用）**、中断恢复、配置与排障 | **已实现（M32 + M34 + M35 + M73）** |
| `docs/sub2api-migration.md` | sub2api → ai_gateway 的用户与 API Key 迁移：搬什么/不搬什么、明文不出源库的哈希导入、标签分配规则、迁移后的结构/功能/保密验证、前缀冲突与回滚、**批量入口（M80，一次 ≤200 把）与逐把对账** | **已实现（M43；+M80 批量）** |
| `docs/org.md` | 组织架构：多根森林、账号多归属、节点标签被整棵子树继承（授权与限速口径）、删除与移动语义、**可复用树形控件**（侧边栏/工作区两处放置）、**飞书通讯录同步入口**、**拼音表（过滤与租户名生成两个消费者）**、排障 | **已实现（M49 + 同步 M70 + M74 租户名）** |
| `docs/dshgw.md` | 多租户 dsh 网关：用 aigw 的 API Key 登录 DeepSeek Harness 浏览器界面、每租户独立工作区与 bubblewrap 隔离、与 aigw/dsh 双向解耦（HTTP-only 集成 + 契约测试 + 导入闸门）、登录与会话、**租户名自动生成（`dsh-<账号拼音>-<账号ID>`）**、**租户模型的参数渲染（能力/上下文/最大输出/图片/推理档位）**、隔离强度、**多机分布式运行（§8：控制面 + 工作节点、控制台「DSH 节点」页、SSH 一键部署）**、排障、已知限制 | **实现中（M51 主机验收待执行；+M68 模型参数 +M74 租户名 +M77 多机分布式规格）** |
| `docs/feishu.md` | 飞书身份：把 API Key 绑定到飞书账号（控制台绑定/解绑、1:1 唯一性）、用飞书登录 DSH 门户（一次性票据 + 账号级复核）、**控制台多管理员与管理员扫码登录/邀请链接**、**通讯录同步（部门树 + 人员合并、所需数据权限）**、**飞书后台配置步骤**、隐私与审计口径、错误码排障 | **已实现（M60 绑定 + M61 门户登录 + M66 控制台管理员扫码登录 + M70 通讯录同步；真机验收依赖飞书应用登记与两个通讯录数据权限）** |
| `docs/deployment-layout.md` | 部署布局与**单一数据根**：数据/产物/运行时安装三分法、每个默认值的落点、相对路径规则（以部署根为基准）、本机布局、迁移与下线 runbook | **已实现（M63）** |

## 构建

本仓库构建必须使用工作区本地工具链环境（`$HOME` 只读、无 CGO）：

```sh
source scripts/goenv.sh
make build      # 产出 bin/aigw（控制台资源先压缩混淆，再用 -overlay 嵌入）
make build-src  # 产出 bin/aigw-src（不混淆；排查/对照用，且**不覆盖** bin/aigw，见 M54）
make test       # 单元测试
make verify     # vet + test + build
make dshgw-verify  # 独立 dshgw 测试/构建/契约，不并入 aigw verify
```

`make build` 会把 `internal/webui/static/` 压缩成 `.cache/ui-dist/static/`（去注释、去换行、局部标识符重命名，
体积 −41%），并为值得压缩的资源生成 `.gz` 副本（js/css 合计 332 KB → 131 KB，−60%），再用
`go build -overlay` 把两者一起嵌进二进制——**工作区源码一个字节都不改**，因此 `go test`、
`internal/webui/tests/*.mjs` 与 `make ui-check` 读的仍是可读源码。运行期由 `internal/webui` 按
`Accept-Encoding` 协商：客户端接受 gzip 就发 `.gz` 副本并带 `Content-Encoding: gzip` 与 `Vary`，否则发原字节。
详见 `docs/design/m50-frontend-minify.md` 与 `docs/design/m55-console-transfer-compression.md`。

## 运行

```sh
./bin/aigw --version
./bin/aigw --config config.yaml
```

配置项见 `config.example.yaml`；管理控制台在 `http://<listen>/admin/ui/`（首次用 bootstrap 里的管理员账号登录，
之后可在控制台「管理员」页新增更多管理员、设角色，并用飞书扫码登录，见 `docs/feishu.md` §5b）。

**数据落在哪**：所有运行态数据默认在**部署根**的 `./data` 之下（库、日志、pid、备份、插件状态、dshgw 租户状态），
配置（`config.yaml`、`dshgw.yaml`、`gwproxy.yaml`）留在部署根，产物在 `bin/` 与 `plugins/`。
相对路径以进程工作目录为基准，而用户单元的 `WorkingDirectory` 就是部署根；启动日志的
`data_dir=` 会打印解析后的绝对路径。完整规则、每个键的默认值与迁移/下线步骤见
[`docs/deployment-layout.md`](docs/deployment-layout.md)。

## 常用入口

| 命令 / 路径 | 用途 |
|---|---|
| `POST /v1/responses` | Responses API（流式 / 非流式） |
| `GET /v1/models` | 可用模型与**对客售价** |
| `GET /version` | 构建身份：`{"version":"a.b.c","revision":"<短 sha>","ui":"minified|source","ui_encoding":"gzip|identity"}`（公开，带 `base_path` 前缀；`ui` = 控制台是否混淆，`ui_encoding` = 是否按 `Accept-Encoding` 发预压缩副本） |
| `GET /healthz` | 存活探针（同样带 `version`、`revision`、`ui` 与 `ui_encoding`） |
| `POST /mcp` | MCP 服务（账户查询 + 按 scope 可执行后台接口，`aigw_mcp_` 令牌） |
| `bin/aigw mcp-serve --account <name>` | 本地 stdio MCP（11 个只读查询工具） |
| `bin/aigw mcp-serve --endpoint <完整 MCP URL> --token-env GW_MCP_TOKEN` | stdio 转发到运行中网关，按令牌 scope 提供查询与后台工具（M42） |
| `/admin/api/v1/*` | 管理面：账户/Key/标签/供应商/模型/路由/映射/定价/账单/充值/对账/备份/审计 |
| `/admin/ui/*` | 内置控制台（源码零构建；发布构建经压缩混淆后嵌入，M50） |
| `scripts/load.sh` | 一键压测（自建 `cmd/loadgen`，输出 rps 与分位延迟） |
| `scripts/deepseek-smoke.sh` | DeepSeek 接入走查（离线假上游；加 `--live` 与 `DEEPSEEK_API_KEY` 打真机） |
| `scripts/verify-m34.sh` | 可交互预览的自查（29 项，真实 HTTP、不产生模型费用、结束自动清理）；`GW_ADMIN_PASSWORD=… scripts/verify-m34.sh` |
| `scripts/ui-harness/run.sh`（`make ui-check`） | 控制台走查：API 快照 + headless firefox 渲染真实页面并断言（无 node 环境下的 UI 验证手段） |
| `scripts/format-smoke.sh` | `response_format` 语义走查：真实二进制 + 假 DeepSeek，逐条读上游收到的请求体（无网、无 key、不花钱） |
| `scripts/responses-thinking-smoke.sh` | `/responses` 流式思考走查：真实二进制 + 假 DeepSeek `/responses` 上游，断言客户端收到的 SSE 里思考先到、`item_id` 是上游的（无网、无 key、不花钱） |
| `scripts/ui-badge-test.mjs`（`make ui-base`） | 左上角版本角标的 node 断言（三行 DOM shim，不需要浏览器）：两格内容、revision 为 `none` 时不显示、端点读不到时不报错 |
| `scripts/release.sh`（skill `release-version`） | 发版：升 `VERSION`（a.b.c）→ 提交打 tag → `make build`；用法 `scripts/release.sh patch/minor/major` |
| `scripts/move_dshgw_state.sh` | 把 dshgw 的状态树搬进数据根：搬目录 + 改写 registry 与每租户文件里的绝对路径（dry-run 默认，见 `docs/deployment-layout.md` §7） |
| `scripts/decommission_legacy_dshgw.sh` | 归档并下线旧的 root/systemd dshgw（先归档校验、再停单元与 nginx 转发、最后删三处目录；`--apply` 需 root） |
| `make build` / `make ui-dist` | 发布构建：`ui-dist` 生成压缩混淆镜像 + gzip 副本（`.cache/ui-dist/static` + `overlay.json`），`build` 用它嵌入 `bin/aigw` |
| `make build-src` | 不混淆构建：写 `bin/aigw-src`，**不碰** `bin/aigw`（源码版实例仅供调试，见 M54） |
| `make verify` | vet + 全量测试 + 控制台 node 断言 + 构建 |

## 压测基线（本机 i7-12700K，testecho 供应商）

```
32 并发 × 10s：619.6 rps，p50 1.73ms，p95 44.5ms，p99 1.73s
结算：usage 行与账本 charge 行逐条对齐，四条不变量 ok，无兜底文件
微基准：pricing.Evaluate 4.2µs，routing.Plan 4.7µs（并行 4.0µs）
```

细节与两处压测发现的缺陷见 `docs/design/m13-performance.md`。
