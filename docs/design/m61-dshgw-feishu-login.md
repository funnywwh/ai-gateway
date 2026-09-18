# M61 设计文档：dshgw 门户的飞书登录（消费 aigw 的票据）

> 状态：**已实现（M61，代码、单测与监督形态端到端验收完成；真机验收待执行——需要飞书自建应用）**。
> 规格：[docs/feishu.md](../feishu.md) §5；上游绑定（谁是谁）见 [M60](m60-aigw-key-feishu-binding.md)。
>
> 需求原话：「同时实现dshgw飞书登录到dsh」；部署面确认「我要配置走gwproxy 8090」。

## 1. 目标

被绑定的人打开 DSH 门户，点「飞书登录」，进入自己的租户，全程不粘贴 API Key；未绑定、被停用、
凭据被吊销的人一律被拒，且门户给出能看懂的原因。

## 2. 关键决策

### D1 dshgw 不持有任何飞书凭据（本里程碑的核心取舍）

飞书应用、App Secret 与**唯一**的重定向 URL 都留在 aigw。aigw 完成 OAuth 交换、解析出 `open_id`、
查出「这个人属于哪个租户」，然后签发一张**短期一次性票据**交给 dshgw。

代价：多一个进程间契约（票据格式）。
收益：飞书后台只登记一个回调地址；`app_secret` 只存在于一处；dshgw 不需要出站访问 `accounts.feishu.cn`；
新增租户/端口与飞书配置彻底解耦。**被否决的替代方案**是 dshgw 自带飞书应用（或复用同一应用的第二个
回调），它需要第二份凭据、第二条重定向 URL，并且 dshgw 仍要反过来问 aigw「这个 open_id 是谁」。

### D2 票据契约（两侧各自实现，共享向量保证一致）

```
payload = {"v":1,"mode":"dsh","tenant":"dsh-alice","key_id":7,"account_id":3,
           "open_id":"ou_…","nonce":"<16B hex>","exp":<unix>}
wire    = base64url(payload) + "." + base64url(HMAC_SHA256(secret, "feishu-ticket:" + base64url(payload)))
```

- aigw 侧实现在 `internal/feishu/ticket.go`，dshgw 侧在 `internal/dshgw/feishu/ticket.go`；
  **两侧互不 import**（`internal/arch` 的分层闸门要求 aigw 不得 import `internal/dshgw/**`，反之亦然）。
- 一致性由**同一份测试向量**保证：`internal/dshgw/contract/testdata/feishu_ticket_vectors.json`
  （含有效、过期、超前过期、未知 mode、错误版本、缺租户、异密钥七类样本），两侧测试都读它；
  重新生成：`go run ./cmd/gen-feishu-vectors internal/dshgw/contract/testdata/feishu_ticket_vectors.json`。
- 验票规则（dshgw 侧）：签名（常量时间）、版本、`mode=="dsh"`、必填字段、`exp` 未过期、
  `exp ≤ now+10m`（拒绝被人为拉长的有效期）、nonce 未被用过。

### D3 票据交付：同主机走 cookie，异主机才回退 query

票据价值 = 一次登录，因此默认不让它出现在地址栏。aigw 与门户同主机（本部署：8090 与 18300）时，
aigw 下发 **host-only** cookie `aigw_dshgw_ticket`（`HttpOnly; SameSite=Lax; Path=/; Max-Age=ticket_ttl`，
`Secure` 随部署 scheme）；cookie 不按端口隔离，所以门户在自己的端口上收得到。
门户与 aigw 不同主机时 cookie 不会到达，这时 aigw 改为在 URL 里带 `?ticket=` 并记一条 WARN。

dshgw 优先读 cookie；两者都存在且不同 → 拒绝（这是被篡改的请求，不是偏好问题）。

### D4 单次使用在**消费侧**记录

票据的 nonce 由 dshgw 在兑换时记入内存集合（有界 4096 + 按 TTL 清理）。发证侧无法权威地知道票是否
被用过（那需要回执），而"谁消费谁记录"天然正确。进程重启会丢失该集合，窗口被票据 TTL（默认 120 秒）
限住，文档明示。

### D5 收票后仍然复核账号级授权

dshgw 拿到票后，用**该租户当前的 worker Key** 调 aigw `POST /v1/dshgw/authorize` 再问一次
「这个账号现在还能用 dsh 吗」。理由：票是 120 秒前的事实，而停用可能发生在这一瞬间；并且这让
`dsh_enforce` 的 `interval`/`per-request` 档位对飞书登录与 Key 登录**完全一致**。
判定失败（超时/5xx/未配置）一律 503 fail-closed；明确拒绝（403）按原因给出「未启用 dsh」/「账号已停用」；
worker Key 被吊销（401）给「凭据已被吊销」。

### D6 登录成功后的会话与 Key 登录逐字节相同

同一个 `Sessions.Issue` + `setSessionCookie` + `Activity.MarkLogin`，因此 TTL、续期节流、会话上限、
单域名模式下的 cookie Path、退出语义全部不变。飞书只替换了「身份解析」这一步。

### D7 失败一律由门户自己渲染

门户是用户当时看着的页面，所以错误页复用既有 `renderLogin(status, message)`：
`GET <portal>feishu/error?reason=<code>`（未启用时 404）。aigw 的 `mode=dsh` 分支把所有拒绝都 303 到这里，
不自己渲染（唯一的例外是 state 不可信、连 flow 都不知道时——见 M60 §D8）。

### D8 门户按钮只在配置齐全时出现

`feishu.enabled=true` 才渲染「飞书登录」，并且 Key 表单**保留**：它既是飞书不可用时的回退，也是
「先登录再绑定」这条自助路径的入口（绑定本身在 aigw 控制台完成）。

### D9 `public_scheme` 必须在明文 HTTP 部署上显式声明

dshgw 在端口模式默认按 `https` 生成 URL 并发 `Secure` cookie；明文 HTTP 下浏览器会丢弃该 cookie，
表现成「登录成功又被弹回门户」。本里程碑把 `dshgw.public_scheme` 从 aigw 配置透传到子进程
（`internal/dshgwsup` 原先没有这个字段），并在 `feishu` 校验里直接拒绝「明文 http + Secure cookie」的
组合，让它在启动时就失败而不是在用户点击时。`doctor` 也复核这一条。

## 3. 接口与配置

### 3.1 dshgw 侧

```yaml
feishu:
  enabled: false
  aigw_login_url: http://192.168.190.86:8090/feishu/login   # 浏览器可见的 aigw 入口
  ticket_secret: ""                                          # 与 aigw 相同（监督形态由 aigw 注入）
```

| 方法 | 路径 | 行为 |
|---|---|---|
| GET | `<portal>/` | 登录页；启用飞书时多一个「飞书登录」链接 |
| GET | `<portal>/login/feishu` | 验票 → 复核授权 → 下发 `dshgw_s_<tenant>` → 302 到租户 origin |
| GET | `<portal>/feishu/error?reason=<code>` | 复用登录页渲染错误（未启用时 404） |

校验（`enabled=true` 时）：`aigw_login_url` 绝对 URL、path 以 `/feishu/login` 结尾、`ticket_secret` 非空、
且不得是「明文 http + Secure cookie」的组合。

### 3.2 aigw 侧（生成子进程配置）

aigw 在 `cfg.Feishu.Enabled && cfg.Feishu.DSHLogin` 时，把
`{enabled: true, aigw_login_url: <callback_url 的 origin>/feishu/login, ticket_secret: <派生或显式值>}`
写进子进程配置；未启用时 `Feishu` 为 nil 且 `omitempty`，生成的配置与之前**逐字节相同**。
`aigw_login_url` 与票据密钥都从同一处派生，因此运维不需要在两个文件里对齐任何值。

## 4. 数据流

```
门户「飞书登录」（链接到 aigw 的 /feishu/login）
  → aigw：签 state{flow:dsh} → 飞书授权页 → aigw /feishu/callback
      → 换 token / 取 user_info / 解析 open_id
      → 未绑定 → 303 <portal>/feishu/error?reason=unbound（不出票）
      → 账号停用/未启用 dsh/租户未分配 → 303 …reason=account_status|dsh_disabled|tenant_missing
      → 通过 → 签票 → 设 host-only cookie → 303 <portal>/login/feishu
  → dshgw：验票（签名/过期/单次）→ 租户存在？→ worker Key 调 /v1/dshgw/authorize 复核
      → 拒绝：403/503 + 门户错误页（不发会话）
      → 通过：Issued 会话 + setSessionCookie + MarkLogin → 审计 feishu_login_success
              → 302 租户 origin（path 模式为 <base>/t/<tenant>/）
```

## 5. 异常与边界

- 票据缺失/篡改/异密钥/版本不符/字段缺失 → 「登录链接无效，请重新点击飞书登录」。
- 票据过期或已被使用 → 分别提示「已过期」/「已使用过」。
- 票据指向的租户不在 registry → 「租户在本网关上不存在，请联系管理员」。
- 未绑定 → 「尚未绑定任何 API Key，请先用 API Key 登录，或联系管理员在 aigw 控制台完成绑定」。
- 停用/吊销 → 「该账号未启用 dsh」/「该账号已停用」/「凭据已被吊销」。
- aigw 不可达 → 503「认证服务暂不可用，请稍后再试；API Key 登录不受影响」。
- 跨站请求（Origin 不符）→ 403，与 Key 表单同一条 CSRF 栅栏。
- **错误文案不外泄内部词汇**：`bad signature`/`malformed` 等只进日志，进页面的是人话（单测逐个钉住）。
- 已建立的会话不因飞书侧变化而撤销：吊销仍由 `dsh_enforce`/`key_revalidate`/控制台停用负责。

## 6. 实现与设计差异（回填）

1. **收票用门户自己的页面，而不是重定向到门户错误页**：dshgw 就是门户，`renderLogin` 已经是它的
   错误渲染入口，因此 `feishu/error` 只是把这个页面挂上路径（同时让 aigw 的 303 有地方可去）。
2. **验票失败原因直接进文案映射**：第一版把所有验票失败都映射成「登录链接无效」，被端到端验收
   与单测暴露——`already used` 与 `expired` 对用户是不同的处境，现在分开提示，且内部词汇
   （`bad signature` 等）被归到同一条人话里。
3. **`checkFeishuLogin` 加进 `doctor`**：设计里只写了「配置校验」，实际把「票据密钥是否为空」与
   「明文部署却会发 Secure cookie」两件事做成 doctor 检查，因为这两种错都表现为「点了没反应」。
4. **端到端验收需要 `public_base_url` 带端口**：监督形态验收要让 dshgw 生成的 URL 可被真正访问，
   于是把 gwproxy 的端口提前分配并写进 `dshgw.public_base_url`；顺带发现既有断言
   `path-mode-tenant-api-401` 把 Origin 写死成 `http://localhost`，现在改为使用配置里的公开 origin。
5. **`StubAigw` 增加 `POST /v1/dshgw/authorize`**：D5 的复核在验收里必须有对手方，否则飞书登录会
   以 503 结束；该 stub 同时提供 `deny` 开关，用来验证「签发后立刻吊销」这条路径。

## 7. 测试策略（已实现）

- `internal/dshgw/feishu`：共享向量（与 aigw 签名侧逐位一致）、单次使用、篡改/畸形输入、
  禁用（空密钥）时拒绝一切、单次集合有界。
- `internal/dshgw/proxy`：成功登录（会话 cookie 属性与 Key 登录一致、302 到租户 origin）；
  重放/过期/篡改/异密钥/未知租户各自的文案且**不建会话**；票据缺失或 cookie 与 query 不一致 → 拒绝；
  授权复核三态（DSHDenial 两种 reason / ErrInvalidKey / 不可达 503）；authorizer 未配置 → 503；
  未启用时门户**没有**飞书按钮且两条路由 404；跨站 Origin 被拒；错误文案表逐条覆盖。
- `internal/dshgw/config`：`validateFeishu` 的开关矩阵（含「明文 http + Secure cookie」）。
- `cmd/aigw`：`TestBuildDshgwChildInjectsTheFeishuHandoff` —— aigw 签的票能被注入给子进程的密钥验证，
  且未启用时生成的配置里完全不出现 `feishu`。
- 端到端（`scripts/dshgw_supervised_e2e.py`，真实 aigw + dshgw + bwrap worker + gwproxy + 飞书 stub）：
  控制台登录 → 建账户/Key → 绑定飞书（走 stub 授权页）→ 绑定落库 → 从**门户**点飞书登录 →
  出票 → 收票 → 进租户（UI 200）→ 复核被调用 → 解绑后被拒（未绑定文案）→ 上游吊销后被拒（403）→
  恢复后再次成功。共 15 步，全绿（总计 45 步通过）。
