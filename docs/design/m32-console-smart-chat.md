# M32 控制台智能问答、私有技能库与图表

设计已确认，开始实现。首版仅 `admin` 可绑定计费 Key、发送问答和生成技能草稿；`viewer` 只能查看自己的历史并管理自己的技能。

## 目标

在概览下新增「智能问答」（`#/chat`）与「技能库」（`#/skills`）。聊天复用 Responses 数据面与 MCP admin 工具并正常计费；技能按 `admin_users.id` 隔离；Markdown、chart JSON、SVG 与 HTML 预览采用安全的 DOM/沙箱策略。

## 安全决策

- 每个模型步骤重新校验 admin 登录会话、账户、Key 状态与账户/Key 匹配关系；客户端提交的 ID 不构成授权。
- 聊天仍经过正常配额、限流、计量与结算，但请求正文、技能内容、模型输出与工具结果不进入全局请求内容记录、hooks 或全局审计；只保留身份、用量、费用与状态元数据。
- MCP 工具使用聊天专用允许列表；凭据发行、权限变更、备份恢复、hooks、供应商凭据等高危写操作不暴露给聊天。
- HTML/SVG 预览使用 owner 绑定的短时票据，不使用永久公开能力 URL；默认禁止外部资源加载与网络连接。
- 每轮使用幂等 turn ID；工具执行前先持久化 pending，崩溃或断线后不自动重放写操作。

## 数据流

浏览器创建 owner-scoped 会话并选择模型、账户与 API Key。每个 step 通过内部已验证身份调用现有 Responses handler，生成独立 request ID。模型的工具调用经 adminBackend 执行，结果回灌下一 step。消息保存规范 provider items 与展示字段；费用从现有 usage records 实时关联，不在聊天表中复制。

## 图表

优先输出 `chart` JSON，由前端手写 SVG 渲染；原始 `svg`/`html` 通过短时票据在隔离文档中预览。上限为 8 个 series、500 个点，展示来源工具与 request ID，但不宣称服务端能验证模型改写的数据真实性。CDN 图表库默认被 CSP 拦截，需显式开启 `chat.artifact_allow_network`。

## 验证

覆盖 owner 隔离、viewer 禁止计费、隐私录制、幂等与中断恢复、工具终态、票据失效、CSP、图表边界、真实计量与 UI harness。实现完成后在本文末尾回填实现差异。

## 验证（隔离实例，真实二进制）

`make verify` 全绿（含新增的 chat/store/httpapi/config/responses/webui 测试），`make ui-check` 12 个视图全绿。
另外用一个**全新库、`:8099`、新二进制**的隔离实例走查了真实链路（不动 8088）：

- 建会话 → 提问：SSE 依次 `turn → step → text → usage → message → done`，回答内容来自真实上游调用；
- 请求日志：`client=console`、`session_id` = 会话 id、`request_json` 为空、输出与思考均未录制，
  但 token、成本与状态齐全；
- 账本：出现 `charge:req_<id>:1` 的扣费记录（按 request_id 幂等）；
- 幂等：同一个 `turn_id` 重发不产生新的请求日志行，也不再有 `text` 事件；
- 技能提炼：正常计费，模型无法产出 JSON 时返回带说明的骨架草稿；
- 预览：带票据 200 且响应头为 `sandbox allow-scripts; default-src 'none'; connect-src 'none'` +
  `no-store`/`nosniff`/`no-referrer`；无票据 404；**登出后同一票据 404**；SVG 只有 `sandbox`（无 `allow-scripts`）；
- 资源：`/admin/ui/` 下的 chat/skills/chart/markdown/chat_artifact 全部 200（控制台 CSP 未改动）。

未验证的部分：模型真正发起 `admin_request` 的工具循环在真机上没有跑（内建 `testecho` 不发起工具调用），
由 `internal/chat` 的脚本化 runner 与 `internal/httpapi` 的桥接测试覆盖；生产部署重启留给人工执行。

## 实现与设计差异

实现与设计基本一致，实际落地中有四处按代码事实收紧或简化：

1. **预览产物没有用「能力 URL」**。设计初稿考虑过不可猜测的长期 URL，实现改成
   「cookie 会话 + 短时票据」两件事都要：票据是 HMAC 签名的
   `artifact|admin_session|expiry`，校验时还会核对那个登录会话仍然有效。
   因此登出即失效、重启即失效（签名密钥是进程内随机生成），
   而沙箱 iframe 不发 cookie 也不影响预览——签发票据的请求来自控制台页面本身。
2. **票据过期精度用纳秒**。最初按 Unix 秒比较，1 秒内到期的票据实际还能用一秒；
   测试用一个 1ms 的 TTL 才发现。改成 `UnixNano` 后，写进配置的任何 TTL 都是真的。
3. **`chat.high_risk_tools` 是「在允许清单之外再减」，不是「再加」**。
   设计文档写的是「叠加内置禁止清单」，实现采用**默认拒绝的写接口允许清单**：
   只有显式列入的写接口可用，未列入的一律拒绝（包括未来新增的），
   配置项只能再往下减。这比枚举禁止项更抗腐坏，文档（`docs/chat.md` §4）按实现的语义描述。
   > **已被取代（见下方「权限模型改为绑定 MCP 令牌」）**：写接口允许清单、这个配置项、
   > 以及控制台专属的 `SourceConsole` 身份都已删除。权限现在只有一个决定点——会话绑定的
   > MCP 令牌的 scope。
4. **历史超限时明确失败，而不是截断发送**。设计里只说了「按整轮裁剪」，
   实现额外判断「最新一轮本身就超限」并让该轮以 `failed` 结束（错误文案说明是上下文上限）。
   发半个问题会让模型基于不存在的上下文作答，那比明确失败更糟。

5. **步数与工具次数默认不设上限**。设计里写的是 8 步 / 16 次调用作为默认值，
   实现改为 `0 = 不限制` 且默认 0：一次提问会一直进行到模型不再调用工具为止，
   配置成正整数时才在达到上限处停止并提示。理由是这种「先查 A 再查 B 最后汇总」的
   调查本来就不该被一个猜出来的数字截断；代价（一次点击的花费由模型的步数决定）
   写进了 `docs/chat.md` 的配置一节。

6. **工具名与端点名不再混淆**。模型会把两级约定压平，直接用**端点名**当工具名调用
   （真机上出现过 `unknown tool "admin_request_dimensions"`，而它手里其实握着正确的
   端点名与参数形状）。`chatTools.Call` 因此把「不是三个工具之一、但确实是已注册端点」的
   调用按 `admin_request` + `name=<该端点>` 转发；判定仍然走同一条允许清单，
   所以这不是放宽权限，只是把意图明确的调用接住。既不是工具也不是端点的名字，
   回一句「本网关提供 admin_endpoints / admin_describe / admin_request」。
   > 折叠逻辑保留，但「走同一条允许清单」这句话已被取代：现在判定走的是令牌 scope。

另外两点实现细节值得记录：

- **`stepObserver`**：设计说「内部 step 结果观察端口」，实现是一个
  由 `chatRunner` 安装、由 `handleCreateResponse` 在候选命中时调用的回调。
  之所以需要它，是因为 provider/canonical/degraded 只在非流式分支写进响应头，
  而聊天的每一步都是流式的；重新跑一遍路由去猜是第二份实现，会漂移。
- **`viewer` 的写开关**：即使会话的 `write_mode=allow_writes`，角色不是 `admin` 时
  `chatPrincipal` 仍然给 `admin_read` 作用域。两道闸门（会话开关 + 角色）都要过，
  而且角色是**每次工具调用**重新读库的，不是从请求里带下来的。
  > 已被取代：`chatPrincipal` 与 `write_mode` 作为输入都不存在了。角色闸门移到
  > `CreateSession`/`UpdateSession`（绑定 `scope=admin` 的令牌必须是管理员），
  > 每轮重新读库的机制不变，只是读的是令牌行。

## 权限模型改为绑定 MCP 令牌（取代第 3 条与 `viewer` 那一段）

原实现给控制台聊天造了一套**专属**权限：从 `Role + WriteMode` 现推 scope，再叠一层
默认拒绝的写接口允许清单，发凭据/动钱/备份/hooks/删除这几类一律拒绝，理由是"聊天转录
里做不可逆操作看不清"。这个取舍本身自洽，但代价是**同一个能力有两套实现**：MCP 令牌
能做的事和控制台能做的事不同，且新增接口默认对聊天不可达。

现在改成：**智能问答就是一个 MCP 客户端**。

- 会话在新建时绑定一个 MCP 令牌，库里只存 `mcp_token_id`（迁移 0011），不存明文也不存
  `token_hash`——明文只在签发时出现一次。
- 每次工具调用都用该 id 回查令牌行，校验 `status`/`expires_at`，组装成 `Principal`，
  再构造一个 `POST /mcp` 请求交给**同一个** `handleMCP`。scope 因此不是快照：
  撤销或过期在下一次调用立刻生效。
- 工具面来自 `ToolsFor(principal)`：`query` 得 11 个查询工具；`admin_read` 得 11＋3
  （后台只读）；`admin` 得 11＋3（全部控制台操作，含发凭据、动账、删除）。UI 上不再有
  "写权限"开关——选令牌就是选权限。
- `write_mode` 降级为按 scope 派生的展示字段，不再接受客户端设置，所以不会出现"会话说
  自己能写、令牌却写不了"的矛盾状态。

三个刻意的选择：

1. **进程内构造请求，而不是走 TCP**。仓库已有先例：`chatRunner` 就是这样调
   `/v1/responses` 的。真 HTTP 会逼着网关长期持有**明文**令牌（表里只有 hash，
   取不回来），等于凭空造出第二种凭据保管机制。合成请求经过 `handleMCP` 的全部逻辑
   （含 `mcp.enabled` 守卫、scope 检查、`confirm` 要求、审计），跳过 TCP 与
   `Authorization` 头对这条路径没有信息损失——调用方本身就是已认证的管理员。
2. **删除 `chat.high_risk_tools`**：它只能对已被删除的允许清单做减法，留着就是一个
   没有作用的配置键。
3. **不自动注入 `confirm`**：`chat_tool_calls.arguments` 是持久化的，技术上能"同会话确认
   过一次就放行后续危险调用"，但那样删除类操作会在长会话里静默通过。

分层约束：`internal/chat` 不允许 import `internal/mcpsrv`（`internal/arch` 断言），
所以 scope 名在 chat 侧本地拼写，并由 `TestChatScopeVocabularyMatchesMCP`
（`internal/httpapi`，两边都可见的唯一位置）钉住等价关系——否则 mcpsrv 改个作用域名就会
让所有会话静默变成只读。

