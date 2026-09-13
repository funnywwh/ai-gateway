---
name: release-version
description: 发布 ai-gateway 的一个版本：升 VERSION（a.b.c）、提交打 tag、构建带版本号与 revision 的二进制、部署到 gpt001、并用 /version 与后台角标验证线上版本。当用户说「发布版本 / 发版 / release / 升版本号 / 部署新版本」时使用。
whenToUse: 用户要发布或部署 ai-gateway 的新版本，或要确认线上跑的是哪个版本时。
---

# 发布 ai-gateway 版本

版本号的**真值在仓库根的 `VERSION` 文件**（`a.b.c`），revision 是构建时的 `git rev-parse --short HEAD`。
两者由 `make build` 通过 `-ldflags -X` 注入二进制，再由三处对外暴露：

| 出口 | 形态 | 用途 |
|---|---|---|
| `GET /version` | `{"version":"0.1.0","revision":"56df9b5"}` | 脚本/探针（不带 base_path 前缀时在根，gpt001 上是 `/aigw/version`） |
| `GET /healthz` | `{"status":"ok","version":…,"revision":…}` | 部署校验 |
| 控制台左上角 | `AI Gateway  v0.1.0  56df9b5` | 人眼一眼看到 |

**不要**用 `git describe` 当版本号：仓库没有 tag 时它给的是 commit 前缀（看起来像版本、却跟任何东西都没法比）。
`VERSION` 的内容必须是 `a.b.c`，否则 `make build` 直接失败（`version-check`）。

## 步骤

### 1. 看现状，定 bump

```bash
cd /home/winger/ai-gateway
git status --porcelain          # 必须干净：发版要把版本号提交进 tag
./scripts/release.sh            # 打印当前版本与 patch/minor/major 三个候选
git log --oneline $(git describe --tags --abbrev=0 2>/dev/null || echo HEAD~10)..HEAD
```

选档位：

- **patch**：只修缺陷（本次 `response_format` 这类修复）；
- **minor**：新增对外能力（新端点、新配置项、新控制台页面）；
- **major**：破坏性变更（改接口形状、改默认语义、需要运维改配置才能继续跑）。

### 2. 升版本并构建

```bash
./scripts/release.sh patch        # 改 VERSION + 提交 + 打 v0.1.1 标签 + make build
./scripts/release.sh patch --no-tag   # 只改 VERSION 并构建，不提交/不打 tag
./bin/aigw -version               # aigw 0.1.1 (revision <sha>, built …)
```

`--no-tag` 用在"先构建验证、确认没问题再提交"的场合；正常发布不加。

### 3. 部署到 gpt001

部署是**独立一步**（构建物先落地、再切服务），这样部署失败可以重试或回滚，而不用再发一个版本号。

```bash
# 3.1 上传（先落到临时名，避免覆盖正在运行的二进制）
scp bin/aigw gpt001:/opt/aigw/aigw.new

# 3.2 备份线上二进制（回滚点），换入新版本并重启
ssh gpt001 'set -e
  cp /opt/aigw/aigw /opt/aigw/aigw.prev-$(date +%Y%m%d-%H%M%S)
  install -m 0755 /opt/aigw/aigw.new /opt/aigw/aigw
  rm -f /opt/aigw/aigw.new
  systemctl restart aigw
  sleep 1
  systemctl is-active aigw'
```

`/opt/aigw/config.yaml` 与 `/opt/aigw/data/` 不由发布流程改动；发布只换二进制。

### 4. 验证（必做，缺一不可）

```bash
# 4.1 版本号与 revision 真的生效了（这是本版新增的能力，也是最快的自检）
ssh gpt001 'curl -s localhost:8088/aigw/version; echo'
#   期望：{"revision":"<新短 sha>","version":"<新 a.b.c>"}

# 4.2 探针
ssh gpt001 'curl -s -o /dev/null -w "healthz=%{http_code}\n" localhost:8088/aigw/healthz
             curl -s -o /dev/null -w "readyz=%{http_code}\n"  localhost:8088/aigw/readyz'

# 4.3 启动日志：版本号是 a.b.c、且没有 ERROR
ssh gpt001 'journalctl -u aigw --since "-3min" --no-pager | grep -E "aigw starting|level=ERROR"'

# 4.4 控制台角标：浏览器打开 https://mnl.iotalking.top/aigw/admin/ui/ ，
#     左上角应为「AI Gateway  v<a.b.c>  <短 sha>」；或直接看资源与端点
curl -s https://mnl.iotalking.top/aigw/version
```

### 5. 回滚（部署后发现问题时）

```bash
ssh gpt001 'ls -t /opt/aigw/aigw.prev-* | head -1'   # 找最近的备份
ssh gpt001 'cp /opt/aigw/aigw.prev-<时间戳> /opt/aigw/aigw && systemctl restart aigw'
```

版本号会退回上一个 `a.b.c`，这就是"线上跑的是哪个版本"能当回滚依据的原因。

### 6. 记录

在 `docs/TODO.md` 当前里程碑下追加发布记录（日期、版本号、revision、回滚点路径、验证结果），
然后提交：

```bash
git add docs/TODO.md && git commit -m "release: v<版本> 部署记录"
```

## 注意

- **仓库脏就不发版**：`scripts/release.sh` 会直接拒绝。tag 必须指向包含它名字里那个版本的 commit。
- **`VERSION` 改了就要提交**：只改文件不提交，构建出来的版本号在 `git log` 里查不到来源。
- **不要手改 `/opt/aigw/config.yaml` 来"配合"发布**：发布只换二进制；配置变更走单独一步并记录原因。
- **本次发布顺带修掉的坑**：如果线上某个 `openai-chat` 供应商的 `config.response_format` 是 `json_object`
  且碰到"普通请求全 400（`Prompt must contain the word 'json' …`）"，那是因为 0.1.1 之前该字段被无条件下发。
  升级到含 M39 修复的版本后，该字段只作能力申报，普通请求不再带 `response_format`。
