# 实现流程约定（每里程碑必须遵守）

用户在评审 v15 计划时明确要求：**实现时要输出详细的设计文档和 todo 文档**。
本文件把这条要求固化成可执行的流程，避免"只写进仓库、没给人看"。

## 每里程碑的标准动作

1. **先写设计文档** `docs/design/{milestone}-{topic}.md`，包含：
   目标 / 关键决策（含取舍理由）/ 接口（类型与签名）/ 数据流 / 异常与边界 / 测试策略 / 依赖。
   文档中"实现与设计差异"一节先留空。
2. **同时先写面向使用者的规格文档**（README 文档表中的 `docs/*.md`，如
   `plugin-protocol-v1.md`、`api-responses.md`、`billing.md`、`pricing.md`、`mcp.md`、`backup.md`、`architecture.md`）。
   规格文档描述**目标行为**，可比实现先行；实现完成后把"状态"从"规格（Mx 实现）"改为"已实现（Mx）"。
3. **把设计文档与相关规格文档贴到对话里给人看**，等确认（或明确"继续"）后再写代码。
   —— 这一步是硬要求：文档不只是落盘，必须**在对话中输出**。
4. 实现代码；同步维护两份清单：`docs/TODO.md`（里程碑 → 任务 → 验收，**只留未完成项**）与
   `docs/todo_done.md`（已完成记录归档）——勾完一项就把该条（连同缩进子条目）搬到归档，
   整节做完搬整节；小节里还剩未完成项时，标题与引言两边各留一份。
5. 完成后回填设计文档的"实现与设计差异"，更新规格文档状态，并在提交信息里引用设计文档路径。
6. 每个里程碑独立提交（提交信息含里程碑编号），保持可回滚。

## 已产出

| 里程碑 | 设计文档 | 对话中已展示 |
|---|---|---|
| M0 | docs/design/m0-scaffold.md | 否（流程确立前，已补展示） |
| M1 | docs/design/m1-store-registry.md | 否（流程确立前，已补展示） |
| M2 | docs/design/m2-plugin-protocol.md | 是 |
| M49 | docs/design/m49-organization.md + docs/org.md | 是 |
| M50 | docs/design/m50-frontend-minify.md | 是 |
| M54 | docs/design/m54-console-asset-shape.md | 是 |
| M55 | docs/design/m55-console-transfer-compression.md | 是 |
| M60 | docs/design/m60-aigw-key-feishu-binding.md + docs/feishu.md | 是 |
| M61 | docs/design/m61-dshgw-feishu-login.md | 是 |
| M66 | docs/design/m66-console-admin-feishu-login.md + docs/feishu.md | 是 |

## 检查项（提交前自检）

- [ ] 设计文档先于代码存在于仓库
- [ ] 设计文档已在对话中展示并获得确认
- [ ] 相关规格文档（README 文档表）已先行写出并展示
- [ ] docs/TODO.md 已同步（完成的条目已搬到 docs/todo_done.md，TODO 里只剩未完成项）
- [ ] 设计文档"实现与设计差异"已回填
- [ ] 规格文档的"状态"已更新为对应实现里程碑
- [ ] **改了 MCP 工具说明或后台路由的 body 字段** → 已按 `docs/mcp.md` §4.5 补齐形状（`Schema`/`RawBody`）、
      示例（`exampleField`）与参数说明，并已跑 `go test ./internal/mcpsrv/ ./internal/httpapi/`。
      守卫测试与构造期 panic 会让漏写的字段直接失败：工具说明不完整时模型不会报错，它会拒绝执行或猜错字段
