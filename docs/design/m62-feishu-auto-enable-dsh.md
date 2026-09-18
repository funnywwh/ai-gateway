# M62 设计文档：绑定飞书即自动启用 DSH

> 状态：**已实现（M62，代码、单测与控制台/端到端验收完成）**。
> 上游：绑定本身见 [M60](m60-aigw-key-feishu-binding.md)，门户登录见 [M61](m61-dshgw-feishu-login.md)；
> 规格：[docs/feishu.md](../feishu.md)、[docs/dshgw.md](../dshgw.md)。
>
> 需求原话：「这里绑定飞书应该自动启用dsh」。

## 1. 目标

管理员在控制台点「绑定飞书」并完成授权后，被绑定的那个人**立刻能用飞书登录自己的租户**，
不需要管理员再回到账户页点一次「启用 DSH」。

现状（M60/M61）是两步：先绑定（身份），再启用 DSH（租户）。第二步容易漏：绑定的人以为搞定了，
被绑定的人点「飞书登录」却收到「该账号未启用 dsh」。本里程碑把第二步并进第一步。

## 2. 关键决策

### D1 绑定成功即调用与「启用 DSH」按钮完全相同的供应流程

不新写一套逻辑，而是把控制台 `enableAccountDSH` 的核心抽成
`s.provisionAccountDSH(ctx, actor, store, keys, account, requested)`，由两处共用：控制台按钮与绑定回调。
租户命名（`dshTenantSlug` + 去重）、worker 凭据铸造、`tenant-create`/`set-key`+`start`、账号映射写入、
`dsh_enable` 审计、缓存失效全部沿用同一条路径——两份实现迟早会在"重名怎么办""重启一个已存在的租户
做什么"这类细节上分歧。

### D2 只启用「从未启用过」的账号，绝不撤销显式停用

判定顺序：

| 账号现状 | 行为 |
|---|---|
| `dsh_enabled = true` | 什么都不做，报告 `already` |
| `dsh_enabled = false` 且 `dsh_tenant` 为空（从未启用） | **自动启用**，报告 `enabled` + 租户名 |
| `dsh_enabled = false` 但 `dsh_tenant` 非空（曾被启用、后被显式停用） | **不启用**，报告 `declined` |

理由：管理员按过「停用 DSH」是有原因的动作，而绑定可能只是"同一个人再绑一把 Key"这类无关操作；
静默把停用撤销，过一段时间看就是"配置自己变了"的 bug。`declined` 会让控制台明确说
「该账号曾被显式停用 DSH，因此未自动启用；如需登录请在账户页手动启用」。

### D3 供应失败不回滚绑定

绑定记的是**身份**（这个人是谁），供应记的是**租户是否在跑**——两件不同的事实，只有后者可重试。
因此供应失败时：绑定照常写入，回调带 `dsh=failed&dsh_reason=<短原因>`，控制台提示"绑定已保存，
请在账户页重试启用"，完整错误进日志。反过来（回滚绑定）会让管理员为了一个临时的 socket 故障
重走一遍飞书授权。

### D4 开关默认开，但只在门户登录开启时才生效

`feishu.auto_enable_dsh`（默认 `true`）。它只在 `feishu.dsh_login=true` 时起作用：如果部署只用飞书
做身份（不做门户登录），绑定就**不该**悄悄给账号开一个租户、起一个 worker、占一个端口。
关掉时回调带 `dsh=off`，控制台不显示任何额外信息。

### D5 结果放在回调新增的查询参数里，不改 `feishu=` 的既有语义

`feishu=bound|replaced|...` 继续表示身份结果；新增 `dsh=enabled|already|declined|failed|off`，
外加 `tenant=` 与 `dsh_reason=`。控制台把两者分别提示（成功用 ok，其余用 error），未知取值照旧原样显示，
以免协议变更被静默吞掉。

### D6 同时修掉一个被本功能暴露的既有缺陷

端到端脚本第一次跑自动启用就失败：

```
dshgw admin channel unavailable at : dial unix: missing address
```

也就是说，**监督形态下 aigw 自己不知道子进程的 admin socket 在哪**：`buildDshgwChild` 为子进程配置
推导出 `<state_dir>/admin.sock`，但 aigw 侧 `dshgw.admin_socket` 仍是空的，而控制台按钮与（现在的）
自动启用都走那个客户端。后果是监督形态下「启用/停用 DSH」按钮一直不可用，只是没人从这条路径走过。
现在 `dshgwChild` 明确带出 `adminSocket`，`cmd/aigw` 的 `dshgwAdminSocket(cfg, child)` 优先用它、
否则退回配置值（独立形态），两侧由构造保证一致。

## 3. 接口与数据流

```
控制台「绑定飞书」→ 飞书授权 → 回调：
  写绑定（不变）→ autoEnableDSHForBinding(ctx, actor, key)
      ├ 开关关 / dsh_login 关         → dsh=off        （不碰账号）
      ├ 已启用                        → dsh=already    （不碰账号）
      ├ 曾被显式停用（有 tenant）      → dsh=declined   （不碰账号）
      └ 从未启用 → provisionAccountDSH（与按钮同一条路径）
                    成功 → dsh=enabled&tenant=<名字>
                    失败 → dsh=failed&dsh_reason=<短原因>（绑定保留）
  → 303 控制台 #/keys?feishu=<身份结果>&key=<id>&dsh=<状态>[&tenant=&dsh_reason=]
```

配置新增：

```yaml
feishu:
  auto_enable_dsh: true    # 绑定成功即启用该账号的 DSH（仅 feishu.dsh_login=true 时生效）
```

## 4. 异常与边界

- 账号 suspended/closed：`provisionAccountDSH` 直接拒绝（沿用按钮的判定），报告 `failed`，绑定保留。
- 账号名生成的租户名非法或已被别的账号占用：沿用按钮的规则（校验 + 重名去重 + 冲突拒绝），报告 `failed`。
- 本机 provisioning 通道未配置（独立形态未设 `dshgw.admin_socket`）：`failed`，原因写清。
- 供应耗时：创建租户要起 worker 并同步模型，因此这次回调会比以前慢（与管理员手点按钮同一个量级，
  客户端超时 5 分钟）。管理员在浏览器里等的是同一件事，只是少了一次点击。
- 自动启用与「停用 DSH」的并发：都经由账号行写入 + 缓存失效，最终以最后一次写入为准；
  自动启用只在"从未启用"时触发，因此不会与停用形成来回翻转。

## 5. 测试策略（已实现）

- **httpapi**（`admin_feishu_test.go`）：四个结果各一条——
  自动启用真的建了租户并写了 `dsh_enabled`/`dsh_tenant`、铸了 worker Key、写了 `dsh_enable` 审计、
  回调带 `dsh=enabled&tenant=`；同账号第二把 Key（换一个飞书身份）→ `already` 且**不再**建租户；
  曾被显式停用 → `declined` 且通道未被调用、账号未被修改；通道故障 → `failed` 且**绑定仍然写入**、
  账号未被标记启用；开关关闭 → `off` 且不调用通道。
- **控制台**（`internal/webui/tests/keys_feishu_test.mjs`）：`enabled/already/declined/failed` 四种提示的
  文案与级别、`off` 不产生第二条提示、未知取值原样显示。
- **cmd/aigw**：`dshgw.admin_socket` 未设置时父子两侧派生到同一个 socket；显式设置时两侧都跟随。
- **端到端**（`scripts/dshgw_supervised_e2e.py`，47 步全绿）：绑定一次 → 断言回调带 `dsh=enabled&tenant=`、
  账号已启用、该租户已在 registry 里 → 门户飞书登录 → 进该租户 UI（200）→ 复核被调用 → 解绑被拒 →
  上游吊销被拒 → 恢复成功。此前该脚本用 SQL 直接写 `dsh_enabled`，现在**删掉了那段**——
  绑定自己完成供应正是本里程碑要证明的事。
- 全量：`go test ./...`、`make ui-base`、`make verify`、`make dshgw-test`、`make dshgw-verify`、
  `make dshgw-supervised-test`。

## 6. 实现与设计差异（回填）

1. **抽出 `provisionAccountDSH` 时保持了 HTTP 行为逐字不变**：原处理器的错误响应改为
   `writeAPIError(w, toAPIError(err))`；因为内部返回的本来就是 `domain.APIError`，
   `toAPIError` 原样透传，客户端看到的 status/code/message 与之前一致（既有 DSH 测试未改动即通过）。
2. **`dsh=off` 在控制台不产生任何提示**：设计里只写了"关掉时不显示额外信息"，实现时把这条做成
   测试（`dsh=off` 只产生 1 条提示而不是 2 条），否则"开关关掉"会被当成一次失败。
3. **`dsh_reason` 长度截断到 120 字**：原因要随 URL 往返并显示给人看，完整错误留在日志里。
4. **端到端还多修了一处竞态**：绑定现在会在运行中途新建租户，而 gwproxy 自持 registry 快照
   （约 2 秒重载），因此断言"租户 UI 200"改为轮询等待代理看到新租户，而不是一次性请求。
