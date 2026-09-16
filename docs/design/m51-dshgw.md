# M51 设计文档：dshgw —— 多租户 dsh 网关（写进本仓库、与 aigw / dsh 双向解耦）

> 状态：**实现中（M51，双租户主机 baseline 与浏览器登录修复复验已通过；其余主机验收待完成）**。本模块位于 `ai_gateway` 仓库内、与 `cmd/aigw` 平级，
> **不修改 aigw 的 Go 核心、不修改 dsh 发行包**；aigw 与 dsh 各自可独立升级。

## 1. 目标

让多名同事各自用一个浏览器使用 dsh（DeepSeek Harness 的 Web 面），并且：

1. **身份用 aigw 的 API Key**：租户粘贴自己的 Key 即登录，不需要第二套密码；用量、限速、余额、审计仍由 aigw 记账。
2. **租户之间在操作系统层隔离**：不是一个进程里的逻辑隔离，而是各自独立的 OS 用户、独立 `$DSH_HOME`、独立工作区。
3. **aigw 与 dsh 都能独立升级**：本模块只通过两边的公开外部契约集成；dsh 换版本不需要改本模块代码，aigw 重构也不要求本模块跟着改。
4. **入口不引入新域名，也不改写 dsh 的前端**：一个既有主机名 + 每租户一个端口（见 D5/D9），dsh 仍挂在端口根路径上，因此**不需要**给 dsh 注入路径垫片。

### 非目标

- 不修改 dsh 源码或发行包（全部定制走 dsh 自己的扩展点：`$DSH_HOME/cordis.patch.yml`、`settings.yaml`、进程环境变量）。
- 不修改 aigw 的 Go 核心（`internal/httpapi`、`internal/routing`、`internal/billing` 等一律不动）。
- 不做 per-tenant 容器 / user-namespace 隔离（实测否决，见 D3）。
- 不新增 DNS 记录、不签发新证书（现状：`chat.tirisen.hk` 有 A 记录，证书 SAN 为 `*.tirisen.hk`）。
- 不把 aigw 的 `/admin/ui` 暴露到公网；不实现"登录即自动建户"；不实现"worker 侧只持占位 token、由网关注入真 Key"的加固形态（记为后续可选项）。

## 2. 关键决策

### D1 用 Go 实现，而不是 Node

本模块是**独立二进制** `cmd/dshgw`：与仓库同工具链（`go 1.25`、`scripts/goenv.sh`、无 CGO），产出单个静态二进制，不引入额外运行时。dsh 自身仍由 Node 跑（`/opt/dsh`），两者互不影响。
取舍：放弃 Node 侧现成的 HTTP 生态，换来"零运行时依赖 + 与仓库构建/测试/发布流程同构"。

### D2 与 aigw 解耦：只走 HTTP + 一条可测的导入闸门

与 aigw 的全部交互只有三类：`GET /v1/models`（登录校验）、dsh 直连的 `POST /v1/responses`（本模块不代理模型流量）、可选的 `POST /mcp`（只读令牌读用量）。
**导入闸门**：`cmd/dshgw/**` 与 `internal/dshgw/**` 只可导入本仓库 `internal/dshgw/**`；导入仓库其它包（包括 aigw 内部包）时 `dshgw-test` 直接失败（见 §6）。
取舍：不复用 aigw 的 `internal/secret` 等实现（哪怕省事），换取"aigw 内部重构不会打断本模块编译"。Key 前 12 字符作为索引是 aigw 自己声明过的对外口径（`/admin/api/v1/keys/import` 的 `key_prefix` 语义），属公开契约，不算内部依赖。

### D3 一租户一进程 + 专用 OS 用户；unshare 与 Docker 均否决（含实测）

- 每租户一个 `dsh web` 进程、一个专用系统用户 `dsh-<t>`、一个 `$DSH_HOME`、一个工作区。
- **unshare 否决**：本机 `kernel.apparmor_restrict_unprivileged_userns=1`，普通用户 `unshare --user` 直接 `Operation not permitted`；`--map-users` 依赖未安装的 `newuidmap`；而 `--map-root-user` 只把当前用户映射成内层 0，**磁盘属主不变**，A 租户对 B 租户文件的可见性与宿主上同一用户完全一致——补不上 uid 边界。
- **Docker 可行但降档**：实测容器内 `dsh web` 能启动、Landlock 运行器可用（bwrap 在默认 seccomp 下不可用），但沙箱只能走 Landlock（partial 档），且要自维护含 git/python 的工具链镜像。
- 结论：**uid 边界只能靠专用系统用户**；dsh 自带沙箱与 `WorkingDirectory` 只提供"默认工作根"，不是租户边界。

### D4 会话网关代持上游 dsh cookie

dsh 的浏览器会话 cookie 由 worker 用自身密钥签发，名字与签名都绑定**请求 Host**（`dsh-auth-<base64url(sha256(authority))>`，签名载荷含 authority 并逐请求比对）。本模块把该 cookie **存在服务端**，浏览器只持有自己的 `dshgw_s_<tenant>`。
取舍：不自己去复刻 dsh 的 HMAC/Cookie 派生（那会与上游实现强耦合、易碎）；代价是本模块要维护"会话 → 上游 cookie"的映射表。
已核实：token 交换是 dsh 唯一的 cookie 签发入口（仅 `GET /?token=<唯一参数>`），**可重复换取**（进程级缓存、校验不消费），返回 `303` + `Set-Cookie`（HttpOnly / SameSite=Strict / 无 Secure，因为 dsh 跑在回环 HTTP）。

### D5 入口 = 一个既有主机名 + 每租户一个端口（不新增域名、不注入垫片）

- 门户（登录页）：`https://<host>:<portalPort>/`。
- 租户应用：`https://<host>:<tenantPort>/` —— **dsh 挂在端口根路径上**。
- 依据：dsh 的浏览器端把 API 地址定死为 `location.origin + /api/...`（`dsh-client-connection/lib/client.js:4665-4667` 的 `resolveBase()` 返回 `location.origin`，端点字面量为 `"/api"`），**不认任何路径前缀**。若改用 `/dsh/<租户>/` 路径前缀，就必须注入 JS 垫片去改写 fetch / WebSocket / EventSource / XHR / sendBeacon（本仓库外那份 `dsh-prefix-shim.js` 即为此存在），那会把 dsh 从黑盒变成"需要跟着版本维护垫片"，违背 D2 的解耦前提。
- 取舍：URL 带端口号（门户页会直接把用户跳到自己的端口），换来**零垫片**（dsh 保持黑盒）+ **真 origin 隔离**（端口属于 origin）。

### D6 跨租户隔离靠"端口即 origin" + 每租户 cookie 名

- 端口是 origin 的一部分：`https://host:32201` 与 `https://host:32202` 互不信任。租户 A 的页面向 B 的端口发请求属于**跨源**，浏览器会带上 A 的 `Origin`，本模块据此 403。
- **cookie 不区分端口**（RFC 6265）：同一主机上所有端口共享 cookie 空间。因此每个租户用**独立 cookie 名** `dshgw_s_<tenant>`，且必须 `HttpOnly`（否则任一租户页面的 JS 都能 `document.cookie` 读到别人的会话）。
- 边缘闸门：租户端口上的所有非 GET 与 WS upgrade，`Origin` 存在则必须等于 `https://<host>:<port>`，`Sec-Fetch-Site` 存在必须为 `same-origin|none`，否则 403 + 审计。（本模块为穿过 dsh 的栅栏必须清空 `Origin`/`Sec-Fetch-*`，故必须在边缘把同等判断补回来。）
- 已知残余风险（记录在案）：同一主机的 cookie 空间可被"投毒"——B 的页面 JS 可以把 `dshgw_s_alice` 覆盖掉，导致 alice 会话失效（**只是拒绝服务，不泄露**）；alice 重新登录即恢复。

### D7 Key 即身份：`prefix→租户` 映射，登录不建户

`prefix = key[:12]` 查 canonical `registry.json` 得租户（`keys.map` 仅为派生运维索引，见 §8）；Key 有效性由 aigw 判定（`GET /v1/models` 的 401/200）。
取舍：放弃"首次登录自动建户"（那要求本模块以 root 运行），换取**网关进程零提权**；建户仍是运维一条命令。身份语义明示：**持有谁的 Key 就是谁**（与 aigw 计费、与 dsh "cookie 即 bearer" 一致）。

### D8 `ProtectHome=tmpfs` ⇒ 租户家必须在 `/srv/dsh/<t>`

`systemd.exec(5)`：`ProtectHome=tmpfs` 会在 `/home`、`/root`、`/run/user` 上挂只读空 tmpfs，且其下路径无法再用 `ReadWritePaths=` 放行。因此租户 HOME 与工作区放 `/srv/dsh/<t>`，`$DSH_HOME` 放 `/var/lib/dshgw/tenants/<t>/.dsh`。
取舍：偏离 `/home/<user>` 的惯例，换来"任何 worker 都看不到任何用户家目录"（含运维账号）。

### D9 入口配置放宿主 nginx 的 include 目录，不动 nginxWebUI 的数据库

nginxWebUI 容器会按其 `sqlite.db` **重生成** `nginx.conf`，手改的 location/server 会被静默丢弃（实测：其 DB 里 `/dsh` 计数为 0，而 `nginx.conf` 里存在手改路由）。
因此：租户端口与门户端口的 server 块全部放**宿主 nginx**（`nginx 1.28.3`，由 `sudo` 管理，已有 `/etc/nginx/sites-enabled/dsh-web-8443.conf` 先例）的 `conf.d/dshgw/*.conf`，证书直接引用现成的 `*.tirisen.hk`（`/home/winger/nginxwebui/.acme.sh/*.tirisen.hk/`，宿主 root 可读）。
**可选**：若希望保留既有习惯 URL `https://chat.tirisen.hk/dsh/`，则在 nginxWebUI 里**正规注册**一个 location（让 DB 拥有它，而不是手改文件）指向宿主 8443 的既有桥接（沿用现网 `container → host:8443 → 127.0.0.1` 模式）。
取舍：多一套宿主 nginx 配置要管（已有先例与部署脚本），换取"控制台点保存不会打掉租户入口"。

### D10 会话网关绑回环

`dshgw serve` 监听 `127.0.0.1:3099`：宿主 nginx 直接回环访问；若启用了 D9 的可选容器路由，走既有的 `容器 → 宿主:8443 → 127.0.0.1:3099` 桥接。网关因此**不需要**监听 `0.0.0.0`、也不需要依赖 docker0 地址。

### D11 资源预算与端口分配

实测空闲 worker ≈ **304 MB RSS / 11 线程**、磁盘 ≈ **193 MB/租户**（多为 sessions）。故：单实例 `MemoryHigh=1.5G` / `MemoryMax=2G`，`Slice=dsh-workers.slice` 设 `MemoryMax=40G` 兜总账（30×4G=120G > 本机 61G 会 OOM）。
端口**两段且互不重叠**：worker 回环段 `127.0.0.1:32100–32299`，公开 TLS 段 `0.0.0.0:32600–32799`（由宿主 nginx 监听）；registry 为每个租户同时分配一对并记录。

### D12 默认不持有 aigw 管理凭据

默认只用租户自己的 Key 调 `/v1/models`，不需要任何管理凭据。`dshgw usage` 可选地使用 `aigw_mcp_` **只读**令牌（`admin_read`）读用量；**不**默认持有可签 Key 的 admin 凭据。
取舍：自动化程度略低（签发 Key 仍在控制台点），换来"本模块被攻破也拿不到 aigw 管理权"。

### D13 目录选择器：只让租户看到/选到自己的工作目录

**事实现状（已核实）**：dsh 的目录选择器是插拔接缝 —— `ctx.directoryPicker.capability()` 返回 `{kind:'native', pick}`（宿主显示器的 OS 对话框）或 `{kind:'browse', list(path?), createDirectory(path,name)}`（口内浏览器）。web profile 挂的行是 `directory-picker-auto`（按"回环绑定 + 非 SSH + 有可用显示会话"在开机时二选一）。**browse 后端没有任何 root/白名单配置**（seam 文档明确写着"whole-filesystem scope，没有 per-deployment browse-root 限制；就算加了也是 UX 约束而不是安全边界"），`list()` 的 `crumbs` 会一路走到文件系统根 `/`，因此租户理论上能点着往上走。

**四层处理**：

1. **真实边界（安全，靠 D3 的 uid）**：其他租户的目录是 `0700`，`ProtectHome=tmpfs` 让 `/home` 整片不可见 ⇒ 即使选择器把路径列出来也读不进去。这是安全边界，不是 UX。
2. **默认体验（零插件，本里程碑实现）**：
   - 单元里 `Environment=HOME=/srv/dsh/%i` ⇒ `list()` 不带路径时返回的就是租户自己的工作目录，选择器一打开就停在那里（browse 后端用 `node:os.homedir()`）。
   - 预注册工作区（渲染 `$DSH_HOME/storages/workspace.json`）⇒ 租户通常根本不需要选目录。
   - **钉死后端**：`directory-picker-auto` 在带显示会话 + 有 zenity 的机器上会挂 native（弹宿主显示器的对话框）。其判定已核实（`dsh-host-directory-picker-auto/lib/index.js:64-68`）：`bindHost !== '127.0.0.1'` → browse；SSH 启动 → browse；darwin/win32 → native；linux 需 `DISPLAY`/`WAYLAND_DISPLAY` **且** PATH 里有 zenity/kdialog 才 native，否则 browse。租户单元（系统用户、无 DISPLAY）大概率会自然落到 browse，但**不要依赖启发式**——在租户 patch 层显式接管（`clamp` 档插我们自己的夹紧实现，`browse` 档插官方两半）：
     ```yaml
     # $DSH_HOME/cordis.patch.yml（clamp 档）
     - id: directory-picker
       name: '@deepseek-ai/dsh-host-directory-picker-auto'   # 守卫：名字不符会被跳过并告警
       disabled: true
     - insert:
         # 必须**两半都在**：auto 行原本同时挂 host 能力与 browser 半，
         # 只插 host 半会让 UI 找不到目录流槽位（"添加工作区"点了没反应）。
         - id: picker-clamp
           name: 'file:///opt/dshgw/share/dsh-plugin/picker-clamp.js'   # 我们的夹紧 host 半（root 拥有）
         - id: picker-clamp-ui
           name: '@deepseek-ai/dsh-client-ui-directory-picker-browse'   # 官方 browser 半
     ```
     `browse` 档（仅调试用，不夹紧）把第一行换成 `@deepseek-ai/dsh-host-directory-picker-browse`。
     （patch 语义已核实：`name` 是**守卫**而非覆盖，所以"换后端"只能 disable 旧行 + insert 新行，不能改一行的 name。）
     **第三方先例**：`gfds2005/dsh-remote-dir-picker` 做的正是"disable auto → insert browse 两半"，其注释也印证了动机：反代之后 bind 仍是回环，resolver 会判成 native，于是远端浏览器永远等不到对话框、"添加工作区"看起来是坏的。**我们不复用该第三方插件**，而是用官方包自己写这几行——少一个供应链面与维护负担。
3. **硬夹紧（默认档，把"只看到自己目录"落到字面）**——`directory_picker: clamp | browse`（**默认 `clamp`**；不设 `off`：选择器必须可用，见下方取舍）：
   - **机制（已核实可行）**：自己写一个约 120 行的 dsh 插件，`extends DirectoryPicker`（接缝包）并注册 `ctx.directoryPicker`，返回 `{kind:'browse', list, createDirectory}`，把路径夹到 `/srv/dsh/<t>`。**原版浏览器 UI 无需改动**——wire 侧控制器只做 `requireCapability('browse', …)`（`kind` 必须等于 `'browse'`，否则 `directory-picker/unavailable`），随后直接调用 `capability.list(path, signal)` / `capability.createDirectory(path, name)`（`dsh-api-workspace-controller/lib/index.js:452-470`）。任何 browse 实现都能驱动官方 Miller 列对话框。
   - **不改 dsh、不装第三方**：用官方接缝包 + 官方 UI 半，只替换"宿主侧能力实现"。第三方 `gfds2005/dsh-remote-dir-picker` 只做"钉死 browse"（不夹紧），我们不依赖它。
   - **装载方式（已验证）**：插件文件放 `/opt/dshgw/share/dsh-plugin/picker-clamp.js`（root 拥有，租户不可改），租户 patch 用 `file://` URL 引用——loader 对非 `./`/`../` 开头的说明符直接交给 `import()`（`cordis-plugin-loader/lib/index.js:275-281`），而 `import('file:///…')` 实测可用；其 `__rewriteRelativeImportExtension` 只处理 `./`/`../` + TS 后缀，不会动 file URL。插件内部按裸包名 `import '@deepseek-ai/dsh-host-directory-picker'` 也能解析（profile 目录可解析到，实测）。
   - **夹紧语义**：`list(undefined)` → 租户根，`crumbs` 从租户根起（不暴露 `/`）；根外路径 → `DirectoryPickerError('directory-unreadable', p)`；`createDirectory` 的父目录必须落在根内；返回行形状与官方一致（`{name, path, hidden}`），保留 `maxEntries`/`truncated`。
   - **必须处理 symlink 逃逸（真细节）**：官方后端**跟随目录符号链接**，所以租户在自己目录里造一个指向 `/etc` 的软链就能绕过前缀检查 ⇒ 夹紧必须对 `fs.realpath()` 解析后的真实路径做根检查，而不是只比字符串前缀。
   - **失败姿态**：插件构造期做自检；若接缝 API 漂移导致无法注册，降级为"一律拒绝的 picker"（并记日志），**绝不放回官方未夹紧的实现**。契约测试在每次 dsh 升级后验证夹紧行为。
   - **诚实定位**：夹紧是**人体工学/防脚枪**，不是安全边界。租户可以改自己家目录里的 `$DSH_HOME/cordis.patch.yml`（该 patch 层是 live 重载）把夹紧摘掉——真正的边界始终是 D3 的 uid（摘掉夹紧也读不到别人的东西）。
   - **为什么不用 `off`**：关掉选择器会让"打开一个已有目录"这类正常操作直接消失（UI 只是隐藏入口），把可用性换成一点范围收敛，不值；夹紧能同时保住两者。

**为什么值得做**：工作区就是 dsh 沙箱的写根（`sandbox-policy.workspaceRoot = session.header.cwd ?? process.cwd()`）。把 `/` 或 `/etc` 选成工作区，agent 会把整个文件系统当自己的地盘去写，然后撞上大量 OS 权限拒绝——夹紧能挡掉这个脚枪。

**必须说清的一点（容易被"remote"这个词误导）**：钉死 browse **不是**让租户选择"浏览器本机的目录"，而是让租户**在浏览器里浏览 dsh 宿主的文件系统**——browse 后端走 `node:fs` 列的是**服务端**（本部署即 rag-server）的目录，浏览器只是遥控器。反过来说，"选浏览器本机目录"在这个模型里**做不到**：① 浏览器的 File System Access API（`showDirectoryPicker()`）出于隐私**不返回绝对路径**，`<input webkitdirectory>` 也只给相对文件名，而 dsh 的工作区必须是**服务端绝对路径**（`workspace.create` 要求 fully-qualified POSIX 绝对路径，且该路径直接成为沙箱写根与 agent cwd）；② 即便拿到了路径字符串，dsh 宿主也打不开浏览器那台机器的目录。浏览器本机的目录只能以**上传文件/附件**的形式进入 dsh，不能当工作区。

**诚实边界**：即使做了 `clamp`，租户的 **agent** 仍然读得到全世界可读的系统文件（`/usr`、`/etc` 等），因为它是普通进程；夹紧管的是"GUI 选目录"，不是"隐藏操作系统"。`PrivateTmp=yes` 额外让 `/tmp` 只显示该租户自己的私有 tmpfs。

### D14 可选插件 `dsh-browser-fs`：让 agent 读写**租户自己那台电脑**上的目录

**它是什么（已核实 `dsh-browser-fs@0.2.0`，MIT，第三方作者 whitefirer）**：一个**双面插件**。host 半 `inject = ["webServer","tools"]`，注册 3 个工具 `browser_fs_list` / `browser_fs_read` / `browser_fs_write`，并用 `ctx.effect(() => ctx.webServer.registerUpgrade({...}))` 开一条 WS 中继（默认 `wsPath: '/browser-fs/ws'`，可配）；client 半用浏览器 **File System Access API**（`showDirectoryPicker`，句柄存 IndexedDB）在**租户自己的机器**上执行 list/read/write，浏览器强制"只能访问用户显式授权的那个目录"；非安全上下文自动降级为 `webkitdirectory` 只读快照。

**它不是什么（必须说清，否则会与 D13 混淆）**：**它不是工作区选择器，也不改变工作区**。工作区、会话 cwd、沙箱根、`bash` 与 `fs` 工具仍然在**宿主**上（见 D3/D13）。browser_fs 那三个工具访问的是**租户本机**（浏览器所在机器）的授权目录——这是"agent 多一条访问租户本机文件的通道"，不是"工作区搬到浏览器上"。因此 **D13 的 clamp 照做**，两者互补：D13 管"宿主侧工作区别乱选"，D14 管"顺带能碰我自己电脑上的文件"。

**兼容性（已实测，针对本机 dsh 0.1.2-rc.1）**
- client 半构建产物是 `window.__ModuleLoader__.load({id, factory})` 包装，运行时只 `require` 平台模块 `react` / `react-dom` / `react/jsx-runtime`；**全文零处引用 `@deepseek-ai/dsh-client-ui-slots`**——尽管 manifest 的 `dsh.client.inject` 里写了它，而该包在本安装闭包里不存在（`MODULE_NOT_FOUND`，属构建期类型残留）⇒ **不需要额外补包**。
- host 半只 inject `webServer` / `tools`，都是本版已有的服务；工具名与 WS 路径已从构建产物确认。

**安全（这部分必须由我们补，不能指望插件）**
- 插件的同源检查实测为：
  ```js
  function isSameOrigin(req) { const origin = req.headers.origin
    if (typeof origin !== "string") return true          // 无 Origin ⇒ 放行
    return new URL(origin).host === host }
  ```
  而我们的网关为了穿过 dsh 栅栏**会清空 `Origin` 并把 `Host` 改写成 `127.0.0.1:<workerPort>`** ⇒ 该检查在我们的链路上**恒为真**。
- 同时 `registerUpgrade` 注册的路由**不走 dsh 的 `/api` Host/Origin 栅栏**（插件作者也明确记录了这点）。
- 结论：**我们的边缘 origin 闸门是这条 WS 通道的唯一防线**，且必须覆盖 upgrade 请求（§3.5 第 8 条）。契约测试要断言：跨源 upgrade 经网关 **403**、同源 upgrade **101**、普通 GET 打 `/browser-fs/ws` 得到 **426**。

**供给方式（保住"新 home 启动不需要网络"）**：provisioner 在**模板 home** 里执行一次 `dsh plugin --profile web add dsh-browser-fs@0.2.0`（固定版本），校验后把 profile 目录连同其依赖作为模板复制给新租户；插件升级时重跑模板并滚动更新各租户。每租户开关 `plugin_browser_fs: on|off`（**默认 on**）。

**策略（已定：默认开）**：启用后，agent 能读取租户本机已授权目录的内容并送入模型（aigw 侧的录制开关可约束内容留存），这是**数据出境**决定。部署方已明确选择**默认开启**，因此它列为默认能力；需要收敛时由管理员按租户把 `plugin_browser_fs` 置为 `off`。文档与门户需如实提示"agent 可读取你授权的本机目录，内容会进入模型请求"。

**已知限制**：浏览器标签页必须开着，工具才可执行（架构使然）；多设备/多标签同时在线时由第一个持句柄的浏览器执行；完整模式仅 Chromium 系；需要安全上下文（我们走 HTTPS，满足）；dsh 处于 rc 阶段，`defineTool` / `registerUpgrade` 可能漂移（作者自述），故纳入 dsh 契约测试。

## 3. 接口

### 3.1 `internal/dshgw/config`

```go
type Config struct {
	PublicHost    string // 既有主机名，如 "chat.tirisen.hk"（不新增 DNS）
	PortalPort    int    // 门户（登录页）端口，如 32600
	TenantPortLo  int    // 公开 TLS 段下限，如 32601（宿主 nginx）
	TenantPortHi  int    // 公开 TLS 段上限，如 32799
	WorkerPortLo  int    // worker 回环段下限，如 32100
	WorkerPortHi  int    // worker 回环段上限，如 32299
	Listen        string // "127.0.0.1:3099"
	AigwBaseURL   string // "http://192.168.190.86:8088"
	ValidateTimeout time.Duration // 5s
	SessionTTL    time.Duration // 7d（滑动续期）
	KeyRevalidate string // "off" | "per-request" | "interval:<sec>"
	LoginRate     RateLimit // 默认 10/min/IP；IP 取 nginx 覆写的 X-Real-IP
	DirectoryPicker string // "clamp"（默认：夹到租户根，选择器仍可用）| "browse"（官方实现，不夹紧，仅调试）
	WorkspaceSeed []string  // 预注册的工作区（相对 TenantRoot 的子目录），如 ["work"]
	ReservedNames []string // login|dshgw（端口方案下仅防混淆，不再有 vhost 撞名）
	Dsh           DshRuntime // NodeBin / BinJS / ReleasesRoot / CurrentLink
	TenantRoot    string // "/var/lib/dshgw/tenants"
	WorkspaceRoot string // "/srv/dsh"
	HandshakeDir  string // "/var/lib/dshgw/handshake"
	StateDir      string // "/var/lib/dshgw"
}
func Load(path string) (*Config, error) // yaml.v3，顶层严格解码（未知键报错）
func (c *Config) TenantOrigin(tenant string) string           // "https://host:port"
func (c *Config) TenantFromPort(port int) (string, bool)      // 由 registry 查表
func (c *Config) SessionCookieName(tenant string) string      // "dshgw_s_<tenant>"（cookie 不分端口，故按租户命名）
func (c *Config) WithTrailingSlash(u string) string           // 门户跳转用，保证相对路径解析正确
```

### 3.2 `internal/dshgw/registry`

```go
type Tenant struct {
	Name       string
	UID        int
	PublicPort int    // 公开 TLS 端口（宿主 nginx）
	WorkerPort int    // worker 回环端口
	KeyPrefix  string
	DshHome, Workspace     string
	CreatedAt, LastLoginAt time.Time
	Handshake              HandshakeState // ok | pending | failed
}
func LoadRegistry(path string) (*Registry, error)        // /etc/dshgw/registry.json，0600
func LoadKeyMap(path string) (map[string]string, error)  // prefix -> tenant，0640 root:dshgw
func (r *Registry) AssignPorts(cfg *Config, taken func(int) bool) (public, worker int, err error)
func (r *Registry) ByPrefix(prefix string) (Tenant, bool)
func (r *Registry) ByPublicPort(port int) (Tenant, bool)
func (r *Registry) Save() error                          // 原子替换 + flock
```

### 3.3 `internal/dshgw/session`

```go
type Upstream struct{ Name, Value, Authority string; ExpiresAt time.Time }
type Session struct {
	Tenant    string
	ExpiresAt time.Time
	Upstream  *Upstream
}
type Store interface {
	Issue(tenant string, ttl time.Duration) (token string, err error) // 只存 sha256(token)
	Get(token string) (*Session, error)
	Touch(token string, ttl time.Duration) error
	Delete(token string) error
	SetUpstream(token string, u *Upstream) error
	ClearUpstream(token string) error
	DeleteTenant(tenant string) error
}
// 持久化：/var/lib/dshgw/gateway/sessions.json，单写者 + 原子 rename
// 说明：无 handoff 票据——门户与租户端口同主机，cookie 主机级作用域，
//      门户的登录响应即可直接下发该租户的 cookie。
```

### 3.4 `internal/dshgw/handshake`

```go
type Source interface{ TokenURL(tenant string) (string, error) } // handshake/<t>.url（0640 root:dshgw）
type Exchanger interface {
	Exchange(ctx context.Context, tokenURL, authority string) (*Upstream, error)
}
// 约束：GET 且 pathname=="/" 且恰好一个 token 参数；redirect 必须 manual（303 + Set-Cookie）；
// authority 由调用方固定为 127.0.0.1:<workerPort>，与后续所有上游请求一致。
```

### 3.5 `internal/dshgw/proxy`

```go
type Proxy struct{ /* cfg, registry, sessions, handshake, aigw, clock, logger */ }
func (p *Proxy) TenantHandler(t registry.Tenant) http.Handler // 监听在本租户的公开端口上（由外部按端口分派）
func (p *Proxy) PortalHandler() http.Handler                  // 门户：登录表单 + POST 校验 + 跳转
func (p *Proxy) LogoutHandler() http.Handler
func (p *Proxy) Dispatch() http.Handler // 按 Host 端口分派到门户 / 各租户；未知端口 404
// 不变量（有测试逐条断言）：
//  1. 剥离浏览器 Cookie；2. 上游 Set-Cookie 一律不透传，只写回服务端会话
//  3. 固定 Host: 127.0.0.1:<workerPort>；4. 清空 Origin / Sec-Fetch-*
//  5. 上游 401 → 清上游 cookie 并重握手一次，仍失败才 401
//  6. 规范化 pathname，拒绝 `.`/`..` 段与 absolute-form；**其余路径一律转发**（不做路径白名单）
//  7. 非 GET/WS 先过边缘 origin 闸门（Origin == https://<host>:<port>）
//  8. WS upgrade 单独走 upgrade 通道，不缓冲；**upgrade 也必须过第 7 条闸门**
```

> **为什么没有路径白名单（D14 的直接后果）**：dsh 的插件可以用 `webServer.registerUpgrade` 注册**任意** upgrade 路由（例如 `dsh-browser-fs` 的 `/browser-fs/ws`），静态面还包含 `/plugins/<pkg>/client.js`。任何写死的白名单都会在装/换插件时失效，且与 D2 的"dsh 可独立升级"冲突。所以策略改为"规范化 + 拒绝遍历与 absolute-form + 其余转发"，未知路径交给 dsh 自己 404；**边界由会话校验 + 边缘 origin 闸门承担，而不是由路径清单承担**。

### 3.6 `internal/dshgw/tenancy`（隔离层，唯一与 OS 打交道的包）

```go
type Isolator interface {
	Create(ctx context.Context, t registry.Tenant, key string) error // useradd → 家目录 → DSH_HOME → unit → start → 401 探针 → enable
	Remove(ctx context.Context, t registry.Tenant, purge bool) error // stop → disable → 快照 → userdel -r → 清理映射
	Restart(ctx context.Context, t registry.Tenant) error
	Status(ctx context.Context, t registry.Tenant) (UnitStatus, error)
	Enable(ctx context.Context, t registry.Tenant, on bool) error
}
type Systemd struct{ /* unit 模板渲染、systemctl 调用、journal 读取 */ }
// 渲染产物：/etc/systemd/system/dsh-worker@<t>.service、/etc/dshgw/tenants/<t>/tenant.env
// 与 dsh 状态：settings.yaml（含 aigw provider 与模型清单）、.credentials.yaml（仅 version/refs/records 三键，0600）
// 另渲染宿主 nginx 片段：/etc/nginx/conf.d/dshgw/<t>.conf（公开端口 → 127.0.0.1:3099）
```

### 3.7 `internal/dshgw/aigw`（唯一的对外集成包）

```go
type Client struct{ BaseURL string; HTTP *http.Client }
var ErrInvalidKey = errors.New("dshgw: invalid api key")

// ValidateKey 走 GET /v1/models：401 → ErrInvalidKey；200 → 返回模型 id 列表（可能为空，见 §5）
func (c *Client) ValidateKey(ctx context.Context, key string) ([]string, error)

type UsageClient interface { // 可选：aigw_mcp_ 只读令牌（admin_read）
	Usage(ctx context.Context, keyPrefix string, days int) (Usage, error)
}
```

### 3.8 `internal/dshgw/contract`

```go
type Check struct{ Name string; Run func(context.Context) error }
func DshContract(rt DshRuntime) []Check           // 7 条（§6）
func AigwContract(baseURL, key string) []Check    // /v1/models 语义、Bearer 与 x-api-key 等价、空清单语义
```

### 3.9 `cmd/dshgw`

```
dshgw serve                                   # 门户 + 各租户端口的会话/路由网关（systemd: dshgw.service）
dshgw tenant create [--allow-empty-models] --key-file /root/key-file <t>
dshgw tenant list | restart <t> | rotate-key --key-file /root/key-file <t> | remove [--purge --yes] <t>
dshgw bind <keyPrefix> <t>                    # keys.map
dshgw login-url [<keyPrefix>]                # 门户地址 / 显式 prefix 对应租户入口
dshgw sync-models <t>                         # 重拉 /v1/models 对齐 settings.yaml
dshgw revalidate                              # 按 KeyRevalidate 模式批量重验
dshgw capture-url <t>                         # ExecStartPost=+ 调用：读启动行写 handshake/<t>.url
dshgw contract [--json] [--key-file PATH] [all|dsh|aigw]
dshgw doctor
dshgw backup
dshgw render-nginx [--reload]                      # 渲染/校验宿主 nginx 片段（nginx -t 通过才落盘）
```
全局 `flock <registry目录>/lifecycle.lock`（capture-url 不争用生命周期锁，避免 ExecStartPost 死锁）；`tenant`/`doctor`/`backup`/`render-nginx` 需 root；`serve` 以 `dshgw` 用户运行。

### 3.10 Makefile 与脚本

```make
dshgw-build:   # 与 build 同工具链，产出 bin/dshgw
dshgw-test:    # go test ./internal/dshgw/... + 导入闸门测试
dshgw-verify:  # vet + test + build + scripts/verify-dshgw.sh
```
`dshgw-verify` **不并入** `verify`（避免拖慢日常）；发布流程中单独跑。

## 4. 数据流

**登录**：`GET https://<host>:<portalPort>/` → 表单 → `POST /login`（限流取 `X-Real-IP` → aigw `GET /v1/models` → `prefix→tenant`）→ 302 → `https://<host>:<tenantPort>/`，同时下发该租户的 `dshgw_s_<tenant>`（HttpOnly + Secure + SameSite=Lax，7 天滑动；cookie 主机级作用域，因此门户下发即可被租户端口携带）。

**首次握手**：租户端口收到请求且服务端无上游 cookie → 读 `handshake/<t>.url`（由 `capture-url` 在 worker 启动后写入）→ 本地 `GET http://127.0.0.1:<workerPort>/?token=…`（manual redirect，`Host: 127.0.0.1:<workerPort>`）→ 存 `Set-Cookie` → 继续代理。上游 401 时丢弃并重试一次（token 可重复换取，无需状态机）。

**请求代理**：`dshgw_s_<tenant>` → 会话（校验租户与端口匹配 + 未过期）→ 边缘 origin 闸门（非 GET/WS）→ 改写（剥 Cookie / 固定 Host / 清 Origin+Sec-Fetch-*）→ 上游 `127.0.0.1:<workerPort>` → 回程吞 `Set-Cookie`。

**Key 轮换**：`tenant rotate-key` → 原子改 `.credentials.yaml`（仅 `version`/`refs`/`records`）→ 更新 `gateway.key`（仅重验模式）与 `keys.map` → dsh 凭据热载生效（无需重启）。

**升级**：`dshgw upgrade-dsh <ver>` → 铺 `/opt/dsh/releases/<ver>` → 切 `current` → 跑 7 条契约 → 通过才逐租户 restart，否则回滚软链。

## 5. 异常与边界

| 场景 | 行为 |
|---|---|
| Key 无效/停用 | aigw 401 → 统一话术"Key 无效/已停用"（与"缺失"同话术，防枚举） |
| Key 有效但模型清单为空 | **不是**认证失败：属"无模型授权"或"候选供应商全不可用"。`tenant create` 默认失败并说明两种可能（`--allow-empty-models` 覆盖）；登录后门户给提示；doctor 列为检查项 |
| Key 未映射到租户 | 拒绝登录并打印运维指引（不自动建户） |
| prefix 已绑到别的租户 | 拒绝（防同一 Key 两实例双开）；轮换期内新旧前缀并存合法 |
| 未知端口/门户端口被当作租户访问 | 404（`Dispatch` 按端口查 registry） |
| 跨租户请求（A 端口页面向 B 端口发请求） | 边缘 origin 闸门 403 + 审计（端口不同即跨源） |
| cookie 被同主机其它租户页面覆盖 | 仅导致该租户会话失效（拒绝服务），重新登录即恢复 |
| aigw 不可达/超时 | 登录 **503 fail closed**；运行期由 dsh 自己报错；doctor 主动探测 |
| 余额耗尽（aigw 402） | 模型调用失败；doctor 提示充值；登录是否同拒由 `KeyRevalidate` 决定 |
| 上游 cookie 失效（凭据文件被换） | 上游 401 → 重握手一次 → 仍失败踢回门户 |
| handshake 文件缺失/过期 | 该租户标记 `failed`，doctor 给 `journalctl -u dsh-worker@<t>` 指引；不影响其他租户 |
| worker 崩溃 | systemd `Restart=always` + `StartLimitBurst=5`；doctor 显示重启计数 |
| 端口被占/段耗尽 | registry ∪ `ss -ltn` 双查；公开段由宿主 nginx 独占，worker 段只用回环；失败回滚已建用户/unit/nginx 片段 |
| 防火墙未放通公开端口段 | doctor 检查端口监听与可达性；runbook 写明需放通 32600–32799（可限制到内网网段） |
| 绝对形式请求行 | 拒绝，只用 `URL.pathname`；规范化后拒绝 `.`/`..` 段 |
| 磁盘/内存 | 193 MB/租户基线 + journal/handshake 轮转；slice 40G 兜总账 |

## 6. 测试策略

1. **单元测试**（`go test ./internal/dshgw/...`）：配置严格解码；`Dispatch` 的端口分派（门户/租户/未知端口）；registry 双端口分配与原子写；session 发行/校验/过期/滑动；**cookie 名按租户区分**且 HttpOnly；`proxy` 的 **8 条不变量逐条断言**（httptest 假上游）；边缘 origin 闸门对"同主机不同端口"拒绝；handshake 的 303+Set-Cookie 解析与 `redirect: manual`；限流桶取 `X-Real-IP`；absolute-form 拒绝。
2. **导入闸门测试**：扫 `cmd/dshgw/**`、`internal/dshgw/**` 的 import，依赖本仓库 `internal/dshgw/**` 之外的包即失败。
3. **aigw 契约测试**（httptest stub + 真实 aigw）：401/200/空清单/超时四类分支；`Bearer` 与 `x-api-key` 等价。
4. **dsh 契约测试**（7 条）：`web --port <n> --no-open` 可启动且 `--host 0.0.0.0` 被拒；启动行可解析；token 交换 303+Set-Cookie；栅栏三态；`$DSH_HOME` 首启自举；凭据三键 + owner-only + 热载；`SANDBOX_UNAVAILABLE` fail-closed。
5. **端到端**（`scripts/verify-dshgw.sh`，需 root、一次性租户）：建户 → 端口探针 → 门户登录 → 模型清单一致 → **跨租户读取被 OS 拒绝** → **跨端口请求 403** → 停 Key 后模型失败 → 浏览器侧不出现 dsh cookie → 清理。
6. **须由宿主机 shell 复核的三项**（本会话环境无法自证）：宿主 nginx 在公开端口段上的 TLS 与 `nginx -t`；handshake 文件实际模式；10–30 worker 并发下的内存与 cgroup 命中。

## 7. 依赖

- 外部：Go 1.25 标准库 + `gopkg.in/yaml.v3`（已在仓库依赖内）。**不新增第三方依赖**。
- 运行时：dsh 发行包（`/opt/dsh`，未修改）、Node 22（跑 dsh）、systemd、aigw HTTP 面、**宿主 nginx**（公开端口 TLS，证书复用现有 `*.tirisen.hk`）。
- **不依赖**：新 DNS 记录、新证书、nginxWebUI 的数据库改动（可选）、docker0 地址。
- 契约依赖（由契约测试锁定）：`GET /v1/models` 的 401/200 语义与 `key_prefix = 明文前 12 字符` 口径；dsh 的 CLI 参数、启动行格式、token 交换与 cookie authority 绑定、`$DSH_HOME` 布局与首启自举、凭据文件三键与权限要求。

## 8. 实现与设计差异

以下回填对应当前实现；不改变 D1–D14 的独立交付、UID 隔离、端口 origin、零 shim 与默认插件方向。

| 项目 | 实现及理由 |
|---|---|
| 导入闸门 | 允许 `internal/dshgw/**` 自身依赖，禁止仓库其它包；修正初稿字面上连自身依赖也禁止的矛盾 |
| 授权状态 | `registry.json` 为 canonical 真相，`keys.map` 是派生索引；两个文件各自原子替换，崩溃后的派生索引可在下次 Save 修复。登录不会让 serve 写 root 管理的 provisioning 状态 |
| 登录时间 | 独立 `gateway/activity.json`（0600、跨进程锁）；避免 gateway 登录与 root 生命周期 CLI 写 registry 相互覆盖 |
| 会话并发 | FileStore 使用 flock + reload/merge，并检查原子替换文件的 inode；只存 browser token hash。upstream generation 清除后仍单调递增，清除与迟到响应更新都使用 CAS；握手使用固定大小锁条带，不永久积累 token 锁 |
| 边缘校验 | nginx 覆写固定 Host 与 `X-DSHGW-Port`，网关核对。unsafe/WS 缺 Origin 也拒绝；仅放行必要的顶层同站导航，无 Origin 的跨站子资源仍拒绝 |
| 请求/响应 | 保留合法 Path/RawPath/query，不用 path.Clean；拒绝 token query、CONNECT、OPTIONS *、多层编码遍历。worker Cookie、Set-Cookie/Set-Cookie2（含 101/trailers）隔离；只重写安全的 loopback Location。第二次 401 原样返回，不再承诺自动踢回门户 |
| cookie 与容量 | 默认 header 上限 128 KiB；nginx 大头缓冲 4×32k。会话上限 10,000、文件 64 MiB、登录限流桶有界；请求回放体上限 64 MiB。nginx 不隐藏 gateway 自身的 Set-Cookie |
| 密钥与 CLI | Key 只经 stdin 或绝对 mode-0600 文件读取，不进入 argv。选项在位置参数前；login-url 接受可选 prefix，无参数返回门户地址。bind 为显式 alias，Key 的显示名不参与派生；usage/MCP 管理能力未实现 |
| 文件与锁 | root 文件读写以 openat + O_NOFOLLOW 逐级打开父目录，固定目录 fd 后 renameat；拒绝叶/祖先 symlink 和 FIFO，读操作在同一 fd 上限流。credentials/settings 使用 DSH 的 O_EXCL `.lock` 协议，轮换时持有两把锁直到事务或回滚完成 |
| 空模型 | 有效但无模型时默认拒绝建户；显式允许则标记 models_pending，不生成空 aigw provider。sync/rotation 保留其它 provider 与 credentials records，并修正失效的 aigw 默认模型 |
| DSH 工具兼容 | 生成 `compat.supportsStrictMode: true`，让普通 Responses 函数工具明确发送 strict:false，保留可选字段。已在本会话由用户修正同一配置后恢复执行工具；不变更沙箱/审批策略 |
| picker 接缝 | patch 位于 `profiles/web/cordis.patch.yml`，同时插 host/UI 两半。插件以 createRequire(DSHGW_DSH_ANCHOR) 解析实际 DSH 接缝与 Schemastery，而不是假定裸 import 能从插件安装位置解析 |
| browser-fs 静态面 | 本版 DSH 实际发布的是 HTML 广告的 `/plugins/??...dsh-browser-fs/client.js` 批量 URL，而非初稿假定的独立路径。验证器跟随公开页面中真实资源 URL，不改写 dsh |
| 模板 | 固定 0.2.0 及 SHA-512 integrity；供应阶段需要 pnpm/Corepack 和包源。验证器复制到第二个 fresh HOME，并放置失败 pnpm wrapper 验证首启不安装；这不是网络 namespace 隔离证明 |
| 安全建户 | 在 useradd 前拒绝既有路径、保留目录、symlink 和旧 handshake；仅清理本事务独占建立的目录。停 worker/删用户失败会保留数据并报告，不盲目 RemoveAll。外部占端口仍可能 TOCTOU，失败安全回滚后由操作者重试 |
| 生命周期与资源 | systemd 状态读取失败不当成 inactive；恢复在变更前登记，取消后以限时独立 context 恢复并合并错误。TasksMax 实际为 512；单 worker 1536M/2G 与汇总 40G 未改 |
| nginx | 额外维护 `/etc/nginx/conf.d/dshgw.conf` include shim；候选写入后 nginx -t，再按需 reload，失败恢复文件并报告恢复失败。migrate-nginx 是 reconcile 别名，不写 nginxWebUI 数据库 |
| 升级 | 先备份/验证候选，再切 current，比“先切再测”更安全；仅重启原先 active 的租户。回滚先恢复链接，再恢复尝试过的 worker；链接恢复失败就不在候选上假装回滚。安装器重跑不会覆盖已选择的独立 DSH/Node 版本 |
| 备份与删除 | 全局备份先停 gateway/活跃 workers；snapshot 唯一命名、不覆盖旧归档，含 manifest v1、源路径映射和租户记录。只允许预检时缺失的可选根被跳过；已知租户目录缺失或遍历失败拒绝 purge。userdel 一旦尝试视为不可逆边界，失败保留数据/快照且不恢复可能已删除身份的路由 |
| 运维探针边界 | doctor 是文件/属主/模板/单元/nginx 检查，不声称能证明公网防火墙、模型额度或真实资源限额命中。Key 用 revalidate/contract 验证，主机端到端另列未完成验收 |

已知信任限制：tenant agent 能读自己的 Key；root/dshgw 可冒充租户；同 hostname 的所有服务与日志必须可信；Cookie 投毒仍可 DoS；worker 共享网络 namespace；picker 的 Node TOCTOU/bind mount 不是安全隔离；已升级 WebSocket 不因 logout/TTL 立即关闭。

## 9. 验收记录（实测）

当前自动化证据（实现阶段，尚未提交）：

- `go test -count=1 ./...` 与 `go vet ./...` 的最终全仓库回归：通过。
- `go test -count=5 -shuffle=on ./internal/dshgw/tenancy ./internal/dshgw/upgrade` 与对应 vet：通过，覆盖保留数据、部分 systemd 变更、状态失败、取消恢复、partial registry Save、快照唯一性与缺失数据、升级 inactive/未尝试 worker 与回滚失败。
- 代理/安全状态回归通过：真实 raw-TCP 101/echo、跨端口拒绝、重复 cookie、32 请求并发 401 刷新、最终 401、cookie trailers、迟到 cookie CAS、合法 `%25` 路径保留、会话文件删除与同时间戳替换、unsafe/logout Origin、祖先 symlink/FIFO 拒绝。
- disposable DSH 7 项基础契约、真实 picker 15 项断言：通过；另已加入并通过第 8 项 `credentials-live-hotload`。Node 使用 `/home/winger/.local/node-v22.23.1-linux-x64`，DSH 使用 `/home/winger/.local/dsh-0.1.2-rc.1`，未修改发行包。
- 临时 browser-fs 0.2.0 模板：供应成功；复制后的 fresh HOME 不调用 pnpm；页面与实际广告的插件 client URL 200；普通 WS 路径 GET 426；有效 upgrade 101。
- `make dshgw-verify` 的最终组合（最新二进制、unit 校验、模板与全部契约）：通过。`file bin/dshgw` 确认 x86-64 静态 ELF；后续 diff-check 发现的文档 EOF 空行已随清单归档修正。
- 真实 aigw `http://192.168.190.86:8088/v1/models` 无凭据认证栅栏：CLI contract 通过（401）；没有使用真实 Key，不能据此认定其余 live Key 契约通过。
- race 检测未运行：当前 PATH 无 gcc，`CGO_ENABLED=1 go test -race` 的前置条件缺失；普通并发测试已运行，不冒充 race 检测。

新增自动化与人工执行接缝（2026-09-16）：

- 第 8 项热载 contract：真实 DSH/PID/session 上完成两次 dummy-Key 模型请求，以公开 reload 事件作栅栏；A→B 请求头变化、BrowserAuth records 保留、一次启动及清理均通过。JS 探针嵌入 dshgw 二进制并纳入候选升级闸门；取消使用 SIGTERM 并留清理时间，不用 SIGKILL 跳过清理。
- `tenant list --json` 追加只读 UID/user/unit/路径/创建时间/prefix alias/origin 元数据，不输出 Key/cookie；原有表格与 JSON 字段保持。
- root 分阶段脚本 baseline/revoked/cleanup，外部机器单独 external：仅操作专用、身份匹配的临时租户。状态与 CLI 日志私有，公开报告始终不冒充全里程碑完成。
- 脚本自测试通过 13 项；其模型请求协议另在真实一次性 DSH + 假 Responses 上验证，启用标题插件时依靠显式 rename 保证没有额外标题 LLM 请求。使用公开 session/list/rename/page 及 rpcId/turn 终态，不读取私有 session 日志猜游标。
- 最新 `make dshgw-verify` 已包括以上检查并通过；真实 root 结果尚未回传。用户已选择人工 root 执行，测试实例 `http://192.168.190.86:8088`、Key 文件 `/root/dshgw-e2e/a.key`/`b.key`、模型 `deepseek-flash` 已确认；未读取或在聊天接收 Key 明文。

**尚未完成（不计为通过）**：

1. 目标主机更新下述部署修正、两真实 UID 相互拒绝读取、实际 systemd sandbox/handshake 属主与权限（初次 root 安装已通过阶段 1）。
2. 宿主 nginx 公开端口段 TLS、外部来源防火墙可达、两租户登录与跨端口 HTTP/WS。
3. 有效真实 Key 的模型调用与停用 Key 后拒绝；A/B 的模型清单认证与 Bearer/X-API-Key 等价已通过阶段 1，但不能据此认定模型调用已通过。
4. 10–30 个 worker 的真实 RSS/cgroup 与限额命中、备份恢复演练、浏览器授权本机目录的人工走查。

本轮实测 EUID=1000、`sudo -n` 不可用，没有修改宿主服务/证书/防火墙或 nginxWebUI 数据库。完整验收前保留 TODO，不标记 M51 完成或提交。

用户回传的第一组宿主只读结果：`uid=0(root)`；两份测试 Key 文件均为 `600 root:root`；`nginx.service` active；dshgw 尚未安装到 `/opt`、服务 inactive；cgroup v2 存在。这是用户执行结果，不是 agent 直接拥有 root。下一步先安装与运行契约，不立即启动公网入口或创建租户。

授权前置检查：工作区 `config.yaml` 的 `auth.default_grant` 仍为 `all`。未修改该文件或运行中 aigw；上线前需由运维确认实际配置来源、补齐现有 Key 显式授权后切换为 `none`，避免未经确认中断已有客户端。

### 阶段 1 宿主回传与启动前修正

- **用户 root 终端执行回传**：宿主 `nginx -t` 通过；安装成功；`/opt` 模板固定 `dsh-browser-fs@0.2.0` 供应成功；安装的 dshgw `0.14.1 / f7765f6 / 2026-09-15T19:11:04Z` 跑完基础 7 + 热载 1 条 DSH 契约，全部 PASS。
- 两把真实测试 Key（只传文件路径，没有在聊天或 agent 工具读取明文）的 `aigw-models-auth-fence`、`aigw-key-model-list`、`aigw-key-header-equivalence` 全部 PASS。该阶段没有启动 gateway 公网入口、没有创建租户。
- 启动前检查发现：安装器 `/var/lib/dshgw` 的 `750 root:dshgw` 会阻止独立 tenant UID 穿过父目录；已改为 state `0751 root:dshgw`、tenant root `0711 root:root`，私有 tenant 叶仍 `0700`、Key 模式不变、绝不把 tenant 加入 gateway 组。建户在 registry 发布/worker start 前增加真正的 `runuser -u <tenant> -- /usr/bin/test` 读写探针，doctor 增加共享根遍历检查。新建配置叶目录显式 chmod0750，避免操作者先前 `umask077` 把 gateway 组 search bit 屏蔽。
- gateway unit 由逐文件 ReadOnlyPaths 改成 `/etc/dshgw /var/lib/dshgw` 的目录级只读挂载，保留唯一可写 `/var/lib/dshgw/gateway`，避免原子替换 registry 时被单文件 bind mount 固定旧 inode。已加静态回归；真实 mount namespace 下的创建/registry可见性仍由下一阶段验收，不冒充 root mount 实测。
- serve 与生命周期改为共用配置的 session store，修正 `max_sessions` 之前仅在生命周期加载生效的遗漏；容量回归通过。
- **用户显式批准**现有 Key 已授权、可切 `auth.default_grant: none`。只读核对正在运行的 aigw（UID1000，工作目录本仓库，配置参数 `/home/winger/work/ai_gateway/config.yaml`，无 `GW_AUTH_DEFAULT_GRANT` 覆盖）。已仅修改该配置项及解释注释，私有回滚副本为 `.cache/dshgw-auth/config.before-none-7a4a5aa529a9.yaml`（0600，父目录0700，gitignored）；**尚未由 agent 重启进程，不宣称运行态已变更**。实际停启继续由用户 root 终端执行。
- 上述代码修正后 `go test ./...`、`go vet ./...`、`make dshgw-verify`、静态 ELF 检查与 `git diff --check` 均通过；新 dshgw 构建时间 `2026-09-15T22:34:17Z`，待宿主更新。没有创建 M51 commit。

已提供人工接续脚本 `scripts/dshgw_stage2.sh`：要求显式授权/重启确认、拒绝活动 dshgw/workers、核对当前本地 aigw 身份/实际配置来源/环境，更新 dshgw 文件后按原 winger 身份重启 aigw；检查新日志 none、两把 Key 和 deepseek-flash，再仅启动回环 gateway。shell/嵌入 Python 语法及未确认/非 root 拒绝测试通过，**没有在 agent 工具执行真实重启，仍待用户 root 结果**。不自动打开 nginx 公网入口。

### 阶段 2 回传（已执行）与 nginx 链路回归

用户回传：aigw 成功重启、新启动日志 default_grant=none、两把 Key 的认证/header 契约通过。Key A 因模型清单为空触发守卫，dshgw 留在停止状态；B 能看到 deepseek-flash，说明缺口在 A 的授权/可选路由而非全局模型不存在。用户在管理面补齐 A 的测试模型标签后回传共同可用模型=[deepseek-flash]，没有撤销 none 策略。

为避免重复重启已正常的 aigw，新增 `--verify-and-start`。用户继续执行后回传两 Key 的 deepseek-flash 验证通过、gateway active、loopback门户 HTTP200（初次连接失败由受控重试恢复）。该阶段未生成/reload nginx公网片段、未建立租户。agent 只读 systemctl show 也确认运行中的 RO=/etc/dshgw /var/lib/dshgw、RW=/var/lib/dshgw/gateway，及实际共享目录751/711。

新增 `TestNginxTLSProxyIntegration`：只在非 root + DSHGW_TEST_NGINX=1 下运行临时真实 nginx，真实 RenderNginx 片段与 Proxy.Dispatch 串联假 validator/worker，临时 ECDSA 证书通过显式可信 CA 验证，绝无 InsecureSkipVerify。验证门户302与gateway安全cookie、跨端口cookiejar与续期、双租户隔离、Host/可信端口头覆写、跨源POST/WS403、101帧回传和worker cookie/header隔离；10次全proxy回归通过。`make dshgw-nginx-test` 可单独强制执行；verify 在本机具备非root/nginx条件时自动纳入，否则明确SKIP。这不是宿主生产 nginx 的 TLS/外部可达性或真实worker验收替代。

下一阶段已交付给用户：render-nginx --reload → doctor → 双真实租户 baseline；目前仍等实际结果，不提前勾选 UID隔离、真实模型调用、资源与恢复验收。

### 双真实租户 baseline：部分通过，模型 turn 待排查

用户执行 `render-nginx --reload`/doctor 成功，run-001 建立 `m51-e2e-b405d9cc-a/b`。实际UID/EACCES、worker/cgroup/listener、TLS门户/client200、browser-fs426/101、cookie租户绑定与跨端口HTTP/WS403均PASS。**首个真实模型turn未达到带assistant回复的completed终态，baseline整体失败**；重启自愈/退出隔离及后续停用/清理阶段尚未执行。脚本按失败策略停止测试workers、保留run目录，agent只读systemctl确认两unit inactive/dead/MainPID0。

同时发现Python3.14 HTTPResponse析构时flush已关闭fp的警告；已修复资源关闭顺序并以socketpair检查显式close/重复close/finalizer路径，14项脚本回归通过。该警告与模型turn失败分开处理；不能在没有错误code/status之前猜测余额、模型能力或其它根因。

### baseline 失败定位与原租户续验通过

安全限定匹配 aigw 本地日志得到该测试请求的明确拒绝：`key=m51-test-a account=m51-test-a reason=insufficient_quota available_micros=0 reserve_micros=42116`（request_id=req_awvfoockgbbf7g4fhrdcfebv）。账本币种USD，预留0.042116USD不是最终扣费。用户通过管理面补齐了两测试账户的小额额度；没有恢复默认all、关闭计费或打开透支。

新增保留状态的 resume-baseline，先校验原config/key指纹和两个run-owned UID/unit，预检全部unit状态才启动，拒绝配置漂移/陌生unit/新模型覆盖，保留旧report与失败历史；不创建/删除租户、不重启aigw/nginx。无特权30项fake回归通过（输出与真实主机PASS分离），真实一次性DSH协议仍通过。

**用户root续验回传全部PASS**：A/B认证与deepseek授权、UID/EACCES、实际cgroup/listener、TLS/cookie/client/WS、跨端口403、两次真实worker模型调用、重启后请求恢复与退出隔离。报告 `/root/dshgw-e2e/run-001/report.json`；私有state不分享。后续仍需停用Key/真实轮换、外部机器TLS、10–30worker资源与恢复演练；不把baseline通过当成完整M51完成。


### 门户原生表单 Forbidden：修复设计（待确认与实现）

- 用户回传 Key A 停用验收 `disabled-key-model-401-and-configured-session-policy` PASS；当前 `key_revalidate: off`，模型401而既有UI会话继续可用符合规格，B未受影响。真实新Key轮换仍未验收。
- 用户浏览器登录与退出的安全审计均为 `origin mismatch` / 403，Origin摘要为 `[invalid]`，并非退出成功；尚未取得该浏览器的原始Origin。现有Python主机测试手工设置Origin，未覆盖原生表单。
- **临时真实Firefox 155.0.1已复现该机制**：独立profile、随机HTTP回环端口，原生form.requestSubmit()（不是fetch）；no-referrer下/login与/logout均发送字面量Origin:null、无Referer；same-origin下两路径均发送正确回环Origin和同源Referer。四例Sec-Fetch-Site均same-origin、Mode均navigate、Dest均document。所有临时进程与profile已清理；未访问用户profile、真实Key或线上服务。这证明候选机制，尚不等价于取得用户原浏览器的原始Origin或生产HTTPS复验。
- 修复目标：门户采用 `Referrer-Policy: same-origin`，同源表单保留Origin，跨源（包括租户不同端口）不发送Referer。保留CSP与精确Origin/Fetch Metadata闸门，不放行null/缺失/跨源Origin，不用Referer替代认证校验。
- 回归目标：真实浏览器原生login/logout表单，旧策略负对照；实际PortalHandler的响应头与同源成功路径，以及null/缺失/跨端口请求仍403。临时浏览器回归不等价于线上用户复验。
- 部署边界：确认后仅构建更新gateway；不重启aigw、DSH GUI或租户workers、不改VERSION、不提交M51。用户刷新门户后用B复验登录/退出，并回传不含Key/cookie的结果。


- **用户真实主机复验回传**：更新 gateway 后，Key B 的 `POST /login` 返回 302，Location 为 `https://chat.tirisen.hk:32602/`，租户页面可打开。证明 same-origin 策略修复了原生浏览器登录跳转；退出按钮仍需单独确认成功状态。


### 浏览器CSP修正与发布门槛确认

此前仅凭租户页面可手工打开便记录“自动跳转成功”不准确。用户后续明确：POST /login为302但没有后续GET，浏览器报告form-action self阻止提交重定向。实际修复为门户CSP form-action包含self及registry的租户origins，保持其余CSP和严格Origin校验；用户已核对线上响应头并最终确认自动跳转成功。proxy回归新增精确CSP断言。退出仍待单独复验。

用户最终决定保留 https://chat.tirisen.hk:32600/，取消/dsh/改动；要求所有主机验收完成后才提交发布。未完成项目保持TODO，不能以baseline或浏览器登录成功替代资源压力、真实轮换、外部TLS、目录授权与恢复演练。race仍缺gcc，不宣称通过。


### 外部TLS探针回传（网络位置待确认）

用户按不带-k、不跟随重定向的curl探针回传：32600为200、32601/32602均302到https://chat.tirisen.hk:32600/，三者tls_verify=0。公开端口HTTPS行为符合预期；待确认执行设备确为服务器之外及同局域网/不同网络后界定外部可达范围，不把此结果当作任意公网均可达。
