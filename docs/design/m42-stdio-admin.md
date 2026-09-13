# M42：stdio MCP 后台工具

> 状态：已实现；设计已在对话展示并获确认。关联规格：`docs/mcp.md` 第 10 节。

## 目标

本地 MCP 客户端可以通过 stdio 使用现有网关的查询与后台工具。保留现有 `--account` 本地只读模式。

## 关键决策

新增显式 HTTP 转发模式，复用运行中网关的 `/mcp`，避免独立进程写数据库后线上 registry、凭据缓存、Hooks 与插件状态不刷新。原 M21 待办提出抽取 composition root；此次采用转发，不复制后台依赖和权限实现。

`--endpoint` 指定完整 MCP URL；`--token-env` 指定环境变量名称，默认 `GW_MCP_TOKEN`。不提供命令行明文 token 参数。`--endpoint` 与 `--account` 互斥；转发模式不读取本地配置或数据库。远端使用 HTTPS；HTTP 仅允许 loopback 字面地址或 localhost；禁止 URL 内嵌用户名密码、query 和 fragment，禁止重定向，避免凭据转发至另一端点。

## 接口与数据流

CLI：`aigw mcp-serve --endpoint https://gateway.example/aigw/mcp --token-env GW_MCP_TOKEN`。

拟新增 `serveMCPRelay(ctx context.Context, input io.Reader, output io.Writer, endpoint, token string, client *http.Client) error`。输入按换行分隔 JSON-RPC，逐条 POST，携带 Bearer 与 JSON Content-Type；成功响应保留 JSON-RPC id/result/error，换行后立即 Flush；通知的空响应不输出。

仅 stdout 输出协议数据，诊断走 stderr，不输出 token 或原始错误响应正文。每条请求均经服务器鉴权，吊销、降权和部署开关立即沿用 HTTP 行为；管理调用沿用 confirm、审计 actor、响应截断和既有热更新。

## 异常与边界

输入行与 HTTP 响应设有限上限，超限明确失败而不截断成合法请求；网络、非 2xx、畸形 JSON 响应或写出失败终止转发并返回非零退出码，不自动重试管理写操作。设置请求超时并支持进程取消；EOF 正常结束，最后一行无换行仍处理。只支持本项目现有 JSON HTTP MCP 响应，不宣称支持任意第三方 SSE MCP 服务。

## 测试策略与依赖

先添加失败测试：参数互斥、缺 token、URL 校验、请求鉴权头和 JSON 原样转发、通知无输出、末行处理、错误和超限、重定向不泄露 token、取消与输出错误。使用 httptest，不调用付费上游。

集成验证通过真实 httpapi 与 store fixture 覆盖 query 隐藏后台、admin_read 拒绝写、admin 写入和后续读回、撤销 token 下一请求失败。执行命令包、httpapi、mcpsrv 聚焦测试及 make verify。只使用 Go 标准库，不引入新依赖。

## 实现与设计差异

采用已确认的 HTTP 转发方案，未抽取独立后台依赖。输入行与响应上限均为 10 MiB，默认每次 HTTP 请求超时 2 分钟。通知即使收到旧网关返回的 JSON-RPC 错误，也不写到 stdout；有 id 的请求必须收到对应 id 且只有 result/error 之一的响应。SIGINT/SIGTERM 同时取消 HTTP 并关闭 stdin，以退出空闲读取。

验证：新增测试首次因缺少转发实现失败；实现后命令包、httpapi、mcpsrv 聚焦测试通过。真实服务与临时 SQLite 集成覆盖 base_path、三种 scope、管理写入后读回和吊销后的 401；`make verify`（vet、全量 Go 测试、21 项前端断言、构建）通过。未调用付费上游。
