package testecho

import (
	"context"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

func TestCompleteEchoesAndReportsUsage(t *testing.T) {
	p, err := New(`{"prefix":"you said:"}`, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte(`"ping"`)
	resp, err := p.Complete(context.Background(), &pluginapi.Request{
		Model: "testecho",
		Input: []pluginapi.Item{{Type: "message", Role: "user", Content: content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "completed" || len(resp.Items) != 1 {
		t.Fatalf("response mismatch: %+v", resp)
	}
	if got := string(resp.Items[0].Content); !contains(got, "you said: ping") {
		t.Fatalf("content mismatch: %s", got)
	}
	if resp.Usage.Dimensions["output"] == 0 || resp.Usage.Dimensions["input"] == 0 {
		t.Fatalf("usage dimensions missing: %+v", resp.Usage.Dimensions)
	}
	if p.Calls() != 1 {
		t.Fatalf("call counter = %d", p.Calls())
	}
}

func TestStreamEmitsDeltasAndIncrementalUsage(t *testing.T) {
	p, err := New(`{"prefix":"x","chunks":3}`, "")
	if err != nil {
		t.Fatal(err)
	}
	content := []byte(`"abcdefghij"`)
	var text string
	deltas, finalUsage := 0, 0
	err = p.Stream(context.Background(), &pluginapi.Request{
		Model: "testecho",
		Input: []pluginapi.Item{{Type: "message", Role: "user", Content: content}},
	}, func(ev pluginapi.Event) error {
		switch ev.Type {
		case pluginapi.EventTextDelta:
			text += ev.Text
			deltas++
		case pluginapi.EventUsageDelta:
			finalUsage++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if deltas < 2 || finalUsage < 2 {
		t.Fatalf("expected multiple deltas and usage.delta events, got %d/%d", deltas, finalUsage)
	}
	if !contains(text, "abcdefghij") {
		t.Fatalf("streamed text mismatch: %q", text)
	}
}

func TestFailureModes(t *testing.T) {
	cases := []struct {
		mode    string
		kind    string
		retry   bool
	}{
		{"retryable", pluginapi.KindRetryable, true},
		{"quota", pluginapi.KindQuotaExhausted, true},
		{"fatal", pluginapi.KindFatal, false},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			p, err := New(`{"fail_mode":"`+tc.mode+`"}`, "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Complete(context.Background(), &pluginapi.Request{Model: "testecho"})
			apiErr, ok := pluginapi.IsError(err)
			if !ok {
				t.Fatalf("expected a protocol error, got %v", err)
			}
			if apiErr.Kind != tc.kind || apiErr.Retryable != tc.retry {
				t.Fatalf("error classification mismatch: %+v", apiErr)
			}
		})
	}
}

func TestMidStreamFailure(t *testing.T) {
	p, err := New(`{"prefix":"abcd","chunks":4,"fail_mode":"retryable","fail_after":2}`, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	err = p.Stream(context.Background(), &pluginapi.Request{Model: "testecho"}, func(ev pluginapi.Event) error {
		if ev.Type == pluginapi.EventTextDelta {
			seen++
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected a mid-stream failure")
	}
	if seen == 0 {
		t.Fatal("expected at least one delta before the failure")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
