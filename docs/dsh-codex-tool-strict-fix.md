# DSH 使用 Codex 订阅时反复请求无效提权（2026-09-13）

Full access 下，模型仍为 Bash 填入 `sandbox_permissions: "danger-full-access"`，
DSH 因目标权限不比当前权限更宽而拒绝调用。运行时上下文已经明确禁止提权，
问题不是权限说明丢失，也不是此前的 `input[0].tools` 解析错误。

本机 DSH 使用 pi-ai 的通用 `openai-responses` 路径，默认不声明支持 strict，
工具定义因而省略 `strict`。Codex 订阅上游会把省略 strict 的函数工具规范化为
严格模式，导致可选的权限参数也被要求填写。pi-ai 自己的 Codex 路径则默认
支持 strict，并发送 `strict: false`。

## 修复

`provider-codex` 构造上游请求时，为未指定 strict 的普通函数工具补上
`strict: false`。保留显式 true/false、参数 schema 和非函数工具，且不修改
调用方的请求；不改写模型生成的工具参数，不改变 DSH 权限策略。

## 验证

- 回归测试先复现缺少显式 false，再验证修复；覆盖插件 JSON 边界、显式
  true/false、schema 保留、自定义工具以及调用方不可变性。
- `go test ./...`、`go vet ./...`、`git diff --check` 通过。
- gpt001 上以 `gpt-6-astra`、相同提示和 Bash schema 做流式对照：修复前省略
  strict 会产生 `sandbox_permissions: "danger-full-access"` 和空 justification；
  显式 false 只产生 command/description。更新插件后，省略 strict 也只产生
  command/description。
- 实际执行模型要求的 `pwd` 并回传 function_call_output，第二轮正常返回
  工作目录且状态 completed；临时测试 API Key 已撤销。
- 已更新 `/opt/aigw/plugins/provider-codex` 并重启 aigw，服务 active。
  回滚插件保存在 `/opt/aigw/plugins/provider-codex.pre-dsh-strict-fix`。

验证使用 DSH 等价的工具 schema 直接请求网关；未操作用户浏览器中的原会话。
