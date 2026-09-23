package openaiimages

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// lf is the line feed used to build SSE frames (escape-free construction).
var lf = string([]byte{0x0A})

// newUpstream starts a fake images upstream and returns a provider pointed at it. The
// handler sees the raw request so the tests can assert what the gateway actually sends.
func newUpstream(t *testing.T, handler http.HandlerFunc) (*Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	raw, err := json.Marshal(map[string]any{"base_url": srv.URL + "/v1", "timeout_s": 5})
	if err != nil {
		t.Fatal(err)
	}
	p, err := New("images", string(raw), t.TempDir(), map[string]string{"api_key": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	return p, srv
}

// imageJSON is one full non-streaming images response.
const imageJSON = `{"created":1767225600,"data":[{"b64_json":"R0lGOD"}],` +
	`"usage":{"input_tokens":25,"input_tokens_details":{"text_tokens":20,"image_tokens":5},` +
	`"output_tokens":4160,"total_tokens":4185},` +
	`"size":"1024x1024","quality":"high","background":"auto","output_format":"png"}`

// TestImagesGenerateSendsTheCanonicalRequestBody pins the JSON surface: modelled fields are
// translated one by one, an unset field is omitted (so the upstream's own default applies),
// and an unmodelled client field travels through.
func TestImagesGenerateSendsTheCanonicalRequestBody(t *testing.T) {
	var (
		gotPath   string
		gotAuth   string
		gotAccept string
		gotBody   map[string]any
	)
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAccept = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Accept")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, imageJSON)
	})

	compression := 90
	resp, err := p.Images(context.Background(), &pluginapi.ImageRequest{
		Model: "gpt-image-2", Op: pluginapi.ImageOpGenerate, Prompt: "a fox", N: 2,
		Size: "1024x1024", Quality: "high", OutputFormat: "webp",
		OutputCompression: &compression,
		Extra:             map[string]json.RawMessage{"moderation": json.RawMessage(`"low"`)},
	})
	if err != nil {
		t.Fatalf("Images: %v", err)
	}

	if gotPath != "/v1/images/generations" {
		t.Fatalf("path = %q, want /v1/images/generations", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Fatalf("accept = %q", gotAccept)
	}
	want := map[string]any{
		"model": "gpt-image-2", "prompt": "a fox", "n": float64(2), "size": "1024x1024",
		"quality": "high", "output_format": "webp", "output_compression": float64(90),
		"moderation": "low",
	}
	for key, value := range want {
		if gotBody[key] != value {
			t.Errorf("body[%q] = %v, want %v", key, gotBody[key], value)
		}
	}
	for _, key := range []string{"background", "input_fidelity", "user", "stream", "partial_images"} {
		if _, exists := gotBody[key]; exists {
			t.Errorf("body must omit unset %q (the upstream's own default applies), got %v", key, gotBody[key])
		}
	}

	if resp.Created != 1767225600 || len(resp.Data) != 1 || resp.Data[0].B64JSON != "R0lGOD" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Size != "1024x1024" || resp.Quality != "high" || resp.OutputFormat != "png" {
		t.Fatalf("echo fields lost: %+v", resp)
	}
}

// TestUsageDimensionsSplitTextImageAndOutput pins the metering mapping the billing path
// depends on: the prompt is `input`, reference images are `image_input`, and the produced
// image is `image_output` — not all of it folded into the text dimensions.
func TestUsageDimensionsSplitTextImageAndOutput(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, imageJSON)
	})
	resp, err := p.Images(context.Background(), &pluginapi.ImageRequest{
		Model: "gpt-image-2", Op: pluginapi.ImageOpGenerate, Prompt: "a fox",
	})
	if err != nil {
		t.Fatal(err)
	}
	dims := resp.Usage.Dimensions
	if dims["input"] != 20 || dims["image_input"] != 5 || dims["image_output"] != 4160 {
		t.Fatalf("dimensions = %v, want input=20 image_input=5 image_output=4160", dims)
	}
	if resp.Usage.Estimated {
		t.Fatal("an upstream that reports usage is not an estimate")
	}
}

// TestUsageWithoutDetailFallsBackToPlainInput: a relay may report only the totals; the
// prompt tokens must still be metered (as `input`), never dropped.
func TestUsageWithoutDetailFallsBackToPlainInput(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"AA"}],"usage":{"input_tokens":30,"output_tokens":7}}`)
	})
	resp, err := p.Images(context.Background(), &pluginapi.ImageRequest{
		Model: "gpt-image-2", Op: pluginapi.ImageOpGenerate, Prompt: "a fox",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.Dimensions["input"] != 30 || resp.Usage.Dimensions["image_output"] != 7 {
		t.Fatalf("dimensions = %v, want input=30 image_output=7", resp.Usage.Dimensions)
	}
}

// TestEditSendsMultipartWithRepeatedImageFields pins the edit surface: the reference images
// go out as repeated image[] fields (what the official SDKs send), the mask under `mask`, and
// the media type the client declared is preserved.
func TestEditSendsMultipartWithRepeatedImageFields(t *testing.T) {
	var (
		gotPath        string
		gotContentType string
		gotFields      map[string]string
		gotFiles       map[string][]string
		gotMediaTypes  map[string]string
	)
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		mediaType, params, err := mime.ParseMediaType(gotContentType)
		if err != nil || mediaType != "multipart/form-data" {
			t.Errorf("content type = %q (%v)", gotContentType, err)
			return
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		gotFields, gotFiles, gotMediaTypes = map[string]string{}, map[string][]string{}, map[string]string{}
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			body, _ := io.ReadAll(part)
			if part.FileName() == "" {
				gotFields[part.FormName()] = string(body)
				continue
			}
			gotFiles[part.FormName()] = append(gotFiles[part.FormName()], string(body))
			gotMediaTypes[part.FormName()] = part.Header.Get("Content-Type")
		}
		_, _ = io.WriteString(w, imageJSON)
	})

	_, err := p.Images(context.Background(), &pluginapi.ImageRequest{
		Model: "gpt-image-2", Op: pluginapi.ImageOpEdit, Prompt: "make it blue", N: 1,
		Size: "1024x1024", InputFidelity: "high",
		Input: []pluginapi.ImageInput{
			{Name: "a.png", MIME: "image/png", Data: []byte("PNG-A")},
			{Name: "b.png", MIME: "image/png", Data: []byte("PNG-B")},
		},
		Mask: &pluginapi.ImageInput{Name: "m.png", MIME: "image/png", Data: []byte("PNG-M")},
	})
	if err != nil {
		t.Fatalf("Images: %v", err)
	}

	if gotPath != "/v1/images/edits" {
		t.Fatalf("path = %q, want /v1/images/edits", gotPath)
	}
	if gotFields["model"] != "gpt-image-2" || gotFields["prompt"] != "make it blue" ||
		gotFields["size"] != "1024x1024" || gotFields["input_fidelity"] != "high" || gotFields["n"] != "1" {
		t.Fatalf("form fields = %v", gotFields)
	}
	if got := gotFiles["image[]"]; len(got) != 2 || got[0] != "PNG-A" || got[1] != "PNG-B" {
		t.Fatalf("image[] parts = %v, want both reference images in order", got)
	}
	if got := gotFiles["mask"]; len(got) != 1 || got[0] != "PNG-M" {
		t.Fatalf("mask part = %v", got)
	}
	if gotMediaTypes["image[]"] != "image/png" {
		t.Fatalf("reference image media type = %q, want image/png", gotMediaTypes["image[]"])
	}
}

// TestEditWithoutReferenceImagesIsRejected: the gateway normalises the request, but the
// provider is the last line of defence and must not send a bodiless edit upstream.
func TestEditWithoutReferenceImagesIsRejected(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the upstream must not be called")
	})
	_, err := p.Images(context.Background(), &pluginapi.ImageRequest{
		Model: "gpt-image-2", Op: pluginapi.ImageOpEdit, Prompt: "make it blue",
	})
	if err == nil {
		t.Fatal("an edit without reference images must fail")
	}
}

// TestImagesStreamTranslatesPartialAndCompleted pins the streaming translation: upstream
// event names become canonical events, the terminal frame carries the finished image, usage
// is reported once as a usage event, and finish terminates the stream.
func TestImagesStreamTranslatesPartialAndCompleted(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`event: image_generation.partial_image` + lf +
				`data: {"type":"image_generation.partial_image","b64_json":"cGFydA==","partial_image_index":0,"size":"1024x1024","output_format":"png"}`,
			`event: image_generation.completed` + lf +
				`data: {"type":"image_generation.completed","b64_json":"ZmluYWw=","created_at":1767225601,"size":"1024x1024","quality":"high","background":"auto","output_format":"png","usage":{"input_tokens":25,"input_tokens_details":{"text_tokens":20,"image_tokens":5},"output_tokens":4160}}`,
		} {
			_, _ = io.WriteString(w, chunk+lf+lf)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	var events []pluginapi.Event
	err := p.ImagesStream(context.Background(), &pluginapi.ImageRequest{
		Model: "gpt-image-2", Op: pluginapi.ImageOpGenerate, Prompt: "a fox", PartialImages: 2,
	}, func(ev pluginapi.Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("ImagesStream: %v", err)
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["stream"] != true || gotBody["partial_images"] != float64(2) {
		t.Fatalf("streaming flags missing from the upstream body: %v", gotBody)
	}

	// partial → completed → usage → finish: the terminal marker is what stops the host from
	// serving a stream that died mid-answer as a complete one.
	if len(events) != 4 {
		t.Fatalf("events = %+v, want partial + completed + usage + finish", events)
	}
	if events[0].Type != pluginapi.EventImagePartial || events[0].Image.B64JSON != "cGFydA==" {
		t.Fatalf("partial event = %+v", events[0])
	}
	if events[1].Type != pluginapi.EventImageCompleted || events[1].Image.B64JSON != "ZmluYWw=" ||
		events[1].Image.Created != 1767225601 || events[1].Image.Quality != "high" {
		t.Fatalf("completed event = %+v", events[1])
	}
	if events[2].Type != pluginapi.EventUsage || events[2].Usage.Dimensions["image_output"] != 4160 {
		t.Fatalf("usage event = %+v", events[2])
	}
	if events[3].Type != pluginapi.EventFinish || events[3].Reason != pluginapi.ReasonStop {
		t.Fatalf("finish event = %+v", events[3])
	}
}

// TestImagesStreamWithoutCompletedEventFails: half a picture is not a picture. The upstream
// dropping the connection mid-generation must fail the attempt, never serve what it sent.
func TestImagesStreamWithoutCompletedEventFails(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `event: image_generation.partial_image`+lf+
			`data: {"type":"image_generation.partial_image","b64_json":"cGFydA==","partial_image_index":0}`+lf+lf)
	})
	var events []pluginapi.Event
	err := p.ImagesStream(context.Background(), &pluginapi.ImageRequest{
		Model: "gpt-image-2", Op: pluginapi.ImageOpGenerate, Prompt: "a fox",
	}, func(ev pluginapi.Event) error {
		events = append(events, ev)
		return nil
	})
	if err == nil {
		t.Fatal("a stream without a completed event must fail")
	}
	if apiErr, ok := pluginapi.IsError(err); !ok || !apiErr.Retryable {
		t.Fatalf("error = %v, want a retryable protocol error", err)
	}
	if !strings.Contains(err.Error(), "partial frames") {
		t.Fatalf("the error must say what was missing: %v", err)
	}
}

// TestUpstreamErrorsAreClassified: the router decides failover from these, so the
// classification has to match the chat providers' (401 fatal, 429 quota, 503 retryable).
func TestUpstreamErrorsAreClassified(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
	}
	for _, tc := range cases {
		p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream says no"}}`)
		})
		_, err := p.Images(context.Background(), &pluginapi.ImageRequest{
			Model: "gpt-image-2", Op: pluginapi.ImageOpGenerate, Prompt: "a fox",
		})
		apiErr, ok := pluginapi.IsError(err)
		if !ok {
			t.Fatalf("status %d: error = %v, want a protocol error", tc.status, err)
		}
		if apiErr.Retryable != tc.retryable {
			t.Errorf("status %d: retryable = %v, want %v", tc.status, apiErr.Retryable, tc.retryable)
		}
		if !strings.Contains(apiErr.Message, "upstream says no") {
			t.Errorf("status %d: the upstream message must survive: %q", tc.status, apiErr.Message)
		}
	}
}

// TestChatShapedCallsAreRefusedWithAPointer: an operator who points a text model at this
// provider must get an answer that names the fix.
func TestChatShapedCallsAreRefusedWithAPointer(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the upstream must not be called for a chat-shaped request")
	})
	_, err := p.Complete(context.Background(), &pluginapi.Request{Model: "gpt-image-2"})
	apiErr, ok := pluginapi.IsError(err)
	if !ok || apiErr.Code != "image_model_only" || apiErr.Retryable {
		t.Fatalf("error = %v, want a non-retryable image_model_only", err)
	}
	if !strings.Contains(apiErr.Message, "/v1/images/generations") {
		t.Fatalf("the error must point at the endpoint to use: %q", apiErr.Message)
	}
	if err := p.Stream(context.Background(), &pluginapi.Request{Model: "gpt-image-2"}, func(pluginapi.Event) error { return nil }); err == nil {
		t.Fatal("Stream must refuse a chat-shaped request too")
	}
}

// TestListModelsCarriesCapabilities: the console's "refresh models" turns this into
// provider-model rows, so a declared image model has to arrive with its capability keys or
// every image request would be routed nowhere.
func TestListModelsCarriesCapabilities(t *testing.T) {
	raw := `{"base_url":"http://127.0.0.1:1/v1","models":[
	  {"public":"gpt-image-2","upstream":"gpt-image-2-2026-04-21",
	   "capabilities":{"image_generation":true,"image":true}}]}`
	p, err := New("images", raw, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "gpt-image-2" || models[0].UpstreamModel != "gpt-image-2-2026-04-21" {
		t.Fatalf("models = %+v", models)
	}
	if !models[0].Capabilities["image_generation"] || !models[0].Capabilities["image"] {
		t.Fatalf("capabilities lost: %+v", models[0].Capabilities)
	}
}
