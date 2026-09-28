# M90 租户插件自己挂浏览器 RPC 通道（修 dsh 0.1.7 下 `transport failure … HTTP 405`）

## 0. 需求原话

> 修复 dsh transport failure for /ssh-workspace/hosts: HTTP 405
> 错误：transport failure for /dshgw-git-diff/hello: HTTP 405

两块面板（SSH 工作区的「我的主机」、会话主区的「变更」）在后端报同一个错。这不是两个插件的两个
bug，而是**所有租户侧宿主半共用的一条通道挂了**：浏览器 POST 出去，没人接，被 dsh 的 SPA 兜底座位
回了 405。

## 1. 现状（实测）

复现：直接对租户 dsh 的 web 口打该通道（本机 `127.0.0.1:18401`，`--trusted-host` 与浏览器同源）：

```console
$ curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:18401/ssh-workspace/hosts \
    -H 'content-type: application/json' -d '{"type":"client-request","rpcId":"x","method":"hosts","payload":{}}'
405
```

| 事实 | 证据 |
|---|---|
| 四个宿主半插件在 loader 里都是 active，但通道一个也没挂上 | `plugin_manager list_plugins`：`include:ssh-workspace` / `dshgw-git-diff` / `dshgw-web-tty` / `dshgw-workspace-files` 全部 `enabled:true fiberPhase:"active"`；同一时刻 POST 四个通道 + `picker-clamp` 全 405，只有 `/api` 回 401（路由在、要求鉴权） |
| 405 来自 dsh 的 SPA 兜底座位 | `@deepseek-ai/dsh-host-frontend-static` 抢的是 webserver 的 **fallback seat**：`if (req.method !== 'GET' && req.method !== 'HEAD') → 405`。请求没命中任何前缀路由才会掉到这里 |
| 根因在 `dsh-client-connection@0.1.7-alpha.2`：`rpc.handle` 对**任何**调用方都会抛 | `HostConnectionService` 的 `get rpc()` 闭包捕获的是**服务自己的** context（`const owner = this.ctx`，构造函数里是 `@deepseek-ai/dsh-client-connection` 插件的 ctx，`inject = ['credentials']`），`register()` 里读 `owner.webServer` ⇒ `cannot get property "webServer" without inject`。改调用方没用：owner 不是调用方 |
| 把调用包进 `ctx.inject(['connection','webServer'])` 也修不好（宿主已试过这一版） | 部署树里四个插件的现行写法的确是 `ctx.inject(['connection','webServer'], (child) => child.effect(() => child.connection.rpc.handle(…)))`；实测（下面第 3 节的探针）A、B 两种写法抛同一个错，而 `child.webServer` 本身是好的 |
| 逐字复现（0.1.7-alpha.2，默认 web profile，探针插件） | `apply: ctx.webServer threw: cannot get property "webServer" without inject`；`B: rpc.handle threw: … at Proxy.register (dsh-client-connection/lib/index.js:656:16)`；`A: rpc.handle threw:` 同上（A 的 child 里 `typeof child.webServer = object`） |
| 浏览器侧的错误串就是"整条通道不在"的翻译 | `dsh-client-connection/lib/client.js`：`if (!response.ok) throw new Error('transport failure for ' + channel + '/' + endpoint + ': HTTP ' + response.status)` ⇒ 405 唯一来源是"路由未挂" |
| 0.1.2-rc.1 没有这个坑 | 上一个运行时可挂通道（M75 起四个插件都在用），2026-09-24 升到 0.1.7-alpha.2 后才出现；租户 trace 最后一条正常 RPC 是升级前 2026-09-24T10:26Z，进程重启（`pid:2`）之后再没写过一次 |
| dsh 自己挂路由用的原语是公开且稳定的 | `/api` 就是这样挂的：`webServer.register({ kind:'prefix', path:'/api', handler })` + `connection.admit(req)`（`requestRejection` 的注释明说这是给"another Web route"用的）；`webServer.register` 是 `@deepseek-ai/dsh-host-webserver` 的核心 API，`HostConnectionHandle.admit` 在 `.d.ts` 里是公开成员 |
| 通道的线路协议很小 | 客户端 `createWebConnectionRpc`：`POST <channel>/<endpoint>`，body `{type:'client-request',rpcId,method,payload}`；只认 2xx，再按 `parseConnectionResponse` 解 `{type:'server-response',rpcId,result:{ok:true,value}\|{ok:false,error:{code,message,details}}}`，`details` 必须是 record |

## 2. 设计

### 2.1 不再依赖 `ctx.connection.rpc.handle`，四个宿主半共用一个小模块

新增 `cmd/dshgw/plugin/lib/rpc-channel.js`，只做一件事：**用 dsh 自己挂 `/api` 的那两个原语，把
插件自己的通道挂上去**。

```
ctx.inject(['connection', 'webServer'], (child) => child.effect(() =>
  child.webServer.register({
    kind: 'prefix', path: channel,
    handler: (req, res) => route(req, res)          // route 里第一步就是 child.connection.admit(req)
  }), '<plugin>: browser RPC channel'))
```

- **鉴权不自己发明**：`connection.admit(req)` 与 `/api` 同一把尺子（Host/Origin fence + 浏览器鉴权），
  拒了就是 401/403（body `unauthorized` / `forbidden`，与 dsh 一致）。
- **信封逐条对齐 dsh 的 `rpcFetchHandler`**，包括状态码：非 POST / 路径不是端点 → 404；非
  `application/json` → 415；body 不是 JSON → 400；信封不合法 → 400 + `{ok:false,error:{code:
  'gateway/bad-request',…}}`（rpcId 能读就读出来）；`method` 与端点不一致 → 200 + 同一个 bad-request
  信封；handler 抛异常 → 500 `handler failure: …`。请求体按上限（默认 8 MiB）读，超了 413 并断开，
  不无界缓冲。连接断了就把 `AbortSignal` 交给端点（web-tty 的长轮询靠它释放）。
- **浏览器半一行不改**：客户端照旧 `ctx.connection.rpc.call(channel, endpoint, payload)`——它拼的相对
  URL、`rpcId` 关联与信封校验全都不变。这也是选"自己挂同一条线路"而不是改用 `/api` 精确路由
  （`connection.fetch.register` 每个端点一条路径，还要重做客户端）的原因。

### 2.2 为什么不去修 dsh，或去改 `node_modules`

- 修 dsh 要动安装目录（`/home/operator/.local/dsh-0.1.7-alpha.2/` 这类 pinned runtime）里的第三方包，
  仓库管不到、升级即丢；
  而本仓库的全部插件都必须在一个 pinned runtime 上跑，得自己站得住。
- 升级 dsh 也不是这次的选择：0.1.7-alpha.2 是刚定的版本，且没有证据表明新版修了这个（0.1.7 的
  `get rpc()` 就是这个形状）。
- 代价与边界写在这里：**线路协议是 dsh 的**。升级 dsh 时要按 §3 的探针重跑一次；失效的现象是
  "200 之外的状态码"或信封解析报错，两者都会在 `rpc-channel.test.mjs` 与探针里先炸，不会等到租户
  浏览器。

### 2.3 部署形状变了：多一个 `lib/`

插件目录旁边多了共用的 `lib/rpc-channel.js`（四个宿主半 `import '../lib/rpc-channel.js'`，git-diff
那一份还带上自己的 `?rev=` 缓存后缀）。部署动作仍是"把 `cmd/dshgw/plugin/` 按原样放过去"，但**旧的
三目录同步法会漏掉 `lib/`**，漏了的表现是整行加载失败（`Cannot find module …/lib/rpc-channel.js`）。
三处一起堵这个坑：

- `dshgw doctor` 与 `dshgw node doctor` 新增一条 `tenant-plugins-lib` 检查
  （`tenancy.SharedPluginModulePath` / `SharedPluginModuleRequired`：只要有一个用到它的插件开着就查，
  全关的部署不欠这一项）；
- `deploy/dshgw/README.md`、两份 `config.example.yaml` 与 `docs/dshgw.md` §7f 写明部署形状；
- 多机部署不受影响：`nodedep` 的载荷整个复制 `plugin_path` 所在目录（`buildPayload` → `copyTree`），
  `lib/` 自然跟着走。

## 3. 验证（全部本机实测）

**① 探针：同一台机器起一个独立 dsh web（`/tmp/dshprobe`，端口 18999），把仓库里**改好的**
`ssh-workspace/index.js` 与 `git-diff/index.js` 挂进一个干净 profile**：

```console
# 探针插件（复现用）：A = ctx.inject([...]) 包住的 rpc.handle，B = 裸 rpc.handle，C = 直接 webServer.register
$ cat /tmp/dshprobe/plugin.log
apply: ctx.webServer threw: cannot get property "webServer" without inject
B: rpc.handle threw: Error: cannot get property "webServer" without inject
A: child ready; typeof child.webServer = object      <-- 注进来的 webServer 是好的，owner 才是坏的
A: rpc.handle threw: Error: cannot get property "webServer" without inject
C: webServer.register returned

# 改好的插件（本仓库代码）：
$ curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:18999/ssh-workspace/hosts \
    -H 'content-type: application/json' -d '{"type":"client-request","rpcId":"rpc-1","method":"hosts","payload":{}}'
401                                    <-- 通道在了（要鉴权），不再是 405
$ curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:18999/dshgw-git-diff/hello …
401

# 走完 token 交换拿到 cookie 后，用真端点（就是报错里的两个路径）：
$ curl -s -b cookies.txt -X POST http://127.0.0.1:18999/ssh-workspace/hosts \
    -H 'content-type: application/json' -d '{"type":"client-request","rpcId":"rpc-42","method":"hosts","payload":{}}'
{"type":"server-response","rpcId":"rpc-42","result":{"ok":true,"value":{"aliases":[],"entries":[],"allowList":[],
 "mountSubdir":"ssh","mountRoot":"/tmp/dshprobe/ssh","home":"/tmp/dshprobe","sshConfig":false,"identity":false}}}
$ curl -s -b cookies.txt -X POST http://127.0.0.1:18999/dshgw-git-diff/hello … 
{"type":"server-response","rpcId":"rpc-42","result":{"ok":true,"value":{"version":"0.2.0","root":"/tmp/dshprobe/work",
 "rootLabel":"工作区","anchor":"fallback","workspace":null,"git":{"available":true,"version":"git version 2.53.0"},
 …,"reason":"blank"}}}
```

**② 单测：`cmd/dshgw/plugin/lib/rpc-channel.test.mjs`**（11 项，真实 socket）：端点解析、挂载/卸载、
成功信封、失败信封、401/403、非 POST 404、415、400、`method` 不一致、异常 500、body 上限 413、
abort 传到端点信号、构造期的三种拒绝。已并入 `make dshgw-test`。

**③ 插件自身的测试**：`ssh-workspace.test.mjs`（248 断言）与 `web-tty/test/host.test.mjs`（14 项，含
长轮询 abort）改成从**真路由**发请求（fake ctx 提供 `inject` + `webServer.register` + `connection.admit`），
`workspace-files`（19 项）、`git-diff`（48 项）宿主半照旧全绿。

## 4. 实现与设计差异 / 尚未做

- 设计里没有"回退到 `connection.rpc.handle`"这一支：既然自挂的路由在**任何** dsh 版本上都成立，
  而 `rpc.handle` 在新版本上不成立，双路只会多一条没人走的分支。等哪天 dsh 修好，本模块可以整体退役。
- 探针里发现的一条细节写进了测试注释：编码过的路径穿越（`/probe/%2e%2e/secret`）在 `new URL().pathname`
  就被规范化掉了，根本到不了路由（dsh 自己的路由匹配同样如此），所以它不是插件要挡的东西。
- **待宿主执行（部署 + 重启 + 线上验收）**：见 `docs/TODO.md` M90 小节。
