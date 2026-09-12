package httpapi

// The console chat's management routes.
//
// Every entry is NoTool: a conversation and a skill library are owned by a logged-in
// administrator account (admin_users), and an MCP token has no such account to own them
// with. Offering them to an agent would also invite an agent to hold a billed conversation
// inside another agent's session, which is a loop nobody can audit.
//
// The Role column is the first of two gates: the route table decides who may reach the
// handler, and the chat service re-checks the role before it spends anything. A viewer can
// read their own conversations and keep their own skills; binding a billing key, asking a
// question and distilling a skill are administrator work, because they spend an account's
// balance.

const groupChat = "chat"

// chatNoTool is the reason shown to MCP clients for every chat route.
const chatNoTool = "会话与技能按登录的管理员账号归属，MCP 令牌没有该身份；" +
	"而且聊天会以某个 API Key 计费，不应由另一个 agent 的会话代持"

func (s *Server) chatAdminRoutes() []adminRoute {
	return []adminRoute{
		{
			Method: "GET", Path: "/admin/api/v1/chat/sessions", Handler: s.handleAdminChatListSessions,
			Name: "admin_chat_list_sessions", Group: groupChat, Role: roleViewer,
			Summary: "列出当前登录管理员的智能问答会话（按最近活动排序，只含自己的会话）",
			Query:   chatPageSpec.fields(),
			NoTool:  chatNoTool,
		},
		{
			Method: "POST", Path: "/admin/api/v1/chat/sessions", Handler: s.handleAdminChatCreateSession,
			Name: "admin_chat_create_session", Group: groupChat, Role: roleAdmin,
			Summary: "新建一个智能问答会话：绑定模型、计费账户与 API Key，可选是否允许写操作",
			Body: []adminField{
				bodyOptional("title", "string", "会话标题（省略时用第一个问题命名）"),
				bodyRequired("model", "string", "模型名（须在该 API Key 的授权范围内）"),
				bodyRequired("account_id", "integer", "计费账户 id"),
				bodyRequired("api_key_id", "integer", "用于计费的 API Key id（明文密钥不会交给浏览器）"),
				enumField(bodyOptional("write_mode", "string", "写权限：read_only 只读（默认），allow_writes 允许执行写操作"), "read_only", "allow_writes"),
				bodyOptional("skill_ids", "array", "要加载的私有技能 id 列表"),
			},
			NoTool: chatNoTool,
		},
		{
			Method: "GET", Path: "/admin/api/v1/chat/sessions/{id}", Handler: s.handleAdminChatGetSession,
			Name: "admin_chat_get_session", Group: groupChat, Role: roleViewer,
			Summary: "读取一个会话：消息（含 Markdown 部件与工具调用）、工具调用明细、已加载技能与实时的 cost/charge",
			Params:  []adminField{pathParam("id", "会话 id")},
			NoTool:  chatNoTool,
		},
		{
			Method: "PATCH", Path: "/admin/api/v1/chat/sessions/{id}", Handler: s.handleAdminChatUpdateSession,
			Name: "admin_chat_update_session", Group: groupChat, Role: roleAdmin,
			Summary: "修改会话的标题、模型、计费绑定、写权限与已加载技能",
			Params:  []adminField{pathParam("id", "会话 id")},
			Body: []adminField{
				bodyOptional("title", "string", "新的标题（重命名只需 owner 权限）"),
				bodyOptional("model", "string", "改绑模型"),
				bodyOptional("account_id", "integer", "改绑计费账户"),
				bodyOptional("api_key_id", "integer", "改绑 API Key"),
				enumField(bodyOptional("write_mode", "string", "写权限开关"), "read_only", "allow_writes"),
				bodyOptional("skill_ids", "array", "替换要加载的技能 id 列表"),
			},
			NoTool: chatNoTool,
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/chat/sessions/{id}", Handler: s.handleAdminChatDeleteSession,
			Name: "admin_chat_delete_session", Group: groupChat, Role: roleViewer,
			Summary: "删除一个会话（连同它的消息、工具调用记录与预览产物）",
			Params:  []adminField{pathParam("id", "会话 id")},
			NoTool:  chatNoTool,
		},
		{
			Method: "POST", Path: "/admin/api/v1/chat/sessions/{id}/turns", Handler: s.handleAdminChatTurn,
			Name: "admin_chat_turn", Group: groupChat, Role: roleAdmin,
			Summary: "**SSE** 提一个问题并流式接收回答：事件为 turn/step/text/reasoning/tool_call/tool_result/usage/notice/message/error/done",
			Params:  []adminField{pathParam("id", "会话 id")},
			Body: []adminField{
				bodyRequired("content", "string", "问题内容；会话已加载技能时可以留空，服务端会按默认文案「按本会话已加载的技能执行」提问"),
				bodyOptional("turn_id", "string", "幂等 id：同一 id 重发返回既有结果而不会重复计费"),
			},
			Notes:  "每一步模型调用都是一条独立的计费请求（客户端记为 console），因此一次提问可能产生多条请求日志；被截断的步骤不会执行工具调用。content 留空只在会话确实加载了技能时成立（技能全部被删掉后同样会被拒绝），否则返回 400：空提问没有任何东西可执行，却会产生计费调用。",
			NoTool: chatNoTool,
		},
		{
			Method: "POST", Path: "/admin/api/v1/chat/sessions/{id}/skill-draft", Handler: s.handleAdminChatSkillDraft,
			Name: "admin_chat_skill_draft", Group: groupChat, Role: roleAdmin,
			Summary: "把一段会话交给模型提炼成技能草稿（名称/描述/指令），草稿不落库，由用户确认后保存；本次调用正常计费",
			Params:  []adminField{pathParam("id", "会话 id")},
			NoTool:  chatNoTool,
		},
		{
			Method: "GET", Path: "/admin/api/v1/chat/skills", Handler: s.handleAdminChatListSkills,
			Name: "admin_chat_list_skills", Group: groupChat, Role: roleViewer,
			Summary: "列出当前登录管理员的私有技能（仅自己可见，按最近更新排序）",
			Query:   chatPageSpec.fields(),
			NoTool:  chatNoTool,
		},
		{
			Method: "POST", Path: "/admin/api/v1/chat/skills", Handler: s.handleAdminChatCreateSkill,
			Name: "admin_chat_create_skill", Group: groupChat, Role: roleViewer,
			Summary: "新建一个私有技能（手写，或保存由会话生成的草稿）",
			Body: []adminField{
				bodyRequired("name", "string", "技能名（同一账号内唯一）"),
				bodyOptional("description", "string", "一句话说明何时使用"),
				bodyRequired("instructions", "string", "给模型执行的指令（Markdown）"),
				bodyOptional("session_id", "string", "来源会话 id（仅作标注）"),
			},
			NoTool: chatNoTool,
		},
		{
			Method: "PATCH", Path: "/admin/api/v1/chat/skills/{id}", Handler: s.handleAdminChatUpdateSkill,
			Name: "admin_chat_update_skill", Group: groupChat, Role: roleViewer,
			Summary: "修改一个私有技能的名称、描述或指令",
			Params:  []adminField{pathParam("id", "技能的数字 id")},
			Body: []adminField{
				bodyRequired("name", "string", "技能名"),
				bodyOptional("description", "string", "一句话说明何时使用"),
				bodyRequired("instructions", "string", "给模型执行的指令"),
			},
			NoTool: chatNoTool,
		},
		{
			Method: "DELETE", Path: "/admin/api/v1/chat/skills/{id}", Handler: s.handleAdminChatDeleteSkill,
			Name: "admin_chat_delete_skill", Group: groupChat, Role: roleViewer,
			Summary: "删除一个私有技能（已加载它的会话下次提问时自动忽略）",
			Params:  []adminField{pathParam("id", "技能的数字 id")},
			NoTool:  chatNoTool,
		},
		{
			Method: "GET", Path: "/admin/api/v1/chat/models", Handler: s.handleAdminChatModels,
			Name: "admin_chat_models", Group: groupChat, Role: roleAdmin,
			Summary: "列出指定 API Key 实际可路由的模型（与 GET /v1/models 同一套授权与路由判断，附带是否支持工具调用）",
			Query: []adminField{
				queryParam("account_id", "integer", "计费账户 id（须与该 Key 的所属账户一致）"),
				queryParam("api_key_id", "integer", "用于计费的 API Key id"),
			},
			NoTool: chatNoTool,
		},
		{
			Method: "POST", Path: "/admin/api/v1/chat/sessions/{id}/artifacts", Handler: s.handleAdminChatPutArtifact,
			Name: "admin_chat_put_artifact", Group: groupChat, Role: roleViewer,
			Summary: "登记一份待预览的 HTML/SVG 正文，返回短时预览票据（预览链接不绑定 Cookie，因此由票据授权；bridge=true 时票据可交互）",
			Params:  []adminField{pathParam("id", "会话 id")},
			Body: []adminField{
				bodyRequired("key", "string", "代码块的稳定标识（同一会话内重复预览同一个块会覆盖，不堆积副本）"),
				bodyRequired("format", "string", "html 或 svg"),
				bodyRequired("body", "string", "要预览的正文"),
				bodyOptional("title", "string", "预览窗标题"),
				bodyOptional("bridge", "boolean", "true = 交互预览：页面可把表单提交回本会话（每次提交都是一条正常计费的模型请求）；仅 html 且 chat.ui_bridge_enabled=true 时可用"),
			},
			NoTool: chatNoTool,
		},
		{
			Method: "POST", Path: "/admin/api/v1/chat/sessions/{id}/artifacts/{art}/ticket", Handler: s.handleAdminChatTicket,
			Name: "admin_chat_artifact_ticket", Group: groupChat, Role: roleViewer,
			Summary: "为已有的预览产物重新签发短时票据（票过期后重新打开预览用；bridge=true 签交互票据，默认只读）",
			Params: []adminField{
				pathParam("id", "会话 id"),
				pathParam("art", "产物 id"),
			},
			Body: []adminField{
				bodyOptional("bridge", "boolean", "true = 签发交互票据（仅 html 且 chat.ui_bridge_enabled=true 时可用）"),
			},
			NoTool: chatNoTool,
		},
	}
}
