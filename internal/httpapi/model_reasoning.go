package httpapi

import (
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// applyModelReasoning operates on an attempt-local request. Copy the reasoning
// object as well, so neither a retry nor a caller sharing it observes mutation.
func applyModelReasoning(req *pluginapi.Request, policy *domain.ModelReasoning) {
	if req == nil || policy == nil {
		return
	}
	if policy.Mode == "default" && req.Reasoning != nil && req.Reasoning.Effort != "" {
		return
	}
	reasoning := pluginapi.Reasoning{}
	if req.Reasoning != nil {
		reasoning = *req.Reasoning
	}
	reasoning.Effort = policy.Effort
	req.Reasoning = &reasoning
}
