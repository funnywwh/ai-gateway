# M6 设计文档：MCP 只读查询服务

## 目标
让**外部 LLM/Agent** 用账户令牌连入网关，只读查询**本账户**的统计与请求内容：
统计（余额/账本/用量/模型/限额）与内容（输入文本；思考/最终输出按录制开关）。

## 关键决策
1. **方向**：网关是 **MCP Server**（对外提供查询），不是 MCP Client。
2. **传输**：`POST /mcp`（JSON-RPC 2.0 over HTTP，MCP Streamable HTTP 的核心子集）。
3. **认证**：`Authorization: Bearer aigw_mcp_<token>`；令牌只存 SHA-256 哈希 + 前缀索引
   （`mcp_tokens` 表），支持吊销/过期；每次调用更新 `last_used_at`。
4. **账户强作用域**：所有查询强制 `account_id = token.account_id`；跨账户一律返回空/拒绝。
5. **只读**：不存在任何写操作或副作用工具；不返回成本价、上游/供应商细节、其他 Key 的明文。
6. **内容可见性随录制策略**：输入文本默认可见（若 `record_input != off`）；
   思考文本与最终输出**仅当该 Key 勾选**时才返回，否则返回 `recorded=false` 与原因说明。
7. **限额**：`mcp.max_query_rows`（默认 1000）、`mcp.request_window_days`（默认 30）双重约束。
8. **实现方式**：JSON-RPC 方法子集 `initialize` / `tools/list` / `tools/call`（+ `ping`）
   自实现（约 200 行），避免引入重型依赖；协议形状与 MCP 规范一致，后续可平滑切到官方 SDK。

## 工具清单（M6 首个版本）
| 工具 | 参数 | 返回 |
|---|---|---|
| `get_balance` | — | 余额/信用额度/计费模式/状态/低水位 |
| `get_ledger` | `period`, `limit` | 账本流水（新→旧） |
| `get_usage_summary` | `period` | 请求数、各维度 token 合计、尝试状态分布 |
| `list_requests` | `period`, `limit` | 请求列表（含 `reasoning_recorded`/`output_text_recorded`） |
| `get_request` | `request_id` | 输入文本（脱敏后）；思考/最终输出按开关 |
| `get_models` | — | 可用模型与**对客售价** |

`period` 支持 `today|yesterday|last_7_days|last_30_days|this_month|last_month`（默认 `last_7_days`）。

## 数据流
```
POST /mcp  { "jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_balance","arguments":{}} }
  └─ 鉴权（mcp token → account_id）
      └─ mcpsrv.Call(accountID, name, args)
           └─ store/registry 只读查询（账户作用域；LIMIT 生效）
  ◀─ {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"<json>"}]}}
```

## 异常与边界
- 无/错令牌 → HTTP 401（JSON-RPC error 亦带 `code: -32001`）。
- 未知工具 → JSON-RPC error `-32602`（invalid params）。
- 参数非法（period 拼写错误、limit 超上限）→ 截断到上限或返回错误说明。
- 跨账户 `request_id` → 视为不存在（不泄露存在性）。
- 未录制的通道 → 返回 `recorded:false` + 说明，**不返回空字符串冒充内容**。

## 测试策略
- JSON-RPC 形状：initialize 返回 serverInfo/capabilities；tools/list 列出全部工具且 schema 合法。
- 工具行为：余额/账本/用量汇总/请求列表/单请求（含三通道开关）/模型列表。
- 安全：跨账户查询返回空；无令牌 401；令牌吊销后 401；响应中不含成本字段。
- 限额：limit 超过 max_query_rows 时被截断。

## 依赖
标准库 + `internal/{domain,store,registry,secret,config}`。

## 实现与设计差异
（实现完成后回填）
