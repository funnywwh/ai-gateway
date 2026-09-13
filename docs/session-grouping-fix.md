# 请求日志会话标识修复（2026-09-13）

## 查证结果

对应本机 Codex `0.153.4`，已检出 OpenAI 官方源码标签 `rust-v0.153.4`，
提交 `3d2ee51ca2d5db578f328aa75e20aa22c0197c9a`。

- [会话初始化源码](https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/core/src/session/session.rs#L774)：
  `session_id` 等于根线程 ID，子代理继承根会话，`thread_id` 是当前线程。
- [元数据序列化源码](https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/core/src/responses_metadata.rs#L212)：
  权威快照为 `client_metadata["x-codex-turn-metadata"]` 的 JSON 字符串；包含 `session_id`、
  `thread_id`、`parent_thread_id` 等。平铺兼容字段是 `session_id`、`thread_id`、
  `x-codex-parent-thread-id`（注意平铺父 ID 带前缀）。
- [传输头源码](https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/codex-api/src/requests/headers.rs#L5)：
  HTTP 头为 `session-id`、`thread-id`，并有 `x-codex-turn-metadata` 兼容快照。
- [缓存键源码](https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/core/src/client.rs#L540)：
  `prompt_cache_key` 可被覆盖，内部辅助请求还可使用 `source:parent_thread_id`，不能当作唯一会话标识。
- [App-server 文档](https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/app-server/README.md#L483)：
  `thread.sessionId` 是当前活跃会话树根；fork 创建独立线程。不能把 `forked_from_thread_id` 当作归组依据。

官方开发者站点与 platform 文档访问均返回 403，以上结论来自实际读取的官方仓库文档和对应版本源码，
没有将最新版源码行为未经核对套到本机版本。

## 实现

日志维度在请求解析后读取少量身份字段：先找显式 `session_id`，没有时找 `thread_id`，最后才回退
`prompt_cache_key`。同类字段优先级为：正文权威快照、正文平铺字段、请求头快照、通用 `metadata`、
直接请求头。父线程不是根会话，不代替根 ID；`x-client-request-id` 不参与会话识别。

`turn_trigger=thread_title` 可直接识别 Codex 标题调用。仍兼容已有提示词前缀识别。
头部身份只用于日志，不写进请求体，不改缓存键、上游内容或粘性路由。
`record_input=off` 仍记录身份，`redact_paths=session_id` 仍清空身份列。

不新增表、索引、SQL 查询、正文扫描或历史匹配；统计和筛选继续使用现有 `session_id` 索引。

## 现场边界

- 标题辅助请求在当前客户端作为独立临时线程运行。已查看本机标题调用启动记录，
  `turn_trigger=thread_title`，`parent_turn_id`、`root_turn_id`、`responsesapi_client_metadata` 均为空。
  未捕获这条请求的完整网络元数据，不能断言它携带了主会话 ID。
  缺少根会话/父关系时，网关不能保证自动合并，需调用端补充稳定标识。
- DSH 两个裸 UUID `dad45d16-…`、`115a2f11-…` 是“维度统计请求日志添加缓存列”
  （`session-b78a7610-…`）的子代理，已从本机该会话工具调用及结果确认。
  本机 DSH 适配器给 pi-ai 的是当前 `sessionId`，没有发送父/根标识。
- 线上日志使用 `record_input=user`，已存正文没有 `client_metadata`，不能仅凭已有行恢复根 ID。
  没有按时间、工作区或相似提示词合并历史记录，也没有修改线上数据。

## 验证

- `go test ./...`、`go vet ./...` 通过，覆盖流式/非流式、同根不同缓存键与线程的分组/筛选、标题辅助调用、
  录制关闭、身份脱敏、元数据优先级/损坏/空值/截断、缓存键与上游元数据不变。
- 本机 Xeon E5-2696 v4 上 `BenchmarkLogSessionKey` 连跑 3 次：
  无元数据回退 39–42 ns/op、0 分配；平铺身份 1.62–1.65 µs/op、440 B/op；
  典型 Codex 快照 9.75–10.19 µs/op、1776 B/op。
  这是身份提取函数的微基准，不是端到端负载测试；更大的元数据解析耗时随长度增加。
- 未增加数据库操作；现有索引查询/排序守卫测试通过。
- 未部署，未修改历史日志。

## 独立标题线程补充（2026-09-13）

`gpt001` 0.3.0 上截图中的 `01a09aef-df70-78a1-9553-72fdcbcb42f9`
是主线程 `01a09aef-d6d8-75c2-8931-602b094a23b5` 的标题调用。
本机实际客户端为 0.154.0-alpha.6.1，标题提交没有 parent/root turn 和额外客户端元数据。
已确认归属的这一条历史记录已手动归并，备份位于服务器
`/opt/aigw/backups-release/session-merge-20260913-212739.json`。

新增有限的自动推断：仅 Codex 已知标题模板，与首条纯文本用户提示词做 SHA-256 精确匹配
（只去除首尾空白）。同账户、API Key、工作区，主线程首条观测请求与标题请求开始时间相差
不超过 120 秒，且恰有一个主线程候选时，标题行归入主线程。主线程之间不会合并。
后续出现另一个候选时恢复标题行原始线程。显式身份已经不同于缓存线程 ID 的标题请求不参与推断。

迁移 0014 保存关联证据和原始线程，不保存提示词明文。同步/批量写入都在日志事务内关联；
既支持标题先完成，也支持主请求先完成；状态落库后不依赖进程内存。
日志删除级联删除证据，session_id 变更复用小时汇总失效触发器。
`record_input=off/metadata`、任何 `redact_paths`、图像等混合输入、未知模板均不生成指纹。
不扫描历史正文，不改变上游输入、缓存键或计量数据。

这是有边界的推断：若另一个真实主线程从未经过网关，或在窗口之外，唯一候选仍可能误匹配。
缺少正文、工作区或候选不唯一时保持独立，可靠的长期方案仍是客户端传递根会话关系。

### 本次验证与部署

- `make verify` 通过（全量 Go 测试、vet、UI 基础断言、构建）；最后收紧 XML 输入处理后，
  responses 测试再次通过并重新构建。
- 覆盖两种完成顺序、候选冲突恢复、账户隔离、窗口外会话、重复写入、同步/事务写入、
  已发布汇总失效、日志删除清理证据、录制/脱敏策略、混合媒体拒绝。
- 2026-09-13 21:37 部署 `gpt001`，运行 revision `de5be00-title-link-fix`；
  `/aigw/healthz` 返回 ok；迁移 0014 已创建；管理 API 确认示例会话 total=1、requests=6。
  此旧示例是之前人工修正；部署时尚未观察到新的线上标题自动关联，不能把旧示例当作自动关联验证。
- 回退二进制 `/opt/aigw/aigw.pre-title-link-fix`，升级前数据库备份
  `/opt/aigw/backups-release/pre-title-link-20260913-213704.db`。
  日常回退只替换二进制，不覆盖已经产生新账目的数据库。
