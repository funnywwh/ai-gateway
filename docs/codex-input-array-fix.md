# Codex 工具结果数组解析修复（2026-09-13）

## 原因与复现

Codex CLI 0.153.4 通过 gpt001 的 `/aigw/v1` 调用模型，首轮可以
执行 shell；回传工具结果的第二轮返回 400：
`input must be a string or an array of items`。

捕获 CLI 实际请求确认，顶层 `input` 是合法数组，失败发生在嵌套项：

```json
{
  "type": "custom_tool_call_output",
  "call_id": "call_example",
  "output": [
    {"type": "input_text", "text": "Script completed\nWall time 0.1 seconds\nOutput:\n"},
    {"type": "input_text", "text": "gateway_input_check"}
  ]
}
```

`pluginapi.Item.Output` 原先仅为 string，数组解码失败后被 Responses
解析器包装成顶层 input 错误。服务器请求日志中首轮 completed 不能
证明工具结果续接成功；此次以 CLI 的 `turn.failed` 和请求正文复现。

## 修复

- 保留现有 string Output 接口，增加 OutputContent 存储数组结果。
- JSON 编解码保留数组内容，覆盖 function 和 custom 工具结果，
  穿过插件子进程协议并原样传递给 Codex 上游。
- Chat Completions 适配器使用现有文本转换逻辑读取 function 工具结果数组；
  输入 token 估算纳入数组内容。该适配器仍只转换文本，不新增多模态支持。
- 数字、布尔值和对象形式的 output 继续拒绝。

## 验证与部署

- 回归测试先在修复前复现失败，再验证修复后通过，覆盖文本数组、图片数组、
  空数组、字符串兼容、非法类型、插件协议和 Codex 上游请求构造。
- `go test ./...`、`go vet ./...`、`git diff --check` 通过。
- 18:49 同时更新 gpt001 网关和 provider-codex；构建标识为
  `0.2.2 / 595c650-input-array-fix`，服务 active。
- Codex CLI 分别使用 `gpt-5.6-luna`、`gpt-6-astra` 执行
  `printf gateway_input_check`，捕获第二轮 output 为数组，最终回复均为
  `gateway_input_check`，收到 `turn.completed`，进程退出码均为 0。
- CLI 仍有 `OutputTextDelta without active item` 日志，但上述测试均正常完成；
  此次修复仅处理 input 解析错误，不宣称所有流式事件兼容问题已解决。
- 临时测试 API Key 已禁用，本地凭据文件已删除。
- 回滚文件：`/opt/aigw/aigw.pre-input-array-fix`、
  `/opt/aigw/plugins/provider-codex.pre-input-array-fix`。
