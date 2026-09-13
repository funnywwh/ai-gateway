# Codex 工具调用丢失修复（2026-09-13）

gpt001 上的 Codex 会话在 18:09、18:11 收到“继续检查”的文字后结束。
网关日志显示 completed，不代表客户端收到了模型实际生成的工具调用。

## 原因与复现

- `provider-codex` 只翻译 `function_call`，忽略 `custom_tool_call`。
  强制模型调用自定义 echo 工具：直接访问上游能收到完整工具项，
  修复前通过网关却得到 `status=completed, output=[]`。
- 17:39–17:40 的历史失败是 `missing_required_parameter`（`input[0].tools`）。
  网关在 18:25 更新了输入未知字段保留逻辑，但线上插件仍是 18:00 构建；
  插件也依赖同一套 Item JSON 编解码，部署时必须同时更新。
- 18:11:56 的失败约耗时 7 秒，对应本机客户端的 `turn_aborted/interrupted`。
  这些证据不能归因为长连接超时。

## 变更

新增插件事件 `output_item.done`，传递现有增量事件未覆盖的完整输出项。
Codex 适配器在上游完成该项时转发，保留工具的 `input`、`call_id`、
`name`、`namespace` 等字段。组装器输出 item added/done，并将其纳入最终响应、
非流式响应、存储和续接。已经交付工具调用后禁止切换供应商重试。

自定义工具目前在完整输出项到达时交付，未逐块转发其输入增量。
已有文本和 function_call 增量路径不变。

## 验证与部署

- 回归测试：`examples/provider-codex/custom_tool_test.go`，覆盖上游 SSE、
  插件 JSON 边界、客户端工具完成事件、最终响应、存储恢复及非流式重放。
- `go test ./...`、`go vet ./...`、`git diff --check` 通过。
- 18:35 同时更新 gpt001 的网关和插件，服务 ready。
  实测网关返回 `custom_tool_call`，回传工具结果后模型完成后续回复。
  `gpt-5.6-luna` 与 `gpt-6-astra` 均通过；后者还覆盖带
  `role=developer` 的 `additional_tools` 输入项。
  测试使用的临时 API Key 已禁用。
- 当前构建标识：`0.2.1 / 3f4da3b-custom-tool-fix`。
  回滚二进制保留在 `/opt/aigw/aigw.pre-custom-tool-fix` 和
  `/opt/aigw/plugins/provider-codex.pre-custom-tool-fix`。
