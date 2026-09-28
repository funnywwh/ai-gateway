# M89 代码脱敏：全仓清洗 + 发版强制检查 + 历史重写

> 状态：进行中（2026-09-24 起）。需求原话：「项目代码脱敏」「修改 skill 要求发布版本时脱敏」，
> 经确认的口径：**全仓库清洗**（代码 / 测试夹具 / 示例配置 / 脚本 / `docs/`）、**发布与部署入口列入例外保留真值**、
> **连 git 历史一起重写并强推 origin**、**Go 模块路径改为 origin 的路径**。

## 1. 目标

这个仓库同时是一条真实部署线的操作记录：主机别名、内网与公网地址、真实域名、个人姓名与邮箱、
主机密钥指纹、API Key 前缀、操作者的用户名与家目录、部署里的业务标识（上游账号/供应商/标签名、
飞书应用 ID、租户名）——它们出现在**代码、测试夹具、示例配置、脚本与文档**的每一类文件里。目标是：

1. 仓库（工作树 + 全部 git 历史）不再携带真实环境标识与个人信息；
2. 「脱敏」成为发版流程里**机器能拦住**的一步（技能写清楚 + `release.sh` 门禁）；
3. 运维链路（发版技能、`Makefile`/脚本里的本机工具链默认值、部署命令）不中断。

## 2. 关键决策

| # | 决策 | 取舍理由 |
|---|---|---|
| D1 | 规则分两层：**提交进仓库的泛化规则**（`scripts/desensitize.py`：模块路径归属、私网段、非文档网段公网地址、非允许域名邮箱、真指纹形态、非允许用户名下的 `/home/<user>`、可疑 `sk-` 前缀）与**不入 git 的局部表**（`.cache/desensitize/tokens.tsv`：真实值 → 占位、文件改名、`[exempt]` 片段） | 最初的方案是把整张表放进脚本，实现时被自己的规则打脸：**列出真实值的表本身就是它要防的泄漏**，`--apply` 甚至会把表里的模式串改掉。所以「已知真实值」必须留在操作者机器上（`.cache/` 已在 `.gitignore` 里），仓库里只留不依赖秘密的泛化规则——新克隆仍然拦得住真正危险的那几类 |
| D2 | 例外分两层：**路径例外**（`.dsh/skills/**` 整体不动）与**片段例外**（表里的 `[exempt]` 正则，例：操作者家目录下的 `.local`/`.cache` 工具链族） | 路径例外保住运维 runbook 里的真实主机名；片段例外保住 `Makefile`/`scripts/**` 的默认值。工具链路径在文档里被引用（`docs/deployment-layout.md` 等），若只豁免 `Makefile` 与 `scripts/**`，文档里的同一条路径会被改掉、与脚本默认值对不上——那才是真正的坑 |
| D3 | 占位值用 RFC 5737 / RFC 2606 的文档地址族与中性主机别名（`gw-a`…`gw-d`、`*.example.com/org/net`），内网真实地址 → `192.0.2.101`…，公网真实地址 → `198.51.100.101`…；人名连带拼音 slug 一起换（租户名由账号名派生，测试两头都钉） | 一眼可辨识是假值；不落进任何真实网段；与仓库里已在用的 `192.0.2.x`/`198.51.100.x` 测试值不冲突 |
| D4 | 规则按**字面长度降序**执行（不是模式串长度——`(?<!…)x(?!…)` 的括号会骗过排序）；裸词规则带边界，且**只带需要的边界** | 排序错一次就会把邮箱里的账号名先换掉、留下一个公共邮箱域名（实测）；边界错一次就打坏 `github.com/<owner>/ai-gateway`（模块路径）与 `git@github.com:<owner>/ai-gateway.git`（origin）——这两处是刻意保留的公开身份 |
| D5 | 显式表之外还有**泛化拒绝规则**，且规则要能命中**粘连与转义**形态：被前一个词粘住的主机别名（用户原话里的引文）、与后缀连写的拼音 slug（租户名）、被源码字符串转义序列顶开、词边界根本不存在的用户名 | 这些都是实现中真实踩到的漏网：按 `\b…\b` 写的规则对它们视而不见，而它们在仓库里就是明文。泛化规则只挡住**已知类别**，粘连/转义是显式表必须自带的能力 |
| D6 | 发版门禁：`scripts/release.sh` 在脏树检查之后、写 `VERSION` 之前调用 `--check --require-table`；技能里写成「第 2 步」 | 技能是给人的流程，`release.sh` 是机器的闸门，两者都不可省。`--require-table` 让「局部表丢了」变成硬失败——少了它，门禁会在操作者机器上静默退化成只跑泛化规则 |
| D7 | 历史重写用 `git filter-branch --tree-filter` 跑**同一份脚本**，并用 `--msg-filter` 跑同一条 `--filter-message` 入口 | 本机没装 `git filter-repo`。tree-filter 逐提交套用同一份规则，语义与工作树天然一致；**提交信息里也有真实值**（部署记录），只重写树是不够的。代价是慢（479 个提交，分钟级） |
| D8 | 文档里的历史 revision 短 sha **不改写** | 重写会让它们不可解析，但把旧→新 sha 映射回填等于把 479 个提交再改一遍，收益为零。作为已知后果记在这里 |
| D9 | `docs/todo_done.md` 的「只增不改」约定让位一次 | 它是**记录**，脱敏是措辞改写、事实与数据不变。这一例外必须写明，否则下一个人会以为记录被动过 |
| D10 | 运维验证脚本（`scripts/test_dshgw_ssh_identity.py` 等）里的租户名同样被换成占位，因为它们是**自建夹具**的离线测试（临时目录里跑一遍脚本），不连真机 | 只有连真机的入口才需要真实值；这些脚本读自己的夹具，换名不影响其保证。连真机的入口只有发版技能（D2 的路径例外保留真值） |

## 3. 替换口径（类别 + 占位约定）

真实值与占位的**逐条映射在操作者的局部表里**（`.cache/desensitize/tokens.tsv`，格式见 §4）。
仓库里只记录约定，不记录值：

| 类别 | 占位约定 |
|---|---|
| 主机别名 | `gw-a` / `gw-b` / `gw-c` / `gw-d`（沿用它们在部署里的角色顺序） |
| 域名 | `*.example.com`（主域名）、`*.example.net`（同实例的别名域名）、`*.example.org`（旧域名）、`chat.example.com`（租户面）、`example.com`/`example.net`（裸域名） |
| 地址 | 内网 → `192.0.2.101` 起（RFC 5737 TEST-NET-1），公网 → `198.51.100.101` 起（TEST-NET-2）；带端口的网关地址用 `aigw.internal`（比裸 IP 规则长，优先命中） |
| 人 | 中文名换成通用假名，**拼音 slug 同步换**（租户名 = `dsh-<slug>-<id>`，测试两头都断言）；多音字、以及与其他字连读后拼音变形的名字，都在表里按「实际出现的字符串」逐条列出 |
| 邮箱 | `<某人>@example.com`、`owner@example.com` |
| 业务标识 | 供应商/上游账号 → `ProviderA`/`corp-a`/`acct-a`…，标签 → `测试标签`/`客户组一`…，Key 名 → `K7QX` |
| 凭据痕迹 | Key 前缀 → `sk-000000000` / `sk-gw-00000000`，主机密钥指纹 → `SHA256:` + 43 个 `0`，飞书应用 ID → `cli_0000000000000000` |
| 路径/用户 | ssh 用户名 → `operator`，家目录 → `/home/operator/...`（工具链族除外，见 D2） |
| 模块路径 | 改成 origin 的 `github.com/<owner>/ai-gateway`（泛化规则：**任何**别的 owner 都算泄漏并改写过来，所以仓库里不留旧值） |
| 文件改名 | 名字里带真实主机的设计文档改为中性名（`m36-deploy-<host>-prefix.md` → `m36-deploy-prefix.md`） |

**刻意保留**：origin 的 URL 与模块路径（公开身份）、上游模型名（`gpt-5.6-luna` 等）、第三方公共域名
（`api.deepseek.com`/`feishu.cn`…）、合成夹具名（`张三`/`李四`/`alice`…）、多音字测试对（`张伟`/`长伟`）、
无字母账号名的兜底租户名 `dsh-tenant`、二进制与构建产物的 SHA-256 摘要、数据与压测数字。

## 4. 接口（`scripts/desensitize.py`）

```
python3 scripts/desensitize.py --check                    # 扫 tracked 文件；0 = 干净，1 = 有命中
python3 scripts/desensitize.py --check --require-table     # 局部表不在就直接失败（发版门禁）
python3 scripts/desensitize.py --check --strict            # 把例外路径也当普通文件（审计残留）
python3 scripts/desensitize.py --check --root DIR          # 扫目录（历史重写时用，不依赖 git index）
python3 scripts/desensitize.py --check-history [--messages]     # 全部 ref 的全部 blob（+ 提交信息）
python3 scripts/desensitize.py --apply [--root DIR]        # 就地替换 + 按表改名
python3 scripts/desensitize.py --inventory [--json]        # 按规则汇总当前命中，用来补表
python3 scripts/desensitize.py --filter-message            # stdin → 脱敏后的提交信息（--msg-filter）
```

局部表格式（TAB 分隔，`#` 注释，节头切换语义）：

```
[token]  name <TAB> regex <TAB> replacement     # regex 是正则，按字面长度降序执行
[rename] old-path <TAB> new-path
[exempt] regex                                  # 命中的片段规则不看（工具链路径族）
```

- 退出码：`0` 干净 / `1` 有错误级命中 / `2` 用法或环境错误（含 `--require-table` 缺表）。
- 错误级：表命中、私网 IPv4、非文档网段公网 IPv4、非允许域名邮箱、真指纹形态、模块路径 owner 不符、
  非允许用户名下的 `/home/<user>`；警告级：疑似真实 `sk-` 前缀（提醒补表，不拦）。
- 实现约束：按 **bytes** 读写且只在命中时写回（不改换行、不动其它字节）、跳过二进制（前 8KB 含 NUL）、
  跳过 `.git/.cache/bin/data/node_modules/__pycache__`、`[exempt]` 片段原样复制、**幂等**（跑两遍第二次 0 改动）。

## 5. 数据流

```
发版：  scripts/release.sh ──(脏树检查)──▶ desensitize.py --check --require-table ──0──▶ 写 VERSION → commit → tag → make build
                                                       └─1/2─▶ 拒绝，打印 --apply 的修法
清洗：  desensitize.py --apply ──▶ 逐文件逐片段套用表（字面长度降序）──▶ 按表改名 ──▶ 报告
新泄漏：--inventory 打印命中的规则与样例 ──▶ 把新值补进 [token] ──▶ --apply
重写：  git bundle（备份）──▶ filter-branch --tree-filter 'desensitize.py --root . --apply'
        + --msg-filter 'desensitize.py --filter-message' ──▶ --check-history --messages = 0
        ──▶ 强推 main 与 tags ──▶ 新克隆复核
```

## 6. 异常与边界

- **局部表缺失**：`--check` 仍跑泛化规则并打印提醒；`--require-table` 直接失败（发版路径用它）。表不入 git（D1）。
- **例外片段**：`[exempt]` 命中的片段连规则都不看；`--strict` 会把整份文件当普通文件扫，用来审计刻意保留的残留。
- **转义与粘连**（D5）：Go/JSON 字符串里的 `\n`/`\t` 会把 token 顶到一个词字符后面，`\b` 类规则看不见；
  拼音 slug 会与后缀连写（`dsh-<slug><suffix>-<id>`）。表里为这两类各留了专门的行。
- **生成的产物**：`bin/`、`.cache/`、`data/` 不进 git，规则不碰；但清洗后必须 `make build`/`make dshgw-build` 重建，
  否则运行中的二进制里仍嵌着旧的控制台资源。三个带 `.src.js` 的插件要能被 `build-client.mjs` 重建出**逐字节相同**的 `client.js`。
- **历史重写中的脚本位置**：tree-filter 在旧提交的树里跑，而脚本是本次新加的文件 → 先把脚本复制到树外（`/tmp/dsh-rewrite/`），
  用绝对路径调用；局部表也必须从树外读（`GW_DESENSITIZE_TABLE`）。
- **重写不可逆**：唯一退出路径是重写前打的 `git bundle`；强推前先 `git bundle verify` 并记下提交数/tag 数/HEAD 作为对账基线。
- **其它 clone**：强推后必须重新 clone（历史全部换了 sha）；部署机只收二进制，不受影响。
- **文档里的历史短 sha**：重写后不可解析，不改写（D8）。

## 7. 测试策略

- `scripts/test_desensitize.py`（纯 `python3`，与 `scripts/test_*.py` 同风格）：
  表的解析（含错误行）、最长字面优先、例外片段、模块路径 owner（用 origin override）、泛化规则命中与允许名单、
  `--apply` 的幂等/二进制跳过/改名、缺表时 `--require-table` 的退出码、临时 git 仓库上的 `--check-history`
  与 `--messages`、`--filter-message`。
  夹具**在运行时拼装**（例如 `"192.168." + "77.7"`）：本文件也在 `git ls-files` 里、门禁会扫它，所以它自己不能含
  规则要抓的字符串——这条约束本身就是一道回归线。
- 回归：`make test`（全量 go test）、`make ui-base`（控制台 node 断言）、`make dshgw-test`（dshgw 的 go/node/python 夹具）、
  `go build ./...`（模块路径改名后必须重编全部包）、三个插件的 `build-client.mjs` 重建一致性。
- 门禁负例：临时塞一条真实 IP → `--check` 退出 1、`scripts/release.sh` 拒绝发布。
- 历史：重写前 `--check-history --messages` 应报出命中（证明扫描有效），重写后为 0；`/tmp` 新克隆再跑一遍。

## 8. 依赖

- `git 2.53.0`（`filter-branch`、`rev-list --objects`、`bundle`）、`python3`（标准库：`re`/`ipaddress`/`subprocess`/`argparse`），无第三方依赖。
- 与运行时行为无关，唯一例外是 `internal/dshgw/config/config.go` 的两个**默认值**（`public_host`/`aigw_base_url`）——
  它们原本指向真实主机与域名，现为占位；线上依赖默认值的部署必须在 `dshgw.yaml` 显式写出（示例配置本来就是显式的）。

## 9. 实现与设计差异

- **D1 是改出来的**：计划里规则表放在提交进仓库的 `scripts/desensitize.py` 里；第一次 `--apply` 把表自己的模式串
  也替换掉了，暴露了「表即泄漏」这一硬伤。改成「泛化规则入库 + 真实值入 `.cache/desensitize/tokens.tsv`（gitignored）」，
  并新增 `--require-table` 让发版门禁在缺表时硬失败。
- **D4 的排序是实现中修掉的**：按模式串长度排序时，带 lookaround 的裸词规则排到了邮箱规则前面，先把邮箱里的用户名换掉、留下了公共邮箱域名；改成按「剥掉正则语法后的字面长度」排序。
- **D5 的三类漏网都是实测抓到的**：被前一个词粘住的主机别名（出现在用户原话的引文里）、与后缀连写的拼音 slug
  （由 `internal/httpapi/admin_dsh_tenant_name_test.go` 的断言抓出）、被 Go 字符串转义序列顶开的用户名。
  另外还补进了两个真实租户 slug 与两个飞书应用 ID。
- **本里程碑不修但记录在案的既存问题**：`make vet` 在 `internal/dshgw/config/sandboxview_test.go` 上因
  `go vet` 的 copylocks 分析失败（`candidate := *base` 复制含 `sync.RWMutex` 的 `Config`）——已核对 pristine HEAD 同样失败，
  与本次改动无关，故不在本里程碑范围内（验收记录里如实写明）。
