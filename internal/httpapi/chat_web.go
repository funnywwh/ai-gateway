package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/chat"
	"github.com/winger/ai-gateway/internal/webaccess"
)

// The console's internet access (M73) adds two tools that are not MCP tools at all.
//
// They live here, next to the console-only create_skill / update_session_title, for the same
// reason those do: they must never appear on the external MCP surface. They differ from every
// other tool in the list in one important way — they need no MCP token. Searching the public web
// is not a management operation, so tying it to a token would mean an operator who wants "chat
// plus search" has to hand out admin authority to get it. What gates them instead is a pair of
// switches: the deployment's (chat.web_access.enabled, carried in chat.Config.WebAccess) and the
// conversation's own (domain.ChatSession.WebAccess, carried in chat.Access.WebAccess).
const (
	toolWebSearch = "web_search"
	toolWebFetch  = "web_fetch"
)

// turnBudgetTTL is how long a turn's call counter is kept. A turn that finished an hour ago
// cannot make another call, so its counter is only taking up space.
const turnBudgetTTL = time.Hour

// maxTrackedTurns bounds the counter map. It is far above the number of turns a real deployment
// has in flight; hitting it means something is creating sessions in a loop, and the response is
// to forget every counter rather than to grow without limit. Forgetting is safe: the worst case
// is that a turn gets its budget back.
const maxTrackedTurns = 4096

// webTools is one deployment's web access client plus the per-turn call budget.
//
// The budget is counted per turn rather than per conversation because that is the unit a user
// perceives: one question may reasonably search twice and read three pages, while a conversation
// left open for a week should not accumulate a thousand searches. Counting here — next to the
// tool dispatch — is also what keeps the counter independent of the turn loop's own bookkeeping.
type webTools struct {
	client   *webaccess.Client
	maxCalls int

	mu    sync.Mutex
	turns map[string]turnUse
}

// turnUse is one turn's counter and when it was last touched.
type turnUse struct {
	calls int
	seen  time.Time
}

// newWebTools builds the tool state. A nil client means the deployment has no web access
// configured, and every entry point treats that as "these tools do not exist".
func newWebTools(client *webaccess.Client, maxCalls int) *webTools {
	if client == nil {
		return nil
	}
	if maxCalls <= 0 {
		maxCalls = 1
	}
	return &webTools{client: client, maxCalls: maxCalls, turns: map[string]turnUse{}}
}

// enabled reports whether this deployment can serve the web tools at all.
func (w *webTools) enabled() bool { return w != nil && w.client != nil }

// claim reserves one call for a turn and reports whether the budget allowed it.
//
// key is the turn identity (see webTurnKey). The remaining count is what the message shown to
// the model reports, so a model that keeps going after the budget is spent gets the same number
// every time instead of a growing one.
func (w *webTools) claim(key string, now time.Time) (bool, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked(now)
	use := w.turns[key]
	if use.calls >= w.maxCalls {
		use.seen = now
		w.turns[key] = use
		return false, 0
	}
	use.calls++
	use.seen = now
	w.turns[key] = use
	return true, w.maxCalls - use.calls
}

// pruneLocked drops counters for turns that can no longer be running, and — in the pathological
// case of a flood of distinct turns — gives up on counting rather than growing.
func (w *webTools) pruneLocked(now time.Time) {
	if len(w.turns) < maxTrackedTurns {
		for key, use := range w.turns {
			if now.Sub(use.seen) > turnBudgetTTL {
				delete(w.turns, key)
			}
		}
		return
	}
	for key, use := range w.turns {
		if now.Sub(use.seen) > turnBudgetTTL {
			delete(w.turns, key)
		}
	}
	if len(w.turns) >= maxTrackedTurns {
		w.turns = map[string]turnUse{}
	}
}

// webTurnKey identifies the bucket a call is counted against. A turn id comes from the console's
// own turn record; without one (an older console, or a direct API caller) the session becomes the
// bucket, which is the stricter reading and never under-counts.
func webTurnKey(access chat.Access) string {
	return access.SessionID + "/" + access.TurnID
}

// webToolDefs renders the two tool definitions. The description states the deployment's own
// limits, because a model that knows it may ask for "at most 6 results" writes better queries
// than one that discovers the cap by being truncated.
func (w *webTools) webToolDefs() []chat.Tool {
	if !w.enabled() {
		return nil
	}
	limit := strconv.Itoa(w.maxResults())
	search := fmt.Sprintf(
		"搜索公网并返回若干条结果（标题、URL、摘要、来源、发布时间）。需要最新信息、公开事实或自己不知道的"+
			"内容时使用；摘要通常不足以给出准确数字，重要结论请再用 %s 读原文。"+
			"可选 count（本次要几条，最多 %s 条）与 freshness（时间范围：noLimit/oneDay/oneWeek/oneMonth/oneYear）。",
		toolWebFetch, limit)
	return []chat.Tool{
		{
			Name:        toolWebSearch,
			Description: search,
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"query":{"type":"string","description":"检索词，尽量具体（加上产品名、年份或站点名）"},` +
				`"count":{"type":"integer","description":"本次要几条结果，取值 1..` + limit + `","minimum":1,"maximum":` + limit + `},` +
				`"freshness":{"type":"string","enum":["noLimit","oneDay","oneWeek","oneMonth","oneYear"],` +
				`"description":"时间范围过滤；不是所有后端都支持，不支持时结果里会说明"}},` +
				`"required":["query"],"additionalProperties":false}`),
		},
		{
			Name: toolWebFetch,
			Description: "抓取一个公网页面并返回可读正文（HTML 会去掉脚本与样式）。" +
				"只允许 http/https 公网地址与 80/443 端口；内网、回环与云元数据地址会被拒绝，这是部署的安全边界。" +
				"正文过长会被截断并在结果里标注；二进制文件（PDF、图片等）不会被解析。",
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"url":{"type":"string","description":"完整的 http/https 地址"}},` +
				`"required":["url"],"additionalProperties":false}`),
		},
	}
}

// maxResults is how many hits one search may return; the client clamps to the same number, this
// is only what the schema advertises.
func (w *webTools) maxResults() int {
	return w.client.MaxResults()
}

// callWebTool runs one web tool call.
//
// Every failure — a switched-off feature, a spent budget, a refused address, a dead backend —
// comes back as an ordinary tool result carrying an explanation, never as a failed turn: the
// model can still answer from what it has, and the operator reading the transcript sees exactly
// which switch or which address was the problem.
func (t *chatTools) callWebTool(ctx context.Context, access chat.Access, name string, args map[string]any) chat.ToolResult {
	if !t.web.enabled() {
		return webErrorResult("这个部署没有启用控制台联网（chat.web_access.enabled），无法使用 " + name + "；请让运维配置搜索后端后再试")
	}
	if !access.WebAccess {
		// Reaching here means the tools were listed and then switched off, or the model
		// invented the name. Both are worth the same sentence.
		return webErrorResult("当前会话没有开启联网，因此不能使用 " + name + "；请在会话里打开「联网」开关后重试")
	}
	if allowed, _ := t.web.claim(webTurnKey(access), time.Now()); !allowed {
		return webErrorResult(fmt.Sprintf(
			"本轮问答的联网调用已达上限（%d 次）：请基于已经拿到的结果作答，或让用户在新的一轮里继续查。",
			t.web.maxCalls))
	}
	switch name {
	case toolWebSearch:
		return t.webSearch(ctx, args)
	case toolWebFetch:
		return t.webFetch(ctx, args)
	default:
		return webErrorResult("未知的联网工具：" + name)
	}
}

// webSearch runs one search and renders the documented result shape.
func (t *chatTools) webSearch(ctx context.Context, args map[string]any) chat.ToolResult {
	query, _ := args["query"].(string)
	freshness, _ := args["freshness"].(string)
	result, err := t.web.client.Search(ctx, query, jsonInt(args["count"]), freshness)
	if err != nil {
		return webErrorResult(err.Error())
	}
	items := make([]map[string]any, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, map[string]any{
			"title":     item.Title,
			"url":       item.URL,
			"snippet":   item.Snippet,
			"source":    item.Source,
			"published": item.Published,
		})
	}
	value := map[string]any{
		"provider": result.Provider,
		"query":    result.Query,
		"count":    result.Count,
		"results":  items,
	}
	if result.Note != "" {
		value["note"] = result.Note
	}
	return chat.ToolResult{Value: value}
}

// webFetch downloads one page and renders the documented result shape. The content is returned
// verbatim: it is the model's source material, and summarising it here would mean the console
// deciding what the answer may be based on.
func (t *chatTools) webFetch(ctx context.Context, args map[string]any) chat.ToolResult {
	rawURL, _ := args["url"].(string)
	page, err := t.web.client.Fetch(ctx, rawURL)
	if err != nil {
		return webErrorResult(err.Error())
	}
	value := map[string]any{
		"url":          page.URL,
		"final_url":    page.FinalURL,
		"title":        page.Title,
		"content":      page.Content,
		"content_type": page.ContentType,
		"bytes":        page.Bytes,
		"truncated":    page.Truncated,
	}
	if page.Note != "" {
		value["note"] = page.Note
	}
	return chat.ToolResult{Value: value}
}

// webErrorResult renders a web tool failure. The message is written for the model to relay, and
// it deliberately never echoes the URL or the query: those are already in the tool call the
// model made, and repeating them here would only duplicate attacker-controlled text.
func webErrorResult(message string) chat.ToolResult {
	return chat.ToolResult{Value: map[string]any{"error": strings.TrimSpace(message)}, IsError: true}
}

// jsonInt reads a JSON number argument, tolerating the float64 every decoder produces and the
// string a model occasionally writes instead.
func jsonInt(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case string:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &parsed); err == nil {
			return parsed
		}
	}
	return 0
}

// webAccessFacts are the deployment-level facts the console needs to render its switch: whether
// the capability exists here at all, and which backend is configured. They carry no credential
// and no query text — only what the UI has to know to be honest about the feature.
func (s *Server) webAccessFacts() (available bool, provider string) {
	if s.deps.WebAccess == nil || s.deps.Config == nil || !s.deps.Config.Chat.WebAccess.Enabled {
		return false, ""
	}
	return true, s.deps.WebAccess.Provider()
}
