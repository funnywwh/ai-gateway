package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// distillingInstructions ask the model for a reusable procedure rather than a summary.
// A summary of "what we talked about" is useless as a skill; what makes a skill valuable
// is the part a future session cannot re-derive: which endpoints to call, in what order,
// and what to check afterwards.
const distillingInstructions = `你在帮一位 AI Gateway 管理员把刚才这段对话沉淀成一个可复用的技能（skill）。

技能会在以后的对话里被当作操作指令注入，所以它必须写成**能直接照做的步骤**，而不是这次对话的总结。

要求：
- name：简短的中文名，说明这个技能解决什么问题（不超过 40 字，不要带「技能」二字）。
- description：一句话说明什么时候该用它（不超过 120 字）。
- instructions：给模型的执行指令，用 Markdown 写，包含：
  1. 适用场景；
  2. 需要调用的后台接口（写清接口名与关键参数）；
  3. 判断与分支（例如哪种数据说明有问题）；
  4. 需要提醒用户注意的边界（例如写操作要先确认）。
- 只写这段对话里已经验证过的做法；不要编造接口名或字段名。不确定的地方写成「需要先确认」。

只输出一个 ` + "```json" + ` 代码块，内容是一个对象：
{"name":"…","description":"…","instructions":"…"}`

// draftTranscript renders the conversation for the distillation call, keeping the newest
// messages and dropping the middle if the transcript is too long.
func draftTranscript(messages []*domain.ChatMessage, maxBytes int) string {
	var b strings.Builder
	start := 0
	for {
		b.Reset()
		for _, m := range messages[start:] {
			switch m.Role {
			case domain.ChatRoleUser:
				b.WriteString("### 用户\n")
				b.WriteString(strings.TrimSpace(m.Content))
				b.WriteString("\n\n")
			case domain.ChatRoleAssistant:
				b.WriteString("### 助手\n")
				for _, part := range m.Parts {
					switch part.Type {
					case "text":
						b.WriteString(strings.TrimSpace(part.Text))
						b.WriteString("\n")
					case "tool_call":
						status := "成功"
						if part.IsError {
							status = "失败"
						}
						b.WriteString(fmt.Sprintf("- 调用 %s（%s），参数 %s\n", part.Name, status, clampForTranscript(part.Arguments, 400)))
					}
				}
				b.WriteString("\n")
			}
		}
		if b.Len() <= maxBytes || start >= len(messages)-2 {
			break
		}
		// Drop the oldest messages and retry; the tail is what the skill is about.
		start += 2
	}
	return b.String()
}

func clampForTranscript(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// SkillDraft asks the model to distill a conversation into a skill proposal. The draft is
// never stored: the operator edits and saves it, so a bad generation costs one编辑 step
// rather than a polluted library.
func (s *Service) SkillDraft(ctx context.Context, ownerID int64, username, role, sessionID string) (*SkillDraft, error) {
	if ownerID <= 0 {
		return nil, domain.ErrUnauthorized("chat skills require an authenticated administrator session")
	}
	if err := requireAdmin(role, "generate a skill with a billed model call"); err != nil {
		return nil, err
	}
	session, err := s.store.GetChatSession(ctx, sessionID, ownerID)
	if err != nil {
		return nil, err
	}
	if session.AccountID <= 0 || session.APIKeyID <= 0 {
		return nil, domain.ErrInvalidRequest("bind an account and API key to this conversation first")
	}
	messages, err := s.store.ListChatMessages(ctx, session.ID, 0)
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, domain.ErrInvalidRequest("this conversation has nothing to distill yet")
	}
	transcript := draftTranscript(messages, maxDraftBytes)

	// The distillation call is an ordinary billed step: same account, same key, same
	// recording rules (metadata only). No tools are offered — this is a writing task, and
	// a tool loop here would just be another way to spend money by accident.
	step := Step{
		Model:          session.Model,
		Instructions:   distillingInstructions,
		Items:          []pluginapi.Item{userItem("这是需要沉淀成技能的对话：\n\n" + transcript)},
		PromptCacheKey: session.ID,
		AccountID:      session.AccountID,
		APIKeyID:       session.APIKeyID,
		Actor:          username,
	}
	result, err := s.runner.RunStep(ctx, step, nil)
	if err != nil {
		return nil, err
	}
	draft := parseDraft(result.Text)
	draft.RequestID = result.RequestID
	draft.Model = session.Model
	draft.Usage = result.Usage
	if draft.Instructions == "" {
		// Nothing parseable came back. Rather than inventing a skill and calling it the
		// model's work, hand the operator a truthful skeleton built from the conversation
		// itself, with a note explaining what happened.
		title := firstQuestion(messages)
		draft = &SkillDraft{
			Name:         truncateRunes(title, 40),
			Description:  "由这段对话整理而成，请补充适用场景。",
			Instructions: skeletonInstructions(messages),
			Note:         "模型没有返回可解析的 JSON，下面是按对话自动生成的骨架，请手工补充后保存。",
			RequestID:    result.RequestID,
			Model:        session.Model,
			Usage:        result.Usage,
		}
	}
	draft.Name = truncateRunes(strings.TrimSpace(draft.Name), maxSkillNameRunes)
	draft.Description = truncateRunes(strings.TrimSpace(draft.Description), maxSkillDescRunes)
	draft.Instructions = strings.TrimSpace(draft.Instructions)
	if len(draft.Instructions) > s.cfg.MaxSkillBytes {
		draft.Instructions = draft.Instructions[:s.cfg.MaxSkillBytes]
		draft.Note = strings.TrimSpace(draft.Note + " 草稿超过了技能长度上限，已截断，请删减后再保存。")
	}
	return draft, nil
}

// parseDraft pulls the JSON object out of the model's answer. Models wrap it in a fenced
// block, sometimes add a sentence before it, and occasionally emit trailing commas, so the
// parse is deliberately forgiving — and returns an empty draft rather than an error, which
// the caller turns into an honest fallback.
func parseDraft(text string) *SkillDraft {
	body := extractJSONObject(text)
	if body == "" {
		return &SkillDraft{}
	}
	var raw struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return &SkillDraft{}
	}
	return &SkillDraft{Name: raw.Name, Description: raw.Description, Instructions: raw.Instructions}
}

// extractJSONObject returns the first balanced {...} block in text.
func extractJSONObject(text string) string {
	start := strings.Index(text, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inString:
			escaped = true
		case ch == '"':
			inString = !inString
		case inString:
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
		}
	}
	return ""
}

func firstQuestion(messages []*domain.ChatMessage) string {
	for _, m := range messages {
		if m.Role == domain.ChatRoleUser && strings.TrimSpace(m.Content) != "" {
			return titleFromQuestion(m.Content)
		}
	}
	return "未命名技能"
}

// skeletonInstructions is the deterministic fallback: it records what actually happened in
// the conversation (which tools ran, in what order) without pretending to know why.
func skeletonInstructions(messages []*domain.ChatMessage) string {
	var b strings.Builder
	b.WriteString("## 适用场景\n\n（请补充：什么情况下应该使用这个技能。）\n\n## 步骤\n\n")
	step := 1
	for _, m := range messages {
		if m.Role != domain.ChatRoleAssistant {
			continue
		}
		for _, part := range m.Parts {
			if part.Type == "tool_call" {
				fmt.Fprintf(&b, "%d. 调用 `%s`，参数 `%s`。\n", step, part.Name, clampForTranscript(part.Arguments, 200))
				step++
			}
		}
	}
	if step == 1 {
		b.WriteString("1. （这段对话没有调用管理接口，请手工补充步骤。）\n")
	}
	b.WriteString("\n## 注意事项\n\n（请补充：写操作前要确认什么、哪些数据说明有问题。）\n")
	return b.String()
}
