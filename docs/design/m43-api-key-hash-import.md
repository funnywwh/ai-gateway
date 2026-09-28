# M43：API Key 哈希导入（`POST /admin/api/v1/keys/import`）

> 状态：已实现。目标：把**已经在别处生效**的 API Key 搬进网关，让客户端不必改 key，
> 而网关**全程不接触明文**。首个使用场景：gw-b 上 sub2api 的「客户组一」用户与密钥迁移
> （运行手册见 `docs/sub2api-migration.md`）。

## 1. 问题

网关的 API Key 是「只存哈希」的：表里只有 `key_prefix`（明文前 12 字符，建索引用于查找）
与 `key_hash`（明文的 SHA-256），校验时用 `secret.Prefix` / `secret.Hash` 现算比对
（`internal/apikey/verifier.go`）。因此现有唯一的签发入口 `POST /admin/api/v1/keys` 只能
**随机生成**一把新 key 并把明文返回一次。

跨系统迁移时这条路走不通：

- 源系统（sub2api）里这些 key 已经在用户的客户端配置里，换 key 意味着逐人重配；
- 明文一旦被网关或运维脚本经手，就会出现在终端回显、shell 历史、临时文件或对话记录里，
  而「不泄露密钥」正是迁移的硬约束。

**关键取舍**：与其让网关收下明文再自己算哈希（等于让明文穿过网络与进程），不如让**源库
自己算**——PostgreSQL 11+ 自带 `sha256(bytea)`，`encode(sha256(convert_to(key,'UTF8')),'hex')`
与 Go 的 `sha256.Sum256` 逐字节一致（实现时已用同一字符串在两边比对过）。于是迁移脚本只
需要搬运 `key_prefix` 与 `key_hash` 两个非秘密值，明文永远留在源数据库进程内。

## 2. 关键决策

| 决策 | 理由 |
|---|---|
| 新增独立端点而不是扩展 `POST /keys` | 既有端点「生成明文并返回」是一条完整的语义；让它按参数切换成「不生成、只写哈希」会把两种相反的信任模型塞进一个 handler，也会让「明文只返回一次」这句接口承诺变得有条件。 |
| **只接受 `key_prefix` + `key_hash`** | 接口无法校验两者同源，但这不构成风险：写错只会得到一把认证不通过的 key。反过来，若接口接受明文，明文就会经过 HTTP 与进程内存——那正是本设计要避免的。 |
| 拒绝不存在的标签名（400） | 标签解析会**丢弃**找不到的名字（`registry.ResolveTagRecords`），而一个没有任何授权的 key 会回落到 `auth.default_grant`（默认 `all`，即所有供应商）。一个拼错的标签因此不是小错，而是静默放大授权。既有 `POST /keys` 早于这条校验，行为保持不变（见 §6）。 |
| 校验 `account_id` 存在（404） | 否则外键错误会以 500 的形式冒出来，读起来像网关故障而不是「账户 id 写错了」。 |
| 一行一个明文 | 规则是「哈希相同 → 更新（幂等重跑）；不同哈希 → 新建一行」。**M87 之后这条简化了**：当时 `key_prefix` 是唯一索引、也是明文查找的入口，所以还要额外规定「同前缀只允许被导入方自己占用（`created_by` 以 `import:` 开头）→ 否则 409」。现在行的身份是 `key_hash`（唯一索引），前缀只是标签、可以重复，`import:` 标记退回成纯粹的出处信息（`docs/design/m87-api-key-hash-lookup.md`）。 |
| 不接收明文、不返回明文、不把哈希写进审计 | 审计只需要回答「谁在什么时候导入了哪个前缀」；哈希本身不是明文，但没有理由把它抄进一份长期保存、所有管理员可见的日志。 |
| 单条导入，不做批量接口 | 一次导入就是一次审计与一次热更新；批量会把「部分成功」变成需要额外语义的状态机。31 把 key 就是 31 次调用。 |

## 3. 接口

`POST /admin/api/v1/keys/import`（route name `admin_import_key`，`groupKeys`，`roleAdmin`，
`Dangerous: true`；MCP 侧随路由表自动出现在 `admin_endpoints`，scope=admin 才能调用）

请求体：

| 字段 | 必需 | 说明 |
|---|---|---|
| `key_prefix` | 是 | 明文的前 `secret.PrefixLen`（12）个字符，可打印 ASCII 且不含空白 |
| `key_hash` | 是 | 同一明文的 SHA-256 十六进制（64 位；大写会被归一化为小写） |
| `name` | 是 | Key 名称，迁移时沿用源系统名称便于对账 |
| `account_id` / `account` | 二选一 | 所属账户 id 或账户名；账户必须存在 |
| `tags` | 否 | Key 自有标签名数组；**每个名字都必须已存在** |
| `grants` | 否 | 与创建 Key 相同的授权对象（与标签授权取并集） |
| `policy` | 否 | 与创建 Key 相同的扁平策略文档，未知字段 400 |
| `status` | 否 | `active`（默认）或 `disabled` |
| `expires_at` | 否 | RFC3339；省略表示不过期 |

响应（200，**从不含明文、不含哈希**）：

```json
{"id":7,"name":"lilei@example.com","account_id":4,"key_prefix":"sk-000000000",
 "status":"active","tags":["测试标签3"],"created":true,
 "note":"only the prefix and its hash were written: the gateway does not know the plaintext"}
```

错误：`400`（标签字符集/长度、哈希形状、未知标签、非法状态/时间/策略、缺 name 或账户）、
`404`（账户不存在）、`403`（非管理员）、`501`（标签端口未接线，本部署关闭管理面）。
**M87 之后不再有 409**：前缀撞车是合法的，导入只会「更新同一明文那一行」或「新建一行」。

存储侧新增一个只读方法 `DB.FindAPIKeyByHash`：命中返回行，未命中返回 `(nil, nil)`。
它与数据面的 `GetAPIKeyByHash` 刻意不同——数据面上「哈希未知」与「密钥错误」必须不可区分
（都是 401），而导入方必须先分清「新建」与「已存在」才敢覆盖。

## 4. 数据流

```
源库（postgres）                迁移脚本（源主机，仅内存）            aigw
  key 明文 ─┬─ left(btrim(key),12) ──────────────► key_prefix ─┐
            └─ encode(sha256(...),'hex') ────────► key_hash ───┤
             （明文到此为止，脚本从不 SELECT key）               ├─► POST /keys/import
                                                                │     ├ 形状/标签/账户校验
  账户名 ──────────────────────────────────────────────────────┘     ├ FindAPIKeyByPrefix → created / 409
                                                                     ├ UpsertAPIKey（created_by=import:<actor>）
                                                                     ├ audit(action=import, 含前缀、不含哈希）
                                                                     └ reload(注册表 + 清 key 缓存)
```

导入后的校验路径与普通 key 完全一致：`Verify` 用 `secret.Prefix(bearer)` 查找、常数时间比对
`key_hash`、再取账户与标签——**没有任何「导入 key 专用」的旁路**，这是本设计能被信任的前提。

## 5. 异常与边界

- **前缀冲突**：M87 起不存在这回事——前缀是标签，两把 key 可以共用；行的身份是哈希。
  同一明文重导 = 幂等更新（还能顺带改正标签），不同明文 = 新建一行。
- **标签与哈希不同源**：接口无法发现，而且 M87 之后**不再是功能问题**：行按哈希查找，
  标签只是显示内容，写错了只会让控制台里那一栏看着不对（原设计里它会让 key 认证失败）。
- **导入 key 的明文永不可再取**：控制台只能改状态/标签/策略，看不到明文；这正是迁移场景需要的
  性质，但控制台里必须能看出来（列表里 `created_by=import:<actor>`）。
- **`btrim` 与服务端归一化**：数据面先做 `secret.Normalize`（去首尾空白、去掉可能的 `Bearer `）。
  源脚本对明文取 `btrim()` 后再算前缀与哈希，二者一致；手册要求先断言源库里的 key 不含空白字符。
- **热更新**：导入后 `reload` 清空 key 缓存并重载注册表快照；新前缀本来没有缓存条目，重导同一
  前缀时旧的正向缓存条目也会被清掉。
- **审计量**：每把 key 一行 `import`，在 31 把量级下可忽略。

## 6. 与既有端点的一处有意分歧

`POST /admin/api/v1/keys` 至今不校验 `account_id` 是否存在、也不校验 `tags` 里的名字是否存在
（不存在的名字被静默丢弃，授权回落到默认通配）。本次**只在新端点上收紧**：迁移是一次性、
成批、无人在旁边逐个复核的操作，静默降级不可接受；既有端点保持原行为以免破坏控制台与脚本
的既有调用方。是否把同样的校验补到创建端点上，留作后续独立改动（需要同步改控制台文案）。

## 7. 测试策略与依赖

`internal/httpapi/admin_import_key_test.go`（全部先失败后实现）：

| 用例 | 断言 |
|---|---|
| 合成 token → 导入 → 带该 bearer 请求 `/v1/models` | 200；响应体不含明文与哈希 |
| 用 12 字符前缀当 bearer / 截断一个字符的 bearer | 401（前缀只是标签，不是密钥） |
| 审计 | 存在 `action=import` 行，含前缀、不含哈希 |
| 形状表：含空白前缀、超长前缀、短哈希、非 hex 哈希、缺 name、未知账户、未知标签、非法状态、非法时间、含未知字段的策略 | 400 / 404 |
| 同一明文重复导入（换标签） | 200 且 `created=false`、id 不变、表里只有一行、标签被改正 |
| 控制台签发的 key 共用一个前缀（不同明文） | 200 且新建一行；控制台那把 key 照常可用（M87） |
| viewer 角色导入 | 403 且没有写入 |
| `FindAPIKeyByPrefix` | 未命中 `(nil,nil)`；命中返回行 |

同时更新 `internal/httpapi/admin_routes_test.go` 的端点清单（路由表 104 条），
`internal/mcpsrv` 的字段形状守卫随路由表自动覆盖新端点。执行
`go test ./internal/httpapi/ ./internal/mcpsrv/ ./internal/store/` 与 `make verify`。

依赖：仅标准库；无新配置项；无数据库迁移（复用既有 `api_keys` 表与唯一索引）。

## 8. 实现与设计差异

- 设计里未写、实现时补上的一条校验：`account_id` 指向不存在的账户时返回 404（原本会以
  外键错误变成 500）。
- `expires_at` 只接受 RFC3339（不接受 unix 秒、不接受日期），与本项目其它时间字段的对外
  形状保持一致。
