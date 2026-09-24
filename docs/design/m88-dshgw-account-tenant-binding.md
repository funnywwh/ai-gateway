# M88：dshgw 只按账户映射租户（退役 key 前缀绑定）

## 0. 需求原话

> dshgw 也用 key hash 找租户

字面方案是"把 dshgw 的租户绑定从前缀换成哈希"。评审后**否决了哈希化，改为直接删掉本地解析**：那条路只服务"aigw 不返回租户名"的老 deployment，而今天的登录前置必然先问 aigw（`authorizeDSH`），所以本地解析已是死代码；给死代码换一把钥匙（哈希）还要背一次磁盘格式迁移（见 §2.1），不如删掉。账户 → 租户的权威映射由 M74 建立（`accounts.dsh_tenant`），本里程碑是把它的承诺补完。

## 1. 现状与证据

| 事实 | 证据 |
|---|---|
| 登录的权威路径是账户映射，前缀不参与 | `internal/dshgw/proxy/proxy.go:405-428`：先 `authorizeDSH(key)` 拿 `authTenant`，再 `resolveTenant` → `Registry.Get(租户名)`；`internal/httpapi/v1.go:1084-1105` 的回答里带着 `tenant` 与 `account_id`，注释写明"every key of this account logs into it, so dshgw no longer needs a per-key prefix binding" |
| 前缀兜底只在"aigw 回了但没带租户"时走 | `internal/dshgw/proxy/proxy.go:1104-1118`：`authTenant != ""` 走账户映射，否则 `aigw.KeyPrefix(key)` + `Registry.ByPrefix`。代码自己的注释："The account mapping is authoritative; legacy prefix binding only applies when aigw returned no tenant name (older aigw without the mapping)" |
| 支撑这套兜底的设施成堆 | `registry.go`：`Tenant.KeyPrefix`(39)、`PreviousPrefixes`、`Prefixes()`(86)、`ByPrefix`(317)、`AddPrefix`(578)、`RotatePrefix`(~549)、`validateUnique` 的前缀唯一校验(264)、`encodeKeyMap`(713)、`LoadKeyMap`(732)；`cmd/dshgw/runtime.go:237-246`（`len(prefix)!=12` 校验）、`cmd/dshgw/ops.go:24-59`（`bind`/`whereis`）、`cmd/dshgw/tenant.go:180`（`--keep-old-prefix`） |
| 节点侧早就不绑定 | `internal/dshgw/nodeops/ops.go:181` `NoKeyPrefix: true`；`registry.validateTenant` 允许空前缀（M77 注释） |
| 展示面也不依赖它 | `dshgw_nodes.js` 不渲染 `key_prefix`；`localdshgw.TenantInfo.KeyPrefix` 只是从 admin socket 透传的一个字段，控制台没有任何页面读它 |
| Feishu 登录不走前缀 | `internal/dshgw/proxy/feishu.go`：ticket → 租户名，再用该租户的 worker key 向 aigw 复核访问 |
| 文档漂移 | `docs/dshgw.md:42` §3 第 3 步仍写"用 Key 前 12 字节查 canonical `registry.json`"，与代码不符 |

**结论**：今天这套前缀绑定只服务"老 aigw（无账户映射）"，而本部署早已 ≥ M74。它的实际作用只剩两类边角：老别名（`PreviousPrefixes`）与 `bind`/`whereis` 这两条运维入口。

## 2. 关键决策

| # | 决策 | 理由 / 代价 |
|---|---|---|
| D1 | **删掉本地解析**：`authTenant == ""` 直接拒绝（403），不再按前缀兜底 | 兜底只服务老 aigw；失败路径已有完整话术（"该账号的 dsh 租户尚未就绪，请联系管理员启用"），只是审计 reason 从 `unbound key prefix` 改成 `tenant_unmapped` |
| D2 | 绑定设施整体退役：`KeyPrefix`/`PreviousPrefixes` **不再参与认证**；`bind` 删除，`whereis` 改成"按账户名查租户"（支持场景真正常问的问题） | 支持的替换路径：`dshgw tenant list` 已按租户列账户；`whereis <account>` 做本地扫描，不需要密钥 |
| D3 | `PreviousPrefixes` 与 `--keep-old-prefix` 退役 | 账户映射下，同账户的任意 key（含轮换前的旧 key，只要它在 aigw 仍 active）都能登录 ⇒ 别名机制没有意义 |
| D4 | **回滚保险**：新版本仍然写 `key_prefix`（创建/轮换时一行 `key[:12]`，明文就在手里），只是认证不再读它 | 回滚到老二进制后老前缀绑定依然有效，注册表文件不需要任何还原。一个版本后再删字段 |
| D5 | `keys.map` **停写并在下次 Save 时删除**；`key_map_path` 配置项**保留但标 deprecated** | 该文件是派生索引（`registry.go:676-680`："Authentication always uses canonical registry.json"），仓库内没有认证路径读它；配置是 `KnownFields(true)` 严格解析（`config.go:723`），删字段会让老 config 起不来 |
| D6 | 节点与 Feishu 流程不动 | 已不依赖前缀（§1 第 4、6 行） |
| D7 | 与 M87 的关系：独立上线，互不依赖 | M87 的 D13（mint 侧预检）在本里程碑之后不再是安全必需（前缀不参与任何认证），可降级为"展示观感"考虑；反过来 M88 也不依赖 M87 的哈希查找 |

### 2.1 被否掉的方案：把绑定换成 key hash

- 字面需求是要"哈希找租户"，技术上可行（registry 存 `key_hash`，`ByHash(sha256(token))`），但：
  1. 它服务的那条路是**死代码**（登录前置必然先问 aigw）；
  2. **迁移有硬点**：当前租户凭据的明文还在 `<TenantConfigRoot>/<tenant>/gateway.key`（0640），能回填哈希；但 `PreviousPrefixes` 的明文**已经不存在**——`rotateKeyLocked` 直接用新 key 覆盖 `gateway.key` 与 `.credentials.yaml`（`internal/dshgw/tenancy/manager.go:1040-1060`，回滚只把旧值留在内存变量里）。老别名转不成哈希，只能再留一条"前缀 → 租户"的兼容分支；
  3. 于是结果是"为死代码加一层兼容分支 + 一次磁盘格式迁移（`DisallowUnknownFields` 还要求控制面与节点一起升）"。
- 结论：删掉更短、更安全。若将来真的需要 dshgw 在**不问 aigw** 的情况下解析租户（例如 aigw 长期不可用也要能登录），那是另一件事，届时按哈希方案重新评估。

## 3. 改动清单

| 文件 | 改动 |
|---|---|
| `internal/dshgw/proxy/proxy.go` | `resolveTenant`：删掉 `aigw.KeyPrefix` + `ByPrefix` 分支；`authTenant == ""` → 返回未解析（沿用现有 403 话术，审计 reason 改 `tenant_unmapped`）。`aigw.KeyPrefix()` 的调用点随之退役 |
| `internal/dshgw/aigw/client.go` | `func KeyPrefix(input string)` 删除（如仍有展示用途则保留并注明"仅展示"） |
| `internal/dshgw/registry/registry.go` | 删 `ByPrefix`、`AddPrefix`、`RotatePrefix`、`Prefixes()`、`validPrefix`、`encodeKeyMap`/`LoadKeyMap`；`validateUnique` 去掉前缀校验（保留端口/worker 端口校验）；`Save` 不再写 `keys.map` 并删除已存在的文件；`KeyPrefix`/`PreviousPrefixes` 字段保留（D4，只写不读） |
| `internal/dshgw/tenancy/manager.go` | 建租户/轮换：继续写 `KeyPrefix`（D4），删掉 `ByPrefix` 冲突检查与 `RotatePrefix` 调用、`keepPrevious` 相关分支 |
| `internal/dshgw/tenancy/remote.go` | 同上（`RotatePrefix`、`PreviousPrefixes` 分支） |
| `internal/dshgw/tenancy/backup.go` | 备份清单去掉 `keys.map` |
| `cmd/dshgw/ops.go`、`runtime.go`、`tenant.go`、`tenantrows.go` | 删 `bind` 与 `cleanPrefix`；`whereis` → 按账户名查租户；删 `--keep-old-prefix`；`tenant list` 的 KeyPrefix 列保留（标明"仅展示/回滚保险"） |
| `internal/localdshgw/client.go` | `TenantInfo.KeyPrefix` 删除（控制台没有任何页面读它） |
| `internal/dshgw/config/config.go` | `KeyMapPath` 标 deprecated（继续接受、不再使用）；启动日志提示该文件已废弃 |
| `docs/dshgw.md` | §3 第 3 步改写为"按账户映射租户"；删掉 keys.map 的说法；§7 里 bind/whereis 的说明同步 |
| `docs/deployment-layout.md`、`docs/design/m51-dshgw.md`、`docs/design/m63-data-root.md` | 路径清单里 `keys.map` 的说明改为历史遗留 |
| `docs/design/m87-api-key-hash-lookup.md` | D9/D13 补一句"M88 起 dshgw 不再有前缀兜底" |

## 4. 迁移与兼容

- **registry.json 不升版本**：不新增字段、不删字段（`key_prefix` 照写）⇒ 新旧二进制都能读同一个文件。
- **升级前的前置要求**：aigw ≥ M74（`/v1/dshgw/authorize` 会返回 `tenant`）。本部署 4.x 已满足；`deploy/dshgw/README.md` 补一句显式要求。
- **回滚**：把二进制换回上一版 + 注册表不用动（D4 保证前缀仍在），老前缀绑定继续有效。这正是 D4 存在的原因。
- **老别名**：`PreviousPrefixes` 里的历史前缀在回滚后仍然有效（字段没删），但前滚之后不再有任何效果——这与账户映射的语义一致（旧 key 只要在 aigw 仍 active 就能登录）。
- **keys.map**：升级后第一次 `Save` 会删除该文件；它出现在备份清单里的部分同步移除。

## 5. 测试策略

- 登录：① aigw 回 tenant → 进该租户（现有用例保留）；② aigw 回 `allowed:true` 但**没有** tenant → 403 + 审计 reason `tenant_unmapped`；③ **回归**：造一个带 `key_prefix` 的注册表，用一个前缀匹配但账户不匹配的 key 登录 → 必须拒绝（证明兜底真的没了）。
- 注册表：`validateUnique` 不再拦前缀重复（两个租户同前缀也能 `Put`+`Save`）；`Save` 后 `keys.map` 不存在；带 `key_prefix`/`previous_prefixes` 的老 registry.json 仍能加载。
- CLI：`bind` 不再存在；`whereis <account>` 按账户名返回租户（多命中时列出全部）；`tenant rotate` 不再接受 `--keep-old-prefix`。
- 兼容：新版本产生的 registry.json 交给老二进制能加载且老前缀仍能登录（回滚演练）。
- 节点：M77 的分配记录（空前缀）仍能加载/保存。

## 6. 依赖与非目标

依赖：无新依赖；无数据库迁移；无注册表版本变更；一个配置项标记废弃。

非目标：① 不动 aigw 的 key 表与鉴权（那是 M87）；② 不引入哈希绑定（§2.1 已否掉）；③ 不改节点侧与 Feishu 登录流程；④ 不从 registry.json 里删除已有的 `key_prefix`/`previous_prefixes`（回滚保险，留一版）；⑤ 不删除 `key_map_path` 配置项（留一版，避免老 config 起不来）。

## 7. 实现与设计差异

（实现完成后回填）
