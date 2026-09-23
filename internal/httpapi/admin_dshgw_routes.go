package httpapi

// dshgwAdminRoutes is the multi-machine node surface (M77) plus the two tenant operations the
// 节点 page needs (a restart and a placement change).
//
// Everything here is `admin`: registering a machine, deploying to it, moving a tenant's data and
// purging a node's state are all decisions that change where a tenant's files live.
func (s *Server) dshgwAdminRoutes() []adminRoute {
	return []adminRoute{
		{
			Method: "GET", Path: "/admin/api/v1/dshgw/nodes", Handler: s.handleAdminListDshgwNodes,
			Name: "admin_dshgw_node_list", Group: groupDshgw, Role: roleViewer,
			Summary: "工作节点清单（M77）：注册信息、最近一次状态、可提供的能力与租户数",
			Notes: "默认**不探测**：`reachable` / `features` 来自记录里最近一次探测结果，因此这个接口是廉价的，" +
				"控制台可以随页面刷新调用。加 `probe=true` 才会逐个节点发起一次健康检查（走局域网，慢），" +
				"并把结果写回记录。单机部署（没有 `nodes:`、没有注册过节点）返回空清单。",
			Query: []adminField{
				queryParam("probe", "boolean",
					"true=现在就探测每个节点（会写回 `reachable`/`version`/`features`），默认 false=只读记录里的最近结果"),
			},
		},
		{
			Method: "POST", Path: "/admin/api/v1/dshgw/nodes", Handler: s.handleAdminAddDshgwNode,
			Name: "admin_dshgw_node_add", Group: groupDshgw, Role: roleAdmin,
			Summary: "注册一个工作节点（只写记录，不碰目标机；安装用 deploy 接口）",
			Notes: "注册与安装是两件不同的事，也是两种不同的失败：这个接口只把「这台机器是谁、怎么连、装在哪里」" +
				"写进控制面的节点记录，目标机上什么都不会发生。写完之后用 `deploy` 接口一键安装。" +
				"名字必须匹配 `^[a-z][a-z0-9-]{0,25}$` 且不能是 `local`（`local` 保留给控制面自己）。",
			Body: []adminField{
				bodyRequired("name", "string", "节点名（^[a-z][a-z0-9-]{0,25}$，不能是 local）"),
				bodyRequired("listen", "string",
					"节点在**目标机上**绑定的地址 host:port（例如 192.168.190.87:18400）。不能是 0.0.0.0：控制面用它作 URL"),
				bodyRequired("ssh_host", "string", "ssh 目标主机或 IP"),
				bodyRequired("ssh_user", "string", "ssh 账号（只支持密钥认证，不支持密码）"),
				bodyOptional("ssh_port", "integer", "ssh 端口，默认 22"),
				bodyOptional("ssh_key_file", "string",
					"控制面上的私钥路径；默认 <state_dir>/node-ssh/<节点名>/id_ed25519（可用 `node update --ssh-key-in` 上传）"),
				bodyOptional("url", "string",
					"控制面访问该节点的 URL；默认由 ssh_host + listen 端口推导（http://<ssh_host>:<端口>）"),
				bodyOptional("deploy_dir", "string", "目标机上的部署根目录，默认 /srv/dshgw-node（状态在 <deploy_dir>/state）"),
				bodyOptional("node_state_dir", "string", "目标机上的状态目录，默认 <deploy_dir>/state"),
				bodyOptional("plugin_dir", "string", "目标机上的插件目录，默认 <deploy_dir>/plugins"),
				bodyOptional("template_home", "string", "目标机上的模板目录，默认 <deploy_dir>/template-home"),
				bodyOptional("token_path", "string", "目标机上的令牌文件，默认 <deploy_dir>/<节点名>.token"),
				bodyOptional("worker_port_lo", "integer", "目标机上租户 worker 端口带下界，默认 32800"),
				bodyOptional("worker_port_hi", "integer", "目标机上租户 worker 端口带上界，默认 32899"),
				bodyOptional("bwrap_bin", "string", "目标机上的 bubblewrap 路径，默认 /usr/bin/bwrap"),
				bodyOptional("node_bin", "string", "目标机上的 Node 运行时路径（预检要求可执行）"),
				bodyOptional("bin_js", "string", "目标机上的 dsh 启动脚本路径（预检要求可读）"),
				bodyOptional("current_link", "string", "目标机上的 dsh release 目录"),
				bodyOptional("default", "boolean", "true=同时设为新租户的默认落点"),
			},
		},
		{
			Method: "PATCH", Path: "/admin/api/v1/dshgw/nodes/{name}", Handler: s.handleAdminUpdateDshgwNode,
			Name: "admin_dshgw_node_update", Group: groupDshgw, Role: roleAdmin,
			Summary: "修改节点记录（只改传了的字段；不会自动重新部署）",
			Notes: "改了路径或运行时位置之后需要再调一次 `deploy`，否则目标机上的配置仍是旧的。" +
				"对配置文件中声明的节点，这个接口会拒绝：身份由配置文件决定，记录只保存部署元数据。",
			Params: []adminField{pathParam("name", "节点名")},
			Body: []adminField{
				bodyOptional("listen", "string", "目标机上的监听地址 host:port（改了要重新 deploy）"),
				bodyOptional("url", "string", "控制面访问该节点的 URL"),
				bodyOptional("ssh_host", "string", "ssh 目标主机或 IP"),
				bodyOptional("ssh_port", "integer", "ssh 端口"),
				bodyOptional("ssh_user", "string", "ssh 账号"),
				bodyOptional("ssh_key_file", "string", "控制面上的私钥路径"),
				bodyOptional("deploy_dir", "string", "目标机上的部署根目录"),
				bodyOptional("node_state_dir", "string", "目标机上的状态目录"),
				bodyOptional("plugin_dir", "string", "目标机上的插件目录"),
				bodyOptional("template_home", "string", "目标机上的模板目录"),
				bodyOptional("token_path", "string", "目标机上的令牌文件"),
				bodyOptional("worker_port_lo", "integer", "worker 端口带下界"),
				bodyOptional("worker_port_hi", "integer", "worker 端口带上界"),
				bodyOptional("bwrap_bin", "string", "目标机上的 bubblewrap 路径"),
				bodyOptional("node_bin", "string", "目标机上的 Node 运行时路径"),
				bodyOptional("bin_js", "string", "目标机上的 dsh 启动脚本路径"),
				bodyOptional("current_link", "string", "目标机上的 dsh release 目录"),
				bodyOptional("default", "boolean", "true=设为默认落点"),
			},
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/dshgw/nodes/{name}", Handler: s.handleAdminRemoveDshgwNode,
			Name: "admin_dshgw_node_remove", Group: groupDshgw, Role: roleAdmin,
			Summary:   "忘记一个节点；`purge=true` 还会停掉它并删除它在目标机上的一切（含租户数据）",
			Dangerous: true, ConfirmReason: "purge=true 会删除该节点上的租户工作区、备份与状态目录，不可恢复",
			Notes: "默认只删除控制面的记录，目标机分毫不动（再部署一次就能回来）。" +
				"`purge=true` 需要 `confirm=<节点名>`，它会停掉 systemd 用户单元、删除部署产物与状态目录。" +
				"节点上还有租户时两种方式都会被拒绝：先用 `tenant-set-node` 把租户挪走。",
			Params: []adminField{pathParam("name", "节点名")},
			Query: []adminField{
				queryParam("purge", "boolean", "true=同时停掉节点并删除目标机上的部署与状态（默认 false）"),
				queryParam("confirm", "string", "purge=true 时必须等于节点名，防误删"),
			},
		},
		{
			Method: "POST", Path: "/admin/api/v1/dshgw/nodes/{name}/deploy", Handler: s.handleAdminDeployDshgwNode,
			Name: "admin_dshgw_node_deploy", Group: groupDshgw, Role: roleAdmin,
			Summary: "一键部署/升级：ssh 预检 → 上传 → 激活 → 安装单元 → 启动 → 校验（失败回滚）",
			Notes: "**异步**：返回 202 与一个「正在部署」的状态，随后用 `GET .../nodes/{name}/deploy` 跟进度（阶段表 + 日志尾部）。" +
				"第一次部署会停在主机指纹确认：返回的 `deploy.error` 里带 SHA256 指纹，确认后用 `accept_host_key` 重发。" +
				"节点上的 `state/` 目录永远不会被部署触碰——租户数据在升级中保持不变；失败时自动回滚到上一版二进制与配置。" +
				"目标机没有可用的 systemd 用户管理器时（容器、未开 linger 的会话）会退化为 detached 启动，`systemd=false` 即表示「重启机器不会自动拉起」。",
			Params: []adminField{pathParam("name", "节点名")},
			Body: []adminField{
				bodyOptional("accept_host_key", "string",
					"确认目标机的主机密钥指纹（SHA256:...）。首次部署必须带上它，之后记录里已固定，重装无需再传"),
				bodyOptional("prepare_template_on_node", "boolean",
					"true=不传模板，让节点自己制备（需要节点上有 corepack 与网络）。默认 false=从控制面传已备好的模板"),
				bodyOptional("with_packages", "boolean",
					"true=用 `sudo -n apt-get` 安装缺失的系统包（需要目标机免密码 sudo）。默认 false，缺包时预检直接报错"),
				bodyOptional("rotate_token", "boolean", "true=同时更换节点共享密钥（控制面记录一起更新）"),
			},
		},
		{
			Method: "GET", Path: "/admin/api/v1/dshgw/nodes/{name}/deploy", Handler: s.handleAdminDshgwNodeDeployStatus,
			Name: "admin_dshgw_node_deploy_status", Group: groupDshgw, Role: roleViewer,
			Summary: "部署进度：是否在跑、当前阶段、各阶段结果、日志尾部与最终状态",
			Notes: "`running=false` 时 `state` 是 `ready`（成功）或 `failed`（失败，`error` 说明原因，`log_tail` 是现场）。" +
				"`phases` 每个阶段带耗时，`systemd` 说明是 systemd 用户单元还是退化启动，" +
				"`fingerprint` 是本次固定的主机密钥指纹。日志有界（服务端只返回尾部 64 KiB）。",
			Params: []adminField{pathParam("name", "节点名")},
		},
		{
			Method: "POST", Path: "/admin/api/v1/dshgw/nodes/{name}/rotate-token", Handler: s.handleAdminRotateDshgwNodeToken,
			Name: "admin_dshgw_node_rotate_token", Group: groupDshgw, Role: roleAdmin,
			Summary:   "更换节点共享密钥：目标机与控制面记录同时更新（一次重部署）",
			Dangerous: true, ConfirmReason: "轮换期间该节点的租户流量会短暂失败；控制面在部署激活后立即换用新密钥",
			Notes: "与 `deploy` 一样是异步的（返回 202），用 `GET .../nodes/{name}/deploy` 跟进度。" +
				"运行中的控制面会监视节点记录并自动换用新密钥，不需要重启。",
			Params: []adminField{pathParam("name", "节点名")},
		},
		{
			Method: "POST", Path: "/admin/api/v1/dshgw/nodes/{name}/reconcile", Handler: s.handleAdminReconcileDshgwNode,
			Name: "admin_dshgw_node_reconcile", Group: groupDshgw, Role: roleAdmin,
			Summary: "把控制面的权威租户表推给该节点：启动该起的、停该停的、剪掉它自己多出来的记录",
			Notes: "返回 `started`/`stopped`/`pruned`：操作员据此看到这次对账**做了什么**，而不只是意图。" +
				"被剪掉的记录**不会删除数据**。控制面在每次启动时会对所有节点自动做一次同样的对账。",
			Params: []adminField{pathParam("name", "节点名")},
		},
		{
			Method: "GET", Path: "/admin/api/v1/dshgw/nodes/{name}/audit", Handler: s.handleAdminDshgwNodeAudit,
			Name: "admin_dshgw_node_audit", Group: groupDshgw, Role: roleViewer,
			Summary: "读该节点最近的安全事件（节点本地审计文件的尾部）",
			Notes: "节点侧事件（拒绝的 ssh 挂载、被迫拆挂载的登出）发生在挂载所在的机器上；控制面每 30 秒把它们并入" +
				"自己的审计流并打上节点标签，这个接口是「那台机器上到底发生了什么」的即时答案（原始 JSONL 行）。",
			Params: []adminField{pathParam("name", "节点名")},
			Query: []adminField{
				queryParam("lines", "integer", "返回最近多少行，1..500，默认 100"),
			},
		},
		{
			Method: "POST", Path: "/admin/api/v1/dshgw/tenants/{name}/restart", Handler: s.handleAdminRestartDshgwTenant,
			Name: "admin_dshgw_tenant_restart", Group: groupDshgw, Role: roleAdmin,
			Summary: "重启租户 worker（不动凭据、不动数据；租户在哪个节点就在哪个节点上重启）",
			Notes: "控制台用它在迁移后或 worker 卡死时恢复服务。它是一次原子操作而不是 stop+start 两次调用：" +
				"控制台在两次调用之间被打断会留下一个停着的租户。",
			Params: []adminField{pathParam("name", "租户名")},
		},
		{
			Method: "POST", Path: "/admin/api/v1/dshgw/tenants/{name}/node", Handler: s.handleAdminSetDshgwTenantNode,
			Name: "admin_dshgw_tenant_set_node", Group: groupDshgw, Role: roleAdmin,
			Summary:   "记录租户的落点（迁移；`node=local` 表示搬回控制面本机）",
			Dangerous: true, ConfirmReason: "租户数据存放在节点本地磁盘上；这个接口只改落点，不会搬运文件——数据要靠运维自己拷过去",
			Notes: "**不会搬运数据**：租户的工作区在原来那台机器的磁盘上，改落点只是让控制面以后去新机器找它。" +
				"运行中的租户会被拒绝（先停），否则会出现「控制面说的机器」和「数据所在的机器」不一致。" +
				"先停租户 → 把数据拷到新节点 → 再调这个接口 → 启动。",
			Params: []adminField{pathParam("name", "租户名")},
			Body: []adminField{
				bodyRequired("node", "string", "目标节点名；`local` 表示控制面本机"),
			},
		},
	}
}
