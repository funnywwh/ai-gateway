# M9 设计：Web 管理控制台

> 前置：`docs/design/m8-admin-api.md`（会话）、`docs/design/m8b-admin-resources.md`（资源端点）。
> 本文档在编码前输出，实现后回填第 8 节差异。

## 1. 目标与约束

目标：M8b 暴露的每一个管理端点都要有对应的界面操作，使运维不再需要 curl；界面与 API 同源同会话。

约束（环境决定，不是偏好）：
- **没有 node/npm**，也没有构建步骤：界面必须是浏览器直接可跑的静态资源；
- 因此采用**原生 HTML + ES modules + fetch**，不用框架、不用打包器、不用 CDN（离线可用）；
- 资源用 `go:embed` 编进二进制，`/admin/ui/*` 提供，**不引入独立前端服务**。

## 2. 结构

```
internal/webui/
  embed.go        //go:embed static  + 静态资源 handler + SPA fallback
  static/
    index.html    // 外壳：导航 + 主区域
    app.css       // 唯一的样式表（含深浅色变量）
    js/api.js     // fetch 封装：Cookie、JSON、401 跳登录、错误 toast
    js/router.js  // hash 路由：表驱动，页面懒加载 import()
    js/ui.js      // 组件：表格、表单、对话框、确认、toast、徽章、JSON 编辑器
    js/pages/*.js // 每页一个模块
```

分层规则：页面模块只依赖 `api.js` + `ui.js`；`api.js` 只依赖 fetch；`ui.js` 不依赖任何页面。
新增一个页面 = 加一个文件 + 在路由表加一行，不改外壳。

## 3. 鉴权与安全

- 复用 M8 的会话 Cookie（HttpOnly、SameSite=Lax、Path=/admin），不做第二种登录态；
- 前端启动先 `GET /admin/api/v1/auth/me`：401 → 渲染登录页；403（viewer 写操作）→ toast 提示只读；
- 写请求统一带 `Content-Type: application/json`；后端**新增 CSRF 收紧**：管理面的
  POST/PATCH/PUT 必须声明 JSON（表单跨站提交无法伪造该类型），DELETE 无体不校验；
- 明文密钥（API Key、MCP 令牌、提供商凭据）只在创建响应里出现一次，界面立刻弹出
  "现在复制" 对话框并在关闭后清空；列表页永远不显示明文；
- 录制内容（输入/思考/输出）按 M7 的开关显示；未录制时显示原因而不是空白。

## 4. 页面清单

| 页面 | 数据来源 | 关键交互 |
| --- | --- | --- |
| 登录 | auth/login | 口令；失败按后端限速提示 |
| 概览 | stats | 注册表计数、熔断/冷却表、key 缓存占用；只读 |
| API Keys | keys | 新建（一次性明文）、双勾选（最终输出文本 / 思考文本）、输入录制模式、启停 |
| Providers | providers + models + logs + actions | 新建/编辑、凭据写入（只写不回显）、探测、模型发现、日志、动作、重启、删除（引用守卫） |
| Models & Routes | models + routes + providers | 模型表；按模型分组的路由矩阵，行内改权重/优先级/启停 |
| Mappings | model-mappings + router explain | 规则表；试算器预览解析链路 |
| Tags | tags | grants/policy JSON 编辑 + 描述 |
| MCP 令牌 | mcp-tokens | 签发（一次性明文）、吊销 |
| Hooks | hooks | 新建/删除、事件过滤、采样率、内容附带开关 |
| 请求日志 | requests + requests/{id} | 过滤（天数/账户）、分栏详情（输入/思考/输出） |
| 审计 | audit-logs | 只读列表，changes 折叠展示 |
| 设置 | settings | 键值读写（JSON） |
| 定价 / 账单 / 对账 / 备份 | — | 占位页：明确标注 M11/M12/M16，不假装可用 |

## 5. 需要补的只读端点

- `GET /admin/api/v1/router/explain?model=&account_id=&api_key_id=`：复用 `routing.Explain`，
  返回解析到的模型、有序候选与排除原因；Mappings 页的试算器用它。
  没有这个端点，映射规则在界面上只能"盲改"。
- 其余端点全部复用 M8b，不再新增写接口。

## 6. 交互约定

- 写操作成功后**局部刷新受影响列表**（不整页重载），失败保留表单内容；
- 删除、清空凭据、吊销令牌一律二次确认，确认框写明对象名；
- 所有表格支持客户端过滤（一个输入框）与列排序；本轮**不做服务端分页**
  （管理数据量在校验过的 500 行上限内），服务端游标分页列入 TODO；
- 表单来源优先级：插件 handshake 的 `config_schema`/`credentials_schema` → 通用 JSON 文本域；
- 任何后端 4xx 都把 `error.message` 原样展示（后端已给出人类可读文案）。

## 7. 测试

1. `internal/webui`：静态资源可访问、`index.html` 存在、未知路径回落 SPA、未登录返回 401；
2. `internal/httpapi`：CSRF 中间件（form 类型 400、JSON 通过）；explain 端点返回候选与排除原因；
3. 用真实二进制 curl 走查：`/admin/ui/` 返回 HTML，`/admin/ui/js/app.js` 返回 JS 且
   带正确 Content-Type，未登录 `/admin/ui/` 不泄露数据（页面本身公开，数据靠 API 401 把关）。

## 8. 实现与设计差异

1. **`router/explain` 的语义改了**：设计里只说复用 `routing.Explain`。实现时发现 `Explain` 在
   「模型解析成功但没有任何候选」时返回错误且丢掉排除原因——而这恰恰是试算器最需要的信息。
   现在 `Explain` 只在**请求无法解析**时返回错误；只要有解析结果，就以 `failure` 字段承载失败原因、
   以 200 返回候选与排除列表。数据面走的是 `Plan`，不受影响。
2. **`Placeholder` 页面是真的占位**：定价/账单/对账/备份四页明确写出所属里程碑，不做假交互。
3. **Key 的录制开关分两步写**：创建 Key 的接口不接受录制参数（M8 的契约），界面在创建成功后
   立刻补一次 PATCH，保证「勾了就生效」，即使操作者马上关掉明文弹窗。
4. **弱化了服务端分页承诺**：本轮列表统一 `limit=500` + 客户端过滤/排序。
5. **新增 CSRF 收紧**：管理面 POST/PATCH/PUT 必须声明 `Content-Type: application/json`，
   跨站表单无法伪造该类型；这一条在设计与 API 文档里都补记了。
6. **界面资源也带 CSP**：`default-src 'self'`，无内联脚本、无外部 CDN，离线可用。
7. **模拟器支持 `features`/`provider`/`strategy` 查询参数**：设计只要求 model，实现顺带把
   能力过滤与钉死供应商也能试算出来（对排障很有用）。
8. **用 Node（环境里 DSH 自带的 v22）对 13 个页面模块做了语法检查与导入图校验**：
   浏览器代码不会被 `go test` 覆盖，这是目前能做的最接近「跑一遍」的验证；
   真正的浏览器交互验证仍然只能靠人工打开 `/admin/ui/`。