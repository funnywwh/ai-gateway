# M30：请求日志的用户（账户）与 API Key 维度

> 规格文档：`docs/request-log.md` §2/§4/§6。前置：`docs/design/m27-request-dimensions.md`
> （身份七列与维度统计）、`docs/design/m24-console-pagination.md`（分页与排序器契约）。
> 起因（用户原话）：「请求日志添加用户,api key 维度统计」。

## 1. 目标

请求日志从 0001 起就在每一行写了 `account_id` 与 `api_key_id`（服务路径与本地拒绝路径都写），
但这两个事实一直是「只有 id、没有名字、不能筛、不能统计」的半成品：

- 列表/详情回的是裸数字，读的人得自己去「账户」页或「API Keys」页对 id；
- 只能按 `account_id` 过滤（而且控制台根本没有这个控件），**不能按 Key 过滤**；
- 维度统计的白名单（`group_by`）里没有它们。

于是「谁在用、用哪个 Key、各自花了多少」——请求日志最该回答的问题之一——在库里
查得到、在界面上答不出。本里程碑把**账户**与 **API Key** 补成与 client/model/workspace 同级的
一等维度：列表列、筛选、维度统计、详情、MCP 与文档。

### 成功标准

| # | 标准 |
|---|---|
| 1 | 列表/详情每行带 `account_name` / `api_key_name` / `api_key_prefix`（读时解析，行上仍只存 id） |
| 2 | `GET /admin/api/v1/requests?api_key_id=N` 精确过滤，可与 `account_id`、六个身份维度、`days` 窗口叠加；`total` 与页一致 |
| 3 | `GET /admin/api/v1/requests/dimensions?group_by=account\|api_key` 按账户/Key 汇总请求数、已计量数、token、成本，并带名字 |
| 4 | 控制台：列表新增「用户」「API Key」两列；工具栏新增账户、Key 两个筛选下拉（Key 随账户联动）；「维度统计」卡新增两个分组；详情弹框显示用户与 Key |
| 5 | MCP：`admin_request_dimensions` 的 `group_by` 自动多出两个取值；查询工具 `list_requests` / `get_request` 返回 `api_key_id` + `api_key_name` |
| 6 | 历史行/异常行不丢：`account_id`/`api_key_id` ≤ 0 归入「（未知）」桶；id 有值但没有对应行时显示 `#id` 而不是空白 |
| 7 | 迁移 0009 两条索引生效，按账户/Key 筛选的列表 EXPLAIN 走新索引且无 temp B-tree；插入放大按 M27 口径实测并记录 |
| 8 | `make verify` 全绿；`make ui-check` requests 视图断言 48 → ≥60 全绿 |

## 2. 关键决策

| # | 决策 | 理由与取舍 |
|---|---|---|
| D1 | 「用户」= **账户**（`accounts`），维度名 `account`，控制台列头写「用户」，详情写「用户（账户）」 | 请求只携带凭据，`api_key_id → account_id` 是唯一可归因的身份。`portal_users` 与 API Key 没有绑定（门户只有自助登录，`internal/portal` 没有数据面），因此「哪个门户用户在调用」今天**无法判定**；控制台也没有门户用户页面。做成维度名 `account` 是让界面用词、DB 表名、既有 `account_id` 参数三者一致 |
| D2 | 名字**读时解析**，不落列（与 token/成本同一条口径，M27 D3） | ① 改名不应分裂/合并分组——id 才是身份，名字是标签；② `accounts.name` 唯一，但 `api_keys.name` **不唯一**（同一账户可重名），按名字分组会把两个 Key 并成一个；③ 两表都没有硬删除路由（`api_keys` 只有 status 切换），按 id 一定查得到名字；④ 落列 = 再抄一份会静默漂移、不参与任何不变量校验的可变事实 |
| D3 | 页面行的名字用**批量点查**（`AccountNames`/`APIKeyLabels`，形制同 `RequestUsages`），**不把 join 塞进页面查询** | 页面查询的 `ORDER BY created_at DESC, id DESC` 依赖 `idx_request_logs_time`（M24 §8.10 修掉的排序器物化整个窗口）；`accounts`/`api_keys` 也都有 `id` 与 `created_at`，join 进来会让这两个列名变歧义，逼迫改写 `historyPageOrder` 契约与它的 EXPLAIN 断言。M27 为 usage 选的正是这条路径，这里沿用 |
| D4 | 维度聚合**不改 SQL**：`group_key` 是数字 id 的字符串形式，名字由 handler 用同一套批量点查补上 | 聚合查询会扫整个窗口；在它里面 join `accounts`/`api_keys` 等于**窗口内每一行**各做一次 PK lookup，而 top-N（≤200，通常 20）之后补标签最多 200 次。顺带让列表、详情、聚合共用同一个取名机制，只有一处解释「名字是读时标签」 |
| D5 | 维度名 `account` / `api_key`，过滤参数 `api_key_id`（与既有 `account_id` 对称） | `group_by` 白名单在末尾追加，默认 `group_by=client` 不变（既有调用方行为不变） |
| D6 | 迁移 0009 加两条 `(维度, created_at, id)` 索引：`idx_request_logs_account`、`idx_request_logs_key` | 与 M27 三条同形：等值前缀让「按账户/Key 筛选的列表」从「扫整个窗口的每一行（含 1 MiB 正文的溢出页）」变成索引前缀扫描；带 `id` 列才能让等值前缀继续满足 `ORDER BY created_at DESC, id DESC`（否则退回 temp B-tree）。键都是小整数，两条约 +30 B/行，对比 M27 三条的 +150 B/行 |
| D7 | 账户/Key **不参与 `redact_paths`** | 它们来自凭据本身，不是请求正文的解析结果——正文口径管不到凭据维度。要「让某个 Key 不留痕」，操作者的手段是停用 Key，而不是脱敏列名。写进 `config.example.yaml` 与规格文档，避免读者以为 `redact_paths: [api_key_id]` 有效 |
| D8 | `account_id`/`api_key_id` 非法值返回 **400**（现在是静默忽略） | 「筛了却返回全部」正是 `/invoices?account_id=` 修过的同一类静默失败（`docs/TODO.md` M24 段）。两个参数行为必须一致，所以既有 `account_id` 的静默忽略一并改掉，并在规格文档里写明 |

### 2.1 为什么不做「Key 名字快照列」

把名字抄进 `request_logs` 的诱惑是「读的时候不用查第二张表」。代价：

1. 名字会漂移：改名后新行写新名、旧行留旧名，同一个 Key 在维度统计里裂成两桶；
2. Key 停用/重命名是运维日常动作，而日志保留期 30 天，两套名字会长期并存；
3. 它不参与任何不变量校验（钱有 `sum(ledger.charge) == sum(usage.charge_micros)` 守着，
   名字没有），漂移不会被任何测试发现。

id 是身份、名字是标签，这条分工与 usage/ledger 的分工一致。

## 3. 接口

### 3.1 `internal/domain`（`entities.go`）

```go
type RequestLogFilter struct {
    AccountID int64
    APIKeyID  int64   // 新增：0 = 不过滤，与 AccountID 同义
    From, To  time.Time
    Client, Model, ResolvedModel, Workspace, SessionID, CallKind string
}

// APIKeyLabel 是 Key 维度的读时标签：操作者起的名字 + 稳定前缀。不含 hash。
type APIKeyLabel struct{ Name, Prefix string }
```

`RequestLogRecord` **不动**：`AccountID`/`APIKeyID` 从迁移 0001 起就在列里。

### 3.2 `internal/store`（`request_logs.go` + 迁移 0009）

- `requestLogFilter`：`if f.APIKeyID > 0 { where += " AND " + prefix + "api_key_id = ?" }`。
- `requestLogGroupExpr`：新增 `case "account": return "CAST(r.account_id AS TEXT)"`、
  `case "api_key": return "CAST(r.api_key_id AS TEXT)"`；错误信息列出全部 8 个取值。
- `RequestLogDimensionNames` += `"account", "api_key"`（末尾追加）。
- 新增两个批量点查（形制照抄 `RequestUsages`：`strings.Repeat("?,", n)` 占位、去重、
  跳过 ≤0、空入参返回非 nil 空 map、错误统一包装）：

```go
func (db *DB) AccountNames(ctx context.Context, ids []int64) (map[int64]string, error)
func (db *DB) APIKeyLabels(ctx context.Context, ids []int64) (map[int64]domain.APIKeyLabel, error)
```

- 迁移 `0009_request_log_owner_indexes.sql`：

```sql
CREATE INDEX IF NOT EXISTS idx_request_logs_account ON request_logs(account_id, created_at, id);
CREATE INDEX IF NOT EXISTS idx_request_logs_key     ON request_logs(api_key_id, created_at, id);
```

### 3.3 `internal/httpapi`

- `AdminStore` += `AccountNames`、`APIKeyLabels`（只有 `store.DB` 实现这个接口，无测试替身要跟改）。
- `requestLogFilterFromQuery` 改为 `(domain.RequestLogFilter, error)`：非法数字 → 400（D8）。
- 新 helper `ownerLabels(ctx, accountIDs, keyIDs)`：一次批量点查两张表，返回两张 map；
  列表页、详情、维度统计共用。
- 列表 payload 增 `account_name` / `api_key_name` / `api_key_prefix`（保留 `account_id`/`api_key_id`）；
  详情同样三个字段；维度统计在 `group_by=account|api_key` 时给每组补名字。
- `handleAdminRequestDimensions`：group key 解析回 id 取名；id ≤ 0 输出 `key: ""`
  （与历史行「（未知）」桶同一处理），**该组计数保留**。
- `admin_routes.go`：`dimensionQueryFields()` 增 `api_key_id`，并把 `account_id` 从
  `/requests` 的显式声明移入该 helper——`/requests/dimensions` 本来就接受 `account_id`，
  只是没在路由表里声明，MCP `admin_describe` 因此看不到它。

### 3.4 `internal/mcpsrv`（`service.go`）

- `listRequests`：每行增 `api_key_id`、`api_key_name`（每次调用一次
  `store.ListAPIKeys(ctx, accountID)` 建 map；该查询失败时只回 id，不让工具失败）。
- `getRequest`：同样两个字段。
- 后台工具 `admin_request_dimensions` 的 `group_by` 取值由 `store.RequestLogDimensionNames`
  自动扩展，无需改代码。

### 3.5 控制台（`internal/webui/static/js/pages/requests.js`）

- `DIMENSIONS` 前置 `['account','用户（账户）']`、`['api_key','API Key']`。
- 列（插在「状态」之后）：`用户`（`account_name`，无则「—」，title 显示 `账户 #id`）、
  `API Key`（`api_key_name`，无则 `#id`，title 显示 `key_prefix · #id`）。
- 工具栏：账户下拉（`/accounts?limit=1000`）、Key 下拉（`/keys?limit=1000`，选了账户就带
  `account_id`）；切账户 → 重载 Key 选项 + Key 筛选复位 + 分页复位；两者都只在有值时进
  `filterParams()`。
- 维度统计卡：`dimensionKeyCell(row, groupBy)` 对两个新分组渲染 `名字（#id）`（前缀进 title），
  空 key 沿用「（未知）」。
- 详情弹框 `identityBlock` 增 `用户（账户）`、`API Key`（名字 + `#id` + 前缀）。

## 4. 数据流

```
写入（本里程碑不动）：
  POST /v1/responses ──▶ recordContent / recordDenied ──▶ request_logs(request_id, account_id,
                                                             api_key_id, …, 身份七列)
读取：
  列表  ──▶ ListRequestLogsPage(filter{APIKeyID}) ──▶ RequestUsages(ids)          → usage
        └─▶ AccountNames / APIKeyLabels(页内去重 id)                              → 名字
  统计  ──▶ RequestLogDimensions(filter, group_by=account|api_key) → key=id 字符串
        └─▶ AccountNames / APIKeyLabels(top-N 的 id)                              → 分组名
  控制台 ─▶ /requests + /requests/dimensions（服务端筛选与聚合）
            + /accounts、/keys（只用于下拉选项）
```

## 5. 异常与边界

| 场景 | 行为 |
|---|---|
| `account_id`/`api_key_id` ≤ 0（历史行、极少数兜底行） | 维度归入「（未知）」桶（`key: ""`），计数保留；列表该列显示「—」 |
| id 有值但没有对应行（当前模型下不该发生：两表都无硬删除） | 显示 `#id`，不丢行、不 500 |
| 名字查询报错 | 与 `RequestUsages` 一致：整页返回错误。同一个库、同一时刻，页面查询多半也失败了；把审计行降级成空白比报错更糟 |
| 本地拒绝的请求（无 usage 行） | 照计入账户/Key 桶，`metered` 计数暴露差异——这正是「哪个 Key 被拒最多」的读法 |
| 骨架行重试（同一 request_id 二次写入） | `account_id`/`api_key_id` 本就在插入列里且不参与 `ON CONFLICT DO UPDATE`，第二次写入不会抹掉第一次的归属；加测试钉住 |
| 控制台下拉超过 1000 个 Key | 下拉只列前 1000（配置类列表 `pageConfig` 的既有上限，与 keys 页一致）；API 过滤对任意 id 仍精确——已知边界，写进文档 |
| `account_id=abc` | 400 并指出参数名，而不是静默返回全部账户 |
| 迁移 0009 在 752 MB 库上 | `CREATE INDEX` 需要一次写锁窗口；实测耗时与 WAL 增长记入 §7 |
| 索引实测超阈值 | 若两条索引的页写放大 > 1.25×（M27 四条悲观上界 1.54× 的一半），退化为**只加 `idx_request_logs_key`**（本期唯一新增的过滤列），`account_id` 过滤继续走时间索引。数字与结论一并写入 §7 |

## 6. 测试策略

- `internal/store/request_dimensions_test.go`：`api_key_id` 过滤对页与计数一致（含与
  `account_id`、窗口叠加）；`group_by=account|api_key` 分组正确；白名单错误信息列出 8 个取值；
  `AccountNames`/`APIKeyLabels` 的批量、去重、缺失 id 缺席而非报错、≤0 跳过、空入参返回空 map；
  聚合 SQL 不含正文列（扩展现有断言）；骨架行重试不抹 `account_id`/`api_key_id`。
- `internal/store/pagination_test.go`：`TestHistoryListsAreOrderedByAnIndexNotASorter` 增
  `by_account`、`by_api_key` 两例（无 TEMP B-TREE）；另加一条断言：两条索引存在于
  `sqlite_master`（迁移落地即可被证明，不依赖 EXPLAIN 文本措辞）。清理语句计划按 M27 的做法
  人工 EXPLAIN 比对并记入 §7，不加易碎的 SQL 字面量测试。
- `internal/httpapi/request_dimensions_test.go`：列表/详情的三个新字段（含无对应行时为空）；
  `api_key_id` 过滤 + `total`；非法 `account_id`/`api_key_id` → 400；`group_by=account|api_key`
  的汇总与名字；空 key 未知桶。测试夹具补一个建 Key 的 helper（`newAdminFixture` 目前只有账户）。
- `internal/webui/embed_test.go`：新增 `TestConsoleDimensionOptionsMatchTheServer`，钉住控制台
  维度选项与 `store.RequestLogDimensionNames` 不漂移（同 `TestConsoleRecordingModesMatchTheServer`
  的形态，起因是当年 `meta`/`metadata` 那次漂移）。
- `make ui-check`：requests 视图断言 48 → ≥60（两列、两个下拉与联动、筛选值进原始 URL、
  统计卡两个分组的名字与「（未知）」桶、详情里的用户与 Key）。
- `make verify` 全绿。

## 7. 实测

探针保留在 `internal/store/index_write_probe_test.go`（默认 skip，只有给环境变量才跑），
所以下面的数字可以复算——与 M27 §2.2 同一形态：**WAL 页字节/行**（确定性指标，墙钟会被
checkpoint 抖动淹没），2.5 KB 行、每 256 行一个事务、行按真机局部性生成（同一会话连续
200 行，账户/Key 在会话内稳定）。

| 方案 | WAL 字节/行 | 相对 | 墙钟 |
|---|---|---|---|
| 现状（0008 的 6 条索引） | 4709.8 | 1.000× | 28.6 µs/行 |
| **+0009 两条（采用）** | 4937.7 | **1.048×** | 31.5 µs/行 |
| +0009，每行不同账户/Key（悲观上界） | 6945.1 | 1.475× | 33.3 µs/行 |

悲观上界 1.475× 落在 M27 为「加 4 条索引」估的 1.54× 之内，而真实局部性下只有 1.048×：
两条索引的键都是小整数，约 +30 B/行。**结论：采纳**（判定阈值是 1.25×，实测 1.048× 远在之下）。

查询计划（真实列表列清单 EXPLAIN，`data/aigw-local.db` 与其迁移后副本对照）：

| 语句 | 0009 之前 | 0009 之后 |
|---|---|---|
| 列表（`api_key_id=?`） | `idx_request_logs_time`（窗口扫描 + 残余过滤） | **`idx_request_logs_key`**（等值前缀） |
| 列表（`account_id=?`） | 同左 | **`idx_request_logs_account`**（等值前缀） |
| 列表（无筛选） | `idx_request_logs_time` | 一致 |
| 清理 `DELETE … ORDER BY id LIMIT 500` | 主键点查 + 子查询 SCAN | **完全一致** |

等值前缀带来的差别（6 万行、5000 个账户、2.5 KB 正文、投影全部列的一页）：

| | 一页（50 行，账户过滤） |
|---|---|
| 0009 之前 | 59.9 ms |
| 0009 之后 | **92 µs** |

没有索引时 SQLite 只能在时间索引上顺序扫、逐行过滤，直到凑够 50 行或扫完窗口——而投影
包含正文列，所以扫的是**每一行的正文页**。

迁移代价（在真实库的副本上测，718 MB / 3430 行 `request_logs`）：`Open`（含迁移 0009 的
两条 `CREATE INDEX`）共 **758 ms**，WAL 增长 0.1 MB。生产上的落地方式是下次启动自动应用，
期间持一次写锁。

## 8. 依赖

无新增包、无新增第三方依赖。改动落在 `internal/domain`、`internal/store`（含一条迁移）、
`internal/httpapi`、`internal/mcpsrv`、`internal/webui`、`scripts/ui-harness` 与 `docs/`。

## 9. 实现与设计差异

1. **§7 的探针被保留下来**（`internal/store/index_write_probe_test.go`，默认 skip）。计划只要求
   「实测并记录」；把探针留在仓库的理由是 M27 §2.2 留下的观察项（workspace/call_kind 索引）
   下次要用同一把尺子量，代码比文档更能保证口径一致。
2. **`account_id`/`api_key_id` 非数字从「静默忽略」改为 400**（D8）——计划里已写明，但它是一次
   行为变更：既有 `account_id` 的语义从「筛不了就返回全部」变为「报错」，规格文档 §4 与路由表
   文案同步写明。
3. **控制台「维度统计」默认分组改为「用户（账户）」**（原先默认是列表第一个选项 `client`）。
   这是为了让打开页面的人先看到「谁在花」；代价是 UI 走查里两条断言要显式切到 `model` 分组去
   验证「已计量 / 未计量」的显示（已在 harness 里注明原因）。
4. **MCP 的 `get_request` 也加了 `api_key_id`/`api_key_name`**（计划里 `list_requests` 与
   `get_request` 都加，实现一致）；两次调用各自做一次 `ListAPIKeys`，失败时只回 id 而不报错。
5. 「`dimensionQueryFields` 增 `api_key_id` 并把 `account_id` 移入」按计划落地：现在 `/requests`
   与 `/requests/dimensions` 都声明了这两个参数（此前 `/requests/dimensions` 接受 `account_id`
   却没在路由表里声明，MCP 的 `admin_describe` 因此看不到它）。
