# M10b 设计：codex 插件出网代理（`proxy`）

> 计划：M10b（已批准）。本文是设计记录，实现完成后回填第 8 节差异。
> 前置：`docs/design/m10-subscription-adapter.md`（本插件本体）。

## 1. 目标与非目标

让 `examples/provider-codex` 能通过 **provider 配置/凭据**声明出网代理，不再依赖"给网关进程设
`HTTPS_PROXY` 再重启网关"。

**要点**：这不是一个便利性功能，而是**可用性前提**。订阅端点与推理端点都由 OpenAI 按客户端 IP 的地区放行；
在当前部署的出口（深圳，CHINANET，113.80.95.12）上，`auth.openai.com/oauth/token` 直接返回
`403 unsupported_country_region_territory`，`chatgpt.com` 更是连不通。凭据再正确也无法从这个出口使用。

非目标：内建 provider 的代理支持（见第 7 节）；`no_proxy` 独立配置；代理健康预检；每供应商代理池；
修正"地区 403 被报成 `token_expired`"的语义（另议）。

## 2. 现状与约束（调研结论）

| # | 事实 | 出处 |
|---|---|---|
| F1 | 插件用裸 `http.Client{}` → `http.DefaultTransport`；代理目前只能靠进程环境变量 | `examples/provider-codex/main.go:129` |
| F2 | 插件进程 `cmd.Env = append(os.Environ(), ...)`，继承网关环境 | `internal/pluginhost/host.go:242` |
| F3 | `net/http` **原生支持** `http/https/socks5/socks5h` 代理 URL；URL 内的 userinfo 会自动转为 `Proxy-Authorization` | `$HOME/sdk/go/src/net/http/transport.go:119-126,1803` |
| F4 | `http.ProxyFromEnvironment` 用 `sync.Once` 记忆化环境变量：进程内首次调用后 `t.Setenv` 不再生效 | `transport.go:955-966` |
| F5 | 上游调用全部汇聚于 `p.http`（token 刷新 525 / session 换取 575 / 推理 879 / 健康 1111）→ 一处注入即可全覆盖 | `grep p.http` |
| F6 | 改 provider 配置或凭据会 `ConfigVersion++` 并 `restartProvider(p)`，**下次请求以新配置重启插件** | `internal/httpapi/admin_providers.go:279,298,349,354` |
| F7 | `pluginapi.Schema`/`ParseSchema` **没有被任何源码调用** → schema 里的 `format:uri` 不产生校验 | 全仓库 grep |
| F8 | 控制台用**原始 JSON 文本框**编辑 provider 配置，不按 schema 渲染表单 | `internal/webui/static/js/pages/providers.js:54,126` |
| F9 | `pkg/providerkit` 已被本插件导入 → 加共享辅助函数**不需要改分层表** | `internal/arch/layering_test.go:63` |
| F10 | 凭据经 `$GW_PLUGIN_STATE_DIR/credentials.json`(0600) 送达，运行期由 `SetCredentials(map[string]string)` 推送（**无返回值**） | `pluginapi/serve.go:19-48` |

F7 + F8 是本设计的硬约束：**校验必须做在插件内部**，schema 更新只作为探测响应里的人类可读说明。

## 3. 关键决策

### D1 配置项 `config.proxy`（字符串 URL），留空 = 回退环境变量

保持向后兼容：未配置时行为与改动前逐字节等价（委派 `http.ProxyFromEnvironment`，连同 `NO_PROXY` 语义）。
不引入 `proxy_from_env` 开关——留空本身就是"跟随环境"，多一个开关只会多一种自相矛盾的状态。

**取舍**：不做独立的 `no_proxy` 配置项。回退路径由标准库的 `NO_PROXY` 处理；显式配置代理时用户意图就是
"上游全部经它出去"（本场景上游都在境外，豁免没有意义）。

### D2 同时支持 `credentials.proxy`，优先级 **凭据 > 配置 > 环境**

代理 URL 可能形如 `http://user:pass@host:port`，而 `config_json` 是**明文存储**且被管理 API 原样回显
（控制台"配置（JSON）"文本框也直接展示）。凭据走既有 AES-GCM 封装、只回显字段名。
于是：无认证代理写在配置里图方便；带认证代理天然不泄露。

**取舍**：不为代理单独开一条加密通道——既有凭据通道已经覆盖这个需求，多一条通道就多一处真源。

### D3 共享辅助函数放 `pkg/providerkit`，transport 与生命周期留在插件内

新增 `pkg/providerkit/proxy.go`，只放两个**纯函数**：`ParseProxyURL` 与 `MaskProxyURL`。
理由：解析与脱敏是可复用且值得单测的逻辑；而 client/transport 的生命周期属于插件自己的关注点。
**取舍**：内建 provider（`internal/providers/*`）有完全相同的地区问题，但本次不做——它们要引入
`internal/providers/httpx → pkg/providerkit` 这条新导入边，必须同步改 `internal/arch/layering_test.go`
与 `docs/architecture.md`，作为独立里程碑。

### D4 不替换 `p.http`，改用"共享 transport + 动态 `Proxy` 函数"

`p.http.Do` 的 4 个调用点都在锁外读 `p.http`，运行期替换 client 会产生数据竞争。改为：

- `transport := http.DefaultTransport.(*http.Transport).Clone()`，只覆盖 `transport.Proxy`；
- 当前代理值用 `atomic.Pointer[proxySetting]` 持有，`Proxy` 函数每次请求**原子读取**——零锁、零竞争；
- 之所以 clone `DefaultTransport` 而不是手搓 `&http.Transport{}`：保留标准库的拨号/TLS 超时、
  `ForceAttemptHTTP2` 等既有调优，避免制造行为漂移（对比 `internal/providers/httpx.ClientWithTimeout`
  就是手搓的，那份超时值是为内建 provider 调的，不适合直接套用）。
- 代理变更时调用 `transport.CloseIdleConnections()`：否则连接池里的旧**直连**连接会被复用，绕过新代理。

### D5 不重启网关即生效

依据 F6：配置/凭据变更会停掉插件进程，下次请求以新配置拉起。这正是本改动相对 env 方案的核心价值——
运维不需要碰网关进程的环境，也不需要 `scripts/local-run.sh restart`。

### D6 用 `Transport.Proxy` 返回错误来承载"配置非法"

Proxy 函数在配置非法时返回 `(nil, err)`，请求自然失败并携带我们的文案，**无需改动 4 个调用点**；
`Health()` 额外前置检查，给出干净的 `proxy_invalid`（fatal，不重试——配置错误重试无意义，也应避免
routing 无谓地故障切换）。

### D7 非法代理一律"记录并上报"，不 exit（实现期修正）

两条来源统一处理：校验失败写进 `proxySetting.err`，由 `Health()` 与请求路径以 **fatal `proxy_invalid`**
暴露，并点名来源（`config` / `credentials`）。`SetCredentials` 无返回值（F10），因此它只能这样上报；
`config` 来源**同样如此**——尽管这与"配置 JSON 非法 → `os.Exit(2)`"的既有约定不一致。

**修正理由（实测，见第 8 节第 1 条）**：按原方案让 `config.proxy` 非法就 `os.Exit(2)`，宿主只会报
`plugin_start_failed: ... read handshake: EOF`；而插件的 stderr 原因在控制台 `last_error`、网关自身日志、
`/providers/{id}/logs`（实测 0 行）**三处都看不到**，运维拿不到任何可行动信息。改为记录后，`last_error`
直接是 `proxy_invalid: ... proxy URL must start with a scheme, for example http://127.0.0.1:2335`，
且插件不再崩溃重启（不再受 `max_restarts_per_min` 节流）。

保留不变的：`GW_PLUGIN_CONFIG` 本身无法解析仍然是 `os.Exit(2)`——那是宿主级引导错误，不是运维在控制台里
可编辑的字段。

## 4. 接口

### 4.1 新增 `pkg/providerkit/proxy.go`

```go
// ParseProxyURL 校验并规范化代理 URL。空字符串返回 (nil, nil)。
// 允许的 scheme：http、https、socks5、socks5h（net/http 原生支持）。
// socks5/socks5h 必须显式带端口；http/https 的缺省端口交给标准库。
func ParseProxyURL(raw string) (*url.URL, error)

// MaskProxyURL 渲染 scheme://host[:port] 用于日志与诊断；永不包含 userinfo。
func MaskProxyURL(u *url.URL) string
```

非法输入的错误要**点名非法部分**，例如：
`unsupported proxy scheme "ftp" (want http, https, socks5 or socks5h)`、
`proxy URL must include a host`、`socks5 proxy URL must include a port`。

### 4.2 `examples/provider-codex/main.go`

```go
type config struct {
    ... // 既有字段不变
    Proxy string `json:"proxy"` // 新增
}

// proxySetting 是一份不可变的生效设置，整体原子换入。
type proxySetting struct {
    url    *url.URL // nil 且 err==nil ⇒ 未配置，回退环境变量
    source string   // credentials | config | env
    err    error    // 非 nil ⇒ 配置非法
}

type provider struct {
    ...
    transport *http.Transport                       // 进程内共享
    proxy     atomic.Pointer[proxySetting]           // 热路径原子读
    envProxy  func(*http.Request) (*url.URL, error)  // 默认 http.ProxyFromEnvironment（测试注入点，见 6.3）
}
```

```go
// installTransport 装配共享 client（只覆盖 Proxy，其余沿用 DefaultTransport 的调优）。
func (p *provider) installTransport()

// effectiveProxy 解析“凭据 > 配置 > 环境”。读 p.creds 时短暂持 p.mu，仅在启动与凭据推送时调用。
func (p *provider) effectiveProxy() *proxySetting

// applyProxy 原子换入新设置；生效 URL 变化时 CloseIdleConnections。
func (p *provider) applyProxy()

// proxyFor 即 transport.Proxy 的实现：
//   s := p.proxy.Load()
//   if s == nil { return p.envProxyFor(req) }
//   if s.err != nil { return nil, s.err }
//   if s.url == nil { return p.envProxyFor(req) }
//   return s.url, nil
func (p *provider) proxyFor(req *http.Request) (*url.URL, error)

// proxyError 把不可用的代理配置变成 fatal 协议错误（Health 与请求入口调用）。
func (p *provider) proxyError() error
```

接线点（均为小改动）：

| 位置 | 改动 |
|---|---|
| `main()` | `installTransport()`（clone DefaultTransport + 设 `Proxy = p.proxyFor`）；`envProxy` 取默认值；代理校验交给 `applyDefaults()`（见 D7，不再 exit） |
| `applyDefaults()` | 末尾调用 `p.applyProxy()`（`p.http.Timeout` 的既有逻辑不动） |
| `SetCredentials()` | 存完 creds 后调用 `p.applyProxy()` |
| `Health()` | 开头检查 `p.proxy.Load().err` → 返回 fatal `proxy_invalid` |
| `describe()` | 增加 `"proxy"`（`MaskProxyURL` 脱敏）与 `"proxy_source"`，供 `whoami` 核对生效值 |
| `configSchema` | 增 `proxy`（`x-advanced`，说明取值与"留空跟随 HTTPS_PROXY"） |
| `credentialsSchema` | 增 `proxy`（`x-secret: true`，说明"覆盖配置，带账号密码时填这里"） |

## 5. 数据流

```
管理面 PATCH /providers/{id}（config.proxy 或 credentials.proxy）
  → ConfigVersion++ → restartProvider() 停掉插件进程                      [F6]
  → 下次请求：宿主写 credentials.json 并拉起插件
  → main()：读 GW_PLUGIN_CONFIG → 校验 cfg.Proxy
  → applyDefaults() → applyProxy()：凭据 > 配置 > 环境
  → transport.Proxy = proxyFor → 请求经代理出网（含 token 刷新、session 换取、推理、健康）
```

未配置代理时：`proxyFor` 直接委派 `p.envProxy`（默认 `http.ProxyFromEnvironment`），与改动前等价。

## 6. 异常与边界

1. **URL 非法**（未知 scheme / 缺 host / socks5 缺端口）：无论来自 config 还是 credentials，都由 `Health()`
   与请求路径报 fatal `proxy_invalid` 并点名来源；请求失败而**不是**静默直连（不重试、不故障切换）。
2. **代理不可达**：表现为 transport 层网络错误 → 既有分类把它映射为 `retryable`
   （`token_endpoint_unreachable` / `upstream_unreachable` 等），routing 可故障切换。这是期望行为：
   代理抖动是瞬时故障，不该判死供应商。
3. **运行期变更**：换代理后 `CloseIdleConnections()`，避免旧直连连接被复用。
4. **凭据与配置同时存在**：凭据优先，`source` 如实回报，便于 `whoami` 排障。
5. **显式代理不豁免回环**：与 Go 原生行为一致；测试里目标就是 127.0.0.1，正好依赖这一点。
6. **不打印密码**：日志与 `whoami` 只出现 `scheme://host[:port]`。既有 `TestWhoamiNeverLeaksSecrets` 扩展覆盖。

## 7. 依赖与范围外

- 依赖：Go 1.25 标准库（无新增第三方依赖）。
- 范围外：内建 provider 的代理（D3 已述）；schema 校验接线（F7）与控制台 schema 表单（F8）；
  "地区 403 → `unsupported_country_region_territory` 被归类为 `token_expired`"的语义修正。三者都与本目标正交。

## 8. 测试策略

`examples/provider-codex/main_test.go`（沿用 `newTestProvider` 辅助；代理用例在其上设 `p.cfg.Proxy` 后调 `p.applyProxy()`）：

1. `TestConfigProxyIsUsedForUpstreamCalls` —— `httptest` 起一个**明文 HTTP 转发代理**（记录绝对 URI 并转发到假上游），
   断言代理被命中**且**上游收到请求。明文即可：`http://` 目标不触发 CONNECT，假代理无需做 TLS 中间人。
2. `TestCredentialsProxyOverridesConfig` —— 配置指向代理 A、凭据指向代理 B，断言仅 B 被命中。
3. `TestProxyUnsetDelegatesToEnvironment` —— 通过注入的 `envProxy` 断言委派发生。**不用 `t.Setenv`**：
   依据 F4，标准库记忆化环境变量，`t.Setenv` 的结果依赖测试执行顺序，必然 flaky。
4. `TestInvalidProxyInConfigFailsStartup` —— 未知 scheme / 缺 host / socks5 缺端口三项逐项断言报错。
5. `TestInvalidProxyFromCredentialsSurfacesInHealth` —— 经 `SetCredentials` 推入非法代理后，`Health()`
   返回 `proxy_invalid` 且标 fatal。
6. `TestDescribeMasksProxyCredentials` —— `http://user:secret@host:2334` 走 `whoami`，断言输出无 userinfo/密码。
7. `pkg/providerkit`：`TestParseProxyURL`（表驱动）、`TestMaskProxyURL`。

回归：既有 11 个插件用例与 providerkit 既有用例全绿；`make verify`（vet + test + build）。

## 8. 实现与设计差异

1. **D7 被实测推翻并修正：非法代理不再 `os.Exit(2)`，改为记录后由 `Health()`/请求路径上报。**
   实测（真实网关）：把 `config.proxy` 设成漏写 scheme 的 `127.0.0.1:2335` 后，宿主报的 `last_error` 是
   `plugin_start_failed: pluginhost: handshake with codex (plugins/aigw-provider-codex) failed: pluginapi: read handshake: EOF`
   ——**零可行动信息**。进一步核实原因的去处：控制台 `last_error` 没有；网关自身日志（`data/aigw-local.log`）
   只有 `plugin exited` / `plugin started`，没有插件 stderr 正文；`GET /admin/api/v1/providers/2/logs`
   返回 **0 行**。三处都拿不到，说明"exit 快速失败"在这个宿主上等于"原因丢失"。
   改为记录后同一场景的 `last_error` 是
   `proxy_invalid: provider-codex: bad config proxy: proxy URL must start with a scheme, for example http://127.0.0.1:2335`，
   且探测响应里 `info.Name = provider-codex`（插件正常握手、不再崩溃重启）。已同步改 D7 与 §6.1。

2. **"忘记写 scheme"的友好提示必须挂在 `url.Parse` 失败分支上。**
   原设计以为 `127.0.0.1:2334` 会被解析成 scheme 为 `127.0.0.1` 的怪 URL，从而走 scheme 白名单分支；
   实际上 `url.Parse` 直接报错（`first path segment in URL cannot contain colon`）。单测 `TestParseProxyURL`
   当场抓到，已把提示移到 parse 失败分支（仅在原文不含 `://` 时改写措辞）。

3. **`Stream()` 入口补了 `proxyError()` 前置检查（设计里没有）。**
   设计只说"由 `Health()` 与请求路径暴露"，但请求路径若不前置检查，transport 层的配置错误会被
   `p.doRequest` 的错误映射归为 **retryable `upstream_unreachable`**，与"配置非法应 fatal、不故障切换"冲突。
   现在非法代理在 `Complete`/`Stream` 入口即以 fatal `proxy_invalid` 返回（`Complete` 复用 `Stream`，一处覆盖两者）。

4. **测试夹具 `newTestProvider` 由裸 `http.Client{}` 改为 `installTransport()`**，并注入 hermetic 的 `envProxy`。
   理由：若不注入，测试会读开发者环境的 `HTTPS_PROXY`；而标准库对环境变量做了记忆化（F4），
   无法用 `t.Setenv` 稳定地纠正。

5. **分层表无需改动**：`pkg/providerkit` 已在 `examples/provider-codex` 的允许集内（F9），
   新增文件未引入新的导入边，`make verify` 中 `internal/arch` 通过。

6. **测试有效性用变异验证过**：把 `proxyFor` 改成永远委派环境变量后，
   `TestConfigProxyIsUsedForUpstreamCalls` 与 `TestCredentialsProxyOverridesConfig` 精确失败
   （`the configured proxy was not used` / `the proxy from credentials was not used`），恢复后全绿——
   证明新用例不是空转。

7. **真实网关端到端实测（2026-09-11，配置→宿主→插件→transport→代理全链路）**：
   用本地记录型替身代理（`127.0.0.1:2335`，对 CONNECT 记一行后回 502）替代真实代理：
   - `PATCH /admin/api/v1/providers/2` 写入 `"proxy":"http://127.0.0.1:2335"` → `config_version=7`；
   - 探测错误由直连时的 `unsupported_country_region_territory` 变为
     `token_endpoint_unreachable: Post "https://auth.openai.com/oauth/token": Bad Gateway`；
   - 替身日志出现 `CONNECT auth.openai.com:443` ——**证明流量确实经代理出去**；
   - `whoami` 回报 `proxy=http://127.0.0.1:2335`、`proxy_source=config`（脱敏、来源正确）；
   - 撤掉代理配置后探测回到 `unsupported_country_region_territory`，替身日志无新增流量
     ——**证明未配置时的默认路径未被破坏**。

8. **真实代理下的端到端（2026-09-11 补测）**：代理 `http://192.168.140.252:2334` 可用后完成实测，**M10b 的目标全部达成**：

   | 环节 | 结果 |
   |---|---|
   | 出口地区 | `ipinfo` 由国家 **CN** 变为 **PH**（Manila）；探测不再报 `unsupported_country_region_territory` |
   | 凭据刷新 | **成功**。`whoami` 报出新的 `expires_at=2026-09-21`、`last_refresh_at`，且轮换后的 refresh_token 已由插件落盘到 `session.json` |
   | 推理端点 | 已穿过 Cloudflare，返回**鉴权后的业务响应**（HTTP 400 + JSON），证明地区与凭据两道门都过了 |
   | 代理生效 | `whoami` 报 `proxy=http://192.168.140.252:2334`、`proxy_source=config` |

   **出字的最后一环是模型 id**：`gpt-5-codex` 等 id 会被拒，而 **`gpt-5.6-luna` 可用**。换上该 id 后
   `/v1/responses` 非流式与流式均返回正确文本（`1+1等于2。` / `收到` / 流式 `delta` 分块拼接一致），
   `usage_records` 落库（含 `reasoning` 维度拆分），零价故不产生账本分录。至此本里程碑目标全部达成。

   **一处我先前的错误结论，在此更正**：我曾据 `GET /backend-api/codex/models?client_version=0.50.0` 带鉴权返回
   `{"models":[]}` 判定"该账号无 Codex 授权"。**这是错的**——同一账号用 `gpt-5.6-luna` 完全可用。
   该目录端点对这类账号**不能作为授权判据**（三种 `chatgpt-account-id` 取值、以及 Codex CLI 的客户端身份头
   `OpenAI-Beta`/`originator: codex_cli_rs`/`User-Agent`/`session_id` 都不改变它的空结果，而请求换个模型 id 就通了）。
   被拒的 id 实为 `gpt-5`/`gpt-5-codex`/`codex-mini-latest`/`o3`/`gpt-5.1-codex`。
   另一条同源约束：`/responses` **强制要求 `stream:true`**（`{"detail":"Stream must be set to true"}`），
   插件恒以流式发送（`Complete` 也复用 `Stream` 聚合），因此不受影响——这一点在本里程碑里属巧合般的好运，值得记录。

9. **这次真实调用额外暴露两个 M10 适配器缺陷（不在 M10b 范围，已记入 `docs/TODO.md`）**：

   - `upstreamMessage` 只识别 `{"error":{"message"}}` 与 `{"message"}`，而该上游对这类错误用的是
     **`{"detail":"…"}`**（FastAPI 形状），于是把唯一有诊断价值的那句话丢成
     `the upstream returned 400 Bad Request`。本次定位正是靠手工 curl 才拿到 `detail` 原文——
     这说明"错误正文丢失"是真实排障成本的来源，不是洁癖问题。
   - 健康检查打 `base_url + /me`，而 `/me` 在此出口被 **Cloudflare 挑战**
     （403 + `cf-mitigated: challenge` + HTML 正文），被 `classifyResponse` 归为
     `token_expired: the upstream rejected the credentials; refresh them and try again`——
     **完全误导**：凭据当时刚刚刷新成功。实测 `/backend-api/codex/models?client_version=…`
     带鉴权返回 200 且未被挑战，是更合适的健康探针。**当前可见症状：业务请求（非流式/流式）全部正常，
     而探测仍返回 `ok=false`，控制台把该供应商显示为不健康**——这正是"错误分类吞掉真因"的代价。
     两项都建议另立里程碑处理。
