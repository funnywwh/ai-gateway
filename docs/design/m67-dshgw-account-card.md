# M67 设计文档：租户侧栏的账号行与退出

> 状态：**已实现（M67，代码与单测完成；真机验收见 §8）**。
> 规格：[docs/dshgw.md](../dshgw.md) §7d；相关：[M61 门户飞书登录](m61-dshgw-feishu-login.md)、
> [M60 Key 与飞书绑定](m60-aigw-key-feishu-binding.md)、[M64 SSH 工作区](m64-ssh-workspace.md)。
>
> 需求原话：「在 ssh 工作区下面添加一行：飞书名或者账号名 "退出"按钮」。

## 1. 目标

进了自己租户的人，在侧栏底部就能看到**自己是谁**（飞书名；没有就账号名；再没有就租户名）和一个
**「退出」**按钮，点一下即登出并回到门户登录页。位置固定在「SSH 工作区」那一行下面。

验收：带会话 cookie 的 `GET /dshgw/session/` 返回该租户的身份；`POST /dshgw/logout/` 撤销该租户
会话并 303 到门户；浏览器里点「退出」回到门户登录页，且同一浏览器中其它租户仍处于登录态；
未开启该功能的部署（以及没有网关的普通 dsh）看不到这一行。

## 2. 关键决策

### D1 名字必须问 aigw，因此开关在 aigw 侧

租户名（`dsh-colin`）是账号名 的 ASCII slug，人认不出来；飞书名（`李智超`）只存在于 aigw 的
`api_keys.feishu_name`。dshgw 手里只有 worker Key，而**飞书绑定在「人的 Key」上，不在 worker Key 上**
（[`mintDshgwKey`](../../internal/httpapi/admin_catalog.go) 造的 key 没有任何飞书字段），所以「只查
当前认证 key」在真实部署里几乎永远查不到名字。

决策：aigw 的 `POST /v1/dshgw/authorize` 200 响应新增 `account`（`accounts.name`）与 `feishu_name`
（该账号**任一** Key 上的绑定）两个字段，纯新增、旧客户端忽略即可；已有调用点（门户登录、逐请求
复核、dshgw 身份解析）共用同一次调用。查询失败只影响名字，绝不影响 `allowed`。

**被否决的替代**：把账号名塞进 dshgw 注册表就完事（省一次调用，但显示的是 `colin` 这种 slug）；
把名字放进登录票据（要动票据契约与共享测试向量，且只覆盖飞书登录路径，API Key 登录仍然没有名字）。

### D2 账号名顺带落库，飞书名每次都问

`account` 随控制台的 `tenant-create`/`tenant-set-key` 写入注册表（`registry.Tenant.Account`，可空、
旧文件照旧加载），dshgw 解析身份时若发现空值也会用 aigw 的答案补写一次。飞书名则不落库：它是可以
随时解绑/换绑的个人属性，按租户缓存 5 分钟并在登录成功时预热即可。

### D3 退出在**租户 origin** 上执行，而不是跳门户的 `POST /logout`

> 客户端的第二次修正（真机抓到）：退出请求必须用 `redirect: 'manual'`。网关的 303 指向门户，而
> **门户 origin 在浏览器上未必可达**（租户端口与门户端口不同）；跟着重定向走的 `fetch` 会在第二跳
> 网络失败并抛 `TypeError: Failed to fetch` —— 会话其实已经撤销，人却被告知「退出失败」。改成不跟随
> 之后，同一个答案以 opaque redirect 回来，那就是「已撤销 + 浏览器被告知去哪」，客户端再做跳转。
> 请求万一真的没回来（网关本身不可达），客户端会先回读 `/dshgw/session/`：被拒即证明退出已生效，
> 于是照样把人送到门户，只有读身份仍然成功时才把失败显示出来。


门户那条路由要求 `Origin` 精确等于门户 origin；租户页面发出的只能是自己的租户 origin（端口模式下
两者端口不同），请求会被 403 —— 用 `<a>` 或不带 Origin 的导航还会撞上「GET 不改变状态」。因此新开
`POST /dshgw/logout/`：同一份 `Sessions` 存储、同一道 origin 栅栏、撤销**本租户**会话后 303 到门户
登录页。语义上也更对：一个租户侧栏里的按钮不该把人在其它租户里也登出。

**被否决的替代**：让门户放宽 Origin 校验（把跨端口的租户 origin 也放进来）——那等于把「只有门户能
登出」这条 CSRF 栅栏拆掉一半，换来的只是省一个路由。

### D4 这一行是独立插件，挂在侧栏 footer 的 list 槽

`sidebar.footer.action` 是 list 槽，注册顺序决定行序：browser 90 → ssh 100 → 本行 120。插件只装
浏览器半边（`dsh.client`，**没有实质的 node 半边**）：它只做两件事——`GET /dshgw/session/` 与
`POST /dshgw/logout/`，都在本 origin 下，不需要 RPC 通道、不需要租户侧进程。开关是
`account_card.enabled`，关掉时两个路由 404、行不出现。

加载行指向 `index.js`（一个什么都不做的 host 半边），**不能**指向 `client.js`：行的 `name` 是宿主
`import` 的模块，而浏览器 bundle 在 import 时就调用 `window.__ModuleLoader__.load`，指错会让整个租户
的插件树挂掉（真机实测：`ReferenceError: window is not defined`，worker 直接退出）。浏览器半边照样被
发现——dsh 通过包里的 `dsh.client` 声明与 `exports["./client"]` 找到它，与另外两个工作区插件同一形状。

**被否决的替代**：并进 ssh-workspace 插件（少一个目录，但 `ssh_workspaces` 关闭时这一行也会消失，
而「我是谁/退出」与 SSH 没有关系）。

### D5 与 worker 请求共用同一条鉴权链

把 `TenantHandler` 里原有的「唯一 cookie → 会话属于本租户 → `key_revalidate` → dsh 授权复核」原样抽成
`tenantSession()`，两个新路由复用同一份。这两个路由在 worker 握手之前处理，所以 worker 没起来也能回答
「我是谁」；反之，任何一条检查漏掉都会变成「会话过期了还能读到名字」。失败一律走既有语义：GET 302 门户
登录页、其它方法 401 JSON；账号被停用/关闭 dsh 时撤销会话并踢回门户。

### D6 名字解析永远不阻塞

`identity()` 的返回值里 `tenant` 来自注册表（一定有），其余尽力而为：拿不到 key、aigw 不通、没有绑定
都只记一条 WARN，名字回退到账号名/租户名，租户页面照常渲染，登录照常成功。缓存按租户加锁，TTL 5 分钟，
租户消失时随注册表重载清理。

## 3. 接口与配置

### 3.1 aigw

| 方法 | 路径 | 变化 |
|---|---|---|
| POST | `/v1/dshgw/authorize` | 200 响应新增 `account`、`feishu_name`（可能缺省）；403/401 不变 |

```yaml
dshgw:
  account_card:
    enabled: false      # 打开后：子进程获得该行与它调用的两个端点
```

### 3.2 dshgw

```yaml
account_card:
  enabled: false        # 独立部署直接写这里；监督形态由 aigw 生成
```

| 方法 | 路径（**租户 origin**） | 行为 |
|---|---|---|
| GET | `/dshgw/session/` | `{ok, value:{authenticated, tenant, account?, feishu_name?, name}}`，`Cache-Control: no-store`；非 GET → 405 |
| POST | `/dshgw/logout/` | 撤销本租户会话 + 清 cookie → 303 门户登录页；缺/跨源 Origin → 403；非 POST → 405 |
| 其它 | `/dshgw/**` | 404（功能关闭时整个命名空间都是 404） |

注册表新增字段：`tenants[].account`（可空，≤128 字节、无控制字符，`DisallowUnknownFields` 兼容旧文件）。
控制台通道新增字段：`tenant-create` / `tenant-set-key` 的 `account`。

### 3.3 侧栏

插件 `cmd/dshgw/plugin/account-card/client.js`：模块 id `dshgw-account-card`，`inject: ['slots']`，
`sidebar.footer.action` `order: 120`。折叠栏只留退出图标（`aria-label` 给全文），读取失败时改为
「重试」，404 时整行不渲染。

## 4. 数据流

```
门户登录（Key 或飞书）
  → aigw /v1/dshgw/authorize：allowed + tenant + account + feishu_name
  → dshgw 发会话 + 预热 identity 缓存（并按需把 account 补写进注册表）
  → 302 租户 origin
租户页面
  account-card(client.js) ──GET /dshgw/session/──► TenantHandler（保留命名空间，先走 tenantSession）
        ▲ {tenant, account, feishu_name, name}
        └─「飞书名 + ⏻ 退出」
  「退出」──POST /dshgw/logout/──► 撤销本租户会话 + 清 cookie ─303─► 门户登录页
```

## 5. 异常与边界

| 情况 | 行为 |
|---|---|
| 普通 dsh / 功能关闭 | `/dshgw/session/` 404 → 行不出现；客户端对 404 静默、不记警告 |
| aigw 不可达 / 无飞书绑定 / 旧租户无 account | 名字逐级回退，直至租户名；不报错、不阻塞登录 |
| 会话过期、跨租户 cookie、重复 cookie | 既有 `unauthenticated` 路径：GET 302 门户，其它 401 JSON |
| 账号停用 / 关闭 dsh | 既有 `enforceDSHAccess`：撤销会话并踢回门户，行随之消失 |
| 跨源 POST 登出 | 403（`checkEdgeOrigin`），会话不受影响 |
| GET `/dshgw/logout/` | 405，不改变状态（否则一张跨站图片就能把人登出） |
| 网络失败导致读取失败 | 行内显示原因 + 「重试」，不弹窗、不冒充已登出 |
| 折叠栏 | 只显示退出图标，`title`/`aria-label` 给全文 |
| 路径模式 / 端口模式 | 303 目标由服务端用 `OriginForPort(PortalPort)` 计算，客户端不猜 origin |
| 其它租户会话 | 退出一条只影响当前租户 |

## 6. 测试策略（已实现）

- `internal/httpapi`：authorize 增开（有绑定 → 带 `feishu_name`；无绑定 → 字段缺省且仍 `allowed`）；
  启用 dsh 时 `account` 随 `CreateTenant` 传给 dshgw。
- `internal/dshgw/registry`：`account` 往返、旧文件加载、非法值拒绝、
  `SetAccount` 对不存在租户/非法值报错。
- `cmd/dshgw`：admin 协议里 `account` 跨 socket 原样到达（含非 ASCII）。
- `internal/dshgw/proxy`：`/dshgw/session/` 正常/回退/无 cookie/错方法/失效会话；保留命名空间下
  未知路径 404；`/dshgw/logout/` 的 403、405、303、cookie 清除、会话撤销、**不影响其它租户**；
  身份缓存一个 TTL 内只查一次 aigw；租户消失后缓存被清。
- `cmd/dshgw/plugin/account-card/client.test.mjs`（假 DOM/React/fetch，37 条断言）：模块契约与行序、
  名字回退、rail 折叠、404 不渲染、读取失败给「重试」、退出是 POST + 跳转、退出失败给出原因。

## 7. 实现与设计差异（回填）

1. **不新增独立调用，而是扩 `authorize` 的响应**：设计初稿考虑过单独的「账号信息」接口，落笔时发现
   dshgw 解析身份必须用租户的 worker Key（唯一它持有的凭据），而那条路径本来就有一次 authorize 调用，
   扩字段等于零额外往返。
2. **飞书名按「账号任一 Key」查**：初稿只想查当前认证的 Key，读代码时确认 worker Key 不携带绑定，
   于是改为扫该账号的 Key（`ListAPIKeys` 一次索引查询，只发生在缓存未命中与登录预热时）。
3. **折叠栏的居中要一路做到按钮（真机抓到的缺陷，改了两轮）**：折叠栏里每一层都是 flex 容器，
   所以「谁被居中」要一层层看。第一轮：行还带着 8px 水平内边距，被居中的是内边距盒，图标右移 8px。
   把内边距去掉后仍偏左——用 CDP 量**运行中的租户页面**才看清：壳子的 `.footerActions` 只有 27px 宽
   （x=14），行自己不给 `justify-content` 时按 flex-start 排，于是按钮停在 x=14（中心 20.50），
   而 ssh / 浏览器的按钮中心都在 27.50。最终规则：rail 档 `padding: 6px 0; justify-content: center;`
   两者必须同时成立（只居中不删内边距会反过来多偏 8px）；改完后线上实测按钮中心 = 27.50。
4. **图标偏左还有一半是「墨迹」偏左**：盒子居中（上一条）之后，人眼仍觉得偏左，把字形本身量出来才
   看清：⏻ 在 14px 下 ink 只占 advance 的 `[0..11]`（13px 箱子里右侧空 2px，重心 −1.00px），所以
   盒子正中也会看着偏左。把它单独放大到 15px 后 ink 填满 advance，偏差降到 −0.50px —— 与旁边的
   `⇄`（ssh 那行，同样是 −0.50px）一致；`line-height: 1` 保证盒子高度仍跟字形走，不改变这一列的
   纵向节奏。这条与上一条**是两件不同的事**：上一条解决「按钮不在栏中央」，这条解决「字形在自己
   盒子里偏左」，两条都改完才真正齐。
5. **退出的重定向不能跟随（真机抓到的缺陷）**：见 §D3 的补记。仓库内的无头 Chromium 复现是
   「303 → 浏览器不可达的门户 origin → `TypeError: Failed to fetch`」，修复后同一场景下 `退出失败`
   计数为 0 且浏览器完成了跳转；单测里两种结局（请求没回来但会话已撤销 / 网关明确拒绝）各有一条。
6. **`tenantSession()` 抽取**：设计里只写了「两个路由要鉴权」，实现时把 `TenantHandler` 原有的整条链
   原样搬进一个方法两处复用，避免第二份实现慢慢与第一份分叉。
7. **`account` 顺带落库**：原设计只打算实时解析；实现时发现注册表里存一份能让「aigw 短暂不可达」与
   「重启后」都还能显示账号名，于是加了 `SetAccount` 与 admin 协议字段（旧租户在下次启用/轮换时补上）。
8. **客户端 404 = 没有功能，而不是错误**：验收目标是「普通 dsh 上什么都不该多出来」，所以 404 静默、
   其它失败才显示行内错误与「重试」。
9. **登录时预热**：卡片在页面加载时就问，而页面加载紧跟在登录跳转之后，因此在 `login`/`feishuLogin`
   成功后同步解析一次，避免第一眼的空白。
10. **加载行指向 host 半边（真机抓到的缺陷）**：第一版把行写成 `account-card/client.js`，浏览器 bundle
   在宿主 import 时就执行 `window.__ModuleLoader__.load`，于是**租户 dsh 直接起不来**
   （`ReferenceError: window is not defined`，worker `exit status 1`，四个租户同时中招）。修法是补一个
   什么都不做的 `index.js` 作为行入口，浏览器半边继续由 `dsh.client` + `exports["./client"]` 发现；
   单测里新增 `TestAccountCardRowNamesTheHostHalfAndShipsAClientBundle` 把这条钉住。

## 8. 真机验收

本机形态与 M64 §14 相同：`bin/dshgw --config ./dshgw.yaml serve`（**直接 serve**，不是 aigw 生成的子进程
配置，因此开关写在 `dshgw.yaml` 里），`gwproxy`（8090，明文）转发 `/dshgw/` 与 `/t/<tenant>/`，
租户端口 18301–18305 由 dshgw 自己用自签证书服务（**端口模式**：浏览器直接访问 `:18303` 的根路径，
不带 `/t/` 前缀）。

已执行并观察到（2026-09-19，进程与数据都在本机）：

| 检查 | 结果 |
|---|---|
| `POST /v1/dshgw/authorize`（用 dsh-colin 的 worker Key，直连 aigw 8088） | `{"account":"李智超(colin)","allowed":true,"feishu_name":"李智超","tenant":"dsh-colin"}` —— 绑定在**另一个** Key（`lzhichao@lagenio.com`）上，证明「扫该账号的 Key」这条路径真的能拿到名字 |
| 账号名回填（admin socket `tenant-set-key` 带 `account`） | 注册表出现 `"account": "李智超(colin)"` |
| `GET /dshgw/session/`（端口模式，带该租户会话 cookie） | 200 `{"ok":true,"value":{"authenticated":true,"tenant":"dsh-colin","account":"李智超(colin)","feishu_name":"李智超","name":"李智超"}}`，二次读取同样命中缓存 |
| `GET /dshgw/session/`（无 cookie） | 302 到门户登录页（`https://chat.tirisen.hk:18300/`） |
| `POST /dshgw/logout/` | 跨源 Origin → 403；`GET` → 405；正确 Origin → 303 + 清 cookie，随后同一 cookie 读身份变回 302 |
| 错误行导致的故障与修复 | 见 §7 第 7 条：修复后四个租户 worker 全部 `ready`，租户页面恢复 |
| 页面确实加载了浏览器半边 | 租户 `/` 的插件串里含 `dshgw-account-card/client.js`（排在 `dshgw-ssh-workspace`、`dshgw-browser-workspace` 之后） |
| 反例（`account_card.enabled: false` 重启） | `/dshgw/session/` → 404，租户页面不再出现 `account-card`；改回 true 后两者都恢复 |

**待人工确认的一项**：侧栏那一行的实际观感与点击「退出」后的浏览器跳转需要人在浏览器里看一眼
（本机无浏览器自动化）；接口与 bundle 两半都已按上表验证。
