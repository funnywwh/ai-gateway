# M8b 设计：管理面资源 CRUD（providers / models / mappings / routes / tags / mcp-tokens / hooks）

> 前置：`docs/design/m8-admin-api.md`（会话鉴权、keys、requests、audit、热更新三件套）。
> 本文件在编码前输出，实现完成后回填第 13 节差异。

## 1. 目标与非目标

目标：让管理面覆盖**全部可配置资源**，使 M9 Web 控制台不需要任何直连数据库或 shell 的旁路。
所有写操作遵循同一条链路：鉴权 -> 校验 -> 落库 -> 失效缓存 -> 重建快照 -> 审计。

非目标：
- 不做批量导入导出（M16 备份/恢复负责）；
- 不做计费规则的评测与模拟（M11a）；
- 不做鉴权面的细粒度 RBAC（当前只有 admin / viewer 两个角色）。

## 2. 为什么拆成多个窄端口，而不是一个大 AdminStore

`AdminStore` 继续只承载已经落地的 keys/queries/audit。资源面新增的端口按**资源族**切分，
每个端口只列该族需要的存储方法：

- 好处 1：单元测试的假实现从 40 个方法降到 5-8 个，管理面测试才有写下去的可能；
- 好处 2：端口方向单一，httpapi 不会因为 registry/pluginhost 的内部结构变化而被迫改动；
- 好处 3：未来若把管理面拆成独立进程，只需要在组合根换实现，handler 一行不动。

代价：组合根（cmd/aigw）需要把同一个 *store.DB 传给多个字段。这是**显式的重复**，
比隐式的巨型接口更好：组合根本来就是唯一知道实现的地方。

## 3. 端口清单（internal/httpapi）

| 端口 | 方法 | 承接实现 |
| --- | --- | --- |
| `AccountAdmin` | List/Get/GetByName/Upsert/SetStatus | store.DB |
| `ProviderAdmin` | List/Get/GetByName/Upsert/SetFlags/SetDiscovered/Delete + 模型 List/Upsert/Delete | store.DB |
| `ModelAdmin` | 模型 List/GetByName/Upsert + 映射 List/Upsert/Delete + 路由 List/Upsert/SetCooldown/Delete | store.DB |
| `TagAdmin` | List/GetByName/Upsert/Delete | store.DB |
| `HookAdmin` | List/Upsert/Delete | store.DB |
| `MCPTokenAdmin` | List/Upsert/Revoke | store.DB |
| `SettingsAdmin` | Get/Set | store.DB |
| `Sealer` | Seal(providerID, plaintext) / Open(providerID, cipher) | internal/creds + 主密钥 |
| `Prober` | Probe(id, mode) / Actions(id) / RunAction(id, name, in) / Logs(id, tail) | internal/runtime.Dispatcher |
| `HookReloader` | ReloadHooks(ctx) error | internal/hook.Dispatcher |

`Sealer`/`Prober`/`HookReloader` 都是**单方法小接口**，方便在测试里用 3 行假实现替换。
httpapi 不 import internal/pluginhost、internal/creds：凭据加解密与插件进程管理留在自己的层。

## 4. 端点表

统一前缀 `/admin/api/v1`，全部要求会话 Cookie；写操作要求 role=admin。

### 账户
| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/accounts` | 列表 |
| POST | `/accounts` | 新建（name, billing_mode, note, auto_suspend/auto_resume, 限额） |
| PATCH | `/accounts/{id}` | 状态/账期属性（不改余额，余额只走账本） |

### 供应商
| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/providers` | 列表，凭据只回 `has_credentials` 布尔 |
| POST | `/providers` | 新建（kind 必须是内建 kind 或 `plugin:<name>`） |
| GET | `/providers/{id}` | 单条 |
| PATCH | `/providers/{id}` | 开关/优先级/权重/并发/配置/超时/降级/凭据替换 |
| DELETE | `/providers/{id}` | 有路由引用时 409，需 `?force=true` 级联删路由 |
| POST | `/providers/{id}/test` | 探测：健康 + 模型数 + 耗时 |
| POST | `/providers/{id}/models/refresh` | 从上游发现模型并写入 provider_models（source=discovered） |
| GET | `/providers/{id}/models` | 该供应商的模型映射 |
| POST | `/providers/{id}/models` | 新增/更新一条模型映射 |
| DELETE | `/provider-models/{id}` | 删除模型映射 |
| GET | `/providers/{id}/actions` | 插件声明的交互动作 |
| POST | `/providers/{id}/actions/{name}` | 执行动作（登录、刷新 token 等） |
| GET | `/providers/{id}/logs?tail=N` | 插件进程 stderr 环形缓冲尾部 |

### 模型 / 映射 / 路由 / 标签
| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/models` | 列表（含 sale_pricing、别名、enabled） |
| POST | `/models` | 新建/更新 |
| PATCH | `/models/{name}` | 局部更新（按 public_name 定位，避免暴露内部 id） |
| GET/POST | `/model-mappings` | 列出/新增映射规则 |
| DELETE | `/model-mappings/{id}` | 删除 |
| GET/POST | `/routes` | 列出/新增路由 |
| PATCH | `/routes/{id}` | 改权重/优先级/开关/策略、清除冷却 |
| DELETE | `/routes/{id}` | 删除 |
| GET/POST | `/tags` | 列出/新增标签（grants + policy） |
| DELETE | `/tags/{id}` | 删除 |

### MCP 令牌 / 钩子 / 设置
| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET/POST | `/mcp-tokens` | 列出/签发（明文只回一次） |
| DELETE | `/mcp-tokens/{id}` | 吊销（保留行，status=revoked） |
| GET/POST | `/hooks` | 列出/新增 |
| DELETE | `/hooks/{id}` | 删除，随后重载投递器 |
| GET | `/settings?key=a&key=b` | 读取设置 |
| PUT | `/settings/{key}` | 写入 JSON 值 |

## 5. 凭据写入路径

1. 请求体接受 `credentials`（JSON 对象，明文），只在内存中存在；
2. 校验主密钥已配置（`Sealer` 为 nil 或返回错误时 400，并提示配置 `credentials_key`）；
3. `Seal(providerID, plaintext)` 使用 AES-256-GCM，AAD = provider id；
4. 写入 `credentials_enc`，同时 `config_version += 1`，使内建 provider 缓存与插件进程被判为过期；
5. 审计只记录 `credentials_updated: true` 与字段名列表，**绝不记录值**；
6. 响应只有 `has_credentials` / `credential_keys`（字段名），没有明文也没有密文。

关键细节：`UpsertProvider` 的 SQL 是整行覆盖，nil 密文会**清空**已有凭据。
因此 PATCH 的先读后写必须显式继承 `CredentialsEnc`；要清空必须传 `credentials: {}`。

## 6. Provider 探测 / 发现 / 动作 / 日志

新增 `runtime.Dispatcher.Probe(ctx, id, mode)`，mode 取 `health` | `models`：
- 内建 provider：`providers.Build` 后调 `Health` / `ListModels`；
- 插件 provider：确保进程已启动，走 `Client.Health` / `Client.ListModels`；
- 返回 `{ ok, mode, latency_ms, models?, error? }`，错误以 200 + ok=false 返回（探测失败是业务结果，不是 HTTP 错误）。

`RefreshModels` 把发现结果与已有映射合并：已存在的只补空字段（context_window、max_output_tokens、
capabilities、pricing_rules），**不覆盖人工设置的 upstream_model 与权重**，新增项 priority=100/weight=100。
人工来源标记 `source=manual` 的行在刷新时只补空值。

`Logs` 直接读 pluginhost 进程的环形缓冲尾部；插件未运行时返回空数组而不是报错。

## 7. 校验规则

- account.name：去除首尾 Unicode 空白后必须非空、为有效 UTF-8，且最多 64 个 Unicode 字符；允许邮箱、中文及其他 Unicode 字符（含 `@`），不再限于 ASCII 标识符字符集。
  同一条规则在三处生效：`domain.NormalizeAccountName`（唯一实现）、`config` 的 bootstrap 校验（**叶子包不能 import domain**，故按 `TestBootstrapAccountNameValidationMatchesDomain` 钉住一致性）、
  以及 `store.UpsertAccount`/`GetAccountByName`（写前与查前都规范化，`bootstrap` 用它保证 YAML 名与 `api_keys.account` 引用是同一个账户）。trim 只是规范化、不是新身份：
  `" acme "` 与 `"acme"` 同一个账户，但 `Ops`/`ops`、`é`/`é` 仍是不同账户（不做大小写折叠、不做 Unicode NFC）。
- provider.kind：内建 kind 或 `plugin:<name>`；name 为非空、长度<=64、字符集 `[A-Za-z0-9._-]`；
- provider.max_inflight >= 0；priority/weight >= 0；degradation 属于 `none|fail_fast|best_effort`；
- provider_model：public_model 必须能在 models 表按名解析（不存在时 404 并提示先建模型），权重/优先级非负；
- mapping：`modelmap.ValidateRule(kind, pattern, target)`，kind 属于 exact|prefix|glob|regex，
  target_model 与 (target_provider_id + target_upstream_model) 至少给一个；
  priority 越小越先匹配，重复 (kind,pattern) 直接 409，避免静默改写；
- route：model 与 provider 必须存在，upstream_model 非空；同一 (model, provider, upstream) 唯一；
- tag：name 非空唯一；grants/policy 必须是 JSON 对象（用 `json.Valid` + 顶层类型检查）；
- hook：type 属于 webhook|jsonl；webhook 必须 https（http 仅当 `allow_insecure_hook` 设置打开）；sample_rate 在 (0,1]；
- 所有 JSON 兜底字段（config、grants、policy、pricing）入库前做 `json.Valid` 校验，防止把坏 JSON 写进快照后在热路径爆炸。

## 8. 删除与引用完整性

- 路由是**唯一**引用 provider 的外键语义来源；删 provider 前先数路由，非空则 409 + 引用数量，
  `?force=true` 时在同一事务语义下先删路由再删 provider；
- model 只提供启停，不提供删除：已被 mapping/route 引用时删除会让历史账单失去模型名；
  需要下线时 `enabled=false`（不再出现在 GET /v1/models，也不再可路由）；
- tag 删除不影响已签发的 key，只影响下一次鉴权快照（key 上残留的 tag 名解析为空即忽略）；
- 全部删除都是软失败优先：先校验后写，写失败返回 500 并保持原状。

## 9. 热更新与缓存失效矩阵

| 写操作 | 目标 key 缓存 | registry 快照 | 钩子重载 | 插件进程 |
| --- | --- | --- | --- | --- |
| provider 新建/改配置/改凭据 | 不失效 | 重建 | - | 停止进程（下次按需重启） |
| provider 开关/权重 | 不失效 | 重建 | - | - |
| provider 删除 | 不失效 | 重建 | - | 停止进程 |
| provider_model / route / mapping / model / tag | **全量失效** | 重建 | - | - |
| key 状态与录制开关 | 定向失效（按前缀） | - | - | - |
| hook | - | - | 重载 | - |

原则：只要改动可能影响授权（grants/policy/tags/模型可见性），就必须全量失效 key 缓存；
只影响路由的改动不需要动 key 缓存。

## 10. 审计

每条写操作写一条 audit：action ∈ create|update|delete|probe|refresh|revoke|action，
target_type ∈ provider|provider_model|model|model_mapping|route|tag|api_key|mcp_token|hook|account|setting。
changes 记录**字段级**信息，凭据/令牌/口令一律只记布尔与字段名。
探测失败也记审计（result=failed）以便回溯供应商故障。

## 11. 并发与性能

- 所有查询走读连接池，写走单写连接；单条 upsert 是毫秒级，管理面 QPS 极低，不做批处理；
- registry 重建是**内容寻址的不可变快照**，创建新快照后原子指针替换，热路径读锁零成本；
- 插件进程重启不在请求线程里阻塞：写库成功后调用 `Stop`（带 2s 超时），失败只记日志；
- 探测端点带 10s 超时（可被 provider 自身的 timeout_overrides 缩短），不占用热路径的并发额度。

## 12. 测试计划

1. `admin_resources_test.go`：用假 store + 假 sealer + 假 prober 覆盖
   登录 -> 建 key -> 建 provider（凭据不回显）-> 建 model -> 建 route -> 列 route
   -> 删 provider（409 有引用 / force 通过）-> 未带 Cookie 401 -> viewer 角色 403；
2. 凭据继承：PATCH 不传 credentials 时密文保持不变，传 `{}` 时清空；
3. 校验：kind 非法 400、坏 JSON 400、mapping 重复 409、route 引用不存在的 model 404；
4. 探测失败返回 200 + ok=false；插件未运行时日志为空数组；
5. hooks 写入后调用 HookReloader；settings 读写往返。

## 13. 实现与设计差异

1. **provider_model 不再强制要求 canonical model 先存在**。设计里写的是「public_model 必须能在 models 表
   按名解析」，实现改为**写入成功 + 返回 warning**。原因：模型发现（refresh）会先落 provider_models
   再落 models，强校验会让发现流程自锁；而 OpenRouter 类网关的惯例是「先接上、再上架」。
2. **`/providers/{id}/rollback` 未实现**。设计里提到配置回滚，但没有配置历史表；需求要等 M9 界面出现
   「回滚到上一版」的真实交互时再补一张 `provider_config_history`（当前用 config_version + 审计里的
   字段级快照可以人工回溯）。
3. **`DeleteModel` 未提供**（设计如此）：模型只启停不删除；`/models/{name}` 用 PATCH 关闭即可。
4. **actions/logs/restart 走 runtime 的探测端口**：`Prober` 端口把 `Probe/Actions/RunAction/Logs/Restart`
   收在一处，httpapi 完全不 import pluginhost；日志读取在插件未运行时返回空数组 + running=false。
5. **`/providers/{id}/test` 同时持久化探测结果**（discovered_json/health_json/last_error），设计里只说返回；
   持久化让界面刷新后仍能看到上次探测结论，失败只记日志不影响响应。
6. **设置读写形如 `{"value":…}` 或裸值**：为兼容 curl 与界面两种调用方式各接受一种，实现取「有 value 键就
   取内层，否则整段作为值」，并在文档中写死，避免歧义。
7. **hook 更新按名字继承密钥**：`UpsertHook` 以 name 为冲突键，不传 secret 时从现有记录继承，
   避免界面改个采样率就把签名密钥清了。
8. **新增 `hooks.allow_insecure` 配置**：默认拒绝 http:// webhook，只有显式打开才允许。
9. **删除 provider 会顺带停止插件进程**（3s 超时，失败只记日志），设计里写了但当时没定超时。
10. **全仓 gofmt 一次**：本轮新增文件暴露了早期手写代码的 34 处对齐差异，一并清理，
    避免下次 diff 里混入格式噪音。