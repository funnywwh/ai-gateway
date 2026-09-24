# M89 代码脱敏：全仓清洗 + 发版强制检查 + 历史重写

> 状态：进行中（2026-09-24 起）。需求原话：「项目代码脱敏」「修改 skill 要求发布版本时脱敏」，
> 经确认的口径：**全仓库清洗**（代码 / 测试夹具 / 示例配置 / 脚本 / `docs/`）、**发布与部署入口列入例外保留真值**、
> **连 git 历史一起重写并强推 origin**、**Go 模块路径改为 `github.com/funnywwh/ai-gateway`**。

## 1. 目标

这个仓库是一条真实部署线的操作记录：主机别名（`gpt001`/`gptjp`/`rag-server`/`aipc`）、内网与公网 IP、
真实域名（`gpt.lagenio.xyz`/`gpt.tirisen.hk`/`chat.tirisen.hk`/`*.iotalking.top`）、
个人邮箱与姓名、主机密钥指纹、API Key 前缀、本机用户名与家目录路径，都同时出现在**代码、测试夹具、示例配置、
脚本与文档**里。目标是：

1. 仓库（工作树 + 全部 git 历史）不再携带真实环境标识与个人信息；
2. 「脱敏」成为发版流程里**机器能拦住**的一步（技能写清楚 + `release.sh` 门禁）；
3. 运维链路（发版技能、`Makefile`/脚本里的本机工具链默认值、部署命令）不中断。

## 2. 关键决策

| # | 决策 | 取舍理由 |
|---|---|---|
| D1 | 口径的**单一真源**是 `scripts/desensitize.py` 里的规则表（`TOKENS`/`RENAMES`/`DENY`/`EXEMPT`），文档只解释、不复述完整表 | 表要同时被三处使用：工作树清洗、历史重写、发版门禁。三份拷贝必然漂移，一份才是安全的 |
| D2 | 例外分两层：**路径例外**（`.dsh/skills/**` 整体不动）与**模式例外**（`/home/winger/.local/**`、`/home/winger/.cache/**` 这两个工具链绝对路径族在**全仓**不视为敏感） | 路径例外保住运维 runbook 里的真实主机名；模式例外保住 `Makefile`/`scripts/**` 的默认值。工具链路径在文档里被引用（`docs/deployment-layout.md` 等），若只豁免 `Makefile` 与 `scripts/**`，文档里的同一条路径会被改掉、与脚本默认值对不上——那才是真正的坑 |
| D3 | 占位值用 RFC 5737 / RFC 2606 的文档地址族与中性主机别名（`gw-a`/`gw-b`/`gw-c`/`gw-d`、`*.example.com`），IP 分两段：内网真实地址 → `192.0.2.101`…，公网真实地址 → `198.51.100.101`… | 一眼可辨识是假值；不落进任何真实网段；与仓库里已在用的 `192.0.2.x`/`198.51.100.x` 测试值不冲突（已核：现有值只占 `192.0.2.0/1/10` 与 `198.51.100.0/9`） |
| D4 | 规则按**模式长度降序**执行；裸词规则一律带边界（`funnywwh` 只在 `[\w./:-]` 之外匹配，`winger` 只在 `[\w./-]` 之外匹配） | 否则会把 `github.com/funnywwh/ai-gateway`（本版刚改成的模块路径）与 `git@github.com:funnywwh/ai-gateway.git`（origin）打坏——这两处是刻意保留的公开身份 |
| D5 | 除「真实值 → 占位」的显式表，还有**泛化拒绝规则**：私网 `192.168/16`、`172.16/12`、非文档网段的公网 IPv4、非允许域名的邮箱、真指纹形态（`SHA256:` + ≥20 位）、非允许用户名下的 `/home/<user>` | 显式表只能挡住**已知**的泄漏；泛化规则挡住「下次谁又粘一条真实配置进来」。允许名单很小且逐条有理由（SSRF 守卫测试的 `192.168.1.1`/`172.16.3.4`、公共 DNS 字面量、example.com 的 `93.184.216.34` 等） |
| D6 | 发版门禁放在 `scripts/release.sh`（脏树检查之后、写 `VERSION` 之前），并在技能里写成「第 2 步」 | 技能是给人的流程，`release.sh` 是机器的闸门；两者都不可省。放在写 `VERSION` 之前，失败时不会留下「版本号已改但没提交」的中间态 |
| D7 | 历史重写用 `git filter-branch --tree-filter` 跑**同一份脚本**，不引入 `git-filter-repo` | 本机没装 filter-repo（pypi 可达，可另装，但要自己保证「按路径豁免」的语义与工作树一致）。tree-filter 逐提交套用同一份规则，语义天然一致；代价是慢（479 个提交，分钟级） |
| D8 | 文档里的历史 revision 短 sha **不改写** | 重写会让它们不可解析，但把旧→新 sha 映射回填等于把 479 个提交再改一遍，收益为零。作为已知后果记在这里 |
| D9 | `docs/todo_done.md` 的「只增不改」约定让位一次 | 它是**记录**，脱敏是措辞改写、事实与数据不变。这一例外必须写明，否则下一个人会以为记录被动过 |

## 3. 替换口径（分类；完整表在 `scripts/desensitize.py`）

| 类别 | 例子（真实 → 占位） |
|---|---|
| 主机别名 | `gpt001` → `gw-a`、`gptjp` → `gw-b`、`rag-server` → `gw-c`、`aipc` → `gw-d` |
| 域名 | `gpt001.iotalking.top` → `gw-a.example.com`、`mnl.iotalking.top` → `gw-a.example.org`、`gpt.lagenio.xyz` → `gw-b.example.com`、`gpt.tirisen.hk` → `gw-b.example.net`、`chat.tirisen.hk` → `chat.example.com`、`lagenio.com/xyz` → `example.com`、`tirisen.hk` → `example.net` |
| 地址 | 内网 `192.168.190.86/.87/.88/.89/.90/.123/.222`、`192.168.140.252` → `192.0.2.101`…`108`；公网 `47.91.16.118` 等 6 个 → `198.51.100.101`…`106`；`http://192.168.190.86:8088` → `http://aigw.internal:8088`（比裸 IP 规则更长，优先命中） |
| 人 | `李智超`/`lizhichao`/`colin` → `李雷`/`lilei`/`alex`（`dsh-lizhichao-colin-8` → `dsh-lilei-alex-8`）；`陈景峰`/`chenjingfeng` → `王强`/`wangqiang`；`杨妙`/`yangmiao` → `刘洋`/`liuyang`；`郑晓婷` → `孙倩`；`lianchangliang`/`ranqiliang` → `acct-c`/`acct-b` |
| 邮箱 | `lzhichao@lagenio.com` → `lilei@example.com`、`funnywwh@gmail.com` → `owner@example.com` |
| 业务标识 | `wisskys` → `corp-a`、`liuhui` → `wangwu`、`zhuyecheng` → `zhangsan`、`LibbyGPT` → `ProviderA`、`antigravity` → `ProviderB`、`蓝精灵` → `测试标签`、`智天成`/`电商`/`收纳` → `客户组一`/`客户组二`/`客户组三`、`E26Q`/`e26q` → `K7QX`/`k7qx` |
| 凭据痕迹 | `sk-62e1a0b4c`/`sk-f69aeca55` → `sk-000000000`、`sk-gw-981065ae` → `sk-gw-00000000`、主机密钥指纹 `SHA256:TG5F…` → `SHA256:` + 43 个 `0` |
| 路径/用户 | ssh 用户 `winger` → `operator`；`/home/winger/...` → `/home/operator/...`（工具链 `.local`/`.cache` 两个子路径族除外） |
| 模块路径 | `github.com/winger/ai-gateway` → `github.com/funnywwh/ai-gateway`（与 origin 一致） |
| 文件改名 | `docs/design/m36-deploy-gpt001-prefix.md` → `docs/design/m36-deploy-prefix.md` |

**刻意保留**：`git@github.com:funnywwh/ai-gateway.git`（公开身份）、上游模型名（`gpt-5.6-luna` 等）、
第三方公共域名（`api.deepseek.com`/`feishu.cn`…）、`张三`/`李四`/`王五`/`赵六`/`周八` 这类合成夹具名、
`张伟`/`长伟`（多音字测试对）、`dsh-tenant`（无字母账号名的兜底租户名）、数据与压测数字。

## 4. 接口（`scripts/desensitize.py`）

```
python3 scripts/desensitize.py --check                 # 门禁入口：扫 tracked 文件，0 = 干净，1 = 有命中
python3 scripts/desensitize.py --check --strict        # 把例外路径也当普通文件（审计残留）
python3 scripts/desensitize.py --check --root DIR      # 扫目录（历史重写时用，不依赖 git index）
python3 scripts/desensitize.py --check-history         # 扫全部 ref 的全部 blob（重写后的复核）
python3 scripts/desensitize.py --apply [--root DIR]    # 就地替换 + 按 RENAMES 改名
python3 scripts/desensitize.py --inventory             # 打印当前命中表（用来补规则）
```

- 退出码：`0` 干净 / `1` 有错误级命中 / `2` 用法或环境错误。
- `--json` 输出机器可读结果；`--quiet` 只留汇总。
- 错误级规则（任一命中即失败）：`TOKENS` 命中、私网 IPv4、非文档网段公网 IPv4、非允许域名邮箱、
  真指纹形态、非允许用户名下的 `/home/<user>`。警告级：疑似真实 `sk-` 前缀（提醒补表，不拦）。
- 实现约束：按 **bytes** 读写且只在命中时写回（不改换行、不动其它字节）、跳过二进制（前 8KB 含 NUL）、
  跳过 `.git/.cache/bin/data/node_modules/__pycache__`、**幂等**（同一份规则跑两遍第二次 0 改动）。

## 5. 数据流

```
发版：  scripts/release.sh ──(脏树检查)──▶ desensitize.py --check ──0──▶ 写 VERSION → commit → tag → make build
                                              └─1─▶ 拒绝，打印 --apply 的修法
清洗：  desensitize.py --apply ──▶ 逐文件按 TOKENS 替换（模式长度降序，路径例外先判）──▶ RENAMES 改名 ──▶ 报告
重写：  git bundle（备份）──▶ filter-branch --tree-filter 'desensitize.py --root . --apply' ──▶ 全历史同规则
        ──▶ --check-history = 0 ──▶ 强推 main 与 tags ──▶ 新克隆复核
```

## 6. 异常与边界

- **例外路径上的命中**：`--check` 不报错，但 `--strict` 会列出来——残留（技能里的真实主机名与域名）是**已知且刻意**的，见 D2。
- **历史重写中的脚本位置**：tree-filter 在旧提交的树里跑，而 `scripts/desensitize.py` 是本次新加的文件，旧提交里没有它
  → 先把脚本复制到树外（`/tmp/dsh-rewrite/`），用绝对路径调用。
- **重写不可逆**：唯一退出路径是重写前打的 `git bundle`；强推前必须先 `git bundle verify` 并记下提交数/tag 数/HEAD 作为对账基线。
- **其它 clone**：强推后必须重新 clone（历史全部换了 sha）；部署机只收二进制，不受影响。
- **文档里的历史短 sha**：重写后不可解析，不改写（D8）。
- **二进制与产物**：`bin/`、`.cache/`、`data/` 不进 git，规则不碰；但清洗后必须 `make build`/`make dshgw-build` 重建，
  否则运行中的二进制里仍嵌着旧的控制台资源。

## 7. 测试策略

- `scripts/test_desensitize.py`（纯 `python3`，与 `scripts/test_*.py` 同风格）：规则命中/最长优先/边界规则（模块路径与 origin 不被裸词规则打坏）/
  路径例外/工具链模式例外/二进制跳过/幂等/退出码/`--root`/临时 git 仓库上的 `--check-history` 端到端/负例。
- 回归：`make verify`（vet + 全量 go test + 控制台 node 断言 + build）、`make dshgw-test`（含 node 与 python 夹具测试）、
  `go build ./...`（模块路径改名后必须重编全部包）。
- 门禁负例：临时塞一条真实 IP → `--check` 退出 1、`scripts/release.sh patch` 拒绝。
- 历史：重写前 `--check-history` 应报出表内命中（证明扫描真的在扫），重写后为 0；`/tmp` 新克隆再跑一遍。

## 8. 依赖

- `git 2.53.0`（`filter-branch`、`rev-list --objects`、`bundle`）、`python3`（标准库：`re`/`ipaddress`/`subprocess`/`argparse`），无第三方依赖。
- 与代码无关：不改任何运行时行为（唯一例外是 `internal/dshgw/config/config.go` 的两个**默认值**，见 §9）。

## 9. 实现与设计差异

（实现完成后回填。）

