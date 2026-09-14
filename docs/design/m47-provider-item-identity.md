# M47 设计：条目身份保真（上游的 item id 与 encrypted_content 必须到客户端）

> 前置：`docs/design/m45-input-item-empty-field-preservation.md`（同日 M45：输入方向的空值保真）。
> 本文档在编码前输出；实现完成后回填第 8 节差异。
> （编号跳过 M46：`docs/design/m46-pricing-target-table.md` 是另一条在途工作。）

## 1. 现象与证据

M45 把「客户端显式发的 `summary: []` 被网关抹掉」修好之后（0.12.1/0.12.2 已部署），
在同一条线上请求上复现出的下一个错误是：

```
invalid_request_error: Item with id 'rs_usomyqvbg4ijbmtsfurdoxsf' not found.
Items are not persisted when `store` is set to false. Try again with `store` set to true,
or remove this item from your input.
```

也就是说：**网关回给客户端的 reasoning 条目，上游根本不认识**。

证据链（gptjp，2026-09-14）：

1. 客户端把上一轮的 reasoning 条目放在 `input` 里回灌 → 上游按 id 查不到 → 400。
   订阅后端是 `store=false` 的无状态后端，条目只能靠**自身携带的 encrypted_content**
   还原，不能靠 id 查服务端。
2. 网关回给客户端的 reasoning 条目是**拼出来的**：
   - id 由网关生成（`ids.Reasoning()`，形如 `rs_<随机>`，与上游的 id 无关）；
   - `summary` 来自 `response.reasoning_summary_text.delta` 累加；
   - 没有 `encrypted_content`（全流里 0 次出现）。
3. 原因在插件的事件翻译：`examples/provider-codex/main.go` 的 `translate` 对
   `response.output_item.done` 有这一句「message / reasoning / function_call 三种类型
   已经走 delta 路径」→ **直接把上游完整的 reasoning 条目丢掉**，只转发 delta 文本。
4. 于是 `include: ["reasoning.encrypted_content"]`（M45 已能透传到上游）也白费：
   加密块即使从上游回来，也会在插件这一跳被扔掉，客户端永远拿不到。

一句话：**delta 只能重建内容，重建不出身份**（item id 与上游附带的加密状态）。
`store` 与 `previous_response_id` 无关紧要——codex 每轮把完整历史发回来，靠的就是这些条目。

## 2. 目标与约束

目标：客户端拿到的每一条 output item，都必须能原样回灌给上游；具体是
**id 用上游的 id**，**上游给的额外字段（`encrypted_content` 等）不丢**。

约束：

- 不能牺牲流式体验：`response.reasoning_summary_text.delta` / `output_text.delta`
  必须照旧逐字发出（DSH 的思考过程是这么显示的，`scripts/format-smoke.sh` 一类的
  既有验收也依赖它）；
- 不能出现重复条目：客户端同一个 `output_index` 只能有一条 reasoning，收到一次
  `output_item.done`；
- 上游原本就没给 id 的场景（`happyFrames` 这类只有 delta 的流）必须继续工作，
  网关生成 id 的兜底要保留；
- 不改插件协议形状（不加新事件类型）；既有的 `EventOutputItemDone` 语义扩展为
  「这一条完成了」，而不是「这一条是新条目」。

## 3. 关键决策（含取舍）

**D1 delta 建的条目就用上游的 item id。**
`Assembler.addReasoning` / `ensurePart` 现在无视事件里的 `ItemID`，一律自己生成 id。
改成：事件带 `ItemID` 就用它（`startFunctionCall` 早就是这么做的），没带才兜底生成。
好处：客户端从第一个 delta 起看到的 id 就是上游的 id，条目身份自始至终一致；
这也是后面「完成条目能对上号」的前提。

**D2 完成条目按 id 就地升级，不追加。**
`EventOutputItemDone` 到达时，若流里已经有一条同 id、且不是上游直出（`Raw == nil`）的条目，
就把上游的完整条目折进这条：保留 `output_index`，`Raw` 指向完整条目（客户端因此拿到
`encrypted_content` 等），同时同步结构化字段供 `closeOpen` 的收尾事件使用。
随后照常走 `closeOpen`，所以客户端收到的仍是**一次** `done`（外加
`reasoning_summary_text.done` / `content_part.done` 这些既有收尾事件）。
条目若已经关闭（收尾事件已发），只升级存下的负载，不再补发 `done`——避免重复事件。

取舍：另一种做法是让插件不再发 delta、只发完整条目（简单，改动最小）。
放弃它的原因：思考过程会从「逐字流出」退化成「结束时一次性出现」，
DSH 的思考显示与思考录制都受影响；而 D2 让两者兼得。

**D3 插件转发所有完成条目。**
`translate` 里那句「message/reasoning/function_call 已走 delta 路径」的跳过逻辑删除：
唯一能携带身份与加密状态的就是完成条目。重复条目的问题由宿主侧的 D2 解决
（协议没有变，是宿主对既有事件的处理更准确了）。

**D4 插件非流式路径（`Complete`）同样按 id 去重。**
`Complete` 用文本 delta 拼 message 条目，现在完成条目也会来，会拼出两条。
处理：文本 delta 记住 `ItemID`，拼条目的 id 用它；完成条目按 id 替换而非追加。
否则 `stream:false` 的客户端会收到重复条目。

## 4. 接口与数据流

- 协议（`pkg/pluginapi`）：**不变**。`EventOutputItemDone` 的语义从
  「上游直出的、delta 路径没覆盖的条目」放宽为「这一条完成了」（字段形状不变）。
- 数据流（输出方向）：

  ```
  上游 SSE
    ├── response.reasoning_summary_text.delta (item_id=rs_x)  → 插件 reasoning delta
    │      → 组装器建条目 (id=rs_x，D1) → 客户端看到逐字思考
    └── response.output_item.done (item{id=rs_x, encrypted_content})
           → 插件 EventOutputItemDone（D3）
           → 组装器按 id 就地升级（D2）→ 收尾一次 done + response.completed.output 带加密块
  ```

- 存储：`responses.output_json` 由 `Assembler.Items()` 生成，因此续接
  （`previous_response_id`）也跟着带上上游 id 与加密块。

## 5. 异常与边界

- 上游只发 delta、不发完成条目（`happyFrames`）：行为与今天完全一致，id 用兜底生成。
- 上游重复发同一条完成条目：第二次时该条目 `Raw != nil` → 走追加（今天的语义），
  不静默丢数据。
- 上游发的 message 完成条目内容与 delta 文本不一致：以完成条目为准（上游是权威），
  delta 已经发出去的部分照旧留在客户端——这与 codex 客户端自己的处理一致。
- 条目 id 为空（上游没给）：不做匹配，追加。
- 工具自造类型（custom tool 等）走的是同一条 `EventOutputItemDone`，匹配不到就追加。
- 非流式 `Complete`：见 D4。

## 6. 测试策略

- `internal/responses`：
  - 组装器：delta（`item_id=rs_x`）→ 完成条目（同 id + `encrypted_content`）后，
    `output` 里只有**一条** reasoning，id 为 `rs_x`，`response.completed.output`
    带加密块，且 delta 事件里的 `item_id` 是 `rs_x`；
  - 无 id 的 delta 仍走兜底生成；重复完成条目仍是两条（既有语义不回退）。
- `examples/provider-codex`：`translate` 对 reasoning/message 完成条目不再丢弃；
  `Complete` 不产生重复条目。
- `scripts/codex-input-fidelity-smoke.sh`（真实二进制 + 假上游）：
  上游按真实订阅后端的事件顺序发流（reasoning delta → reasoning done（带加密块）→
  message delta → message done → completed），断言客户端拿到**一条** reasoning、
  id 与加密块都在，且 message 的 id 也是上游的。
- 线上验收：用网关自己回给客户端的条目原样回灌（codex 的下一轮就是这么做的），
  修复前 `Item … not found`，修复后 `completed`。

## 7. 依赖

无新依赖。网关与插件都要更新（插件负责转发，宿主负责就地升级）。
