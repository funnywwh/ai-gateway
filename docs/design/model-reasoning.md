# 模型级推理覆写

> 状态：已实现。这个功能独立于历史 M20 的 DSH 客户端配置工作；M20 的历史范围和复验记录见 [`m20-dsh-reasoning-effort.md`](m20-dsh-reasoning-effort.md)。

## 1. 目的与范围

对一个**规范/对外模型**设置推理强度策略，而不是对某个供应商实例或路由设置。一个模型的所有可用路由共享该策略；它不会为同一供应商下的其他规范模型改变配置，也不会重启插件进程。

模型策略只决定发给上游的 `reasoning.effort`。上游模型实际支持哪些档位仍由上游决定；网关不按供应商或模型维护额外的允许列表，也不把上游拒绝伪装成成功。

## 2. 存储与迁移

迁移 [`internal/store/migrations/0015_model_reasoning.sql`](../../internal/store/migrations/0015_model_reasoning.sql) 为 `models` 增加独立的：

```sql
reasoning_json TEXT NOT NULL DEFAULT ''
```

它不复用 `policy_json`。空字符串（或解析意义上的 JSON `null`）表示没有模型级覆写。`domain.ParseModelReasoning` 严格解析保存的对象，避免一个未被数据路径读取的键进入数据库。

`bootstrap.models[]` 可声明同形状的 YAML：

```yaml
bootstrap:
  models:
    - public_name: canonical-model
      reasoning:
        mode: force
        effort: high
```

bootstrap 新建模型时写入该设置；merge 已有模型时，只有 bootstrap 明确提供 `reasoning` 才替换已有值，省略则保留。YAML 的未知键、缺失字段或非法值会在加载/校验阶段失败。

## 3. 语义与优先级

配置对象必须恰有以下两个字段：

```json
{"mode":"default","effort":"medium"}
```

| 字段 | 允许值 | 语义 |
| --- | --- | --- |
| `mode` | `default`、`force` | `default` 仅在客户端请求未带 `reasoning.effort` 时填入 `effort`；`force` 无论客户端的 effort 是什么都覆盖它。 |
| `effort` | `none`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max` | 传给上游的 effort 值。 |

处理顺序为：客户端请求 → 规范模型 reasoning 策略 → 该模型选择的任一路由/供应商上游请求。策略只改 effort：`force` 会保留客户端的 `reasoning.summary` 等同一 reasoning 对象上的其他字段；`default` 也会保留客户端显式的 `none`。

reasoning 对象内的未知字段、缺少 `mode` 或 `effort`、或未列出的值均无效；管理请求顶层保持原有兼容性。网关的闭集代表可保存与透传的值，不代表每一个上游都支持每一档；上游不支持时请求会由上游拒绝。

Codex 插件原有供应商配置 Schema 仅列出四档，但请求路径会优先透传请求的 reasoning，而非将请求档位限制为四档。因此模型配置经请求字段生效，不必改共享供应商配置。未配置模型时，插件自身的供应商兜底行为不变。

配置产生的 effort（包括显式 `none`）参与 routing 的 reasoning 能力检查，与客户端显式传入的语义一致。现有 `degradation=strip` 仍是降级标记而非实际删除参数，本功能不改变它；严格拒绝不支持该能力的候选可使用现有 `reject` 模式。

## 4. 管理 HTTP API 与控制台

模型创建和更新使用同一字段：

- `POST /admin/api/v1/models`
- `PATCH /admin/api/v1/models/{name}`

请求中：

- 省略 `reasoning`：保持已有值（新模型即无覆写）；
- 给对象：完整替换，必须通过严格校验；
- `"reasoning": null`：清空覆写并恢复继承。

模型列表和模型写入响应均返回 `reasoning` 对象或 `null`。控制台模型页提供「继承 / 默认 / 强制」：继承提交 `null`；默认与强制显示相同的 effort 选择器。页面说明默认保留客户端显式 effort（包括 `none`）、强制保留 `summary` 但改写 effort、并提醒所有路由共享该规范模型设置及上游可能拒绝不支持的档位。

每次模型写入会使 key 缓存失效并重载 registry；生产组合根的 reload 只调用 `reg.Reload(ctx)`，不重启插件。若数据已持久化但 registry reload 失败，模型写入返回 HTTP 500，并明确说明“已保存但未应用”；同时写入失败 reload 审计。修复 reload 根因后重试更新以应用设置。

## 5. MCP 操作示例

需要 `scope=admin` 的 MCP 令牌。先读取实时 schema：

```json
{"name":"admin_describe","arguments":{"name":"admin_update_model"}}
```

返回的 `body_schema.properties.reasoning` 是无顶层 type 的 `oneOf`：

1. 严格对象：`type: object`、`required: ["mode", "effort"]`、`additionalProperties: false`；
2. `{ "type": "null" }`：清空覆写。

路径名在 `params`，JSON 字段在 `body`。例如强制 `high`：

```json
{"name":"admin_request","arguments":{
  "name":"admin_update_model",
  "params":{"name":"canonical-model"},
  "body":{"reasoning":{"mode":"force","effort":"high"}}
}}
```

清空并恢复继承：

```json
{"name":"admin_request","arguments":{
  "name":"admin_update_model",
  "params":{"name":"canonical-model"},
  "body":{"reasoning":null}
}}
```

MCP 的 `admin_request` 直接调用同一 HTTP handler，因此校验、审计、reload 成功/失败语义和控制台完全相同。

## 6. 验证与限制

覆盖范围包括：迁移保留旧行、store 独立持久化、bootstrap merge、严格 domain 解析、路由计划加载、HTTP/MCP create-update-clear、attempt 级对象复制、默认/强制语义、registry snapshot、reload 失败可见性，以及控制台嵌入资源与 Node VM mocked form 回归。

真实 HTTP 测试使用本地假上游，走网关 `/v1/responses`、路由、dispatcher 和内置 Responses 适配器，验证两个共享供应商模型在流式/非流式下分别 force/high、default/medium、保留客户端 none，以及清除后恢复无模型覆写。Codex 出站构造测试另行验证 high/max 透传和供应商默认值兜底。

验证结果：`go test -count=1 ./...` 通过；本功能相关 HTTP/MCP、runtime、domain、routing、registry 及 Codex race 测试通过。全量 HTTP race 测试仍有五个既有 audit batching 测试失败；已在未修改的 `git archive HEAD` 基线 `/tmp/aigw-reasoning-baseline.830DZU` 独立复现相同失败，因此不属于本次改动引入的问题。

控制台表单回归命令（测试文件不嵌入发布资源）：

```bash
node --experimental-vm-modules --no-warnings internal/webui/tests/models_test.mjs
```

上述测试不等同于真实上游兼容性验证：控制台以 Node VM mocked modal/API 测试，不宣称真实浏览器验证；每个实际上游/模型是否接受某一 effort 仍必须用真实请求验证。插件重启不属于模型 reasoning 写入路径。本次未更改线上配置或部署。
