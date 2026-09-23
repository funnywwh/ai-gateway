# M80：API Key 批量导入与按 Key 查账户（`POST /keys/import-batch` + `POST /keys/lookup`）

> 状态：设计定稿，实现中。
> 需求原话：「实现 mcp 导入自定 apikey admin 接口，实现通过 apikey 查询账户的 admin 接口，
> mcp 客户端要能调用」。
> 相关：`docs/design/m43-api-key-hash-import.md`（单条哈希导入，本里程碑不动它）、
> `docs/mcp.md`（后台三工具桥与 §4.5 工具说明标准）、`docs/sub2api-migration.md`（迁移 runbook）。

## 1. 问题

管理面今天的 key 写入只有两条路，都不覆盖"一次把一批自定义 key 接进来"：

| 现状 | 能力 | 缺口 |
|---|---|---|
| `POST /admin/api/v1/keys`（`admin_create_key`） | 随机生成 `sk-gw-…`，明文只返回一次 | 值由网关定，客户端必须改配置；一次一把 |
| `POST /admin/api/v1/keys/import`（`admin_import_key`，M43） | 只收 `key_prefix` + `key_hash`，明文不进网关 | 一次一把；调用方必须先自己算出前缀与 SHA-256 |

两个真实场景因此很别扭：

1. **自定义值（BYO key）**：客户/运营手里有明文 key（`sk-live-…`、别的平台的 key），要让它
   在网关里生效而客户端不改配置。M43 的哈希形式要求调用方先算哈希；而"算出哈希"这件事在
   人手里就是一次手工计算，在脚本里就是一段额外代码。
2. **迁移批量化**：`docs/sub2api-migration.md` 的 31 把 key 是 31 次调用（M43 有意为之），
   运维希望一次提交、一份错误报告。

同时，管理面**没有**反向查询：拿到一把 key（或它的前缀）想知道"这属于哪个账户"，今天只能
`admin_list_keys` 翻页肉眼比对。

MCP 侧（`docs/mcp.md` §4）已经有渐进披露的三工具桥，管理面路由一进表就自动可被 agent 调用，
所以本里程碑**不需要**新增 MCP 工具或改 `internal/mcpsrv`——要做的只是把两个路由加进
`internal/httpapi/admin_routes.go` 这张唯一的真源表。

## 2. 关键决策

| # | 决策 | 取舍理由 |
|---|---|---|
| D1 | 批量走**新增独立路由** `POST /keys/import-batch`，不把 `POST /keys/import` 改成数组 | M43 拒绝过"把两种信任模型塞进一个 handler"；单条端点是 `docs/sub2api-migration.md` 里已发布的 runbook 承诺，改形状等于毁掉它。新端点纯增量 |
| D2 | 批次语义 = **先全量校验、再单事务写入**（fail-fast），**不做部分成功** | M43 拒绝批量的原话是"部分成功会变成需要额外语义的状态机"。逐项校验（账户存在、标签存在、policy 严格解析、批内前缀不重复、与既有行不冲突）→ 任何一项失败即整体拒绝并点名 `keys[i]`，**一行都不写**；修好后重跑（同前缀同哈希幂等）。调用的代价是"一把坏 key 挡住整批"，换来的是"调用方永远不用理解半成品状态" |
| D3 | 每项凭据**二选一**：明文 `api_key`（网关算前缀与哈希）或 `key_prefix`+`key_hash`（M43 形式），**可混在同一批** | 覆盖上面两个场景，且两种形式的校验、冲突判定、响应形状完全相同，只多一个分支。若只做明文形式，迁移脚本就得继续逐把调用；若只做哈希形式，BYO 场景仍缺 |
| D4 | 接受明文是**对 M43「明文不进网关」取舍的一次有意反转**，护栏四条 | 自定义 apikey 的语义就是"值由调用方给"，绕不开明文。护栏：①`secret.Normalize` 后必须可打印 ASCII、无空白、`len > secret.PrefixLen`（否则 `secret.Prefix` 会把整把密钥当前缀存进**明文列**）、`len ≤ 512`；②明文不落库（只写前缀与哈希）；③不回显；④不进审计、不进日志。M43 的单条哈希导入保持不动，迁移仍推荐哈希形式（见 §6） |
| D5 | 导入行仍用 `created_by = "import:<actor>"` 标记，冲突规则照抄 M43 | 同前缀同哈希 → 幂等更新；同前缀不同哈希且既有行由导入产生 → 允许接管（重跑/改正）；其余 → **409**，绝不静默顶掉控制台签发给别人的 key |
| D6 | 审计 = **每项一行**（`action=import`, `target_type=api_key`），不另做批次摘要行 | 保住 M43 承诺的"谁在什么时候导入了哪个前缀"；控制台审计页与 `admin_list_audit_logs` 不必为批量新增渲染分支。行数上限 = 批次上限（200/次），可接受 |
| D7 | 查询接口用 **POST**，不是 GET + query | 明文放 URL 会进浏览器历史与反向代理日志（本网关自身只在 panic 时记 path，但不能替调用方的链路背书）。读语义走 POST 有先例：`POST /pricing/simulate`、`/pricing/validate` |
| D8 | 查询接口 `roleViewer`、非危险接口 | 它只回答"这把 key 是谁的"。viewer 本来就能从 `admin_list_keys` 看到所有前缀与账户，接口不放大任何可见性。它也**不做鉴权判定**：不校验 `status`/`expires_at`，只如实报告——能不能用是数据面 `internal/apikey` verifier 的职责，两处判定分开才不会出现"管理面说能用、请求却被 401"的鬼故事 |
| D9 | 批次项**不含**内容录制开关（`record_input_mode` 等） | 与单条导入一致（固定 `inherit`）；需要时用 `admin_update_key` 补。少一个批量语义分支 |
| D10 | 加 `dry_run`（默认 false）：全程校验与冲突判定，但不写库、不写审计、不 reload | agent 的自然工作流是"先演练再确认"；`admin_request` 对危险接口本来就要求 `confirm=true`，dry_run 让确认前的那次调用有实际内容 |
| D11 | 上限 200 项/次，`decodeJSON` 的 1 MiB 请求体上限不变 | 200 × ~200B ≈ 40KB，余量充足；再大就是"用 HTTP 传数据库导出"，属于另一个设计（且部分成功的压力会回来） |
| D12 | 不改 `internal/mcpsrv`、不加 MCP 工具 | M21 的"三个工具而不是八十个"是既有契约：路由进表即在 `admin_endpoints` 出现、`admin_describe` 给出 schema、`admin_request` 落到同一 handler。"MCP 客户端要能调用"由这张表保证 |
| D13 | 不做控制台 UI | 需求是管理接口 + MCP 可调。控制台入口可以后补（keys 页加两个弹窗即可），不阻塞本里程碑 |

## 3. 接口

### 3.1 `POST /admin/api/v1/keys/import-batch`

路由条目（`internal/httpapi/admin_routes.go` 的 keys 组，紧邻 `admin_import_key`）：

| 字段 | 值 |
|---|---|
| `Name` | `admin_import_keys` |
| `Group` | `groupKeys` |
| `Role` | `roleAdmin`（`adminActor(w, r, true)`） |
| `Dangerous` / `ConfirmReason` | `true` /「会把一批已存在的明文密钥接入网关：知道其中任何一把明文的人立刻可以消费额度」 |
| MCP | 随路由表暴露（`tool=admin_import_keys`），`admin_request` 必须带 `confirm=true` |

请求体：

| 字段 | 必需 | 说明 |
|---|---|---|
| `keys` | 是 | 数组，**1–200 项**；每项见下 |
| `dry_run` | 否 | `true` 只校验与冲突判定，不写库（默认 `false`） |

`keys[]` 每项：

| 字段 | 必需 | 说明 |
|---|---|---|
| `name` | 是 | Key 名称 |
| `account_id` / `account` | 二选一 | 账户 id 或账户名；账户必须存在 |
| `api_key` | 凭据二选一 | **明文**自定义值。`secret.Normalize` 后算 `secret.Prefix`/`secret.Hash`；校验：无空白、可打印 ASCII、`len > 12`、`len ≤ 512` |
| `key_prefix` + `key_hash` | 凭据二选一 | M43 形式：明文前 `secret.PrefixLen`（12）个字符 + SHA-256 hex（64 位） |
| `tags` | 否 | Key 自有标签名数组；**每个名字都必须已存在**，否则 400（不存在的名字会被解析丢弃，授权随即回落到 `default_grant`，属静默放大授权） |
| `grants` | 否 | 与创建 Key 相同的授权对象（与标签授权取并集） |
| `policy` | 否 | 与创建 Key 相同的扁平策略文档，未知字段 400 |
| `status` | 否 | `active`（默认）或 `disabled` |
| `expires_at` | 否 | RFC3339；省略表示不过期 |

成功响应 `200`（**不含明文、不含哈希**）：

```json
{
  "dry_run": false, "total": 2, "created": 1, "updated": 1,
  "keys": [
    {"index": 0, "id": 12, "name": "laptop", "account_id": 4, "key_prefix": "sk-live-abcd",
     "status": "active", "created": true, "tags": ["blue"]},
    {"index": 1, "id": 9, "name": "phone", "account_id": 4, "key_prefix": "sk-live-efgh",
     "status": "active", "created": false, "tags": []}
  ],
  "note": "明文与哈希都不返回、不落库、不进审计；只有前缀与 SHA-256 写入"
}
```

`dry_run=true` 时形状相同，只是省去创建项的 `id`（还没有 id），`created`/`updated` 表示"会发生什么"。

### 3.2 `POST /admin/api/v1/keys/lookup`

路由条目：`Name: admin_lookup_key`、`Group: groupKeys`、`Role: roleViewer`、`Dangerous: false`、
无 `NoTool`（MCP 可见）。

| 字段 | 必需 | 说明 |
|---|---|---|
| `api_key` | 二选一 | 明文；`Normalize` 后算前缀做索引查找，再 `secret.Equal` 比对哈希 |
| `key_prefix` | 二选一 | 恰好 12 字符（与 `importedCredential` 同规则）；按标识查询，不需要秘密 |

响应一律 `200`（"不是我们的 key"是答案，不是错误）：

```json
{"found": true, "matched": "hash",
 "key": {"id": 7, "name": "laptop", "key_prefix": "sk-live-abcd", "status": "active",
         "tags": ["blue"], "account_tags": ["acct"], "effective_tags": ["acct", "blue"],
         "created_by": "import:ops", "expires_at": null, "last_used_at": null,
         "created_at": "2026-09-23T03:00:00Z", "feishu": {"bound": false}},
 "account": {"id": 4, "name": "acme", "status": "active", "tags": ["acct"],
             "dsh_enabled": true, "dsh_tenant": "dsh-acme-4", "feishu": {"bound": false}},
 "note": "这只回答归属与状态，不做鉴权判定；能不能用由数据面 verifier 决定"}
```

```json
{"found": false, "reason": "unknown_prefix"}
```
```json
{"found": false, "reason": "hash_mismatch", "matched": "prefix",
 "note": "该前缀已有行，但你给的明文与它的哈希不匹配；要按标识查询请改用 key_prefix"}
```

`hash_mismatch` **不回显该行的 key/account**：避免把这个接口变成"知道前缀就能读出账户"的通道
（前缀本来就在 `admin_list_keys` 里可见，真要查就用 `key_prefix` 分支）。任何分支都不回显明文/哈希。

## 4. 数据流

```
调用方（控制台 Cookie / MCP admin_request）
   → adminActor(w, r, true|false)              // 角色闸门
   → 逐项校验（纯函数，不写库）
        api_key  → secret.Normalize → Prefix/Hash          （D4 护栏）
        prefix+hash → importedCredential                    （M43 规则）
        name / account / tags 存在性 / policy 严格解析 / status / expires_at
        + 批内前缀去重 + 与既有行冲突判定（FindAPIKeyByPrefix）
   → 任一项失败：400/404/409 + keys[i] 定位，直接返回，库无变化
   → dry_run：返回"会发生什么"，同样不写库
   → UpsertAPIKeys(ctx, keys)：db.write.BeginTx 单事务，逐行 INSERT … ON CONFLICT(key_prefix) DO UPDATE
   → 逐项 s.audit(action=import, target_type=api_key, changes={name, account_id, key_prefix, created, batch:true, source})
   → s.reload(ctx, "api keys imported (batch)", false)   // 清验证缓存（含负缓存）+ registry 重载
   → 200 报告 {index,id,name,account_id,key_prefix,status,created,tags}
```

查询接口：`api_key` → `Prefix` → `FindAPIKeyByPrefix` → `secret.Equal` → `GetAccount` → 组合响应；
或 `key_prefix` → 同上但跳过哈希比对（`matched:"prefix"`）。

## 5. 异常与边界

| 情形 | 结果 | 说明 |
|---|---|---|
| `keys` 为空 / 超过 200 项 / JSON 畸形 / body 为空 | 400 | `WithParam("keys")` / `("dry_run")` |
| 缺 `name`、凭据缺失、两种凭据同时给、`api_key` 含空白或不可打印字符、`api_key` 长度 ≤ 12 或 > 512 | 400 | 消息点名 `keys[i].<field>` |
| 两种凭据同时给 | 400 | 不做"明文优先"的猜测：静默偏向一种就是把另一种当成没写 |
| 账户 id/名不存在 | 404 | 与单条导入一致；不靠外键错误以 500 冒出来 |
| 未知标签名 | 400 | 授权会静默回落到默认通配（放大授权） |
| `policy` 含未知字段 | 400 | 严格解析，与创建 Key 相同 |
| `status` 非 `active`/`disabled`、`expires_at` 非 RFC3339 | 400 | |
| 批内两把同前缀 | 400 | 点名两个下标；绝不让后一项覆盖前一项 |
| 与既有行同前缀、哈希不同、既有行非导入产生 | 409 | 点名 `keys[i]` 与既有行 `created_by` |
| 与既有行同前缀、哈希相同 | 200，`created:false` | 幂等重跑（换标签/账户即修正） |
| 导入前该前缀被负缓存（5s 内认证失败过） | 导入后第一次请求立即生效 | `reload` 里的 `InvalidateAll`；写成测试 |
| 明文值短于等于 12 字符 | 400 | 否则前缀列会存下整把密钥（明文泄漏到列表页与导出里） |
| `dry_run=true` | 不写库、不写审计、不 reload | 响应 `id` 缺失已在字段说明里写明 |
| tags 端口未接线 | 501 | `portReady(w, s.deps.Tags, "tag management")`，与单条导入同款 |
| viewer 角色调用导入 | 403 | 调用 `adminActor(w, r, true)`；查询接口对 viewer 开放 |
| MCP `admin_read` scope 调导入 | 工具层报"需要 scope=admin" | 桥的 `route.Role == roleAdmin && role != admin` 分支 |

## 6. 与既有端点的关系（一处有意分歧）

- `POST /keys/import`（M43）**不动**：仍只收前缀与哈希，仍是迁移 runbook 的推荐路径
  （明文不进网关这条性质只有它能给）。批量接口存在的价值是"一次多把 + 自定义值"，
  不是替代它。
- `POST /keys`（创建）**不动**：它承诺"明文只返回一次"，与"值由调用方给"是相反的信任方向，
  不合并（M43 的同一理由）。
- **明文形式的适用边界**（要写进 `docs/mcp.md` §5）：明文经网关进程与调用方链路，
  在 MCP 场景下还会进入模型上下文；控制台智能问答里调用时，该会话绑定的 Key 若开了输入录制，
  工具参数会随请求正文进入请求日志。因此迁移/大批量导入仍推荐哈希形式；BYO 场景才用明文形式。

## 7. 测试策略与依赖

新增/修改的测试：

| 文件 | 用例 |
|---|---|
| `internal/httpapi/admin_keys_test.go`（新） | ①三把自定义明文一次导入 → 每把 `bearerCall` 200、响应无明文无哈希、库内哈希=明文 SHA-256；②纯哈希批与混合批；③原子性：未知标签/未知账户/批内重复前缀/与控制台 key 冲突 → 400/409 且**行数不变、无新审计**；④重跑幂等（`created:false`）；⑤`dry_run` 不写库不写审计；⑥明文形状表（空白/不可打印/≤12/>512/两式都给/都不给）；⑦负缓存失效（先 401 → 导入 → 立即 200）；⑧viewer 导入 403；⑨lookup 明文命中/前缀命中/未知前缀/哈希不匹配不回显/disabled 与过期如实报告；⑩lookup 对 viewer 开放 |
| `internal/store/keys_test.go` | `UpsertAPIKeys` 单事务：中途失败 → 无行写入；成功 → id 按序返回 |
| `internal/httpapi/mcp_admin_test.go` | `admin_endpoints(filter=keys)` 两行 `tool` 非 null；`admin_describe admin_import_keys` 的数组 schema 与示例可写；无 `confirm` 被拒；scope=admin 带 confirm 成功且 key 立即可用；scope=admin_read 导入被拒、查询可用 |
| `internal/httpapi/admin_routes_test.go` | `expectedAdminPatterns` 增两条；计数断言随之校验 |

依赖：仅标准库；无新配置项；**无数据库迁移**（复用 `api_keys` 表、`key_prefix` 唯一索引与
`created_by` 标记）。执行 `go test ./internal/httpapi/ ./internal/store/ ./internal/mcpsrv/`
与 `go vet ./...`（守卫测试会在 body 字段缺形状/缺示例时直接失败）。

## 8. 实现与设计差异

- **明文最小长度取 16**（设计写的是"`len > 12`"）：`secret.PrefixLen + 4`，留出余量而不是卡在边界上
  （12 字符的 key 本身熵就不足）。上限 512 与设计一致。查询接口**不设**最小长度：一把畸形/被截断的 key
  应当得到 `found:false` 这个答案，而不是一个暴露存储规则的 400。
- **两种凭据同时给 → 400**（设计只写了"二选一"）：不做"明文优先"的猜测，静默偏向一种就是把另一种当成没写。
- **账户已不存在的 key**：`lookup` 返回 `found:true` + `account:null` + 说明，而不是 404——这种悬挂行正是
  支持工单会遇到的，404 会把 Key 的前缀藏起来。
- **`account_id` 与 `account` 同时给时以 `account_id` 为准**（与单条导入一致，设计中未写明）。
- **`importedPrefix` 从 `importedCredential` 中抽出**：查询接口需要同一套 12 字符前缀规则而不需要哈希，
  规则只留一份。
- **store 层抽出 `upsertAPIKeySQL` 与 `upsertAPIKeyWith`**：单条与批量共用同一段写入语句与同一套默认值，
  设计只要求"单事务"，实现顺带消掉了两条路径漂移的可能。
- **`docs/mcp.md` 的路由计数 86 → 143**：该数字在 M21 之后就没跟上过（M49/M60/M66/M70/M72/M77 都在加路由），
  本次顺手更正。
- **测试多了一条设计没写的**：导入前该前缀被负缓存（5s NegativeTTL）时，导入后第一次请求必须立即成功——
  这正是"错误提示告诉你导入 key"之后客户端会遇到的状态。
