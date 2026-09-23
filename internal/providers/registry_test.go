package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

func TestBuiltinKindsAndClassification(t *testing.T) {
	kinds := BuiltinKinds()
	if len(kinds) != 4 {
		t.Fatalf("builtin kinds = %v", kinds)
	}
	for _, k := range kinds {
		if !IsBuiltin(k) {
			t.Errorf("%s must be classified as builtin", k)
		}
	}
	if IsBuiltin("plugin:replay") || IsBuiltin("nope") {
		t.Fatal("plugin kinds must not be treated as builtin")
	}
}

// emptyBaseURL is a config with an empty base_url (raw string keeps quotes readable).
const emptyBaseURL = `{"base_url":""}`

func TestBuildRejectsBadConfig(t *testing.T) {
	if _, err := Build(KindOpenAIChat, "x", "{}", "", nil); err == nil {
		t.Fatal("openai-chat without base_url must fail")
	}
	if _, err := Build(KindOpenAIResponses, "x", emptyBaseURL, "", nil); err == nil {
		t.Fatal("openai-responses without base_url must fail")
	}
	if _, err := Build(KindOpenAIImages, "x", emptyBaseURL, "", nil); err == nil {
		t.Fatal("openai-images without base_url must fail")
	}
	if _, err := Build("plugin:replay", "x", "", "", nil); err == nil {
		t.Fatal("plugin kinds must be rejected by the builtin registry")
	}
	if _, err := Build("unknown", "x", "", "", nil); err == nil {
		t.Fatal("unknown kind must be rejected")
	}
}

func TestBuildReturnsUsableProviders(t *testing.T) {
	echo, err := Build(KindTestEcho, "echo", "", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(echo.Info().Name, "testecho") {
		t.Fatalf("unexpected info: %+v", echo.Info())
	}
	if _, err := echo.Complete(context.Background(), nil); err == nil {
		t.Fatal("nil request must fail")
	}

	chat, err := Build(KindOpenAIChat, "chat", `{"base_url":"http://127.0.0.1:1/v1"}`, "", map[string]string{"api_key": "k"})
	if err != nil {
		t.Fatal(err)
	}
	if !chat.Info().Capabilities.Stream || !chat.Info().Capabilities.Complete {
		t.Fatalf("chat capabilities wrong: %+v", chat.Info().Capabilities)
	}

	responses, err := Build(KindOpenAIResponses, "responses", `{"base_url":"http://127.0.0.1:1/v1"}`, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !responses.Info().Capabilities.Stream {
		t.Fatalf("responses capabilities wrong: %+v", responses.Info().Capabilities)
	}

	images, err := Build(KindOpenAIImages, "images", `{"base_url":"http://127.0.0.1:1/v1"}`, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !images.Info().Capabilities.Images {
		t.Fatalf("images capabilities wrong: %+v", images.Info().Capabilities)
	}
	if _, ok := images.(interface {
		Images(context.Context, *pluginapi.ImageRequest) (*pluginapi.ImageResponse, error)
	}); !ok {
		t.Fatal("the openai-images builtin must implement pluginapi.ImageProvider")
	}
	// A chat-shaped request must be refused with a message that says where to go instead.
	if _, err := images.Complete(context.Background(), &pluginapi.Request{Model: "gpt-image-2"}); err == nil {
		t.Fatal("a chat request to an image-only provider must fail")
	} else if apiErr, ok := pluginapi.IsError(err); !ok || apiErr.Code != "image_model_only" {
		t.Fatalf("error = %v, want image_model_only", err)
	}
}
