# M11b 设计：账本、批处理结算与在途额度

> 规格：`docs/billing.md` §3–§4（已输出）。计价引擎见 M11a。实现后回填第 9 节差异。

## 1. 目标与拆分

本里程碑分两批提交，避免留下半可用状态：

- **M11b-1（本轮）**：账本原语——单事务结算、单写者批处理、磁盘兜底与重放、在途预留登记、四条不变量巡检、全量重建；
  管理面只读端点（余额 / 账本 / 不变量 / 重建 dry-run / 失败队列）。
- **M11b-2（下一轮）**：数据面接线——准入 402、逐尝试计价、结算投递、在途策略（warn/throttle/abort/allow_overdraft）与中断语义。

## 2. 包结构

```
internal/billing/
  settle.go      // Settlement 类型 + 单事务落地（usage + ledger + balance + counters）
  writer.go      // 单写者批处理队列；满则同步直写
  fallback.go    // billing-fallback.jsonl（fsync）+ 幂等重放
  reserve.go     // 在途预留登记表（内存）+ 准入判定
  invariants.go  // 四条不变量巡检
  rebuild.go     // 从 usage + 快照全量重算账本（dry_run | apply）
```

依赖：只依赖 `internal/domain`、`internal/pricing`、`internal/store`（通过接口）。httpapi 只依赖 billing 的窄接口。

## 3. 结算的事务边界（关键正确性）

一次结算必须在**同一个事务**里完成四件事，否则崩溃会出现「用量在、钱没扣」或「钱扣了、用量没了」：

1. `INSERT INTO usage_records ...`（按 `request_id + attempt_no` 唯一）；
2. `INSERT OR IGNORE INTO ledger_entries ...`（`idem_key` 唯一）；
3. `UPDATE accounts SET balance_micros = ...`；
4. `INSERT ... ON CONFLICT DO UPDATE usage_counters`（账户/Key/标签/账期四个维度）。

新增 `store.SettleAttempt(ctx, usage, entries, counters)` 承载这四步；返回 `applied bool` 表示本次是否真的写入
（重放时为 false，调用方据此统计「已重放」而不是「新入账」）。

幂等键约定：`charge:{request_id}:{attempt_no}`、`grant:{ref_id}`、`adjust:{ref_id}`、`redeem:{code_hash}`。

## 4. 单写者批处理

- 有界通道（`writer_batch_size × 4`）+ 单个 writer goroutine；
- 攒批规则：达到 `writer_batch_size` 或 `writer_flush_interval_ms` 到点即提交；
- **一批一个事务**：批次内所有结算按顺序在同一事务应用，任一失败则整批回滚并逐条降级；
- 队列满：**同步直写**（调用方阻塞等待），保证不丢单也不无界堆积；
- 失败降级链：批事务失败 → 逐条单独事务 → 仍失败 → 追加 `billing-fallback.jsonl`（`O_APPEND` + `fsync`）+ 写 `billing_failures` + 计数与告警。
- 关闭时（`Close(timeout)`）：停止接单、flush 剩余、返回未落盘条数。

**为什么单写者**：SQLite WAL 只有一个写者，多写者只会互相等锁；串行批处理把「每请求一次 fsync」摊薄成「每批一次」，
这是并发吞吐的主要来源。

## 5. 在途预留与准入

```
reserve = 输出单价(最贵档) × min(req.max_output_tokens, model.max_output_tokens)
        + 输入单价(最贵档) × est_input_tokens
        + per_request_fee
可支配 = balance − Σ(in_flight_reserved + in_flight_unsettled)
准入：可支配 ≥ reserve（预付）/ 可支配 ≥ −credit_limit（后付）
```

- 预留登记表按账户分片（`sync.Mutex` 保护 map），键 `reservation_id = request_id`；
- 预留带 TTL（`reservation_ttl_s`）与心跳（`reservation_heartbeat_s`）：GC 协程清理超时预留，避免泄漏永久占额度；
- 结算时先 `Release(reservation)` 再投递；投递失败也不释放（宁可多占，不可少占）；
- `reservation_mode`：`max_tokens`（默认，用上面的公式）| `fixed`（常量 `reserve_micros_default`）| `hybrid`（两者取大）。

## 6. 兜底重放

- 启动时与每 60s 扫描一次：`billing_failures`（未解决）与 `billing-fallback.jsonl`；
- 每条按幂等键重放，金额使用**快照复算**（不重新读当前规则），保证与首次一致；
- 成功则标记 `resolved_at` 并计数；文件重放成功后按行号截断（保留失败行）。

## 7. 不变量巡检

1. `balance == Σ ledger.amount`；
2. 每个账户最后一条 `balance_after == balance`；
3. `Σ charge(ledger) == Σ usage.charge_micros`；
4. 预付账户 `balance >= 0`（`allow_overdraft` 账户允许负至 `overdraft_limit_micros`）。

巡检按账户返回差异明细，供 M12 对账与 `/billing/invariants` 使用。

## 8. 全量重建（rebuild-ledger）

- `dry_run`（默认）：按 `usage_records` 时间序重算 charge，与现有 `charge` 类账本逐条比对，输出差异样例与总额；
- `apply`：**单事务**内删除 `charge` 类账本（保留 topup/grant/adjustment/refund/expire）、按时间序重插、重写 `balance_after`、
  递增 `rebuild_seq`，最后重算 `accounts.balance_micros`；
- 重建不修改 `usage_records`（用量是真源）。

## 11. M11b-3 实现与设计差异（流中在途策略）

1. **决策函数是纯函数**（`billing.DecideInflight`）：输入「已计费金额 + 本次可用上限 + 策略 + 软硬阈值」，
   输出 continue|warn|throttle|abort。软阈值只告警或节流；硬阈值除 `warn` 策略外一律结束调用
   （`warn` 的语义就是「跑完再说，后面靠对账补偿」）。
2. **限额 = 预留额，`allow_overdraft` 时再加上「余额 − 允许下限」**（`billing.InflightLimit`）。
   下限：预付 = 0，后付 = −授信额度。
3. **`throttle` 用真背压实现**：在 emit 回调里暂停读取上游管道（有界 grace，期间监听 ctx），
   上游的 stdout 写满后自然阻塞。因为余额在请求进行中不会变多，grace 到期仍未缓解就转 abort——
   这一点写进了设计（原来只写了「暂停读取」，没说暂停之后怎么办）。
4. **abort 走 ctx 取消**：`context.WithCancel` 取消后，`pluginapi.Client` 会自动发 `provider.cancel{reason:\"context_cancelled\"}`，
   上游被中断；客户端收到 `response.failed` + `insufficient_quota`。
5. **abort 决策点冻结可计费用量**：`Outcome()` 把决策时刻的维度快照作为计费依据，
   之后到达的用量只算成本（`overshoot_cost_micros`）不计费；实测终止时刻上游已停止，overshoot = 0。
6. **配额中断也要计费**：`chargePartial` 让 abort 走的结算绕过 `charge_on_error=false`，
   否则「已产出的内容」会白送（实测 charge=cost=42 微美元，余额 1000000 → 999958，从未为负）。
7. **长调用心跳**：guard 按 `reservation_heartbeat_s` 调用 `Touch` 延长预留，避免 GC 在请求进行中把额度收走。
8. **仍未实现**：`reservation_mode` 的 `fixed`/`hybrid`（当前一律用 max_tokens 公式）与
   `unavailable_charge_policy`（上游连增量都不给时的三种兜底）留待 M12 与对账一起做。
## 10. M11b-2 实现与设计差异（数据面接线）

1. **准入在路由规划之后、发起上游之前**：只有拿到候选才谈得上估算成本（不同供应商成本不同）。
   被拒的请求**不写用量**，只写 `request_logs` + hook `request.denied`（符合规格 §1）。
2. **`usage.Meter` 拆成 `Build` + `Record`**：接入计费后，用量行由结算事务写入，
   避免「用量一次、账本另一次」的两次落库；未接入计费时仍走原来的 `Record`。
3. **失败尝试记成本、是否计费看 `billing.charge_on_error`**；不计费时把
   `usage.charge_micros` 一并置 0，保证不变量 3 在故障切换场景下也成立。
4. **售价倍率优先级修正（实测抓到的真缺陷）**：原实现把 `billing.default_markup_bp` 无条件当成
   `MarkupSet` 传给计价引擎，覆盖了模型自己的 `sale_pricing.markup_bp`——实测请求 cost=7/charge=7（应为 10.5→11）。
   现在全局默认只在模型未声明倍率时生效（试算器同一处也修了）。
5. **`effectiveMaxOutput` 缺省 512**：请求未给 `max_output_tokens` 且配置未给默认值时用它做预留估算，
   宁可少留一点也不能不留。
6. **尚未实现**：在途策略的 `throttle`（暂停读上游做 TCP 背压）与 `abort`（中断中流调用、
   把决策点之后的 overshoot 记为成本不 charge）以及长调用的预留心跳。当前策略值已贯穿到准入判定
   （`allow_overdraft` 生效），但流中检查与中断语义留下一批（M11b-3）。
## 9. 实现与设计差异（M11b-1 账本原语）

1. **新增迁移 `0002_usage_attempt_unique.sql`**：`usage_records(request_id, attempt_no)` 原本只有普通索引，
   `INSERT OR IGNORE` 根本不会忽略，重放会插入第二条用量行，进而破坏不变量 3。这是实现过程中被测试
   抓出来的真实缺陷（`TestSettlementIsAtomicAndIdempotent` 直接失败）。
2. **结算原语做成 `SettleBatch([]*SettlementInput)`**：单条与批量共用同一条代码路径，批量就是「一个事务里的
   一串单条」，避免两套逻辑漂移；`SettleAttempt` 只是它的一元包装。
3. **重建（rebuild）会重写该账户的全部账本行**：同一事务内按时间序重排、重算 `balance_after`、`rebuild_seq+1`，
   行 id 会变化（不是原地 UPDATE）。人工类目（充值/赠送/调整）原样保留，只重算 charge。
4. **`billing_failures` 与兜底文件都写**：文件是崩溃时的最后防线（fsync），
   库表是恢复后可查询的记录；两者都以幂等键为准，重复记录无害。
5. **Writer 的停止用独立 `stop` channel**：最初复用 `done` 导致 `close of closed channel` 崩溃（测试抓到），
   现在 `stop` 只由 Close 关闭、`done` 只由 loop 关闭。
6. **`Service` 门面**：writer + 预留表 + 维护操作合成一个 `billing.Service`，
   main 与 httpapi 各只依赖它一个对象（httpapi 通过 `BillingAdmin` / `LedgerAdmin` 两个窄接口）。
7. **多出一个 `RawExec`**：仅用于测试故意破坏物化余额，以验证巡检能发现、重建能修复。
8. **管理面先落地只读 + dry-run**：`/billing/invariants`、`/billing/status`、`/billing/rebuild-ledger`（默认 dry-run）、
   `/accounts/{id}/balance`、`/accounts/{id}/ledger`；Pricing/Ledger 界面在 M11b-2 与 M12 一起补。
9. **数据面尚未接**：本批只做账本原语，`/v1/responses` 的准入 402、逐尝试计价与结算投递留到 M11b-2，
   这样不会出现「开始扣钱但账本还没验证过」的中间状态。