# sub2api → ai_gateway 用户与 API Key 迁移

> 面向使用者的运行手册。用于把**同一台机器上** sub2api（PostgreSQL + docker compose）里某个
> 客户群体的用户与 API Key 搬到 ai_gateway，并按标签把用户分配到已有的上游订阅上。
> 首次落地：gptjp（`8.211.157.165`）上备注为「智天成」的 22 个用户（2026-09-14，M43）。
>
> 工具：`scripts/sub2api-migrate.py`（服务器上放 `/opt/aigw/sub2api_migrate.py`，0700）。

## 1. 迁移到底搬了什么

| 搬 | 不搬 |
|---|---|
| 用户 → aigw **账户**（`accounts`，名称=sub2api 用户名，备注记源 user id 与邮箱） | 余额、充值、计费历史（留在 sub2api） |
| API Key → aigw **key**（沿用原明文：只写入前 12 字符前缀与 SHA-256） | 已删除的 key、`quota_exhausted` 的 key（报告列出） |
| Key 的可用状态（active → active，disabled → disabled） | key 的 5h/1d/7d 限额与用户的并发上限（aigw 的策略只能挂在 key/tag 上，逐 key 复制会放大额度） |
| 标签绑定（决定这把 key 走哪个上游供应商） | sub2api 里的分组、渠道、订阅、账单关系 |

**明文不进网关**：aigw 的 key 表只有 `key_prefix` 与 `key_hash`，导入接口
（`POST /admin/api/v1/keys/import`，见 `docs/design/m43-api-key-hash-import.md`）只收这两个值。
脚本对源库的查询用 SQL 现算：

```sql
left(btrim(k.key),12)                                  -- key_prefix
encode(sha256(convert_to(btrim(k.key),'UTF8')),'hex')   -- key_hash（与 Go 的 sha256 一致）
```

脚本**从不 `SELECT key`**，所以明文既不出源库进程，也不会出现在终端、文件、日志或对话里。

## 2. 迁移前必须核对的事实（`plan` 自动做，全过才允许 `apply`）

1. 源用户集合：`users.notes = '<备注>' AND deleted_at IS NULL`，逐个报告 id/邮箱/用户名/状态。
2. 源 key 集合：`deleted_at IS NULL AND status='active'`；报告被排除的 key（已删除、配额耗尽）。
3. key 形状：长度、`sk-` 前缀、全是可打印 ASCII、`key = btrim(key)`（否则前缀与哈希会与服务端
   归一化后的明文不一致）。
4. **前缀可以重复**（M87 起）：`left(btrim(key),12)` 只是给人看的标签，数据面按 SHA-256 查找，
   所以两把 key 共用前 12 个字符照原样导入即可，谁都不用换 key。`plan` 会把共享前缀的 key 列出来
   供人核对，但不再因此停下（§6 是 M87 之前的口径，留作历史）。
5. 目标侧干净度：要用的标签存在、每个标签的 `grants_json` 指向的供应商存在且启用；目标账户名
   未被无关账户占用（`accounts.name` 唯一，脚本按名 upsert）。
6. 目标侧前置状态：目标库按**哈希**判重——同一明文已在库里就是幂等更新（重跑），不在就是新行。

## 3. 标签分配规则

标签在 aigw 里就是授权：`tags.grants_json` 决定这把 key 能走哪些供应商。所以「把用户分配到
标签」= 决定他的流量落到哪个上游订阅。

默认规则（`plan` 会打印逐 key 结果，可先复核再落库）：

1. **保留源系统意图**：如果 key 所属的 sub2api 分组里**只有一个**成员账号是本次迁入 aigw 的
   供应商（按 `providers.meta_json.source_account_id` 对应），则该 key 落到「授权该供应商的标签」。
   例：gptjp 分组 21/22/23（1/2/3 研发帐号）→ 各自唯一的 Codex 账号 → 标签 蓝精灵2/3/1。
2. **均衡剩余**：其余 key 按源 key id 升序，逐个落到「(a) 全局计数最少 → (b) 该用户已占用最少 →
   (c) 标签 id 最小」的标签。规则确定、可复算，与执行顺序无关。

gptjp 首次迁移的结果（32 把 key）：蓝精灵1 = 11、蓝精灵2 = 11、蓝精灵3 = 10。

## 4. 怎么跑

```sh
# 0. 只读预检：打印数据集、映射表、计数与全部断言；有失败就退出
python3 /opt/aigw/sub2api_migrate.py plan

# 1. 回滚点：在线快照 aigw 数据库（0600），并断言本次迁移前没有别的账户/key/请求日志
python3 /opt/aigw/sub2api_migrate.py snapshot

# 2. 建账户 + 导入 key（幂等，可重跑；账号按名 upsert、key 按哈希 upsert）
python3 /opt/aigw/sub2api_migrate.py apply

# 3. 结构核对：逐把 key 比对 aigw 库里的 prefix/hash/tags 与源库重算值，并读回生效标签
python3 /opt/aigw/sub2api_migrate.py verify

# 4. 报告：写 /opt/aigw/data/sub2api-migration-<ts>.json（0600，无密钥材料）
python3 /opt/aigw/sub2api_migrate.py report
```

`--gateway` / `--password-file` / `--psql` / `--notes` 可覆盖默认值；`--only-user <id>` 与
`--limit <n>` 用于分批；任何 `apply` 之前都会重跑一次 §2 的断言。

## 4.5 批量化与归属核对（M80）

一次迁移原来是"31 把 key 就是 31 次调用"（M43 有意如此）。M80 起可以一批提交（上限 200 项/次）：

```json
{"name":"admin_request","arguments":{"name":"admin_import_keys","confirm":true,
 "body":{"keys":[{"name":"lzhichao@lagenio.com","account":"acme",
                  "key_prefix":"sk-62e1a0b4c","key_hash":"<64 位 hex>","tags":["蓝精灵3"]}]}}}
```

三条与单条导入一致的性质值得重申：

1. **迁移仍用哈希形式**：`apply` 搬的就是 `key_prefix`+`key_hash` 两个非秘密值，明文留在源库进程内。
   批量接口虽然也接受明文 `api_key`（给"客户端不改 key"的自定义值场景用），但迁移**不要**用它——
   那等于把明文送进网关进程与调用链，见 `docs/mcp.md` §5。
2. **整批原子**：任何一项失败（未知账户、未知标签、哈希格式错）就整体拒绝，
   错误点名 `keys[i]`，一行都不写；修好后重跑是幂等的（同前缀同哈希 → `created:false`）。
3. **先 `dry_run`**：`"dry_run":true` 用同一套校验与冲突判定回报"会发生什么"，不写库、不写审计，
   适合在真正 `apply` 之前拿一次预演结论。

逐把对账用 `admin_lookup_key`（`POST /admin/api/v1/keys/lookup`）：给明文或 12 字符前缀，回答
`key`（含 `tags`/`effective_tags`/`created_by`/`status`/`expires_at`）与 `account`（含 `dsh_tenant`）
两个对象；未命中返回 `found:false`。只读接口，`admin_read` 令牌就够——迁移报告里可以拿它复核
"这把 key 落在谁的账户上、生效标签是不是预期"。

## 5. 迁移后必须做的验证

| 层 | 做法 | 通过标准 |
|---|---|---|
| 结构 | `verify`：源库重算的 prefix/hash 与 aigw 行逐把比对 | 全部一致；每把 key 恰好一个标签；账户不带标签 |
| 授权 | 读 `GET /admin/api/v1/keys?account_id=…` 的 `effective_tags` | 等于预期标签；**不存在空标签的 key**（空标签 = 回落到默认通配授权） |
| 功能 | 每个标签挑一把 key，建控制台问答会话（`POST /admin/api/v1/chat/sessions` 指定 `account_id`+`api_key_id`，无需明文）发一句短提问，然后删除会话 | 有真实回答；`usage_records.provider_id` 等于该标签授权的供应商 |
| 负向 | 用某个 key 的 12 字符前缀当 bearer 请求 `/v1/models` | 401（前缀是标签，不是密钥） |
| 保密 | 对迁移日志、报告文件、`journalctl -u aigw` 搜 `sk-[A-Za-z0-9_-]{20,}` | 命中 0 |

## 6. 前缀冲突（M87 之前的口径，留作历史）

> **2026-09-24 起本节不再是迁移的边界。** M87 把数据面的查找键从 12 字符前缀换成了 SHA-256
> （`docs/design/m87-api-key-hash-lookup.md`）：前缀只是标签、可以重复，`api_keys.key_prefix`
> 上也没有唯一索引了。于是两把 key 撞前缀时**两把都照原样导入**，两把各自可用，没有人需要换 key。
> 下面的流程只描述 M87 之前的世界（gptjp 上 #24/#51 就是这么处理的：`docs/todo_done.md` 里记着
> 保留 #51、把 #24 重签成 aigw #44）。

当时 12 字符前缀是查找入口且唯一，撞了就**只能有一把保留原明文**：

1. 保留一把（默认取最近使用的那把；`plan` 报告会标出冲突对）；
2. 另一把在 aigw 用控制台/`POST /admin/api/v1/keys` **重新签发**，把新明文单独交付本人；
3. 报告里记录「源 key id → 新 aigw key id」，并提醒该用户换 key。

## 7. 回滚

| 场景 | 做法 |
|---|---|
| 标签绑错 | `PATCH /admin/api/v1/keys/{id}` 改 `tags`，不必回库 |
| 整批撤销 | `systemctl stop aigw` → 用 `snapshot` 产出的 `aigw.db.pre-sub2api-<ts>` 覆盖 `/opt/aigw/data/aigw.db`（并删 `-wal`/`-shm`）→ 启服务。前提是迁移前该实例没有别的账户/key/请求日志（`snapshot` 会断言） |
| 版本回滚 | `/opt/aigw/aigw.prev-<时间戳>` 换回并重启（发布流程留下的回滚点） |
| 源系统 | 迁移**不动** sub2api；要停用源 key 是另一步（见 §8） |

## 8. 已知差异与收尾

- **双跑**：默认不改 sub2api，用户在两边都能用；同一批 ChatGPT 订阅额度因此由两个系统共享。
  要停用时执行 `UPDATE api_keys SET status='disabled', updated_at=now() WHERE id IN (...)`，
  并先确认这些用户已经改用 aigw 的地址与同一把 key。
- **计费**：aigw 侧不继承 sub2api 的余额与额度；若 aigw 未配置价格表，请求成本记 0（准入按
  `billing.reserve_micros_default` 计算，价格缺失时会保留一笔默认占用）。
- **模型名覆盖**：两边可解析的模型名集合不一定重合。迁移前用 `usage_logs` 统计待迁用户在用的
  模型名，与 aigw 的 `models`/别名/映射对照，缺的名字要么补 `models`+`provider_models`+路由
  （补完必须真实请求验证一次），要么提前通知用户改名。
- **分组语义损失**：源系统里指向「未迁入 aigw 的账号」的分组（例如 apikey 型 deepseek 账号）
  在这边没有对应标签；这些 key 会按 §3 落到已有标签，若需要保留它们的原路由，得先建对应标签。

## 9. 整库重建（全量重导）：gptjp，2026-09-23

§1–§8 是**增量**迁移（按 `users.notes` 挑一批人，往已有实例里加）。这一节是**整库重建**：
把目标实例清成空库，再从 sub2api 全量重导，使它成为源系统的镜像。

> 工具：`scripts/sub2api-reimport.py`（服务器上放 `/opt/aigw/sub2api_reimport.py`，0700）。
> 与 §1 的工具同一条保密纪律：前缀与哈希由源库 SQL 现算（从不 `SELECT key`），供应商凭据只在
> 进程内与 0600 临时文件里出现（写完即碎），所有打印/异常都过 `redact()`，报告里没有密钥材料。

### 9.1 子命令与执行顺序

```sh
python3 /opt/aigw/sub2api_reimport.py inventory   # 只读：两侧盘点、人群映射、前缀冲突、模型覆盖、断言
python3 /opt/aigw/sub2api_reimport.py snapshot    # 回滚点 + 重建素材（DB/config 备份 + rebuild-export-*.json）
python3 /opt/aigw/sub2api_reimport.py wipe        # 换空库（config 补 bootstrap.admin → 停服 → 移走 db → 起服 → 断言空）
python3 /opt/aigw/sub2api_reimport.py providers   # 按源上游账号建供应商（凭据经 0600 文件）
python3 /opt/aigw/sub2api_reimport.py models      # 恢复对客模型/上游模型/路由 + azure 部署发现与补齐
python3 /opt/aigw/sub2api_reimport.py tags        # 三个标签与授权
python3 /opt/aigw/sub2api_reimport.py accounts    # 建账户 + 导入 key（含前缀冲突重签、替换密钥重导）
python3 /opt/aigw/sub2api_reimport.py selftest    # 三标签各一次真实请求 + 逐模型探测 + 负向 401
python3 /opt/aigw/sub2api_reimport.py verify      # 逐把比 prefix/hash、生效标签、分桶计数
python3 /opt/aigw/sub2api_reimport.py report      # 写 sub2api-reimport-<ts>.json（0600）
```

除 `wipe` 外都可反复跑（账户按名 upsert、key 按前缀 upsert）；`wipe` 发现库内还有业务数据时会拒绝，
除非显式 `--force`。`repair-prefix --keep-source-key N [--disable-key M]` 用于前缀归属纠偏（见 §9.4）。

### 9.2 人群 → 标签（先分组后备注，可复算）

| 标签 | 判定 | grants |
|---|---|---|
| `E26Q` | 有活跃 key 属于分组 20 | `providers:[azure]`、`models:["*"]` |
| `智天成` | 否则 `notes='智天成'` | `providers:[deepseek]`、`models:["*"]` |
| `电商` | 其余 | `providers:[azure]`、`models:["*"]` |

标签写在**账户级**（key 继承），一个账户恰好一个标签。2026-09-23 那次的结果：账户 20 / 88 / 2。

### 9.3 供应商映射（源账号被软删就不建）

| 源账号 | provider | kind |
|---|---|---|
| 2 / 3 / 9 | `lzhichao-lagenio-3-expiry` / `lizhichao-wisskys-3-expiry` / `liuhui-wisskys-8-expiry` | `plugin:provider-codex` |
| 6 | `codex-zhuyecheng` | `plugin:provider-codex` |
| 7 / 8 | `deepseek` / `deepseek-dianshang` | `openai-chat` |
| 10 | `azure` | `openai-responses` |

四条口径：

1. **供应商名只允许 `[A-Za-z0-9._-]`**（网关校验），所以「deepseek电商」落名 `deepseek-dianshang`。
2. **codex 供应商建而不接流量**：本次没有任何标签授权它们，因此不会触发 OAuth 刷新、也就不会把
   sub2api 手里那份正在使用的 `refresh_token` 抢掉。要启用必须两步：先给供应商补上游模型+路由，
   再把 provider 加进某个标签的 grants —— 并先确认 sub2api 侧已停用该账号（同一个 ChatGPT 账号
   只有一方能刷新）。
3. `antigravity`（platform=antigravity，Google Cloud Code 系）没有对应 kind，**不建**。
4. **不调用** `POST /providers/{id}/actions/set_token`：不给任何 OAuth 凭据做主动刷新。

### 9.4 前缀冲突：保留「实例原本持有的那一把」

> **M87 之后这一段是历史。** `sub2api-reimport.py` 的冲突闸门保留着（它是保守的，不会再造成错），
> 但新库按哈希查找、前缀可以重复，所以同一前缀的两把 key 都能原样重导，不需要再挑一个保留、
> 也不需要任何替换密钥。

12 字符前缀是数据面查找入口且唯一（M87 之前）。冲突时保留谁**不能**按「最近使用」拍脑袋：要保留**这台实例
在换库前就持有该前缀的那把**（生产里正在用的），否则等于让一个真实客户端掉线。换库后目标库是空的，
所以判定必须看 `snapshot` 产出的 `rebuild-export-*.json`（里面有换库前的 key→账户快照）。

gptjp 本次：`sk-f69aeca55` 同时属于源 key `#24`（E26Q）与 `#51`（郑晓婷），实例原本持有者是郑晓婷
⇒ 保留 `#51`，`#24` 不导入。E26Q 那一边**不需要新的明文**：它在 9/14 就拿到过这把 key 的替换密钥
（`/opt/aigw/data/E26Q-reissue-key.txt`，0600），重建时把它**原样重导**即可，客户端不用改配置。
脚本误判过一次（换库后按「最近使用」选了 `#24`），用 `repair-prefix --keep-source-key 51 --disable-key 128`
纠偏，并把这次误判的成因写进 `plan_reissues` 的注释里。

### 9.5 azure：部署以**真实请求**为准（本次最大的运维结论）

azure 供应商没有可用的部署目录接口：`POST /providers/{id}/models/refresh` 返回空，`GET <base_url>/models`
给的是 Azure AI Foundry 的**模型市场目录**（430 条，与本资源的部署无关），`/openai/deployments` 已下线。
所以「这个部署到底存不存在」只能用真实请求去证。`selftest` 按证据分三类：

| 证据 | 处理 |
|---|---|
| HTTP 200 | 可用 |
| `upstream_404 · The API deployment for this resource does not exist` | 部署不存在 → 路由 `enabled=false`，写进报告 |
| `upstream_400 · The requested operation is unsupported` | 探测方式不适用（图像模型不接受文本请求）→ 保持开启，记为「未证实」 |

2026-09-23 gptjp 的 azure（源账号 10「azure ChatGPT官key」）实测结果：

- **可用**：`gpt-5.6-sol`、`gpt-5.6-luna`、`gpt-5.6-terra`、`gpt-4o`；另 `gpt-image-1.5`、`gpt-image-2`
  的上游不是 404 而是 400（探测方式不适用），保持开启。
- **不可用（部署不存在，路由已停用、上游模型行已删）**：`gpt-5.5`、`gpt-6-astra`、`gpt-5.4`、
  `gpt-5.4-mini`、`gpt-image-1`、`deepseek-v4-flash`、`deepseek-v4-pro`。
- ⚠️ 电商人群近 30 天用量最高的两个名字 `gpt-5.5`（12,158 次）与 `gpt-6-astra`（3,152 次）**在这台
  azure 上服务不了**。要么在 azure 资源里补这两个部署，要么把 `deepseek`（或某个 codex 供应商）
  也加进「电商」标签的 grants。

### 9.6 清库的连带与唯一登录方式

`wipe` 会把 `request_logs` / `usage_records` / `ledger_entries` / 控制台问答 / MCP 令牌一起清掉，
并且**这些都不在 sub2api 侧**。两条必须做的事：

1. `config.yaml` 会追加 `bootstrap.admin`（口令沿用 `/opt/aigw/.admin-password`，文件保持 0600）——
   没有它清库后无人能登录管理接口。gptjp 原本没有 `bootstrap` 段，本次是新增的。
2. **MCP 令牌要在控制台重新签发**（明文只显示一次，不在对话/文件里出现），并更新到对应客户端。

### 9.7 回滚

```sh
systemctl stop aigw
cp -p /opt/aigw/data/aigw.db.pre-rebuild-<ts> /opt/aigw/data/aigw.db   # 并删掉 -wal/-shm
systemctl start aigw
```

`config.yaml.pre-rebuild-<ts>` 用于配置回滚；`data/plugin-state/` 全程未动，所以 codex 供应商的
令牌链不受影响。不做逐条反向删除（网关没有删除账户/key 的路由）。

