# 不修改 DSH 的 gwproxy `/dsh/` 前缀代理研究

## 1. 结论与边界

**可以实现，但不能只靠 strip-prefix、`X-Forwarded-Prefix` 或当前 gwproxy 的 YAML 开关。**

建议在 gwproxy 中增加独立的 **DSH 前缀适配器**：反向代理负责请求路径、认证边界、响应头及流式转发；适配器负责 HTML/启动元数据和浏览器网络 URL。DSH 安装目录、插件 bundle 文件及构建产物均不修改、不重建。

这里“不改 DSH”指不修改其文件；允许网关对 HTTP 响应做受限转换、注入网关自有脚本。如果连响应转换和脚本注入也禁止，则当前 DSH 不支持完整的 `/dsh/` 挂载。独立 origin 根挂载或跳转到独立端口才是零适配方案，但它们不满足“浏览器始终停留在 `/dsh/`”的目标。

本次仅研究及只读探测，未实施代理功能、修改运行配置、重启服务或进行带认证的浏览器端到端验收。

## 2. 当前事实

### 2.1 gwproxy

- `gwproxy.yaml:2-17`：监听 8090，aigw 根挂载；`portal_redirect`、`tenant_redirect` 均为 true。
- `internal/frontproxy/proxy.go:52-103`：仅门户、租户、aigw 三类路由，没有单实例 `/dsh/ → 3080` 路由。
- `internal/frontproxy/proxy.go:245-300`：已有租户前缀剥离、registry 查端口、Host/edge header 设置和即时 flush。
- `internal/frontproxy/proxy.go:514-545`：仅对 `text/html` 做标签属性改写，不处理启动 graph 中的 URL、浏览器运行请求、重定向及 cookie 路径。
- `internal/frontproxy/config.go:145-149` 严格拒绝未知 YAML 字段。因此不能直接添加尚未实现的 `dsh_upstream` 一类字段就期望生效。

### 2.2 已存在的 nginx 适配器

可读的实际配置在 `/home/winger/nginxwebui/nginx.conf:62-142`：

1. `/dsh` 重定向到 `/dsh/`。
2. `/dsh/` 剥前缀后转到宿主 8443，再转到 DSH 3080。
3. 注入 `dsh-prefix-shim.js`，包装 fetch、WebSocket、EventSource、XHR、sendBeacon。
4. 改写 HTML/部分脚本中的资源 URL、cookie Path、Location。
5. 禁用上游压缩及代理缓冲。
6. 另有 hostname 白名单替换，以开放设置功能；这与前缀代理不是同一个问题。
7. 入口具备 RFC1918/loopback 访问限制。

`/etc/nginx/sites-enabled/dsh-web-8443.conf:72-99` 将所有上游请求固定为 `Host: 127.0.0.1:3080`，移除 Origin/Fetch Metadata，支持 WS 升级及长连接。

**因此历史文档中“路径前缀装不下 DSH”的说法过强。准确表述是：无适配的透明前缀代理不行；代理适配可行，但须承担兼容维护成本。** 参考 `deploy/dshgw/README.md:336-400` 与 `docs/design/m51-dshgw.md:55-60`，后者其实已经记录过 shim 方案，只是当时不愿承担维护成本。

### 2.3 本次实测

未携带认证信息，仅 GET 已有服务：

| 探测 | 结果 | 能说明什么 |
|---|---|---|
| `http://127.0.0.1:3080/` | 401，DSH authentication required | 原始服务正常执行认证 |
| `http://127.0.0.1:3080/dsh/` | 404 | DSH 不直接识别该挂载路径 |
| `http://127.0.0.1:8090/dsh/`，Host 为配置域名 | 404 | 当前 gwproxy 没有这个挂载 |
| gwproxy `/t/dsh-colin/` | 302 到独立租户端口 | 当前是入口跳转，不是前缀 UI |
| `https://chat.tirisen.hk/dsh/` | 401，同样的 DSH 认证提示 | 现有外部前缀可到达受保护的 DSH 入口 |
| `https://chat.tirisen.hk/dsh/dsh-prefix-shim.js` | 200，2092 字节，no-store | 现有适配脚本确实在提供服务 |

另在 Node VM 中执行实际下载的 shim，使用假的网络构造器验证：

- 字符串、绝对 URL、URL 对象、Request 的 `/api` 映射正确。
- Request 的方法、body、headers、AbortSignal 保留。
- WS 的 URL、protocols、prototype、OPEN 常量，以及 SSE/XHR/beacon 映射通过。
- 外部 origin、已带 `/dsh` 的 URL、`/v1/models` 保持不变。
- **现有 shim 不改 `/plugins/events`**；这是观察到的覆盖缺口，不是完整兼容验证。

没有因此宣称现有 nginx 方案或拟议 gwproxy 方案已通过完整 UI 验收。

## 3. DSH 0.1.2-rc.1 的实际约束

以下 `D/` 统一指：

`/home/winger/.local/dsh-0.1.2-rc.1/node_modules/@deepseek-ai/`

本机是发布安装树，证据来自运行实现 `lib/*.js`，不是缺失的 apps/packages 源码树。

| 面 | 实际行为及证据 | 前缀适配要求 |
|---|---|---|
| HTML base | `D/dsh-host-frontend-static/lib/index.js:81-95`，最终强插 `<base href="/">` | 改为 `/dsh/`；不能只追加第二个 base |
| shell 静态资源 | `D/dsh-web-frontend/dist/index.html:6-12` 使用 `./assets/...` 等 | base 正确后相对资源自然落到前缀内 |
| 启动插件 | `D/dsh-client-modules/lib/index.js:182-183,417-429` 固定 `/plugins/??...&rev=...` | 改写 preload、bootstrap script，以及 boot graph |
| 动态插件 | `D/dsh-client-modules/lib/client.js:145-158` 使用 `script.src`，不是 fetch | `__DSH_BOOT__.entries[*].url`、`batches[*].url` 都要处理 |
| RPC | `D/dsh-client-connection/lib/client.js:4618,4665-4667` 用 `location.origin` 构造 URL | 在真实网络 URL 边界加前缀；base 标签无效 |
| 会话实时流 | `D/dsh-api-gateway/lib/client.js:49,541-546` 构造 `/api/remote.mux` WebSocket | 改 WS URL 并透传 101/双向流 |
| HMR | `D/dsh-client-hmr/lib/client.js:42,98` 使用 `/plugins/events` EventSource | 适配该 URL，不缓冲 SSE；测试重连 |
| 导出 ZIP | `D/dsh-session-log-export/lib/client.js:104-115` 先 HEAD，`:26-30` 再 anchor.click | fetch 改写不覆盖实际下载 GET，需处理导航 URL |
| PWA | `D/dsh-web-frontend/dist/manifest.webmanifest:2-10` 的 id/start_url/scope 是 `/`，icon 也是根路径 | 若保留安装能力，按字段改 manifest |
| 登录 | `D/dsh-client-connection/lib/index.js:365-392` 仅根路径 GET token exchange，303 到 `/` | 外部 `/dsh/?token=...` 映射到根；303 改回 `/dsh/` |
| cookie | 同包 `:229-238,257-270,408-417`，名字/签名绑定 Host，Path=/ | Host 全链一致；改 cookie Path，不改签名内容 |
| 信任栅栏 | 同包 `:178-192,529-532` 校验可信 Host、Origin host、跨站元数据 | 不可不经验证就删除浏览器安全头 |

额外注意：

- `/plugins/??...&rev=...` 是组合资源的精确地址；保留 RawQuery，不排序/重新编码它。资源查询是加载契约，不是普通任意参数。
- 本版没有依赖 pathname/history 的业务路由，选择会话走内部 store；无须伪造 `window.location`。DSH 静态 fallback 也不是任意路径都返回 shell，不能凭空承诺 `/dsh/session/...` 深链接。
- `X-Forwarded-Prefix`、`DSH_WEB_URL` 都不是现成的前缀配置入口。后者是运行时输出的本地地址，不是输入的 base URL。

## 4. 推荐架构：兼容逻辑由 gwproxy 单独持有

### 4.1 先支持单实例 `/dsh/`

```text
浏览器 https://chat.tirisen.hk/dsh/
  → 现有 TLS 入口（保留 /dsh/，保留访问控制）
  → gwproxy :8090
      /dsh/       → DSH prefix adapter → 127.0.0.1:3080/
      /admin/ui/   → aigw
      /v1/        → aigw
      /dshgw/、/t/ → 保持现有行为
```

建议新增的 gwproxy 配置外形如下，**这是设计示例，当前版本尚不支持这些字段**：

```yaml
dsh_web:
  enabled: true
  prefix: /dsh
  upstream: http://127.0.0.1:3080
  public_origin: https://chat.tirisen.hk
  compatibility: dsh-0.1.2-rc.1
```

访问限制、canonical upstream Host、是否代持认证等需要显式策略，不能由浏览器任意 header 决定。

### 4.2 入站代理

- 精确匹配 `/dsh` 和 `/dsh/` 路径族；不能误吞 `/dshgw`、`/dsh-other`。
- `/dsh` → 308 `/dsh/`，保留 query。
- `/dsh/api/...` → `/api/...`；`/dsh/plugins/??...` → `/plugins/??...`。
- 在解析/校验后协调处理 Path 与 RawPath，保留合法编码及原始 query；拒绝遍历和异常请求目标，不用 `path.Clean` 静默更改请求语义。
- API 不缓存；移除 API 请求缓存校验头。WS、SSE、ZIP、上传不进入文本响应改写管线。
- 继续使用 `httputil.ReverseProxy` 的升级能力和即时 flush；不能给 SSE/模型流加全量响应缓冲。

### 4.3 HTML 与 boot 元数据

- 仅处理确认的 DSH shell，不扫所有 `text/html` 用户内容。
- 替换已有 base 为外部挂载路径。
- 对真正的 URL 属性做结构化改写，不对正文、注释、脚本文本全局替换。
- 识别 `__DSH_BOOT__` 的已知赋值结构，解析 JSON 后只变更 `entries[*].url`、`batches[*].url`；不更改插件 ID、逻辑 channel 或其他配置数据。
- 在所有 DSH 启动脚本之前同步安装 gwproxy 自有的 adapter；必须早于消费者，不能依赖异步 MutationObserver 去抢资源加载时机。
- adapter 可作为 `/dsh/_gwproxy/prefix-adapter.<rev>.js` 提供。URL 应绝对带前缀，避免脚本自身因 base 顺序出错。
- 若 CSP 存在，使用允许的同源脚本或有效 nonce/hash，不默认删掉 CSP 或放开 unsafe-inline。

### 4.4 浏览器网络与下载适配

一个统一、幂等的 URL 映射器，只处理当前实例的同源 DSH 网络路径：

```text
/api、/api/*                 → /dsh/api、/dsh/api/*
/plugins/*                  → /dsh/plugins/*
/assets/* 等已知静态命名空间 → 对应 /dsh/ 路径
同 authority 的 ws/wss       → 相同路径映射
已带前缀 / 外部 origin / blob / data / aigw 路由 → 不变
```

- 包装 fetch（字符串/URL/Request）、WebSocket、EventSource；按需要覆盖 XHR/beacon 以兼容插件。
- 包装必须保留 Request body/headers/credentials/signal、构造器原型及静态常量；不消费请求流、不更改 method。
- **下载单独处理**：已知导出会创建一个不插入 DOM 的 anchor 并立即 `.click()`。仅 document click listener 或 MutationObserver 不可靠。需要在受限的 anchor URL 设置/原生 click 之前同步映射同源 `/api` 下载 URL，保留 filename、query 和流式下载语义。外部链接及 blob 下载不改。
- 资源由 HTML/boot 元数据改写覆盖；不要误以为 fetch wrapper 能接管 `<script src>`、CSS、manifest、导航等所有浏览器请求。
- PWA manifest 以 JSON 字段转换，或明确将 PWA 标记为不支持；不能只让图标正常就声称安装功能正常。
- sourceMappingURL 等开发资源可单列兼容项目。生产能力与开发便利性应分别验收。

**不建议把现有 nginx 的所有 sub_filter 逐条复制进 Go。** 优先保留上游 bundle 字节不变，转换 HTTP/DOM 网络 URL 边界和启动数据。尤其不能无脑将所有字符串 `"/api"` 改成 `"/dsh/api"`：DSH 把 `/api` 同时当逻辑 channel，`dsh-client-connection/lib/client.js:4598,4669-4671` 对 channel 有自己的校验。

### 4.5 现成扩展点的正确用法

DSH 提供 `globalThis.__DSH_TRANSPORT__`，见：

- `D/dsh-client-connection/lib/client.js:4719-4720`。
- `D/dsh-client-connection/lib/types/client/index.d.ts:40-65`。

可注入 `fetch` 做更窄的 RPC 适配，`loadBundle` 为动态插件加载扩展点。但这些不是完整的 base-path 功能：不能单独解决 HTML 首批插件、原生 WS、HMR、导出下载。

`openStream` 是逻辑流承载接口，不是一个 WS URL 字段。仅为了加前缀就重新实现 mux 协议，维护代价通常比包装网络 URL 更大。

`ownsHost` 会改变 privileged settings 的判定，**不是前缀开关**。当前 dshgw 已在 `internal/dshgw/proxy/proxy.go:917-968` 根据自身设置策略注入它；迁移时应保留/评估既有授权决策，而不是为解决 URL 自动开启更多权限。

### 4.6 认证和响应头

单实例直接接 DSH 可保留其 token → cookie 模式：

1. token exchange 和后续请求使用同一个 canonical upstream Host。
2. `Location: /` 改成 `/dsh/`；仅转换本实例内部重定向，拒绝/保留外部 URL 需明确策略，不能无条件添加前缀。
3. `Set-Cookie Path=/` 改成 `/dsh/`；保持 HttpOnly/SameSite，并在外部 HTTPS 下考虑 Secure。清除 cookie 必须使用同样的 Path。
4. 不记录/暴露 token、Cookie、Authorization；错误日志也不能带完整认证 query。
5. 可保留浏览器 public Host/Origin 并配合 DSH 现有 `--trusted-host`，但设置功能及既有 cookie 与其有独立耦合，不能迁移时静默切 Host。
6. 若沿用现有 loopback Host 策略，必须由可信入口先做 public Host、Origin、Fetch Metadata、认证及访问控制，再清理上游安全头。不能把当前 LAN-only 配置洗头逻辑无保护地暴露到公网。

相同域名下路径不是安全边界；即使只挂一个 DSH，它也与 aigw 控制台共享 origin，需要信任共站应用及其脚本。

### 4.7 编码、缓存及迁移

- 转换目标请求要求上游 identity，或正确解压后转换；不能向 gzip/br 字节插入文本。
- 转换后修正 Content-Length/Content-Encoding，移除或重新生成 ETag 等验证器；避免旧 304 绕过新注入。
- HTML/认证/API 首期 no-store；未修改且有内容 revision 的静态 bundle 保留缓存。按前缀或租户变化的响应不能共用错误的缓存键。
- 转换体超过限额应明确失败或完整无损透传，不能截断后假装透传。当前 `rewriteShellPaths` 的编码、validator、超限处理均需加固再复用。
- 现有 nginx 应改为将原始 `/dsh/...` 转给 gwproxy，保留 TLS、访问控制、正确 public Host 及 WS；移除旧 shim/sub_filter/cookie-path 改写，避免双重适配。
- 不能继续用旧 `proxy_pass .../` 把前缀提前剥掉，否则 gwproxy 看到 `/` 后会落到 aigw。
- 外层和 gwproxy 的真实 client IP/转发 header 信任范围必须明确；gwproxy 直接端口若可达，不能绕过原有访问限制。

## 5. 多租户是另一层决策

`/dsh/ → 3080` 是固定实例；`/dsh/<tenant>/` 是多租户。不能靠 Referer 或全局“当前租户”cookie 把所有裸 `/api` 猜测分流：多标签页会互相干扰，且 WS、导航等请求不提供可靠的路径来源。

若目标是多租户：

- 路径必须显式携带租户身份，adapter 前缀取 `/dsh/<tenant>`。
- gwproxy 继续转 dshgw，不直接绕过它访问 worker。
- 现有 dshgw 已支持 `public_base_url`、`tenant_path_prefix`、`portal_path_prefix`；见 `internal/dshgw/config/config.go:970-1070`，能够产生路径 URL 和路径 cookie。
- 对齐 gwproxy 的 `tenant_prefix` 并关闭该路径的 `tenant_redirect`，但这仅是路由前提；浏览器适配器仍需实现。
- `dshgw` 继续负责认证、会话所属租户检查、账号开关、worker cookie 代持与重握手，见 `internal/dshgw/proxy/proxy.go:481-569`。
- `cmd/dshgw/plugin/browser-workspace/client.js:143-155` 使用 `./browser-workspace/...`，正确 base 也关系到现有插件能力；测试不能只覆盖 `/api`。
- cookie Path 和唯一租户名防误投，但**不提供浏览器 origin 隔离**。同源页面可以发请求到另一租户路径；如果同一浏览器拥有对方会话，cookie 会随目标请求发送。localStorage/IndexedDB 等也默认共享 origin。
- 因此不应把端口隔离迁为路径后声称安全等价；不信任租户/插件需要继续使用独立 origin，并保留服务端租户鉴权。

建议先完成单实例 `/dsh/`，在明确接受共享 origin 的威胁模型后再拓展多租户。

## 6. 实施分工与验收门槛

建议改动范围（尚未实施）：

- `internal/frontproxy/config.go`：新增专用 DSH mount/兼容模式配置和冲突校验。
- `internal/frontproxy/proxy.go`：在 aigw 兜底前增加专用路由。
- 新增专用 adapter 模块及自有 JS，隔离 URL、HTML/boot、cookie/Location 逻辑，避免继续把所有逻辑堆入通用代理。
- 为结构化 HTML 处理评估合适 tokenizer/parser；不要继续扩展正则至任意脚本/JSON。
- 单元测试、浏览器契约测试、升级门禁、示例配置和运维文档。
- 只改变外层 nginx 的路由/适配归属，不改 DSH 发行文件。

必须验收：

1. `/dsh` 规范化及 query 保留；登录 303、cookie Path、退出、过期、重启重登录。
2. shell、base、两个 boot URL 集合、组合插件请求、CSS/字体、动态图标，不逃到裸根路径。
3. RPC 的实际 POST URL、WS 101、会话实时更新、断网重连、长时间流式输出。
4. HMR SSE `/dsh/plugins/events` 与 rebuilt 后的插件 URL；开发验收另需实际构建 watcher，不等于仅开启 receiver。
5. 导出 ZIP 的 **HEAD 和实际下载 GET** 都经前缀；校验产物而非只看按钮提示。
6. 上传/附件/本地浏览器目录/SSH 工作区等已启用插件；外部 URL、已前缀 URL、blob/data 不被误改。
7. 刷新、缓存版本切换、带压缩请求、大响应、未知 DSH 版本或 boot schema 的明确诊断与回退。
8. 跨站 unsafe 请求/WS 被拒，错误 Host 被拒；直连 8090 不绕过访问控制；aigw `/v1` 与控制台零回归。
9. 若做多租户：双标签不同租户、cookie 归属、并发流、错误租户路径、会话撤销，以及明确记录共享 origin 的风险。

完成标准不是“HTML 返回 200”，而是所有关键请求、下载、认证和实时流都保持在正确挂载路径，且未降低原有安全边界。
