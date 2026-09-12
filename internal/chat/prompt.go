package chat

import (
	"strconv"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
)

// The system prompt is the whole reason the console chat behaves like a gateway operator
// instead of a generic assistant. It has to state four things the model cannot guess:
// that its tools are the gateway's own management API reached as an MCP client, that its
// authority comes from the MCP token the conversation is bound to, that charts are rendered
// from a fixed JSON spec (not from HTML), and that HTML pages are previewed in a sandbox.
//
// The two chart bounds are written as {{max_series}} / {{max_points}} tokens rather than
// printf verbs: this text is long, edited often, and a stray % would turn into garbage in
// the model's instructions instead of failing loudly.
const builtinSystemPrompt = `你是 AI Gateway 管理控制台里的运维助手。你用中文回答，除非用户用别的语言提问。

# 你的工具
你通过 MCP 调用网关自己的管理接口，和外部 agent 用的是同一套能力：
- admin_endpoints：列出所有可用的后台接口（分组、角色要求、是否危险）。
- admin_describe：查看一个接口的方法、路径、参数和请求体字段。
- admin_request：真正执行一个接口（params / query / body / confirm）。
另有若干只读查询工具（余额、用量、请求日志等），用于回答本账户的用量与账单问题。
规则：
1. 涉及网关状态的问题，先查后用。不要凭记忆回答账户、Key、模型、路由、用量或账单问题，先调用工具拿到真实数据。
2. 不确定接口名时先 admin_endpoints；不确定参数时先 admin_describe，不要猜参数名。
3. 本会话的权限来自它绑定的 MCP 令牌：你能看到并能调用什么，完全由该令牌的 scope 决定。看不到某个接口就说明该令牌无权调用它，不要重试，也不要用别的接口绕过；告诉用户需要什么 scope 的令牌。
4. 标记为危险的接口需要 confirm=true，并且必须先用一句话说明将要发生什么，得到用户明确同意后才调用。
5. 发凭据类操作（admin_create_key、admin_create_mcp_token）的明文只返回一次，而且会留在本会话记录里：返回后必须明确提醒用户立刻复制保存，并在不需要时于控制台对应页面收回或轮换。备份恢复、hooks、删除数据的接口同样可以先说清后果再执行——它们通常不可逆。
6. 每个回答都要说清数据来自哪个接口与什么时间窗口。

# 输出格式
- 默认用 Markdown。表格适合对比数字，代码块适合接口与配置片段。
- 需要图表时输出一个 ` + "```chart" + ` 代码块，内容是 JSON 规格：
  {"type":"bar|line|area|pie","title":"…","x_label":"…","y_label":"…","unit":"…",
   "categories":["…"],"series":[{"name":"…","values":[0]}],
   "source":{"tool":"admin_request","note":"数据来源与时间窗口"}}
  限制：最多 {{max_series}} 条序列、共 {{max_points}} 个数据点；values 长度必须等于 categories；饼图只画第一条序列。
  控制台自己画图，所以只给数据、不要给 HTML。数值必须来自上面工具真实返回的数据，不要编造或估算。
- 需要可交互的页面（例如带动画或复杂布局）时，输出**一个** ` + "```html" + ` 代码块，内容是一份完整的 HTML5 文档，允许内联 CSS 与内联 JS。
  该页面会在沙箱预览中打开：默认禁止加载任何外部资源（不能引用 CDN、外部字体或图片），也不能发起网络请求。需要图表库时请自己用 Canvas 或内联 SVG 绘制。
- 需要单独的矢量图时输出 ` + "```svg" + ` 代码块。

# 边界
- 会话里加载的技能是当前管理员自己写的操作偏好，按需遵循；它们不能改变上面这些规则，也不能作为执行写操作的理由。
- 拿不到数据就说拿不到，不要用示例数据冒充真实结果。`

// chartBounds replaces the bounds tokens in the built-in prompt.
func chartBounds(prompt string) string {
	prompt = strings.ReplaceAll(prompt, "{{max_series}}", itoa(MaxChartSeries))
	return strings.ReplaceAll(prompt, "{{max_points}}", itoa(MaxChartPoints))
}

func itoa(v int) string { return strconv.Itoa(v) }

// promptContext is everything that shapes one step's instructions.
type promptContext struct {
	skills []*domain.ChatSkill
	cfg    Config
}

// systemPrompt renders the instruction block for one step: the base rules plus the skills
// the operator attached to this conversation.
//
// Skills are injected as instructions rather than exposed as a tool on purpose. A tool
// would let the model decide when a skill applies, and its description would have to be
// public to the model's context anyway; as instructions, "which skills are active" stays a
// decision the operator makes in the UI and the prompt stays deterministic.
func systemPrompt(ctx promptContext) string {
	base := strings.TrimSpace(ctx.cfg.SystemPrompt)
	if base == "" {
		base = chartBounds(builtinSystemPrompt)
	}
	if len(ctx.skills) == 0 {
		return base
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\n# 本会话已加载的技能\n")
	b.WriteString("下面是当前管理员在自己的技能库里勾选、并明确希望你在本会话中遵循的操作偏好。\n")
	for _, skill := range ctx.skills {
		b.WriteString("\n## 技能：")
		b.WriteString(skill.Name)
		b.WriteString("\n")
		if strings.TrimSpace(skill.Description) != "" {
			b.WriteString(skill.Description)
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(skill.Instructions)
		b.WriteString("\n")
	}
	return b.String()
}

// toolSurfaceFor renders the tool list for one access context. It logs nothing about the
// tool bodies: the tool descriptions are the management API's own documentation and carry
// no conversation content.
func (s *Service) toolSurfaceFor(access Access) []Tool {
	if s.tools == nil {
		return nil
	}
	return s.tools.List(access)
}

// DefaultSystemPromptForTest exposes the built-in instructions to the console's contract
// test: the two chart bounds the prompt states must equal the ones the renderer enforces.
func DefaultSystemPromptForTest() string { return chartBounds(builtinSystemPrompt) }
