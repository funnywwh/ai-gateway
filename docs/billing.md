# 计量与计费

> 状态：**规格（M11/M12 实现）**。金额一律 int64 **微美分**（1e-6 USD）；时间 UTC。

## 1. 计量（L1）

- **每次实际出网尝试**写一行 `usage_records`（含失败尝试：`status`、`error_code`、`terminated_reason`、`overshoot_cost_micros`）。
- **本地拒绝（未出网）不写用量**，只写 `request_logs` + 审计 + hook `request.denied`。
- usage 来源优先级：上游最终 usage > 流中 `usage.delta` 累计 > 字符估算（`usage_source=estimated`）> `unavailable`。
- 取整规则：每维度 `fee = ceil(quantity × unit_price_micros / scale)`，token 类 `scale = 1e6`，计数类 `scale = 1`；
  倍率 `charge = ceil(base × markup_bp / 10000)`。全程 int64 并做上限校验；`charge_micros ≥ 0`。

## 2. 计价（L2）

成本价（我方付上游）与售价（对客）**分离**，规则模型见 `docs/pricing.md`。
每次请求同时产生 `cost_micros` 与 `charge_micros`，并写入**内联了命中规则完整副本**的 `pricing_snapshot_json`
（即使规则随后被改/删，历史仍可逐笔复算）。毛利 = Σcharge − Σcost，可按 day/account/key/model/tier/window 查看。

## 3. 账本与余额（L3）

- 计费主体是 `accounts`（租户）；API Key 归属账户，**标签只做分组、成本分摊与分账倍率**。
- `ledger_entries` **append-only**：`credit_grant | topup | charge | adjustment | refund | expire`。
  每条带带符号 `amount_micros`、`balance_after_micros`、`idem_key`（**唯一**）、`rebuild_seq`。
  只插不改不删；纠错一律新增 `adjustment`。
- **扣费与用量同事务**：`usage_records` + `ledger_entries` + `accounts.balance_micros` + `usage_counters`。
- **单写者批处理**：结算记录投递到有界队列，单一 writer goroutine 多行批量落库
  （`writer_batch_size` / `writer_flush_interval_ms`）；队列满则回退同步直写（保正确性）。
- **磁盘兜底**：事务最终失败时同步追加 `billing-fallback.jsonl`（fsync）并记 `billing_failures`，
  启动与每 60s 幂等重放（`request_id + attempt_no`）。**流式响应已交付也必须保证记账。**
- **真源是用量**：`rebuild-ledger` 支持全量重放（保留人工类目，按时间序重算 charge、重写 `balance_after`、
  递增 `rebuild_seq`）；先 `dry_run` 输出差异预览，确认后单事务应用。

### 账本不变量（每日巡检 + 单测）

1. `accounts.balance_micros == Σ ledger_entries.amount_micros`
2. 每个账户最后一条的 `balance_after_micros == balance_micros`
3. `Σ charge(ledger) == Σ usage_records.charge_micros`
4. 预付账户余额不为负（`overshoot` 吸收策略下；`overdraft` 模式允许负至 `overdraft_limit_micros` 并立即 suspend）

## 4. 长调用与在途额度

**准入**（杜绝超卖）：

```
reserve = ceil(min(req.max_output_tokens, model.max_output_tokens) × 输出单价(最贵可能档))
        + ceil(est_input_tokens × 输入单价(最贵可能档))
        + per_request_fee
准入条件：balance − Σ(in_flight_reserved + in_flight_unsettled) ≥ reserve
```

只要上游遵守 `max_output_tokens`，预付账户就不会被超卖。`reservation_mode`：`max_tokens`（默认）|`fixed`|`hybrid`。

**在途计量**：客户端请求 `stream:false` 时，若候选支持流式，网关仍以流式调上游并在网关侧聚合——
这样长调用在途也有增量可计量。插件可上报 `usage.delta`；网关侧兜底 chars/4 估算，两者取**较大值**（保守）。

**策略**（每 `inflight_check_interval_ms` 检查，账户可覆盖）：

| 策略 | 行为 | 默认适用 |
|---|---|---|
| `warn` | 只记事件与告警 | 后付账户 |
| `throttle` | 暂停读取上游（TCP 背压）并停住客户端流 | 可配 |
| `abort` | 立即中断本次调用 | **预付账户默认** |
| `allow_overdraft` | 允许透支至 `overdraft_limit_micros` | 高信任账户 |

**中断语义（abort）**：

1. 发 `provider.cancel{reason:"insufficient_quota"}`；插件取消上游；`cancel_grace_ms` 后强丢该流。
2. **只为 abort 决策点前已计量的用量计费**；此后到连接真正断开之间的 `overshoot`
   记 `overshoot_cost_micros`（只记成本不 charge，`overshoot_policy=absorb`）→ **预付余额不会因物理延迟变负**。
3. 对客户端：`event: error{type:"insufficient_quota"}` + `response.failed`；`terminated_reason=aborted_quota`；保留已产出内容。
4. 释放剩余预留，已计量部分在同一幂等事务转为正式 charge。
5. **不触发故障切换**（换供应商同样花钱），账户随后进入 402/暂停流程。

**上游始终不给 usage**：按在途估算结算并标 `estimated`；连增量也没有时按
`unavailable_charge_policy`：`charge_estimated`（默认）|`charge_reserved`|`charge_zero`（账户级可覆盖）。

## 5. 账期账单（L4）

- 账期默认自然月（`period_start_day` / `timezone` 可配）；账期结束生成 `draft` 并**物化** `invoice_lines`
  （默认 model/key/day 分组，可切 tier/window，cost/charge 双列）。
- 状态流转 `draft → issued → paid`，或 `→ void`；**`issued` 后不可变**（改错走 `void` 重开或账本 `adjustment`）。
- 计费始终发生在请求时；发票是账期**不可变汇总凭证**。后付账户「标记已付」写入一笔 `topup`（还款，`ref_type=invoice`）。
- 同 `(account, period)` 幂等（`force` 才覆盖）；导出 CSV/JSON（不做 PDF）。

## 6. 充值（L5）

- `POST /accounts/{id}/credits`：`topup` | `credit_grant`（可带 `expires_at`；赠送额度到期由每日作业按 `expire` 冲销未用部分，不动充值余额）。
- **兑换码**：批量生成、库中只存 `code_hash`、明文仅返回一次、核销单事务 + 幂等键（并发仅一次成功）。
- 外部支付：留 `billing.topup` hook 与 credits API 作为对接入口（v1 不集成支付网关）。
- 到账后按 `auto_resume` 规则自动恢复账户。

## 7. 对账补偿（L6）

- **每日对账**（或手动 `POST /billing/reconcile`）：以 `usage_records` + 内联快照复算，与账本比对，
  落 `billing_reconciliations`（差异、缺失用量数、估算占比、样例 ≤100 条）；差异 ≠ 0 发 `billing.reconcile_mismatch`。
- **补偿重放**：`billing_failures` 与兜底文件按幂等键重放（用快照复算，金额与首次一致）。
- **巡检不变量**（§3）并记录结果。

## 8. 错误语义

| HTTP | 场景 |
|---|---|
| 429 `rate_limit_exceeded` | 限速（带 `x-ratelimit-{limit,remaining,reset}-{requests,tokens}` 与 `Retry-After`） |
| 402 `billing_hard_limit_reached` | 余额/信用额度不足，或账户被暂停 |

两套控制同时生效：**先账户余额/信用额度，再 Key/标签的月度额度**。
