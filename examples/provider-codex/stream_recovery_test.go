package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

type streamTransport func(*http.Request) (*http.Response, error)

func (f streamTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenStream struct{ io.Reader }

func (b brokenStream) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
func (b brokenStream) Close() error { return nil }

const toolFrames = "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"read\"}}\n\n" +
	"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{}\"}\n\n"

func TestStreamRecoversBeforeFirstOutput(t *testing.T) {
	for _, cut := range []string{"reset", "eof"} {
		t.Run(cut, func(t *testing.T) {
			p := newTestProvider(t, "http://upstream.invalid", "", "", map[string]string{"access_token": "static-token"})
			calls := 0
			p.http.Transport = streamTransport(func(*http.Request) (*http.Response, error) {
				calls++
				var body io.ReadCloser = io.NopCloser(strings.NewReader(toolFrames + happyFrames[3]))
				if calls == 1 {
					body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\"}\n\n"))
					if cut == "reset" {
						body = brokenStream{strings.NewReader("data: {\"type\":\"response.created\"}\n\n")}
					}
				}
				return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
			})
			var events []pluginapi.Event
			err := p.Stream(context.Background(), &pluginapi.Request{Model: "codex"}, func(e pluginapi.Event) error { events = append(events, e); return nil })
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("attempts=%d, want 2", calls)
			}
			want := []string{pluginapi.EventToolCallStart, pluginapi.EventToolArgsDelta, pluginapi.EventUsage, pluginapi.EventFinish}
			if len(events) != len(want) {
				t.Fatalf("events=%+v", events)
			}
			for i, e := range events {
				if e.Type != want[i] {
					t.Fatalf("event %d=%s, want %s", i, e.Type, want[i])
				}
			}
		})
	}
}

func TestStreamTerminalDoesNotReadBrokenTail(t *testing.T) {
	for _, terminal := range []string{happyFrames[3], "data: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n"} {
		p := newTestProvider(t, "http://upstream.invalid", "", "", map[string]string{"access_token": "static-token"})
		calls := 0
		p.http.Transport = streamTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Body: brokenStream{strings.NewReader(toolFrames + terminal)}, Header: make(http.Header)}, nil
		})
		finished := false
		err := p.Stream(context.Background(), &pluginapi.Request{Model: "codex"}, func(e pluginapi.Event) error {
			if e.Type == pluginapi.EventFinish {
				finished = true
			}
			return nil
		})
		if err != nil || !finished || calls != 1 {
			t.Fatalf("err=%v finished=%v attempts=%d", err, finished, calls)
		}
	}
}

func TestStreamRecoveryStopsAtSafetyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, frames        string
		maxCalls            int
		cancel, emitFailure bool
	}{
		{name: "retry exhausted", maxCalls: 2},
		{name: "tool delivered", frames: toolFrames, maxCalls: 1},
		{name: "text delivered", frames: happyFrames[0] + toolFrames, maxCalls: 1},
		{name: "reasoning delivered", frames: happyFrames[1] + toolFrames, maxCalls: 1},
		{name: "cancelled", maxCalls: 1, cancel: true},
		{name: "upstream failed", frames: "data: {\"type\":\"response.failed\"}\n\n", maxCalls: 1},
		{name: "emit failed", frames: toolFrames + happyFrames[3], maxCalls: 1, emitFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProvider(t, "http://upstream.invalid", "", "", map[string]string{"access_token": "static-token"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			p.http.Transport = streamTransport(func(*http.Request) (*http.Response, error) {
				calls++
				if tc.cancel {
					cancel()
				}
				return &http.Response{StatusCode: 200, Proto: "HTTP/2.0", Body: brokenStream{strings.NewReader(tc.frames)}, Header: http.Header{"X-Request-Id": []string{"upstream-123"}}}, nil
			})
			finished := false
			emitErr := pluginapi.NewRetryableError("stream_read_failed", "consumer failed", 502)
			err := p.Stream(ctx, &pluginapi.Request{Model: "codex"}, func(e pluginapi.Event) error {
				if e.Type == pluginapi.EventFinish {
					finished = true
				}
				if tc.emitFailure {
					return emitErr
				}
				return nil
			})
			if err == nil || finished || calls != tc.maxCalls {
				t.Fatalf("err=%v finished=%v attempts=%d want=%d", err, finished, calls, tc.maxCalls)
			}
			if tc.emitFailure && !errors.Is(err, emitErr) {
				t.Fatalf("lost consumer error: %v", err)
			}
			if tc.name == "tool delivered" {
				for _, detail := range []string{"HTTP/2.0", "upstream-123", "response.function_call_arguments.delta"} {
					if !strings.Contains(err.Error(), detail) {
						t.Fatalf("missing diagnostic %q: %v", detail, err)
					}
				}
			}
			if tc.name == "retry exhausted" && !strings.Contains(err.Error(), "unexpected EOF") {
				t.Fatalf("lost read error: %v", err)
			}
		})
	}
}

// The upstream waits for the consumer to receive tool arguments before sending
// its terminal event. Buffering tools until completion deadlocks this exchange.
func TestStreamDeliversToolArgumentsBeforeCompletion(t *testing.T) {
	p := newTestProvider(t, "http://upstream.invalid", "", "", map[string]string{"access_token": "static-token"})
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	p.http.Transport = streamTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: reader, Header: make(http.Header)}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	argumentsReceived := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		if _, err := io.WriteString(writer, toolFrames); err != nil {
			writerDone <- err
			return
		}
		select {
		case <-argumentsReceived:
			_, err := io.WriteString(writer, happyFrames[3])
			writerDone <- err
		case <-ctx.Done():
			writer.CloseWithError(ctx.Err())
			writerDone <- ctx.Err()
		}
	}()
	var types []string
	err := p.Stream(ctx, &pluginapi.Request{Model: "codex"}, func(e pluginapi.Event) error {
		types = append(types, e.Type)
		if e.Type == pluginapi.EventToolArgsDelta {
			close(argumentsReceived)
		}
		return nil
	})
	writeErr := <-writerDone
	if err != nil || writeErr != nil {
		t.Fatalf("tool arguments must arrive before completion: stream=%v writer=%v", err, writeErr)
	}
	want := []string{pluginapi.EventToolCallStart, pluginapi.EventToolArgsDelta, pluginapi.EventUsage, pluginapi.EventFinish}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v, want %v", types, want)
	}
}
