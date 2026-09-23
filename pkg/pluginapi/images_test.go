package pluginapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// imageStub is a Provider that also implements ImageProvider, wired to a live SDK server
// over pipes: these tests exercise the wire contract, not just the structs.
type imageStub struct {
	images   *ImageRequest
	partials int
}

func (p *imageStub) Info() Info {
	return Info{Name: "image-stub", Version: "0.0.1", Capabilities: Capabilities{Images: true}}
}
func (p *imageStub) ListModels(context.Context) ([]ModelInfo, error) { return nil, nil }
func (p *imageStub) Complete(context.Context, *Request) (*Response, error) {
	return nil, NewError("image_model_only", "this plugin serves images only")
}
func (p *imageStub) Stream(context.Context, *Request, func(Event) error) error {
	return NewError("image_model_only", "this plugin serves images only")
}
func (p *imageStub) Health(context.Context) error { return nil }
func (p *imageStub) Actions() []Action            { return nil }
func (p *imageStub) RunAction(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, NewError("unknown_action", "nope")
}
func (p *imageStub) StateDir() string                 { return "" }
func (p *imageStub) SetCredentials(map[string]string) {}

func (p *imageStub) Images(_ context.Context, req *ImageRequest) (*ImageResponse, error) {
	p.images = req
	return &ImageResponse{
		Created: 1767225600,
		Data:    []Image{{B64JSON: "R0lGOD"}},
		Usage:   Usage{Dimensions: map[string]int64{"input": 25, "image_output": 4160}},
		Size:    "1024x1024", Quality: "high", Background: "auto", OutputFormat: "png",
	}, nil
}

func (p *imageStub) ImagesStream(_ context.Context, req *ImageRequest, emit func(Event) error) error {
	p.images = req
	for i := 0; i < req.PartialImages; i++ {
		p.partials++
		if err := emit(Event{Type: EventImagePartial, Image: &ImageEvent{
			B64JSON: "cGFydGlhbA==", PartialIndex: i, Size: "1024x1024", OutputFormat: "png",
		}}); err != nil {
			return err
		}
	}
	if err := emit(Event{Type: EventImageCompleted, Image: &ImageEvent{
		B64JSON: "ZmluYWw=", Size: "1024x1024", Quality: "high", Background: "auto",
		OutputFormat: "png", Created: 1767225601,
	}}); err != nil {
		return err
	}
	if err := emit(Event{Type: EventUsage, Usage: &Usage{
		Dimensions: map[string]int64{"input": 25, "image_output": 4160},
	}}); err != nil {
		return err
	}
	return emit(Event{Type: EventFinish, Reason: ReasonStop})
}

// startImagePlugin serves p over an in-memory pair of pipes and returns a client for it.
func startImagePlugin(t *testing.T, p Provider) *Client {
	t.Helper()
	hostR, pluginW := io.Pipe()
	pluginR, hostW := io.Pipe()

	info := p.Info()
	srv := &server{provider: p, enc: NewEncoder(pluginW), active: map[string]context.CancelFunc{}}
	go func() {
		_ = srv.enc.Write(Handshake{
			Type: FrameHandshake, Protocol: ProtocolVersion, Name: info.Name,
			Version: info.Version, Capabilities: info.Capabilities,
		})
		dec := NewDecoder(pluginR)
		for {
			frame, err := dec.ReadFrame()
			if err != nil {
				return
			}
			if srv.handle(context.Background(), frame) {
				return
			}
		}
	}()

	client, err := NewClient(hostR, hostW, 2*time.Second)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	return client
}

// TestImageRequestRoundTrip pins the canonical image request over the wire: the reference
// images survive as base64 (the frame protocol is text), and the fields the gateway
// normalises are what the plugin sees.
func TestImageRequestRoundTrip(t *testing.T) {
	stub := &imageStub{}
	client := startImagePlugin(t, stub)
	defer client.Close()

	compression := 80
	req := &ImageRequest{
		Model: "gpt-image-2", Op: ImageOpEdit, Prompt: "make it blue", N: 2,
		Size: "1024x1024", Quality: "high", OutputFormat: "webp",
		OutputCompression: &compression,
		PartialImages:     0,
		Input:             []ImageInput{{Name: "in.png", MIME: "image/png", Data: []byte("PNGDATA")}},
		Mask:              &ImageInput{Name: "mask.png", MIME: "image/png", Data: []byte("MASK")},
		Extra:             map[string]json.RawMessage{"moderation": json.RawMessage(`"low"`)},
	}
	resp, err := client.Images(context.Background(), req)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if resp.Created != 1767225600 || len(resp.Data) != 1 || resp.Data[0].B64JSON != "R0lGOD" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Usage.Dimensions["image_output"] != 4160 {
		t.Fatalf("usage dimensions lost: %+v", resp.Usage.Dimensions)
	}

	seen := stub.images
	if seen == nil {
		t.Fatal("the plugin never received the request")
	}
	if seen.Op != ImageOpEdit || seen.N != 2 || seen.OutputCompression == nil || *seen.OutputCompression != 80 {
		t.Fatalf("request fields lost: %+v", seen)
	}
	if len(seen.Input) != 1 || string(seen.Input[0].Data) != "PNGDATA" || seen.Input[0].MIME != "image/png" {
		t.Fatalf("reference image lost or mangled: %+v", seen.Input)
	}
	if seen.Mask == nil || string(seen.Mask.Data) != "MASK" {
		t.Fatalf("mask lost: %+v", seen.Mask)
	}
	if string(seen.Extra["moderation"]) != `"low"` {
		t.Fatalf("unmodelled field lost: %+v", seen.Extra)
	}
}

// TestImagesStreamRoundTrip pins the streaming contract: the partial frames reach the host
// in order, the terminal frame carries the finished image, usage arrives as an ordinary
// usage event (not inside the image payload), and finish becomes the end frame's reason.
func TestImagesStreamRoundTrip(t *testing.T) {
	stub := &imageStub{}
	client := startImagePlugin(t, stub)
	defer client.Close()

	var got []Event
	end, err := client.ImagesStream(context.Background(), &ImageRequest{
		Model: "gpt-image-2", Op: ImageOpGenerate, Prompt: "a fox", N: 1, PartialImages: 2,
	}, func(ev Event) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("ImagesStream: %v", err)
	}
	if end == nil || end.FinishReason != ReasonStop {
		t.Fatalf("end = %+v, want finish reason stop", end)
	}
	if len(got) != 4 {
		t.Fatalf("events = %d, want 2 partial + completed + usage: %+v", len(got), got)
	}
	if got[0].Type != EventImagePartial || got[1].Type != EventImagePartial {
		t.Fatalf("first two events must be partials: %+v", got)
	}
	if got[0].Image == nil || got[0].Image.PartialIndex != 0 || got[1].Image.PartialIndex != 1 {
		t.Fatalf("partial indices must be 0-based and ordered: %+v", got)
	}
	if got[2].Type != EventImageCompleted || got[2].Image == nil || got[2].Image.B64JSON != "ZmluYWw=" {
		t.Fatalf("completed event must carry the final image: %+v", got[2])
	}
	if got[3].Type != EventUsage || got[3].Usage == nil || got[3].Usage.Dimensions["input"] != 25 {
		t.Fatalf("usage event must carry the metered quantity: %+v", got[3])
	}
	// The finish event is protocol plumbing and must never surface as a client event.
	for _, ev := range got {
		if ev.Type == EventFinish {
			t.Fatal("the finish event must not be relayed")
		}
	}
	if end.Usage.Dimensions != nil {
		t.Fatalf("usage belongs in the usage event, not the end frame: %+v", end.Usage)
	}
}

// TestImageMethodsOnPluginWithoutImageSupport: a plugin that predates image support must
// answer with a protocol error, not by hanging or crashing.
func TestImageMethodsOnPluginWithoutImageSupport(t *testing.T) {
	legacy := &legacyPlugin{}
	client := startImagePlugin(t, legacy)
	defer client.Close()

	_, err := client.Images(context.Background(), &ImageRequest{Model: "gpt-image-2", Op: ImageOpGenerate})
	if err == nil {
		t.Fatal("Images on a chat-only plugin must fail")
	}
	if apiErr, ok := IsError(err); !ok || apiErr.Code != "unsupported_method" {
		t.Fatalf("error = %v, want unsupported_method", err)
	}

	_, err = client.ImagesStream(context.Background(), &ImageRequest{Model: "gpt-image-2", Op: ImageOpGenerate}, func(Event) error { return nil })
	if err == nil {
		t.Fatal("ImagesStream on a chat-only plugin must fail")
	}
	if apiErr, ok := IsError(err); !ok || apiErr.Code != "images_stream_unsupported" {
		t.Fatalf("error = %v, want images_stream_unsupported", err)
	}
}

// legacyPlugin implements Provider only.
type legacyPlugin struct{}

func (p *legacyPlugin) Info() Info {
	return Info{Name: "legacy", Capabilities: Capabilities{Complete: true}}
}
func (p *legacyPlugin) ListModels(context.Context) ([]ModelInfo, error) { return nil, nil }
func (p *legacyPlugin) Complete(context.Context, *Request) (*Response, error) {
	return &Response{Status: "completed"}, nil
}
func (p *legacyPlugin) Stream(context.Context, *Request, func(Event) error) error { return nil }
func (p *legacyPlugin) Health(context.Context) error                              { return nil }
func (p *legacyPlugin) Actions() []Action                                         { return nil }
func (p *legacyPlugin) RunAction(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, NewError("unknown_action", "nope")
}
func (p *legacyPlugin) StateDir() string                 { return "" }
func (p *legacyPlugin) SetCredentials(map[string]string) {}

// TestImageFramesExceedTheLegacyCeiling pins the reason MaxFrameBytes was raised: a single
// image payload is base64 inside one JSON frame, so a realistic answer does not fit under
// the old 8 MiB ceiling.
func TestImageFramesExceedTheLegacyCeiling(t *testing.T) {
	if MaxFrameBytes <= 8<<20 {
		t.Fatalf("MaxFrameBytes = %d, want the raised image ceiling", MaxFrameBytes)
	}
	const payload = 9 << 20 // just past the old limit, far below the new one
	frame := Frame{
		ID: "7", Type: FrameEvent,
		Event: &Event{Type: EventImageCompleted, Image: &ImageEvent{B64JSON: strings.Repeat("A", payload)}},
	}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(frame); err != nil {
		t.Fatalf("encoding a %d-byte image frame: %v", payload, err)
	}
	decoded, err := NewDecoder(&buf).ReadFrame()
	if err != nil {
		t.Fatalf("decoding a %d-byte image frame: %v", payload, err)
	}
	if decoded.Event == nil || decoded.Event.Image == nil || len(decoded.Event.Image.B64JSON) != payload {
		t.Fatal("the image payload did not survive the round trip")
	}
}
