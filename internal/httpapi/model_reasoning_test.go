package httpapi

import (
	"sync"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

func TestApplyModelReasoningPreservesSummaryAndCallerObject(t *testing.T) {
	callerReasoning := &pluginapi.Reasoning{Effort: "low", Summary: "concise"}
	req := &pluginapi.Request{Reasoning: callerReasoning}
	policy := &domain.ModelReasoning{Mode: "force", Effort: "high"}

	applyModelReasoning(req, policy)

	if got := req.Reasoning; got == callerReasoning {
		t.Fatal("attempt must receive a copy of the caller reasoning object")
	} else if got.Effort != "high" || got.Summary != "concise" {
		t.Fatalf("reasoning = %+v, want forced effort and preserved summary", got)
	}
	if callerReasoning.Effort != "low" || callerReasoning.Summary != "concise" {
		t.Fatalf("caller reasoning mutated: %+v", callerReasoning)
	}
}

func TestApplyModelReasoningModeMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     *domain.ModelReasoning
		caller     *pluginapi.Reasoning
		wantEffort string
		wantSame   bool
	}{
		{"default fills nil", &domain.ModelReasoning{Mode: "default", Effort: "medium"}, nil, "medium", false},
		{"default fills missing", &domain.ModelReasoning{Mode: "default", Effort: "medium"}, &pluginapi.Reasoning{Summary: "auto"}, "medium", false},
		{"default preserves explicit", &domain.ModelReasoning{Mode: "default", Effort: "medium"}, &pluginapi.Reasoning{Effort: "minimal", Summary: "auto"}, "minimal", true},
		{"default preserves explicit none", &domain.ModelReasoning{Mode: "default", Effort: "medium"}, &pluginapi.Reasoning{Effort: "none", Summary: "auto"}, "none", true},
		{"force fills nil", &domain.ModelReasoning{Mode: "force", Effort: "high"}, nil, "high", false},
		{"force overrides explicit", &domain.ModelReasoning{Mode: "force", Effort: "high"}, &pluginapi.Reasoning{Effort: "minimal", Summary: "auto"}, "high", false},
		{"force none", &domain.ModelReasoning{Mode: "force", Effort: "none"}, &pluginapi.Reasoning{Effort: "high", Summary: "auto"}, "none", false},
		{"cleared policy", nil, &pluginapi.Reasoning{Effort: "low", Summary: "auto"}, "low", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &pluginapi.Request{Reasoning: tc.caller}
			applyModelReasoning(req, tc.policy)
			if req.Reasoning == nil || req.Reasoning.Effort != tc.wantEffort || req.Reasoning.Summary != "auto" && tc.caller != nil {
				t.Fatalf("reasoning = %+v", req.Reasoning)
			}
			if tc.caller != nil && (req.Reasoning == tc.caller) != tc.wantSame {
				t.Fatalf("reasoning pointer copied=%t, want copied=%t", req.Reasoning != tc.caller, !tc.wantSame)
			}
		})
	}
}

func TestApplyModelReasoningRetriesAreIndependent(t *testing.T) {
	caller := &pluginapi.Reasoning{Effort: "low", Summary: "brief"}
	policy := &domain.ModelReasoning{Mode: "force", Effort: "high"}
	first := &pluginapi.Request{Reasoning: caller}
	second := &pluginapi.Request{Reasoning: caller}
	applyModelReasoning(first, policy)
	applyModelReasoning(second, policy)

	if first.Reasoning == second.Reasoning || first.Reasoning == caller || second.Reasoning == caller {
		t.Fatal("each retry must own its reasoning object")
	}
	first.Reasoning.Summary = "first-attempt-only"
	if second.Reasoning.Summary != "brief" || caller.Summary != "brief" || caller.Effort != "low" {
		t.Fatalf("retry or caller leaked mutation: first=%+v second=%+v caller=%+v", first.Reasoning, second.Reasoning, caller)
	}
}

func TestApplyModelReasoningConcurrentSharedCaller(t *testing.T) {
	caller := &pluginapi.Reasoning{Effort: "low", Summary: "shared"}
	policy := &domain.ModelReasoning{Mode: "force", Effort: "high"}
	const attempts = 64
	results := make(chan *pluginapi.Reasoning, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := &pluginapi.Request{Reasoning: caller}
			applyModelReasoning(req, policy)
			results <- req.Reasoning
		}()
	}
	wg.Wait()
	close(results)

	seen := map[*pluginapi.Reasoning]bool{}
	for got := range results {
		if got == caller || got.Effort != "high" || got.Summary != "shared" || seen[got] {
			t.Fatalf("attempt reasoning must be a distinct, correct copy: %+v", got)
		}
		seen[got] = true
	}
	if caller.Effort != "low" || caller.Summary != "shared" {
		t.Fatalf("shared caller mutated: %+v", caller)
	}
}

func TestApplyModelReasoningNilRequestDoesNothing(t *testing.T) {
	applyModelReasoning(nil, &domain.ModelReasoning{Mode: "force", Effort: "high"})
}
