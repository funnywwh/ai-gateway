package chat

import (
	"strings"
	"testing"
)

// These tests pin the behaviour the prompt is responsible for, not its wording: the model has
// to know that a missing parameter is a question rather than a guess, and that a configuration
// document is written from the schema it was handed rather than from memory.
//
// The assertions are literal markers because the failure they guard against is a sentence that
// quietly disappears in a rewrite. They are deliberately about *rules that must reach the
// model*, so they read the prompt the service really builds (FullSystemPromptForTest) rather
// than the constant, which would pass even if the assembly stopped appending it.

func TestPromptTellsTheModelToAskBeforeActing(t *testing.T) {
	prompt := FullSystemPromptForTest()
	for _, want := range []string{"先问再动手", "一次问清", "内联表单"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the built prompt no longer tells the model to ask first (%q is missing); "+
				"the default behaviour becomes guessing at the operator's values", want)
		}
	}
	// The counterweight matters as much: a model told to always ask will ask about things it
	// could have looked up, and a reporting question should be answered, not interrogated.
	for _, want := range []string{"不要拦着用户填表", "纯查询"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt must also say when *not* to use a form (%q is missing)", want)
		}
	}
}

func TestPromptForbidsGuessingFieldNames(t *testing.T) {
	prompt := FullSystemPromptForTest()
	for _, want := range []string{"body_schema", "admin_validate_pricing", "绝不猜字段名"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt must state how a configuration document is written (%q missing): "+
				"an operator's request once ended with the model refusing to write at all, because "+
				"nothing told it that the field shapes are in body_schema", want)
		}
	}
	// A form submission is input, not consent: the dangerous-operation rule has to survive the
	// new emphasis on forms.
	if !strings.Contains(prompt, "表单只是收集参数") {
		t.Error("the prompt must keep saying that a form button is not consent for a dangerous call")
	}
}

func TestPromptPutsTheInlineFormBeforeWholePageHTML(t *testing.T) {
	prompt := FullSystemPromptForTest()
	form := strings.Index(prompt, "需要用户给信息时")
	if form < 0 {
		t.Fatal("the prompt no longer names the inline form as the first choice for asking")
	}
	if !strings.Contains(prompt, "这是默认手段") {
		t.Error("the inline-form section must say it is the default, not one option among two")
	}
	if !strings.Contains(prompt, "仅在需要自由排版或页面脚本时用") {
		t.Error("the sandboxed page must be demoted explicitly, or the two contracts read as equals")
	}
}
