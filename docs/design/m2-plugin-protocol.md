# M2 设计文档：Provider 插件协议 v1 与宿主

## 目标
把「供应商」抽象成**独立子进程 + stdio NDJSON 帧**的插件：定义公开 SDK（`pkg/pluginapi`）、
宿主侧进程管理（`internal/pluginhost`）、内置供应商与转换工具（`pkg/providerkit`、`internal/providers`）。

## 关键决策
1. **协议版本握手**：插件 stdout 首行必须是 handshake 帧（protocol/name/version/capabilities/schema）；
   宿主 3s 内未收到或不兼容 → 标记不可用并 kill。破坏性变更只升 protocol 主版本。
2. **stdout 只走协议帧，日志走 stderr**：宿主按 `provider=<name>` 前缀转写并保留最近 200 行供 UI 诊断。
3. **凭据传递不经过 argv**：优先经 `$GW_PLUGIN_STATE_DIR/credentials.json`（0600，插件自读自删），
   `GW_PLUGIN_CREDENTIALS` 仅作小凭据兼容回退（规避 /proc/<pid>/environ 同 UID 可读与 env 128KB 上限）。
4. **一请求一 id，可并发**：宿主为每个请求分配 id；插件按 id 并发分发，帧写用互斥锁串行化。
5. **取消**：宿主发 `provider.cancel{id,reason}`；插件必须取消上游上下文并以 `end(partial:true)` 或 `error` 收尾；
   宿主在 `cancel_grace_ms` 后强制丢弃该流。
6. **背压**：宿主侧单 reader goroutine + 有界缓冲；缓冲满时插件 `emit` 阻塞（这就是 throttle 的实现基础）。
7. **凭据回写**：插件可发 `notify provider.credentials`，宿主重新 AES-GCM 加密落库并记审计。
8. **增量用量**：`usage.delta` 事件用于在途计量；`Usage.Dimensions` 是统一的计量维度表
   （input_cache_hit/input_cache_miss/input/output/reasoning 等）。

## 接口（M2 产出）
- `pkg/pluginapi.Provider`（插件作者实现）+ `Serve(p)`（握手/帧循环/并发分发/取消/心跳）。
- `pkg/pluginapi`：`Encoder`/`Decoder`、`Frame`、`Handshake`、`StreamEnd`、`Error`、
  `schema.Validate`（受控 JSON Schema 子集）。
- `internal/pluginhost`：`Host`（启动/握手/心跳/重启退避/draining/取消/日志环形缓冲）+ `Client`（实现 `domain` 侧调用）。
- `internal/providers`：内置 `openai-responses`、`openai-chat`、`testecho`（测试回声，可控慢流/维度）。
- `pkg/providerkit`：`SSEReader`、`ChatToResponses`/`ResponsesToChat`、`CharEstimator`。

## 数据流
```
宿主 → 插件： provider.stream{id, params:Request}
插件 → 宿主： event{text.delta} * N → end{usage, finish_reason}
（中途）宿主 → 插件： provider.cancel{id, reason} ；插件 end{partial:true}
（随时）插件 → 宿主： notify provider.credentials{credentials, reason}
```

## 异常与边界
- 握手失败/超时：标记不可用 + kill + 记录 last_error。
- 插件崩溃：指数退避重启（≤5 次/分钟），超过则熔断并告警。
- 心跳：15s ping，连续 3 次无 pong 判死重启。
- 单行超长（>8MB）：判为协议错误，丢弃该帧并记录。
- 未知方法：返回 error{code:"unsupported_method"}，不断连接。

## 测试策略
- 协议往返（Encoder/Decoder、错误帧、未知字段保留）。
- schema 子集校验（必填/类型/枚举/范围/正则/items/format）。
- 进程内 `Serve` 全链路：用 io.Pipe 模拟宿主，断言握手/一元/流式/取消/心跳/动作。
- 真实子进程 E2E：编译示例插件，宿主启动、调用、取消、杀进程后重启。

## 依赖
标准库；示例插件仅依赖 `pkg/pluginapi`。

## 实现与设计差异
- **取消的服务端语义**：插件被取消时以 `end{partial:true, finish_reason:"cancelled"}` 收尾（而非 `error`），
  宿主据 `partial` 判定"部分完成"，与账单的 overshoot 吸收口径对齐。
- **`end` 不携带 usage**：最终用量由 `usage` 事件传递（流式），非流式由 `Response.Usage` 传递；
  这样"增量用量"与"最终用量"共用同一类型，避免两处语义分叉。
- **背压缓冲大小定为 8 帧**（`pluginapi.bufferSize`），而非计划里的 64：更小的缓冲让
  throttle 更早生效；实测 E2E 无吞吐问题。
- **`Serve` 的取消注册表**：按请求 id 保存 `context.CancelFunc`，`provider.cancel` 与
  `shutdown`/信号都会触发取消；panic 会被 `recover` 转换为协议错误帧，不让插件进程崩溃。
- **`ProcBusy` 为占位实现**：协议客户端未暴露在途计数，drain 目前依赖路由层停止接纳新请求；
  在途计数将在 M3 的 provider 运行时补齐（设计文档已标注 best-effort）。
- **`-race` 不可用**：本沙箱无 C 编译器且 `CGO_ENABLED=0`，race detector 需要 cgo。
  `make test-race` 在缺 cgo 时给出明确提示而非失败；测试以普通模式全绿。
- **内置 provider 走同一 Provider 接口**：`internal/providers` 里的 `openai-chat`/`openai-responses`/`testecho`
  都实现 `pluginapi.Provider`，因此"进程内内置"与"子进程插件"对上层完全同构
  （差异只在 M3 的运行时适配：直接调用 vs 帧调用）。
- **chat↔Responses 转换放在 `pkg/providerkit`**（而非 internal）：插件作者也需要它，
  这样"只支持 chat 的上游"适配器只需 ~150 行。
- **用量维度映射规则**：chat 的 `prompt_cache_hit_tokens/prompt_cache_miss_tokens` 存在时映射为
  `input_cache_hit`/`input_cache_miss`（**不再单列 input**，避免重复计数）；否则映射 `input`。
  `completion_tokens_details.reasoning_tokens` → `reasoning`。
- **上游错误分类**：429 带 `Retry-After` → `quota_exhausted` + `reset_at`；408/409/425/5xx → `retryable`；
  其余 4xx → `fatal`（附 `http_status`），并尽力从 `{"error":{"message":...}}` 提取原始信息。
- **`stream_options.include_usage=true`**：openai-chat 流式请求自动注入，以便拿到最终 usage；
  若上游仍未返回，则用 `CharEstimator` 估算并标 `estimated`。
- **`tests` 用 `httptest` 假上游**验证转换与错误映射，不依赖外网。
