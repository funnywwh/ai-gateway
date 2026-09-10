# M0 设计文档：仓库脚手架与模块骨架

## 目标
建立可编译、可测试、可提交的仓库骨架，固定后续所有里程碑都遵守的工程约定：
构建环境、模块分层、配置装载、日志、ID 生成、领域接口骨架。

## 关键决策
1. **构建环境内嵌化**：`scripts/goenv.sh` 把 GOPATH/GOMODCACHE/GOCACHE 指向工作区 `.cache/`，
   并把只读的 `$HOME/go/pkg/mod/cache/download` 作为 file:// 代理首选项。
   原因：沙箱内 `$HOME` 只读、无 C 编译器，必须 `CGO_ENABLED=0` 且缓存可写。
2. **模块路径** `github.com/winger/ai-gateway`；公开 SDK 只放 `pkg/`，其余全部 `internal/`。
3. **分层（依赖单向）**：`internal/domain` 为依赖根（纯类型 + 接口，零内部依赖），
   后续 `store/registry/routing/billing/...` 只依赖 `domain` 的接口，装配集中在 `cmd/aigw`。
4. **日志** 统一用标准库 `log/slog`（text/json 可切）。
5. **ID** 统一走 `internal/ids`，前缀与 OpenAI 对象命名一致（`resp_`、`msg_`、`fc_`、`rs_`）。

## 接口（M0 产出）
- `internal/domain`：实体（Account/Provider/ProviderModel/Model/ModelMapping/Route/Tag/APIKey/…）、
  端口接口（Store 及服务接口骨架）、`APIError`（映射 OpenAI 错误封装）。
- `internal/config`：`Config` 覆盖 server/database/auth/plugins/routing/billing/recording/mcp/hooks/ratelimit/backup/log/bootstrap，
  含默认值、`GW_*` 环境变量覆盖与校验。
- `internal/logx`、`internal/ids`。

## 数据流
M0 无请求路径。`cmd/aigw` 启动流程：解析 flag → 载入配置（缺省文件则用默认值）→ 建日志 → 打印启动信息。

## 异常与边界
- 配置文件不存在：使用内置默认值（方便首次启动），不报错。
- 配置文件存在但解析失败：退出码 2，stderr 打印。
- 校验失败（listen 为空、billing 取值非法等）：退出码 2。

## 测试策略
- `internal/config`：默认值、YAML 覆盖、`GW_*` 覆盖、校验失败分支。
- `internal/ids`：前缀与长度、随机性基本断言。
- `internal/domain`：`APIError` 状态码/类型映射。
- 全部单测不依赖 DB/进程/网络。

## 依赖
仅标准库 + `gopkg.in/yaml.v3`（配置解析）。SQLite 驱动在 M1 引入。

## 实现与设计差异
- 计划用 `fmt.Fprintf` 输出错误并带换行转义，实际改为 `fmt.Fprintln`：生成代码时反斜杠转义会被还原成真实换行，
  因此本仓库约定**生成 Go/YAML 源码时避免反斜杠转义**（多行字面量一律用原始字符串）。
- `config.example.yaml` 与 `config.Default()` 对齐，并额外补了 `log` 段与注释。
- 除计划的 4 个包外，未新增其它依赖；`gopkg.in/yaml.v3 v3.0.1` 经 file:// 本地代理解析成功。
