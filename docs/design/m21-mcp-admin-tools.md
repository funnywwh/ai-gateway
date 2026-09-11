# M21 设计文档：MCP 后台工具（让 MCP 能执行全部后台 API）

> 状态：**实现中**。规格文档见 `docs/mcp.md`；流程约定见 `docs/PROCESS.md`。
> 计划已在对话中展示并获确认（本文即该计划）。

## 1. 目标

MCP 客户端（DSH / Claude / 自研 agent）用一个令牌，既能查自己账户的数据（现状不变），也能**执行全部管理面接口**。
形态上采用**渐进披露**：`tools/list` 只增加三个入口工具，模型先拿"接口概要"，需要时再查"怎么用"，最后发起调用——
避免把 80+ 个工具定义一次性塞进客户端上下文。

**验收标准**

1. 全部管理面路由由**一张声明式路由表**注册（此前是 85 行手写 `mux.HandleFunc`），完整性测试与字面清单逐条比对。
2. 除 3 条明确不暴露的路由外，其余全部可通过 MCP 调用。
3. `scope=query` 令牌看不到任何后台工具；`admin_read` 只能调只读接口（写接口 403）；`admin` 可调全部。
4. 破坏性/涉资金接口必须显式 `confirm: true`，否则被拒并说明原因。
5. 每次后台调用落审计（`actor=mcp:<令牌名>#<id>`）并发 `mcp.call` 事件。
6. 响应正文超过 `mcp.admin_max_response_bytes`（默认 256 KiB）时截断并带 `truncated: true`。
7. `make verify` 全绿 + 隔离实例真机走查。

## 2. 关键决策

| # | 决策 | 取舍理由 |
|---|---|---|
| D1 | **渐进披露三件套**：`admin_endpoints` / `admin_describe` / `admin_request` | 用户明确要求"先给概要、用时再查"。工具定义恒定 3 个（上下文占用最小），能力靠路由表保证完整且不漂移 |
| D2 | 鉴权走 **`mcp_tokens.scope`（query / admin_read / admin）**，默认 `query` | 既有令牌不静默提权；`admin_read` 复用既有 viewer 角色，写接口 403 由现有 `adminActor(requireAdmin)` 自动完成 |
| D3 | **单一路由表**同时是注册来源与工具元数据来源 | "所有后台 API"必须由结构保证，不能靠人工同步两份清单 |
| D4 | 后台工具与账户查询工具**同端点同令牌** | 客户端只配一个 URL/header；权限差异由 scope 表达 |
| D5 | `admin_read`/`admin` 是**网关级**权限，不做账户作用域 | 管理面接口本身跨账户；用审计与最短有效期控制 |
| D6 | 危险接口需 `confirm: true`（表内声明） | 防模型误删/误重建；确认要求写在 `admin_describe` 输出里 |
| D7 | 3 条路由注册但不暴露 | `auth/login|logout` 是 Cookie 语义；备份下载是二进制大文件 |
| D8 | `mcp.admin_tools`（默认 true）作为全局熔断开关 | 真正的闸门是 scope；开关用于加固部署时一键关停 |

## 3. 接口

### 3.1 MCP 工具（`scope != query` 时出现在 `tools/list`）

| 工具 | 入参 | 返回 |
|---|---|---|
| `admin_endpoints` | `filter?`、`group?`、`limit?` | `{count, endpoints:[{name, method, path, summary, group, role, params[], query[], has_body, dangerous, tool, reason?}]}` |
| `admin_describe` | `name` 或 `names[]` | 逐条 method/path/group/summary/role/dangerous/confirm_reason/params/query/body_schema/example |
| `admin_request` | `name`、`params?`、`query?`、`body?`、`confirm?` | `{name, method, path, status, ok, body|text|meta, truncated?}`；HTTP ≥400 时 `isError=true` |

### 3.2 权限模型

- `mcp_tokens.scope`：`query`（默认）/ `admin_read` / `admin`；迁移 `0006_mcp_token_scope.sql`。
- 角色映射：`admin` → `role=admin`；`admin_read` → `role=viewer`；`query` → 不提供后台工具。
- 合成主体 `Username = "mcp:<令牌名>#<令牌id>"` 经 request context 注入，`adminActor` 在 Cookie 分支之前识别；
  context key 为包内未导出类型，只有 bridge 能写，HTTP 请求无法伪造。

### 3.3 路由表

`internal/httpapi/admin_routes.go` 定义 `adminRoute`（Method/Path/Handler + Name/Summary/Group/Role/Dangerous/ConfirmReason/
Params/Query/Body/RawBody/Notes/NoTool）、`adminField` 与 `objectSchema/freeFormSchema` 辅助函数；
各族一个构造函数（`systemAdminRoutes`/`catalogAdminRoutes`/`providerAdminRoutes`/`billingAdminRoutes`/
`invoiceAdminRoutes`/`backupAdminRoutes`/`portalAdminRoutes`/`pricingAdminRoutes`），`adminRoutes()` 只做拼接。
`routes()` 遍历表注册，`Server.registered` 记录已注册 pattern 供测试比对。

### 3.4 执行与审计

`adminBackend` 实现 `mcpsrv.Backend`：查表 → 校验 scope/role/confirm → 拆解 params/query/body →
构造 `http.Request`（`SetPathValue` + `URL.RawQuery` + JSON body）→ context 注入合成主体 →
直接调用表里的 Handler → 自带 `responseRecorder` 捕获状态与正文（带 `admin_max_response_bytes` 上限）。
审计：`action=mcp.admin_call`、`target_id=<name>`、`changes={method,path,params,query,body_keys,status}`（不记 body 值）。
hook：`mcp.call` 事件（actor/tool/method/path/status/duration_ms/ok）。

## 4. 数据流

```
POST /mcp  Authorization: Bearer aigw_mcp_…
  └─ httpapi/mcp.go：token → mcpsrv.Principal{AccountID,TokenID,Name,Scope}
      └─ mcpsrv.Handle(ctx, principal, raw)
          ├─ tools/list → Tools()（11 个只读）+ backend.AdminTools(p)（scope≠query 且配置开启时 3 个）
          └─ tools/call → 只读工具同现状；后台工具走 backend.CallAdmin
                          └─ adminBackend → 路由表 Handler(recorder, req+principal)
                              └─ adminActor 识别合成主体（角色由 scope 决定）→ 现有 handler 原样执行
```

## 5. 改动清单

- **internal/httpapi**：新增 `admin_routes.go`（表 + 索引）、`admin_routes_test.go`、`mcp_admin.go`（bridge）；
  `server.go` 改为按表注册并暴露 `adminEndpointIndex`；`admin.go` 增加合成主体分支；
  `mcp.go` 传 Principal；`admin_catalog.go`/`admin_json.go` 支持 scope 与新端点 `PATCH /admin/api/v1/mcp-tokens/{id}`。
- **internal/mcpsrv**：新增 `scope.go`（scope 常量与校验）；`service.go` 增 `Principal`/`Backend`/`SetBackend`/`toolsFor`；
  `jsonrpc.go` 的 `Handle` 改收 `Principal`，新增 `ToolResult`。
- **internal/store**：迁移 `0006_mcp_token_scope.sql`；`keys.go` 列/扫描/upsert 带 scope。
- **internal/domain**：`MCPToken.Scope`。
- **internal/config**：`mcp.admin_tools`、`mcp.admin_max_response_bytes` + 环境覆盖。
- **internal/webui**：MCP 令牌页支持 scope 选择、展示与修改。
- **文档**：`docs/mcp.md`、`docs/TODO.md`、`README.md`、`docs/architecture.md`、配置样例。

## 6. 边界与失败模式

- 未知 name / 缺路径参数 / 类型不符 → 结构化错误并指向 `admin_endpoints`，不执行 handler。
- `admin_read` 调写接口 → 明确提示"该令牌为只读，此接口需 scope=admin"。
- handler 返回 501（端口未接线） → 原样透传 `ok=false, status=501`。
- 非 JSON/超限响应 → 元数据或截断标记，绝不把二进制塞进 MCP 文本。
- 慢接口（探测、备份、账本重建）在 `admin_describe` 的 notes 里标注。
- 不改变计费、路由与既有接口语义；不引入新依赖。

## 7. 测试策略

- `admin_routes_test.go`：表 == 86 条字面清单、注册 == 表、元数据完整（名称唯一/前缀/摘要/角色/危险必带原因/
  `{param}` 双向一致）、describe 可用且可序列化。
- `mcp_admin_test.go`：scope 矩阵、端到端（建 supplier → model → route）、confirm 闸门、
  describe/目录、CSV 文本响应、截断、审计 actor、CSRF/Cookie 路径不回归。
- `internal/mcpsrv`：toolsFor 合并、ToolResult.IsError 映射、Principal 透传。
- `internal/store`：迁移清单、scope 默认值与往返。
- 变异验证：去掉 confirm 闸门 / scope 校验 / 角色预检 / 截断标记 / 审计写入，各自应有精确失败。
- 真机走查：隔离实例签发 admin 令牌 → 目录/详规/执行 → 控制台可见 → 吊销后 401。

## 8. 明确不做

- 不含 stdio（`aigw mcp-serve` 仍是 11 个只读工具）：没有管理员主体，且需要把 `cmd/aigw/main.go` 的依赖构造抽成
  共用函数（Composition Root 重构），本里程碑不做，在 `docs/mcp.md` 写明。
- 不实现"单接口一个 MCP 工具"的形态；不给后台工具做账户作用域；不新增专用端点。

## 9. 实现与设计差异

1. **路由表集中在一个文件**：设计里写的是"各族条目写在各自的 handler 文件里"，实现改成全部集中在
   `internal/httpapi/admin_routes.go`（按族分成 8 个构造函数）。理由：审计"管理面到底有哪些接口"时只看一个文件，
   评审与新增都更省事；完整性由 `admin_routes_test.go` 与字面清单比对兜底，与放哪里无关。
2. **提交切分**：计划是"纯重构 / 能力 / 界面与文档"三个提交，实际合并为"路由表 + scope 基础"与
   "后台工具 + 界面 + 文档"两次（表里已包含 scope 元数据与新端点，硬拆会让第一个提交无法编译）。
3. **不暴露的接口命名**：`auth/login|logout`、`backups/{id}/download` 仍然有工具名（`admin_login` 等），
   调用时给出"未对 MCP 开放 + 原因"，而不是报"未知接口"——比设计里"列在目录、tool=null"更明确。
4. **新增 `PATCH /admin/api/v1/mcp-tokens/{id}`**：设计里只提到"改权限"，实现落成一个正式端点（scope/status），
   于是管理面路由从 85 条变成 86 条，暴露给 MCP 的是 83 条（86 − login − logout − download）。
5. **端口未接线的错误码**：设计里写"501"，实际 `portReady` 写的是 400 `unsupported_error`；
   桥原样透传 handler 的状态码，不为它改口径（测试按 400 断言）。
6. **body 值一律不入审计**：设计里已写，实现时进一步把"键名列表"（`body_keys`）写进 started 那一条，
   排障时能看出"这次调用带了哪些字段"而不泄露值。
7. **`mcp.call` hook 补上了**：`docs/mcp.md` 早就声明过这个事件但一直没实现，本轮随审计一起补上。
8. **真机走查结论**：MCP 建出来的供应商/模型/路由**不需要重启**即可服务真实请求（走的是同一套 handler +
   既有热更新三件套），`POST /v1/responses` 返回 `echo: ping`；令牌吊销后立即 401。
9. **变异验证（5 处护栏逐一咬合）**：去掉 confirm 闸门、去掉角色预检、去掉截断标记、去掉桥的审计写入，
   各自精确失败；scope 闸门需要**同时**去掉两层（`mcpsrv.AllowsAdminTools` 与桥里的 `AdminRole` 判定）才会让
   `query` 令牌看到后台工具——两层是刻意的纵深，单层失效不会立刻提权。
