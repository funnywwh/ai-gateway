# M78 请求日志的路由路线（供应商 / 上游模型 / 路由 id）

> 需求原话：「请求日志 详情/列表 里要添加 provider，upstream_model，路由路线」。
> 前置阅读：`docs/request-log.md`（规格）、`docs/design/m53-request-provider-dimension.md`
> （供应商维度与「为什么不能做成日志列」）、`docs/routing.md`（路由与失败转移）。

## 1. 目标

控制台的「请求日志」页要能回答「这条请求走了哪条路由、发给哪家供应商、上游模型名是什么、
失败转移时先试了谁、每次尝试的结果如何」：

- **列表**：在既有「供应商」列之后新增「上游模型」与「路由路线」两列。
- **详情**：新增「路由路线」区块（逐次尝试一行），身份块补「上游模型」与「映射规则」。
- **API**：`GET /admin/api/v1/requests`、`GET /admin/api/v1/requests/{id}` 返回 `attempts[]`
  与 `matched_rule`；`providers` 形状不变。
- 旧行（本迁移之前的计量行）与本地拒绝的请求**不猜测**：显示「（未知路由）」/「—」/「无上游尝试」。

## 2. 关键决策与取舍

### 2.1 路线信息落在 `usage_records`，不落在 `request_logs`

一次请求可以有多次上游尝试、落在不同供应商上（M38 的失败转移），所以「走了哪条路由 / 发的是
哪个上游模型」是**一对多**的事实。这与 M53 拒绝 `request_logs.provider_id` 的理由完全同源：

- 取「第一家」「最后一家」或「成功那家」都会把一次失败转移的成本与路线搬到另一家头上；
- `usage_records` 本来就是**一行一次尝试**，成本、token、延迟、错误码都已按尝试归属，路线跟着走
  才能与账单同源、同寿命（保留期清理日志而不清理计费）。

**被否方案**：在 `request_logs` 上加 `route_path_json`。它能让详情一行读全，但代价是与计量行两份
数据描述同一件事，且正文/身份列走的是「写失败退化为骨架行」的通道，而路线事实属于计费侧。

### 2.2 `upstream_model` 存快照，读时不 join `routes`

`routes.upstream_model` 是**配置**，可被编辑、可随供应商删除而消失；而「当时真正发出去的那个
模型名」是既成事实。读时 join 会让历史显示随配置漂移（改一次映射，所有历史行的上游模型集体变样）。
因此：

- `usage_records.upstream_model` 存**当时的字符串**（`internal/httpapi/v1.go` 调用
  `ToProviderRequestWithItems(cand.UpstreamModel, …)` 用的就是它）；
- `usage_records.route_id` 只作**身份**（控制台「模型与路由」页上的那一条），路由被删后这个数字
  仍然保留，界面显示 `#id`，不做读时反查。

### 2.3 `matched_rule` 落在 `request_logs`

命中的映射规则（`model:<public>` / `mapping:<kind>:<pattern>` / `alias:<x>` / `fallback:<y>`，
见 `internal/modelmap`）是**请求级**事实：一个请求只有一个规范模型，也就只有一条命中规则。
它与 `resolved_model` 同规矩——身份元数据，`record_input=off` 也记，不参与 `redact_paths`
（脱敏管的是正文解析出来的列），本地拒绝的请求为空（与 `resolved_model` 一致：它没有走到模型）。

### 2.4 `providers` 保留，改由 `attempts` 导出

`providers: [{id, name}]` 是 M53 已发布的契约（控制台、admin MCP、走查夹具都读它），形状不动。
它现在由 `attempts` 去重得出，顺序从「按 provider id 升序」变为「按首次尝试时间」——后者才是
「先试了谁」的读法，也让两个字段的顺序自洽。

### 2.5 store 的 `RequestProviders` 换成 `RequestAttempts`

`request_id IN (…)` 上一次查询同时供 `attempts` 与 `providers` 两用，页面上少一次查询，也消除了
两个字段顺序口径不一致的可能。

### 2.6 明确不做

- 不新增 `group_by=route|upstream_model` 维度、不新增筛选参数（自然后续，需另立里程碑）；
- 不改 MCP **账户自助**查询工具（`list_requests`/`get_request`）：provider/路由属于管理面事实，
  M53 同样只进管理面，且改动会触发 `docs/mcp.md` §4.5 的工具说明义务；
- 不回填历史行、不给路由加名字、不记录候选全集与被排除原因（那是 `/router/explain` 的实时诊断）。

## 3. 接口

### 3.1 迁移 `0027_request_route_identity.sql`

```sql
ALTER TABLE usage_records  ADD COLUMN route_id       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_records  ADD COLUMN upstream_model TEXT    NOT NULL DEFAULT '';
ALTER TABLE request_logs   ADD COLUMN matched_rule   TEXT    NOT NULL DEFAULT '';
```

不加索引：尝试行按 `request_id IN (…) ORDER BY request_id, attempt_no` 读取，`idx_usage_request`
已覆盖点查，且一个请求的尝试数是候选数级别（通常 1–2）。旧行默认 `0`/`''`，界面显示未知。

### 3.2 领域类型

```go
// internal/domain/interfaces.go
type UsageRecord struct {
    ...
    ProviderID    int64
    RouteID       int64  // 新增：这次尝试命中的路由
    UpstreamModel string // 新增：这次尝试真正发给上游的模型名（快照）
    ...
}

// internal/domain/entities.go
type RequestLogRecord struct {
    ...
    ResolvedModel string
    MatchedRule   string // 新增：把请求的模型解析成规范模型的映射规则；本地拒绝为空
    ...
}

// internal/domain/entities.go
// RequestAttempt 是一条已记录请求的一次上游尝试：网关选了哪条路由、哪家供应商服务、
// 发出去的上游模型名是什么、那一次的结果如何。
type RequestAttempt struct {
    AttemptNo        int
    ProviderID       int64
    RouteID          int64
    UpstreamModel    string
    Status           string
    ErrorCode        string
    TerminatedReason string
    LatencyMS        int
    TTFTMS           int
    CostMicros       int64
    ChargeMicros     int64
    CreatedAt        time.Time
}
```

### 3.3 Store

```go
// internal/store/request_logs.go（取代 RequestProviders）
func (db *DB) RequestAttempts(ctx context.Context, requestIDs []string) (map[string][]domain.RequestAttempt, error)
```

SQL 显式列 `request_id, attempt_no, provider_id, route_id, upstream_model, status, error_code,
terminated_reason, latency_ms, ttft_ms, cost_micros, charge_micros, created_at`，
`ORDER BY request_id, attempt_no`。名字不在这里 join：`provider_name` 由传输层用既有的
`providerLabels` 批量解析（与 M53 同规矩——名字是可变的读时标签）。

写入侧：`usageCols`、`InsertUsage`、`SettleBatch`（`INSERT OR IGNORE`，重放保留首写值）、
`ListUsage`/`ListUsageAsc` 的 scan、`putRequestLogSQL`/`requestLogArgs`/`requestLogColumns`/
`scanRequestLog` 各同步一列；`matched_rule` 与其余身份列一样**插入但不在 `ON CONFLICT` 时刷新**
（骨架行重试不得抹掉首写值）。

### 3.4 管理 API

`GET /admin/api/v1/requests`（列表）与 `/requests/{id}`（详情）每行新增：

```json
{
  "providers": [{ "id": 1, "name": "replay-local" }],
  "attempts": [
    { "attempt_no": 1, "route_id": 12, "provider_id": 1, "provider_name": "replay-local",
      "upstream_model": "deepseek-chat", "status": "failed", "error_code": "upstream_400",
      "terminated_reason": "upstream_error", "latency_ms": 300, "ttft_ms": 0,
      "cost_micros": 12, "charge_micros": 0, "created_at": "2026-09-23T11:00:03Z" }
  ],
  "matched_rule": "mapping:glob:*"
}
```

`attempts` 按 `attempt_no` 升序；无计量行时为 `[]`。`providers` 按首次尝试顺序去重。

## 4. 数据流

```
Router.Plan ──► plan.Resolved{Canonical, MatchedRule}      ─┐
             └► plan.Candidates[i] = Candidate{RouteID, ProviderID, UpstreamModel}
                         │
   v1.handleResponses ───┼──► recordAttempt(cand) ──► usage.Attempt{RouteID, UpstreamModel}
                         │                                   │
                         │                       Meter.Build / billing SettleBatch
                         │                                   ▼
                         │                          usage_records（一行一次尝试）
                         └──► persist(plan.Resolved) ──► RequestLogRecord{ResolvedModel, MatchedRule}
                                                            ▼
                                                       request_logs（一行一个请求）

读：GET /admin/api/v1/requests[/{id}]
    ── ListRequestLogsPage ── request_logs 一行（含 matched_rule）
    ── RequestUsages       ── usage_records 汇总（token/成本/尝试数）
    ── RequestAttempts     ── usage_records 逐次尝试（route_id/upstream_model/结果）
    ── ownerLabels/providerLabels ── 名字（读时标签）
```

## 5. 控制台

- 列表列（`internal/webui/static/js/pages/requests.js`）：`供应商` 之后新增 `上游模型`、`路由路线`。
  - 「上游模型」：无尝试 → 「未计量」（与「供应商」列同一句话）；有尝试但值全空（迁移前的行）→
    「—」并说明原因；否则按尝试顺序去重后 `a → b`，tooltip 逐行列每次尝试的上游模型与供应商。
  - 「路由路线」：无尝试 → 「无上游尝试」；否则每跳 `#<route_id> <供应商>` + 结果标记
    （`completed` ✓ / `failed` ✗ + error_code / 其他 `·`），`route_id<=0` 显示「（未知路由）」；
    tooltip 逐行给出 route id、供应商、上游模型、status、error_code、terminated_reason、延迟、
    首字延迟、成本与对客，并提示路由 id 可在「模型与路由」页对照。
- 详情：`identityBlock` 增加「上游模型」「映射规则」；新增「路由路线」区块，首行
  `请求的模型 x →[mapping:glob:*]→ 路由到的模型 y`，其后每次尝试一行。
- 新单元格只用 `class`/`title`，不使用行内 `style:`（严格 CSP 会丢弃 style 属性）。

## 6. 异常与边界

| 情形 | 记录 | 界面 |
|---|---|---|
| 本地拒绝 / 准入失败 | 无计量行；`matched_rule=''`、`resolved_model=''` | 「未计量」+「无上游尝试」，不显示 0 |
| 迁移前的计量行 | `route_id=0`、`upstream_model=''` | 「（未知路由）」/「—」，tooltip 说明原因 |
| 供应商事后被删 | `provider_id` 保留 | 名字缺失 → 显示 `#id` |
| 路由事后被删/改 | `route_id`、`upstream_model` 为当时快照 | `#id` 照原样显示，上游模型不随配置漂移 |
| 同一家被尝试两次 | 两行 `usage_records` | `attempts` 两行，`providers` 去重（语义不变） |
| 结算重放 / 骨架行重试 | `INSERT OR IGNORE` 与 `ON CONFLICT` 保留首写值 | — |
| `record_input=off` / `redact_paths` | 路线与规则照记（非正文派生列） | — |

## 7. 测试策略

- `internal/usage/meter_test.go`：`TestRecordWritesEveryField` 覆盖新字段。
- `internal/store`：`RequestAttempts` 的顺序/字段/缺席语义；`matched_rule` 往返与骨架行不抹除。
- `internal/httpapi`：真实请求（播种 provider/model/route）证明计量行的 `route_id`/`upstream_model`；
  `TestAdminRequestsFilterByProvider` 断言 `attempts`、`providers` 顺序、`matched_rule`；
  admin MCP 的失败转移行断言两跳。
- `internal/webui/tests/requests_test.mjs`：源码级钉住两个单元格渲染函数的四种形态。
- `scripts/ui-harness`（浏览器）：夹具补 `attempts`/`matched_rule`，断言表头、单元格文本与详情区块。

## 8. 依赖

- 迁移在启动时执行；`ALTER TABLE ADD COLUMN` 常量默认值不重写行。
- 线上需重启才生效（控制台资源内嵌于二进制）。
- 与在制品（控制台 CSP/行内样式修复）无文件重叠之外的影响：新单元格不依赖行内样式。

## 9. 实现与设计差异

实现与设计一致，差异只有三处细化，都是写代码时才定下来、口径不变的：

1. **`attempts` 的字段集**比 §3.4 草案多了 `created_at`（每次尝试的计量时间），并在列表与详情两处
   返回同一份对象；`providers` 的顺序定义明确为「按首次尝试顺序去重」（草案只说「导出」）。
2. **`persist` 的签名**由「多传一个 `rule string`」改为收 `resolved *domain.ResolvedModel`：三个调用点
   本来就在同一作用域持有 `plan.Resolved`，让函数从一个参数里读 `Canonical` 与 `MatchedRule`，比并排
   两个字符串参数更难写错（`inputRecord` 相应多了 `Rule` 字段）。
3. **控制台单元格的文案分级**在实现里明确成三层：`未计量`（没有计量行，本地拒绝）、`（未知路由）`/
   `—`（有计量行但没有该字段，迁移前）、正常值。tooltip 里 `错误码` 对每次尝试都写（即使状态是
   `failed`，单元格的 `✗` 标记已经带了它），因为 tooltip 是操作者复制进问题报告的那一行。

另外两条实现记录：

- `usageCols` 一并扩了两列并同步 `ListUsage`/`ListUsageAsc` 两个 scanner（重建计费的路径也读这张
  表，列序错位会静默读错行）；`SettleBatch` 的 `INSERT OR IGNORE` 因此在校验重放时保留首写值。
- 浏览器走查在本会话的沙箱里**能跑**（官方 Firefox 156.0.1 的 linux64 包解到 `.cache/`，PATH 上原来
  那个是 snap 壳子）：requests 视图 116 项断言全绿，其中 6 项是本次新增（两列表头、两个单元格、
  未计量行、详情区块）；`make ui-check` 的候选探测仍会在没有可用浏览器的宿主上打印原因并跳过。
- 本会话沙箱没有 Go 工具链，验证用的是官方 go1.25.9 包（同样解在 `.cache/`，不进入提交）；
  `make test` 全绿，唯一一次失败是 `internal/dshgw/tenancy` 的 systemd scope 名称冲突
  （`TestStartRetriesUnderAFreshScopeWhenTheLaunchIsRefused`），单测重跑与在 main 工作区重跑都通过，
  与本次改动无关。
