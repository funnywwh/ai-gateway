# M51 剩余真实主机验收执行边界（待运维确认）

门户保持 `https://chat.tirisen.hk:32600/`，租户继续按端口隔离。全部必需验收通过后才允许提交/发布；本文件不是通过报告。

## 1. 已有证据与缺口

- 双真实租户 baseline、A停用模型401、浏览器B登录及跨端口自动跳转已通过。
- 原生浏览器退出、外部设备可信TLS、目录选择及本机目录授权待回传。
- 真实新Key轮换、10–30 worker资源及限额命中、备份恢复仍未执行。
- race缺gcc为可选环境缺口，不能记录为PASS。

## 2. 真实Key轮换

新测试Key由管理员保存到 `/root/dshgw-e2e/b-next.key`，0600 root:root，授予deepseek-flash与小额余额，禁止传入命令行或回传明文。先核对B的UID/unit/config身份、旧Key可用、模型授权、余额，再记录worker PID和既有session标识（仅私有状态保存）。

执行前必须补齐针对当前host acceptance状态的rotation阶段：不能直接轮换后仍沿用旧key_prefix/config指纹执行cleanup，导致身份守卫报漂移。必须保留原不可变身份基线，并用独立rotation journal记录精确的合法前后态；不能直接覆写原state或泛化放松assert_identity。journal使用0600原子持久化并绑定本轮run/config/UID/created_at；异常中断须逐字段核对新旧指纹后重新协调，不默认清理陌生对象。cleanup只接受原基线或经journal证明的合法后态。

验收应验证同PID和同session下新Key生效、真实模型turn成功、旧prefix不再允许登录（不代表旧aigw Key已被停用），非测试租户完全不变。必须在轮换前订阅公开`credentials/reference-updated`事件并等待AIGW_API_KEY更新屏障；事件本身不证明请求实际用了新Key。应将测试请求关联到aigw实际api_key_id等非秘密归属证据，再与新测试Key的管理员元数据匹配；仅同PID+模型成功不足以排除旧Key继续生效。旧Key不得在轮换成功证据收齐前停用；不得公开browser token或Key指纹以外的凭据。

## 3. 资源验收

最小选择10个真实worker。需要预先说明创建测试租户数、专用Key数量、端口/UID预算和可用内存；禁止复制生产Key充当10个不同身份。先以只读方式核对全部既有tenant/unit，再逐个创建有唯一run标识的测试租户，记录UID和created_at。只停用/清理身份匹配且本轮创建的对象。

分两类证据：

1. 10个worker并行运行：每worker PID/UID、RSS、线程、cgroup memory.current/memory.high/memory.max/cpu.max/pids.max及父slice，记录总量；不发真实模型压力请求。
2. 限额命中：不得把现有 `dsh-workers.slice` 的40G顶满，也不得降低共享生产slice限额。需要独立临时slice及只作用于测试unit的runtime override，以降低后的安全阈值证明约束生效，采集memory.events/CPU throttling等证据，完成后撤销override并验证恢复。

第二类仅能证明隔离测试环境的限额机制；不能冒称真实共享40G限额被命中。如发布要求必须命中原40G阈值，需在隔离主机/VM有足够容量时执行，或由用户明确授权维护窗口。不得通过修改测试口径自行勾选完成。

## 4. 备份与恢复

代码审查发现现有 `dshgw backup` 会停止gateway及全部活动租户。它不是无停机的只读命令，未经维护窗口确认不执行。现有CLI没有通用restore子命令；`tenant remove`也不是无副作用的备份，它会删registry、会话和系统用户。

优先使用隔离VM/主机做完整灾备：真实backup产物私有传输，恢复至隔离路径和配置，验证archive路径/文件类型/owner/mode，恢复UID身份、配置、registry与DSH数据，禁止让恢复副本连生产凭据或占用生产端口。启动前明确Key有效性策略。恢复后验证身份一致、原session数据、workspace文件完整性、登录/worker请求与模型调用，并保存不含秘密的证据。

仅暂存解包和hash比对不算完整恢复验收。若在当前主机做备份停止/恢复演练，须用户确认维护窗口与受影响服务名单，预备二进制/配置回滚点，不得自行删改现有租户。

## 5. 发布门槛

本机没有root权限，root部署/破坏性验收由用户终端执行。外部TLS及浏览器权限对话框必须由实际外部设备/浏览器确认。各项证据记录到docs/todo_done.md，TODO只保留未完成项。经用户2026-09-16决定：M51以当前状态提交（不升VERSION、不打tag、不发布）；剩余验收项被M52方向取代优先级，M52设计见 docs/design/m52-dsh-enable.md（后台“启用/停用DSH”账号开关）。发布（含版本号）待后续里程碑另有决定时执行。
