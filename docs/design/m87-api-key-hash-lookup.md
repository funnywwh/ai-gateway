# M87：API Key 按哈希查找，前缀降级为展示列

## 0. 需求原话

> 能不能改成 apikey 的 id 查找，api 请求时，先 hash，用 hash 查找，前缀只是用于给用户显示？

> 我就是要解决"前缀必须唯一"，因为 sub2api 库里的 key 12 位前缀有重复

第二句是**驱动需求**：sub2api 允许自定义 key，前缀重复真实存在——gptjp 的 #24/#51 同为 `sk-f69aeca55`（两把 key 前 37 字符相同、只差 4 位），`docs/sub2api-migration.md` §6 因此让其中一个客户换了 key。所以本里程碑有一条**硬验收标准**：**前缀重复的两把 key 必须能同时导入、同时可用**（§6、§7）。

三句话合起来是一件事，本设计按下面这条读法落地：

- **鉴权查找键换成 `key_hash`**：请求带 `Bearer`，网关先算 SHA-256，用哈希等值点查唯一一行，再常数时间比对；
- **`key_prefix` 不再是身份**：它降级为"给人看的提示"，可以重复，也不再支撑任何唯一约束；
- 第一句里的「用 apikey 的 id 查找」（把 key id 嵌进 token 按 id 点查）**不采纳**，理由见 §2 D2。

## 1. 现状与证据

| 事实 | 证据 |
|---|---|
| 查找键是前缀，`key_prefix` 上有唯一索引 | `internal/store/migrations/0001_init.sql:140`、`internal/apikey/verifier.go:103-118`、`docs/api-responses.md`「认证与限速」 |
| 前缀熵很低：`sk-gw_` + 6 个 base32 = **30 bit**；MCP 是 `aigw_mcp_` + 3 个 = **15 bit** | `internal/ids/ids.go`（`New()` = 前缀 + `_` + 24 位 base32）、`internal/secret/secret.go:14`（`PrefixLen = 12`） |
| 撞车概率不可忽略 | 生日界：API key 1 万把 ≈ 4.6%、2 万把 ≈ 17%；MCP token 200 个 ≈ 46% |
| 签发路径没有预检，撞上就是"静默接管" | `internal/httpapi/admin.go:341`（创建 key）、`admin_catalog.go:1637`（创建 MCP token）→ `UpsertAPIKey`/`UpsertMCPToken` 的 `ON CONFLICT(key_prefix/token_prefix) DO UPDATE`（`internal/store/keys.go:110`、`keys.go:453`） |
| 负缓存按**前缀**键控，命中负条目直接 401、不再比哈希 | `internal/apikey/verifier.go:107-116`、`242-247`：知道某个前缀（前缀不是密钥）的人可以持续投毒，把该前缀下的合法 key 打成 401 |
| 真实发生过一次 | gptjp 上 sub2api 的 key #24/#51 前 12 字符同为 `sk-f69aeca55`（两把 key 前 37 字符相同、只差 4 位），按 `docs/sub2api-migration.md` §6 重签了一把（`docs/todo_done.md:2304`） |
| 导入路径的一堆特例，根源都是"前缀唯一" | `docs/design/m43-api-key-hash-import.md` §33（`import:` 归属规则）、`docs/design/m80-key-batch-import-and-lookup.md` §D5（批内前缀去重 + 409 规则） |
| 现代 dshgw 不依赖前缀 | `internal/httpapi/v1.go:1103`（"every key of this account logs into it, so dshgw no longer needs a per-key prefix binding"）；`internal/dshgw/proxy/proxy.go:1105-1118`（`ByPrefix` 只在 aigw 没回 tenant 时的 legacy 兜底） |
| 限速早已按 key id，不按前缀 | `internal/httpapi/v1.go:106` `scopeForKey(key.ID)` |
| id 作为运营身份已经在用 | `request_logs.api_key_id` / `usage_records.api_key_id`、控制台各页显示 `#id`、`APIKeyLabel{Name, Prefix}` 只是读时标签（M30） |

## 2. 关键决策

| # | 决策 | 理由 / 代价 |
|---|---|---|
| D1 | **鉴权查找键 = `key_hash`**（`key_hash` 上建唯一索引），`key_prefix` 降级为展示列 | 256 bit 的键实际不可能撞（对比现状 30 bit / 15 bit）；仍是唯一索引点查，热路径性能不变；哈希早已在库里 → **已发的 key 零迁移** |
| D2 | **不采纳「token 内嵌 id / 按 id 查」+ pepper** | 按 id 点查本可行，且能顺带把裸 SHA-256 换成带 pepper 的 HMAC；但 M43/M80 的导入要求"明文不进网关"，即**存库的校验值必须是调用方自己也能算的函数**（迁移脚本用 `encode(sha256(convert_to(btrim(key),'UTF8')),'hex')` 现算）。加 pepper 等于废掉导入路径，收益不抵代价 |
| D3 | 正缓存、负缓存、失效、touch 节流**全部按哈希键控** | 顺带修掉 §1 第 5 行的负缓存跨 key 401；`Invalidate` 的入参从"前缀"改成"哈希" |
| D4 | **放开前缀唯一性**：删唯一索引，保留非唯一索引 | 一个明文一行由 `key_hash` 唯一保证；导入冲突规则塌缩为"同 hash → 幂等更新，否则新建"，可整段删掉 `import:` 归属判定、批内前缀去重与 409 分支 |
| D5 | 哈希形式导入时 `key_prefix` **可选、免校验**（给什么存什么，纯展示）；明文形式仍由网关算前 12 字符 | 网关本来就无法验证调用方给的 prefix 与 hash 是否同源（m43 §93）。放开后，"前缀与哈希不同源"从一个"key 永远认证失败"的坑，降级成"展示标签写错了" |
| D6 | MCP token 同规则（`token_hash` 唯一、`token_prefix` 展示） | 收益最大：`aigw_mcp_` + 3 个 base32 = 15 bit |
| D7 | **分两步上线，每步都能回滚**（§6） | 第一步只加哈希索引（老二进制仍按前缀查，回滚安全）；第二步才放开前缀唯一性并改接口形状 |
| D8 | 哈希不写进日志、审计、响应 | 沿用 m43 §34：它在库里当索引键，但不外流 |
| D9 | dshgw 侧本里程碑不改 | `POST /v1/dshgw/authorize` 走 `Verifier.Verify`，自动跟着变；`ByPrefix` 只是 legacy 兜底；`keys.map` 只装租户自己的键，唯一性由 `registry.validateUnique` 保证，与 aigw 前缀是否唯一无关。**M88 会把这条 legacy 兜底整个删掉**（`docs/design/m88-dshgw-account-tenant-binding.md`），此前它仍在，所以 D13 的预检在 M87 期间是必需的 |
| D10 | **不动 `secret.PrefixLen`（保持 12）** | 前缀是**明文片段**，加长它同时削弱密钥；且 `secret.Prefix` 是"按 `PrefixLen` 截断"的派生函数，已存行无法重算。M87 之后前缀不再需要唯一，加长换不到任何东西。量化见 §2.1 |
| D11 | **不改哈希算法**（不用 MD5；确需缩短就用 SHA-256 截断到 128 bit） | 用 MD5 的唯一收益是索引短一半（每行几十字节），代价是引入一个已被选择前缀碰撞攻破的哈希；截断 SHA-256 拿到同样长度且不背这个名声 |
| D12 | **不做"保留前缀查找、只放开唯一性"那条中间路** | SQLite 的 `ON CONFLICT(key_prefix)` 要求该列上有唯一约束：唯一索引一删，`UpsertAPIKey(s)`/`UpsertMCPToken` 的冲突列就必须换（→ 需要 `key_hash` 唯一索引），verifier 还得改成"取候选行列表 + 逐个比哈希"并重做负缓存键。既然哈希唯一索引非建不可，直接按哈希查是更少的机器：一次点查 vs 一次前缀点查 + N 次比较 |
| D13 | **mint 侧的"预检 + 重生成"保留到 M88 上线为止**（原计划在第二步删掉） | ① dshgw 的 legacy 兜底（`proxy.go:1104-1118`，`docs/dshgw.md` §3 第 3 步）仍按 12 字符前缀把 key 映射到租户；aigw 自己 mint 的 key（含 `mintDshgwKey` 签的租户 worker 凭据）永不撞前缀 ⇒ 那条路继续安全、dshgw 零改动；② 控制台/支持场景里同一个前缀尽量不出现两行。代价：每次签发多一次索引点查。**M88（删掉本地解析）之后前缀不参与任何认证，这条从"安全必需"降级为"展示观感"，届时可重新评估** |

### 2.1 评审中被否掉的两个替代方案

**替代 A：把前缀加长（12 → 18），让"前缀唯一"不再危险。**

加长确实能消掉撞车，但它只治一件事，并且自带两个代价：

- 前缀是明文片段，而库里同时存着整串的 SHA-256——**"隐藏了多少位"直接决定库被拖走后能不能离线爆破**。`sk-gw_` + 24 位 base32 的 token（30 字符）：前缀 12 字符 → 隐藏 90 bit（安全）；18 字符 → 隐藏 60 bit（勉强）；20 字符 → 隐藏 50 bit（GPU 天级）。
- `secret.Prefix` 是截断派生：改常量会让**已存行的前缀对不上**（老 key 的明文不在库里，18 字符前缀算不出来）。只能让 verifier 依次试 [新长度, 12]（每次缓存未命中 = 2 次索引点查 + 双长度负缓存），而且 `bootstrap` 的种子 key 会因为"按新前缀查不到"而**再插一行**。dshgw 侧还硬编码了 12（`cmd/dshgw/runtime.go:237`、`registry.validPrefix`）。

| 前缀长度 | API key 熵 | MCP 熵 | 1 万把 API key / 200 个 MCP token 撞车概率 | 库泄露后隐藏位数（API key） |
|---|---|---|---|---|
| 12（现状） | 30 bit | 15 bit | 4.6% / 46% | **90 bit** |
| 16 | 50 bit | 35 bit | 0.0000044% / 0.00006% | 70 bit |
| 18 | 60 bit | 45 bit | ~0 / 0.000000057% | 60 bit |
| 20 | 70 bit | 55 bit | ~0 / ~0 | 50 bit |

结论：M87 之后前缀不再需要唯一，加长它换不到东西，还会削弱密钥。真要加长，必须**同时加长 token**（新 key 用更长的格式），那是独立的一个里程碑。

**替代 B：新 key 存 MD5（而不是 SHA-256）。**

- 若拿它当**查找/校验值**：等于 M87 换了个哈希。唯一收益是索引短一半（32 vs 64 字符，万级 key 差几十 KB，可忽略），代价是引入一个已被选择前缀碰撞攻破的哈希——对未知目标的第二原像虽然仍不可行（≈2^123），但本项目没有任何理由选它。**真要省长度就用 SHA-256 截断**：取前 16 字节 = 128 bit = 32 hex，长度与 MD5 相同，且同样"调用方自己也能算"（SQL 里 `substr(encode(sha256(convert_to(btrim(key),'UTF8')),'hex'),1,32)`）。
- 若拿它当**展示句柄**（`key_prefix` 列存 md5）：一次买到"唯一 + 不可逆 + 定长 32"，但有两个硬伤：① 人拿着 key 看不出它的 md5，"报前 12 个字符"这条最常用的支持路径断掉（M80 的明文查询分支还在，但要求用户把整把 key 贴进来）；② 它把哈希搬进控制台/审计展示面，与 m43 §34「不把哈希抄进长期日志」冲突（M87 D8）。M87 保留 `key_prefix` = "明文前 12 字符"的语义，正是为了保住这个人机接口。

## 3. 数据库迁移（两步 = 两个文件）

### 3.1 `0028_api_key_hash_index.sql`（第一步）

```sql
-- 鉴权改为按 SHA-256 查找：key_hash 成为新的唯一键。
-- 前缀索引这一步不动（老二进制仍按前缀查，可安全回滚）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_tokens_hash ON mcp_tokens(token_hash);
```

**前置条件**：库里不能已有重复 `key_hash`。重复只可能来自 M43/M80 的哈希形式导入（同一个明文配了不同前缀导入两次）——mint 路径不可能产生。有重复时 `CREATE UNIQUE INDEX` 失败 → 迁移在事务内回滚（`internal/store/migrations.go:110-128`）→ 网关启动失败，报 `store: apply migration 0028_api_key_hash_index: UNIQUE constraint failed: api_keys.key_hash`。

**为什么不自动处理**：两行哈希相同意味着同一个明文被登记了两次，删哪一行都是替人做决定（沿用 M43 的 409 哲学）。运维先看再决定：

```sql
SELECT key_hash, group_concat(id), group_concat(key_prefix), group_concat(name)
FROM api_keys GROUP BY key_hash HAVING count(*) > 1;
-- 保留一把（例如 last_used_at 更新的那把），另一把 disable 或删除，再重启
```

### 3.2 `0029_api_key_prefix_display_only.sql`（第二步）

```sql
-- 前缀不再是身份：唯一索引 → 普通索引（供"按前缀问归属"与运维查询）。
DROP INDEX IF EXISTS idx_api_keys_prefix;
DROP INDEX IF EXISTS idx_mcp_tokens_prefix;
CREATE INDEX IF NOT EXISTS idx_api_keys_prefix_lookup   ON api_keys(key_prefix);
CREATE INDEX IF NOT EXISTS idx_mcp_tokens_prefix_lookup ON mcp_tokens(token_prefix);
```

列本身不变（仍 `TEXT NOT NULL`，允许空串），没有表重建。

## 4. 代码改动清单

| 文件 | 改动 | 步骤 |
|---|---|---|
| `internal/store/keys.go` | 新增 `GetAPIKeyByHash` / `FindAPIKeyByHash` / `GetMCPTokenByHash` / `ListAPIKeysByPrefix`；`UpsertAPIKey(s)`、`UpsertMCPToken` 的 `ON CONFLICT` 列换 `key_hash` / `token_hash` | 一 |
| `internal/apikey/verifier.go` | 查找键、缓存键、负缓存键、`Invalidate` 全部换哈希；`storeNegative` 语义从"压制这个前缀"变成"压制这把 token" | 一 |
| `internal/httpapi/admin.go` | 创建 key：生成后先 `FindAPIKeyByPrefix` 预检，撞了就重新生成（≤3 次，避免第一步仍存在的唯一索引导致 500）；`InvalidateKey(target.KeyPrefix)` → 传哈希 | 一 |
| `internal/httpapi/admin_catalog.go` | MCP 签发同上预检 | 一 |
| `internal/httpapi/mcp.go` | `/mcp` 按哈希验证 | 一 |
| `cmd/aigw/main.go` | `InvalidateKey` 回调签名 `func(prefix string)` → `func(hash string)` | 一 |
| `internal/store/bootstrap.go` | 种子 key 的"已存在则 merge"判定从按前缀改成按哈希 | 二 |
| `internal/httpapi/admin.go`、`admin_keys.go` | 导入冲突规则塌缩：同哈希 → 幂等更新，不同哈希 → **新建一行**（前缀撞车不再 409）；删掉 `import:` 归属判定与批内前缀去重；**保留**第一步加的预检重试（D13） | 二 |
| `internal/httpapi/admin_keys.go` | `keys/lookup` 的 `key_prefix` 分支返回 `matches[]`（§5） | 二 |
| `internal/httpapi/admin_field_schemas.go`、`admin_routes.go` | MCP 工具的字段说明与路由文案：前缀不再是索引键；导入字段放宽 | 二 |
| `scripts/sub2api-migrate.py` | 删 `duplicate_prefixes()`、§2.4 断言、§6 重签流程与报告里的冲突列；`admin_lookup_key` 的返回形状跟着改 | 二 |
| `internal/webui/static/js/pages/*.js` | 无需改（前缀继续显示）；可选：`keys.js` 对重复前缀加个提示 | 二（可选） |
| 文档 | `docs/design/m4-auth-quota-metering.md` §1、`docs/api-responses.md`、m43、m80 §D5/§D6、`docs/sub2api-migration.md` §2.4/§6、`docs/design/m30-request-log-owner-dimensions.md`、`docs/mcp.md` | 二 |

## 5. 接口形状变化（第二步）

1. `POST /admin/api/v1/keys/lookup`
   - `api_key`（明文）分支**不变**：哈希唯一锁定一行，仍返回 `key` + `account`。
   - `key_prefix` 分支从"唯一命中"变成"0..N 命中"：`{"found":true,"matched":"prefix","count":N,"keys":[…]}`，元素与 `admin_list_keys` 同形（含 `account_id`），不再回顶层 `account`。
2. `POST /admin/api/v1/keys/import`、`/keys/import-batch`
   - `key_prefix` 变为可选：给了就存，不校验 12 字符、不校验与哈希同源。
   - 明文形式仍要求 `len > secret.PrefixLen + 4`（护栏本意是"别把整把密钥写进展示列"，与唯一性无关，保留）。
   - 同哈希再次导入 = 幂等更新（会改写展示前缀）；不同哈希但同前缀 = 两行。
3. MCP token 同理：`token_prefix` 只是展示。
4. **不变**：控制台各页、审计行、`request_logs` 的 `api_key_name`/`api_key_prefix` 标签、门户 payload、`sk-gw_` token 形状。

## 6. 上线顺序与回滚

**第一步（只加哈希索引 + 签发预检 + 换查找键）**

- 迁移 0028；`verifier` / store / `InvalidateKey` 签名按 §4 第一列改。
- 老二进制仍能工作（前缀索引没动）→ 回滚 binary 即可。
- 验证：随机抽 3 把线上 key 打 `/v1/models` 200；刚改过策略的 key 立即生效（失效路径）；控制台钥匙页正常。

**第二步（放开前缀唯一性 + 接口形状 + 脚本/文档）**

- 迁移 0029；§4 第二列改完；migration 脚本与文档同步。
- 回滚注意：0029 之后**老二进制按前缀查会变得不确定**（同前缀多行时 `QueryRow` 任取一行）。真要回滚，先把重复前缀的行处理掉再回滚，或直接前滚。
- 验证：库里人为造一对"同前缀、不同明文"的 key，两把都能各自鉴权成功；`keys/lookup` 传该前缀返回 `count=2`。
- **本次的真实验收数据**：gptjp sub2api 的 #24/#51（同前缀 `sk-f69aeca55`、不同哈希）——两把都要能导入并各自 200（`docs/sub2api-migration.md` §2.4/§6 的"前缀冲突"流程随之删除）。

> （可选）只服务一个部署、且能接受"回滚前先清掉重复前缀"这个限制时，0028 + 0029 可以**同版本上线**，省一次发布。两步拆分的唯一目的是让"回滚二进制"始终安全。

## 7. 测试策略

- `internal/apikey`：① 同前缀两把 key（哈希不同）各自成功，且各自命中缓存；② 负缓存投毒不再影响另一把（同前缀 + 错误明文打一次，再用正确明文仍成功）；③ `Invalidate(hash)` 立即生效；④ 未知哈希 401 并进负缓存。
- `internal/store`：① 迁移幂等；② 0028 在存在重复哈希时失败并回滚（构造重复行断言报错）；③ `UpsertAPIKey`：同哈希更新、不同哈希新建、同前缀两行共存；④ `ListAPIKeysByPrefix` 返回 2 行。
- `internal/httpapi`：① 创建 key 时桩生成器连续吐同前缀 → 重试后两把都能用、控制台显示两个不同 id；② 导入：同哈希不同前缀 → 一行（展示前缀被改写）、不同哈希同前缀 → 两行、批内同前缀 → 允许（不再 400）；③ `keys/lookup` 两个分支的形状；④ MCP 签发撞前缀（15 bit 那档）被重试兜住。
- `scripts/sub2api-migrate.py`：`plan` 不再因前缀冲突停下；`apply`/`verify` 逐把比对 prefix/hash 仍通过；报告不再有"重签"列。
- **真实数据验收（驱动需求）**：拿 gptjp sub2api 的 #24/#51（同前缀、不同哈希）走一次 `keys/import-batch` 的 `dry_run`，再真实导入：库里出现两行同前缀、不同哈希；两把明文各自打 `/v1/models` 得 200；`keys/lookup` 传 `sk-f69aeca55` 返回 `count=2`。
- 回归：`docs/sub2api-migration.md` §5 的负向用例（拿前缀当 bearer → 401）仍然成立——**前缀不是密钥**这条性质不变。

## 8. 依赖与非目标

依赖：仅标准库与现有 SQLite；无新配置项；两个迁移文件（一个加索引、一个降级索引）。

非目标：① 不改 token 形状（仍是 `sk-gw_` / `sk-gw-` + 高熵随机串）；② 不引入 pepper/HMAC（D2）；③ 不改 dshgw 的租户前缀绑定（legacy 兜底保留；绑定现代化 = account id 为主、哈希兜底，单独立项）；④ 不动 sub2api 侧数据；⑤ 不做"前缀重复就自动改名/去重"之类的展示魔法；⑥ 不改 `secret.PrefixLen`（保持 12）与哈希算法（保持 SHA-256）——理由见 §2.1。

## 9. 实现与设计差异

（实现完成后回填）
