// Package providers builds the builtin (in-process) providers. Plugin providers are
// launched as subprocesses by internal/pluginhost instead.
package providers

import (
	"fmt"
	"sort"

	"github.com/winger/ai-gateway/internal/providers/openaichat"
	"github.com/winger/ai-gateway/internal/providers/openairesponses"
	"github.com/winger/ai-gateway/internal/providers/testecho"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// Builtin kind names. Plugin kinds use the "plugin:<name>" form and are not built here.
const (
	KindOpenAIChat      = "openai-chat"
	KindOpenAIResponses = "openai-responses"
	KindTestEcho        = "testecho"
)

// BuiltinKinds lists the builtin provider kinds (sorted).
func BuiltinKinds() []string {
	kinds := []string{KindOpenAIChat, KindOpenAIResponses, KindTestEcho}
	sort.Strings(kinds)
	return kinds
}

// IsBuiltin reports whether kind refers to an in-process provider.
func IsBuiltin(kind string) bool {
	switch kind {
	case KindOpenAIChat, KindOpenAIResponses, KindTestEcho:
		return true
	default:
		return false
	}
}

// Build constructs a builtin provider.
func Build(kind, name, configJSON, stateDir string, creds map[string]string) (pluginapi.Provider, error) {
	switch kind {
	case KindOpenAIChat:
		return openaichat.New(name, configJSON, stateDir, creds)
	case KindOpenAIResponses:
		return openairesponses.New(name, configJSON, stateDir, creds)
	case KindTestEcho:
		return testecho.New(configJSON, stateDir)
	default:
		return nil, fmt.Errorf("providers: %q is not a builtin kind (use plugin:<name> for plugins)", kind)
	}
}
