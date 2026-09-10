# M12 设计：账期账单、充值与对账补偿

> 规格：`docs/billing.md` §5–§7（已输出）。前置：M11a 计价、M11b 账本与在途额度。

## 1. 范围

| 子系统 | 内容 |
| --- | --- |
| 账单 L4 | 账期计算、`invoice_lines` 物化、状态流转 draft→issued→paid / void、导出 CSV/JSON |
| 充值 L5 | `topup` / `credit_grant`（可带到期）/ `adjustment` / `refund`；兑换码批量生成与单次核销 |
| 对账 L6 | 每日对账（用量复算 vs 账本）、差异落库与告警、失败重放、不变量巡检结果落库 |

包结构：`internal/billing/{invoice,credits,reconcile}.go` + `internal/store/{invoices,credits,reconcile}.go`；
管理面 `internal/httpapi/admin_billing.go` 扩展；控制台新增「账本与账期」页面替换占位页。

## 2. 账期与账单

- 账期：默认自然月；`period_start_day`（1–28）可把账期起点挪到每月某天，`timezone` 决定边界（内嵌 tzdata）。
  函数 `PeriodFor(at, cfg) (start, end time.Time)` 是**纯函数**，便于测试与回放。
- 物化：`POST /accounts/{id}/invoices` 或每日作业生成 `draft`，按 `group_by`（默认 model，可选 key/day）聚合
  `usage_records`，写入 `invoice_lines`（requests / prompt_tokens / completion_tokens / cost / charge 双列）。
- 幂等：`(account_id, period_start, period_end)` 唯一；已存在时返回既有账单，除非 `force=true` 且状态仍是 `draft`。
- 状态：`draft → issued → paid`，`draft|issued → void`；**issued 之后不可变**（内容不再重算），
  改错只能 `void` 后重开，或记账本 `adjustment`。
- 后付账户「标记已付」写一笔 `topup`（`ref_type=invoice`）作为还款，走同一账本与幂等键。
- 导出：`GET /invoices/{id}?format=csv|json`。

## 3. 充值与兑换码

- `POST /accounts/{id}/credits`：`kind ∈ topup|credit_grant|adjustment|refund`，金额为微美元（正数=入账）。
  赠送额度带 `expires_at`，到期由每日作业按 `expire` 冲销**未使用部分**（不动充值余额）。
  幂等键：`ref_id` 必填（外部流水号或人工单号）→ `credit:{kind}:{ref_id}`。
- 兑换码：批量生成 N 个，明文只返回一次；库里只存 `code_hash`（PBKDF2 过重，用 SHA-256 + 前缀盐）。
  核销单事务：`UPDATE redemption_codes SET redeemed_by_account_id=?, redeemed_at=? WHERE code_hash=? AND redeemed_by_account_id IS NULL`，
  影响行数 1 才写账本，保证并发下**只有一次成功**；金额入账用 `credit_grant`（带到期则同样受到期作业管理）。
- 到账后按 `auto_resume` 规则自动恢复被暂停的账户。

## 4. 对账与补偿

- `POST /billing/reconcile`（或每日 cron）：对给定窗口，
  1. 按账户汇总 `usage_records.charge_micros`（真源）与 `ledger_entries(kind='charge')`（记账结果）；
  2. 差异 ≠ 0 时把样例（≤100 条 request_id）写进 `details_json`；
  3. 计算估算占比（`usage_source='estimated'` 的比例，基点）；
  4. 写 `billing_reconciliations`，差异 ≠ 0 触发 `billing.reconcile_mismatch` hook；
  5. 同时跑四条不变量巡检，把结果并入 `details_json`。
- **补偿重放**：`billing_failures` 未解决行 + 兜底文件按幂等键重放（M11b-1 已实现文件重放，这里补库表重放与 `resolved_at`）。
- 对账**只读不写账本**：任何修正都必须走 `adjustment`，保留人工痕迹。

## 5. 管理面

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/accounts/{id}/invoices` | 账单列表 |
| POST | `/accounts/{id}/invoices` | 生成/获取账期账单（`{period, group_by, force}`） |
| GET | `/invoices/{id}` | 详情（含行项目；`?format=csv` 导出） |
| POST | `/invoices/{id}/issue|void|pay` | 状态流转 |
| POST | `/accounts/{id}/credits` | 充值/赠送/调整/退款 |
| GET | `/accounts/{id}/credits` | 额度明细（复用账本，过滤 kind） |
| POST | `/redemption-codes` | 批量生成（返回明文一次） |
| GET | `/redemption-codes` | 列表（只显示 hash 前缀与状态） |
| POST | `/redemption-codes/redeem` | 核销（需要账户；可指定目标账户） |
| POST | `/billing/reconcile` | 触发对账（`{days, account_id?}`） |
| GET | `/billing/reconciliations` | 历史对账结果 |
| POST | `/billing/failures/replay` | 重放失败结算 |

## 6. 并发与性能

- 账单物化是**读多写少**的管理操作：一条 SQL 聚合 + 批量插入行项目，单事务完成；
- 兑换码核销是唯一需要在并发下严格一次的写路径，靠 `UPDATE ... WHERE ... IS NULL` 的条件更新 + 账本幂等键双保险；
- 对账是只读扫描，按账户分组，避免 O(用量行数) 的样例收集（只保留前 100 条样例）。

## 7. 测试

1. `PeriodFor`：自然月、`period_start_day=15`、跨时区边界；
2. 账单：生成 → 行项目聚合正确 → 幂等重放 → issue 后不可重算 → pay 写还款 topup → void；
3. 充值：topup/grant/adjustment/refund 四条路径的账本与余额；幂等键重复提交不重复入账；
4. 兑换码：批量生成只回明文一次、并发核销仅一次成功、过期码拒绝、核销后余额增加；
5. 对账：账本被人为破坏后能发现差异并记录样例；恢复后差异归零；
6. HTTP：上述路径的端到端用例（含 403/404/409 语义）。

## 8. 实现与设计差异

1. **账期边界用纯函数 `billing.PeriodFor`**，时区走 `pricing.LoadLocation`（支持 UTC/Local/固定偏移/IANA），
   与计价引擎共用同一套时区解析，避免「计价按 +08:00、账期按 UTC」这类不一致。
2. **`store.AppendLedger` 改成返回「本次真正写入的条数」**。原实现靠比较余额判断是否重复入账，
   在 `INSERT OR IGNORE` 命中冲突时余额字段保持 0，导致**重复提交的充值被判为已入账**
   （实测抓到：同一 ref_id 连提两次，第二次 `applied=true`）。现在冲突时回填当前余额并返回 0。
   该签名变化同时改了 `domain.Store` 与所有调用点。
3. **账单行聚合用 SQLite 的 `json_extract`** 直接从 `dimensions_json` 取分维度 token 数，
   不需要把用量行读进内存；分组支持 model/key/day。
4. **`invoice_lines` 在 `draft` 状态可重算**（`force=true` 会先删旧行再写），`issued` 之后直接 409；
   设计里只说「issued 后不可变」，实现把「重算」这条路径也显式挡住了。
5. **后付账户 `pay` 才写还款账本**（`topup`，`ref_type=invoice`，幂等键 `invoice:{id}:payment`）；
   预付账户已经实时扣费过，标记已付不再动账本。
6. **兑换码核销与入账是两步**（先条件更新占位、再记账本），若记账失败会**释放占位**
   （`ReleaseRedemptionCode`）让客户可重试；设计里只写了单事务核销。这样做的原因是账本要走
   与结算相同的幂等路径，跨两张表的单事务需要把账本逻辑复制进 SQL，得不偿失。
7. **对账把不变量巡检结果一并写进 `details_json`**，并只保留前 100 条样例，避免 O(用量) 的内存占用。
8. **失败重放同时覆盖兜底文件与 `billing_failures` 表**，成功即 `resolved_at`，失败累加 `retries` 并记录错误。
9. **`amount_usd` 入口**：管理面接受十进制美元字符串并**无浮点**转成微美元（最多 6 位小数），
   方便不熟悉微单位的运维；核心存储仍然只有整数。
10. **未实现**：控制台的「账本与账期」页面仍是占位页（后端接口已就绪，列入下一轮）；
    `credit_grant` 到期冲销的每日作业只提供了 `ExpireGiftCredit` 入口，尚未挂 cron。