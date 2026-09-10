package pluginapi

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// fakePlugin speaks the protocol over pipes, exactly like a real plugin process would.
type fakePlugin struct {
	mu         sync.Mutex
	cancelSeen chan string
	enc        *Encoder
}

func startFakePlugin(t *testing.T, hs Handshake, complete func(*Request) (any, *Error), stream func(*fakePlugin, string, *Request)) (*Client, *fakePlugin) {
	t.Helper()

	pluginR, hostW := io.Pipe()
	hostR, pluginW := io.Pipe()

	fp := &fakePlugin{cancelSeen: make(chan string, 4), enc: NewEncoder(pluginW)}

	go func() {
		if err := fp.enc.Write(hs); err != nil {
			return
		}
		dec := NewDecoder(pluginR)
		for {
			frame, err := dec.ReadFrame()
			if err != nil {
				return
			}
			switch frame.Method {
			case MethodPing:
				_ = fp.enc.Write(Frame{Type: FramePong})
			case MethodCancel:
				var params CancelParams
				_ = DecodeParams(frame.Params, &params)
				select {
				case fp.cancelSeen <- params.ID + ":" + params.Reason:
				default:
				}
			case MethodListModels:
				raw, _ := EncodeParams([]ModelInfo{{ID: "llama-local", UpstreamModel: "llama3.1:8b"}})
				_ = fp.enc.Write(Frame{ID: frame.ID, Type: FrameResult, Result: raw})
			case MethodShutdown:
				return
			case MethodAction:
				raw, _ := EncodeParams(ActionStatus{Status: "pending_user", URL: "https://example.com/device", Code: "AB12", PollMS: 500})
				_ = fp.enc.Write(Frame{ID: frame.ID, Type: FrameResult, Result: raw})
			case MethodComplete:
				var req Request
				_ = DecodeParams(frame.Params, &req)
				if complete == nil {
					_ = fp.enc.Write(Frame{ID: frame.ID, Type: FrameError, Error: NewError("not_supported", "complete not supported")})
					continue
				}
				result, apiErr := complete(&req)
				if apiErr != nil {
					_ = fp.enc.Write(Frame{ID: frame.ID, Type: FrameError, Error: apiErr})
					continue
				}
				raw, _ := EncodeParams(result)
				_ = fp.enc.Write(Frame{ID: frame.ID, Type: FrameResult, Result: raw})
			case MethodStream:
				var req Request
				_ = DecodeParams(frame.Params, &req)
				if stream != nil {
					stream(fp, frame.ID, &req)
				}
			}
		}
	}()

	client, err := NewClient(hostR, hostW, 2*time.Second)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	return client, fp
}

func testHandshake() Handshake {
	return Handshake{
		Type:     FrameHandshake,
		Protocol: ProtocolVersion,
		Name:     "fake",
		Version:  "0.0.1",
		Capabilities: Capabilities{
			Complete: true, Stream: true, ListModels: true, Health: true,
			UsageDelta: true, UsageDimensions: true,
		},
	}
}

func TestClientHandshakeAndListModels(t *testing.T) {
	client, _ := startFakePlugin(t, testHandshake(), nil, nil)
	defer client.Close()

	hs := client.Handshake()
	if hs.Name != "fake" || hs.Protocol != ProtocolVersion || !hs.Capabilities.Stream {
		t.Fatalf("handshake mismatch: %+v", hs)
	}
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].UpstreamModel != "llama3.1:8b" {
		t.Fatalf("models mismatch: %+v", models)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientProtocolMismatch(t *testing.T) {
	pluginR, hostW := io.Pipe()
	hostR, pluginW := io.Pipe()
	go func() {
		enc := NewEncoder(pluginW)
		hs := testHandshake()
		hs.Protocol = ProtocolVersion + 1
		_ = enc.Write(hs)
		_, _ = io.Copy(io.Discard, pluginR)
	}()
	if _, err := NewClient(hostR, hostW, time.Second); err == nil {
		t.Fatal("expected a protocol mismatch error")
	}
}

func TestClientCompleteAndErrorMapping(t *testing.T) {
	client, _ := startFakePlugin(t, testHandshake(), func(req *Request) (any, *Error) {
		if req.Model == "boom" {
			return nil, NewRetryableError("upstream_5xx", "upstream exploded", 503)
		}
		return Response{
			Items:  []Item{{Type: "message", Role: "assistant", Content: json.RawMessage(`[{"type":"output_text","text":"hi"}]`)}},
			Usage:  Usage{Dimensions: map[string]int64{"input": 10, "output": 2}},
			Status: "completed",
		}, nil
	}, nil)
	defer client.Close()

	resp, err := client.Complete(context.Background(), &Request{Model: "llama"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "completed" || resp.Usage.Dimensions["output"] != 2 || len(resp.Items) != 1 {
		t.Fatalf("response mismatch: %+v", resp)
	}

	_, err = client.Complete(context.Background(), &Request{Model: "boom"})
	apiErr, ok := IsError(err)
	if !ok {
		t.Fatalf("expected a protocol error, got %v", err)
	}
	if !apiErr.Retryable || apiErr.HTTPStatus != 503 || apiErr.Kind != KindRetryable {
		t.Fatalf("error mapping wrong: %+v", apiErr)
	}
}

func TestClientStreamDeliversEventsInOrder(t *testing.T) {
	client, _ := startFakePlugin(t, testHandshake(), nil, func(fp *fakePlugin, id string, req *Request) {
		for _, chunk := range []string{"Hel", "lo ", "world"} {
			ev := Event{Type: EventTextDelta, Text: chunk}
			_ = fp.enc.Write(Frame{ID: id, Type: FrameEvent, Event: &ev})
		}
		delta := Event{Type: EventUsageDelta, Usage: &Usage{Dimensions: map[string]int64{"output": 3}, Estimated: true}, Estimated: true}
		_ = fp.enc.Write(Frame{ID: id, Type: FrameEvent, Event: &delta})
		end, _ := EncodeParams(StreamEnd{FinishReason: "stop"})
		_ = fp.enc.Write(Frame{ID: id, Type: FrameEnd, Result: end})
	})
	defer client.Close()

	var got []string
	var usageDims map[string]int64
	end, err := client.Stream(context.Background(), &Request{Model: "llama"}, func(ev Event) error {
		switch ev.Type {
		case EventTextDelta:
			got = append(got, ev.Text)
		case EventUsageDelta:
			usageDims = ev.Usage.Dimensions
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if end.FinishReason != "stop" || end.Partial {
		t.Fatalf("stream end mismatch: %+v", end)
	}
	if len(got) != 3 || got[0] != "Hel" || got[2] != "world" {
		t.Fatalf("events out of order: %v", got)
	}
	if usageDims["output"] != 3 {
		t.Fatalf("usage delta not delivered: %+v", usageDims)
	}
}

func TestClientCancelPropagatesToPlugin(t *testing.T) {
	client, fp := startFakePlugin(t, testHandshake(), nil, func(f *fakePlugin, id string, req *Request) {
		ev := Event{Type: EventTextDelta, Text: "first"}
		_ = f.enc.Write(Frame{ID: id, Type: FrameEvent, Event: &ev})
		// then keep the stream open until cancelled, echoing the partial end
		select {
		case <-f.cancelSeen:
			end, _ := EncodeParams(StreamEnd{FinishReason: "cancelled", Partial: true})
			_ = f.enc.Write(Frame{ID: id, Type: FrameEnd, Result: end})
		case <-time.After(2 * time.Second):
		}
	})
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	seen := 0
	_, err := client.Stream(ctx, &Request{Model: "llama"}, func(ev Event) error {
		seen++
		if seen == 1 {
			cancel() // cancel after the first delta
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected a context cancellation error")
	}
	select {
	case got := <-fp.cancelSeen:
		if got == "" {
			t.Fatal("empty cancel payload")
		}
	case <-time.After(time.Second):
		t.Fatal("plugin never received provider.cancel")
	}
}

func TestClientCredentialsNotification(t *testing.T) {
	pluginR, hostW := io.Pipe()
	hostR, pluginW := io.Pipe()

	go func() {
		enc := NewEncoder(pluginW)
		_ = enc.Write(testHandshake())
		params, _ := EncodeParams(CredentialsParams{
			Credentials: map[string]string{"access_token": "rotated"},
			Reason:      "token_refreshed",
		})
		_ = enc.Write(Frame{Type: FrameNotify, Method: MethodCredentials, Params: params})
		_, _ = io.Copy(io.Discard, pluginR)
	}()

	client, err := NewClient(hostR, hostW, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	got := make(chan string, 1)
	client.OnCredentials(func(creds map[string]string, reason string) {
		got <- creds["access_token"] + "|" + reason
	})
	select {
	case v := <-got:
		if v != "rotated|token_refreshed" {
			t.Fatalf("credentials payload mismatch: %q", v)
		}
	case <-time.After(time.Second):
		t.Fatal("credentials notification not delivered")
	}
}

func TestClientRunActionPendingUser(t *testing.T) {
	client, _ := startFakePlugin(t, testHandshake(), nil, nil)
	defer client.Close()

	raw, err := client.RunAction(context.Background(), "login", nil)
	if err != nil {
		t.Fatal(err)
	}
	var status ActionStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != "pending_user" || status.Code != "AB12" || status.PollMS != 500 {
		t.Fatalf("action status mismatch: %+v", status)
	}
}
