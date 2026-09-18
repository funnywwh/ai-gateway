# 本机 SSH 工作区更新记录（2026-09-19）

## 范围

用户确认仅更新本机，不发布 gpt001；核实 `dshgw-verify.service` 承载在用账号后，再次确认允许重启。

- 更新服务：用户级 `dshgw-verify.service`。
- 部署根：`/home/winger/work/ai_gateway`。
- 二进制：`bin/dshgw`。
- 版本：`2.0.0`，本机构建 revision：`bb32bdf-dirty-sshkeys2`。
- 构建时间：`2026-09-18T17:02:04Z`。
- 二进制 SHA256：`0fe8dda4eaf2f3bfb36c837b84c5d12c3cf45d1f5ea4b408073719f2d6598bbc`，已与运行进程 `/proc/<pid>/exe` 对比一致。
- 门户：`http://192.168.190.86:18300/`。
- 未修改 VERSION、未提交或打标签；未更新 aigw 服务、gpt001 或当前 3080 Harness GUI。
- 插件通过现有 sandbox 只读绑定的 `cmd/dshgw/plugin` 加载；重启账号进程后生效，客户端需要刷新。

## 备份

更新前的二进制备份：

`data/dshgw-verify/backups/ssh-identity-20260919-004724/dshgw`

同目录 `source.patch` 为首次构建前相关已跟踪源码的工作区 diff，不是完整发布包，也不包含随后发现的 SSHFS 别名修复。二进制备份不等于完整插件回滚，回滚时需一并选择兼容插件版本。

## 部署期间修复

首次重启后，原 aipc SSHFS 挂载留下失效 FUSE 端点，影响 dsh-tenant worker 启动。停止本机服务后仅卸载失效挂载点，再启动服务，未删除远端文件或修改任何账号私钥。

恢复时发现 SSHFS 不接受 `-o User=winger`。修复 `internal/dshgw/sshworkspace/exec.go`：将安全解析的别名 User/HostName 转为 SSHFS destination，Port 使用 `-p`，并保留显式用户/端口覆盖规则；不把租户输入插入 ssh_command。

重新构建重启后，通过本账号 mailbox 请求恢复原 `aipc:/home/winger/ZT20Q` 挂载，回执 `ok:true`；宿主及账号 sandbox 内均确认 `fuse.sshfs` 挂载存在，源为 `winger@192.168.140.252:/home/winger/ZT20Q`。

## 验证

- `make dshgw-test` 完整通过：Go dshgw/CLI/arch、picker-clamp 15 断言、SSH 插件后端 189 断言、客户端 40 断言、迁移及下线计划 dry-run。
- 真实 SSHFS 集成三个用例通过（未跳过）：默认私钥、主机专用私钥+显式端口、别名 HostName/User/Port；验证实际挂载、读写及卸载。
- systemd 服务 active，四个在用账号 worker ready。
- 携带正确 Host 请求门户 HTTP 200，未认证账号入口 HTTP 302（跳转登录）。
- 使用现有握手认证验证 dsh-tenant、dsh-colin、dsh-ranqiliang、dsh-lianchangliang 页面及插件资源均 HTTP 200；实际返回的插件包含端口、当前主机专用以及 identityStatus/identityUpload/identityDelete。
- 未在在用账号中上传或删除私钥进行验收，避免改动用户凭据；此部分由自动化测试覆盖。未做真实浏览器视觉验收。
- `git diff --check` 通过。

## 已知既有环境告警

`doctor` 对历史测试账号 verify1 报缺少 gateway.key 和 settings.yaml，启动日志也提示其旧 profile 无法自动添加 SSH 插件。其余四个在用账号检查正常。本次不为该测试账号生成凭据或修改配置。

## 使用

刷新现有账号页面，进入「SSH 工作区」，填写主机、用户名、端口，然后在「SSH 私钥」选择账号默认或当前主机专用，选择本地无密码私钥并上传。主机专用优先于账号默认，私钥最大 64 KiB。现有 HTTP 本机部署未改为 TLS，敏感私钥上传应在可信网络或 HTTPS 环境下进行。
