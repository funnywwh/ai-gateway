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
4. **前缀唯一性**：待迁 key 的 `left(btrim(key),12)` 必须两两不同。网关的 `api_keys.key_prefix`
   上有唯一索引，且数据面**只按 12 字符前缀查找**——同一个前缀不可能同时存在两把不同的 key。
   发现冲突时**停下来**，按 §6 处理（不要试图导入两次：第二次会覆盖第一把，用户静默掉线）。
5. 目标侧干净度：要用的标签存在、每个标签的 `grants_json` 指向的供应商存在且启用；目标账户名
   未被无关账户占用（`accounts.name` 唯一，脚本按名 upsert）。
6. 目标侧前置状态：本次迁移要写入的 key 前缀在 aigw 里要么不存在，要么就是同哈希（重跑）。

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

# 2. 建账户 + 导入 key（幂等，可重跑；账号按名 upsert、key 按前缀 upsert）
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
2. **整批原子**：任何一项失败（未知账户、未知标签、前缀被别人的 key 占用、哈希格式错）就整体拒绝，
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
| 负向 | 用某个 key 的 12 字符前缀当 bearer 请求 `/v1/models` | 401（前缀是索引，不是密钥） |
| 保密 | 对迁移日志、报告文件、`journalctl -u aigw` 搜 `sk-[A-Za-z0-9_-]{20,}` | 命中 0 |

## 6. 前缀冲突（唯一会让人掉线的边界）

12 字符前缀是查找入口，`api_keys.key_prefix` 唯一。若两把待迁 key 撞了前缀（随机 key 属小概率，
但 sub2api 允许自定义 key，会人为造成），**只能有一把保留原明文**：

1. 保留一把（默认取最近使用的那把；`plan` 报告会标出冲突对）；
2. 另一把在 aigw 用控制台/`POST /admin/api/v1/keys` **重新签发**，把新明文单独交付本人
   （交付前不要把它写进任何文件或对话）；
3. 报告里记录「源 key id → 新 aigw key id」，并提醒该用户换 key。

不要用「导入两次」绕过：第二次会覆盖第一把。

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
