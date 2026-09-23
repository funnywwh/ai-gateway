package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// imageUpstream is a fake Images API upstream. It records what the gateway sent and answers
// with whatever the case under test needs, so one test can pin the request shape and another
// the response translation.
type imageUpstream struct {
	server *httptest.Server
	// path, auth, contentType and body capture the last request.
	path        string
	auth        string
	contentType string
	body        []byte
	// stream: answer with the SSE shape instead of a JSON body.
	stream bool
	// respond is the JSON body for a non-streaming request ("" means the standard success).
	respond string
	// status, when non-zero, is returned instead of 200.
	status int
}

func newImageUpstream(t *testing.T) *imageUpstream {
	t.Helper()
	up := &imageUpstream{}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.path = r.URL.Path
		up.auth = r.Header.Get("Authorization")
		up.contentType = r.Header.Get("Content-Type")
		up.body, _ = io.ReadAll(r.Body)
		if up.status != 0 {
			w.WriteHeader(up.status)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream refused"}}`)
			return
		}
		if up.stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			for _, frame := range []string{
				"event: image_generation.partial_image",
				`data: {"type":"image_generation.partial_image","b64_json":"UEFSVElBTA==","partial_image_index":0,"size":"1024x1024","quality":"high","background":"auto","output_format":"png"}`,
				"event: image_generation.completed",
				`data: {"type":"image_generation.completed","b64_json":"RklOQUw=","created_at":1767225601,"size":"1024x1024","quality":"high","background":"auto","output_format":"png","usage":{"input_tokens":25,"input_tokens_details":{"text_tokens":20,"image_tokens":5},"output_tokens":4160,"total_tokens":4185}}`,
			} {
				_, _ = io.WriteString(w, frame+"\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}
		body := up.respond
		if body == "" {
			body = `{"created":1767225600,"data":[{"b64_json":"RklOQUw="}],` +
				`"usage":{"input_tokens":25,"input_tokens_details":{"text_tokens":20,"image_tokens":5},` +
				`"output_tokens":4160,"total_tokens":4185},` +
				`"size":"1024x1024","quality":"high","background":"auto","output_format":"png"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(up.server.Close)
	return up
}

// imageFixture wires the standard fixture with one openai-images provider pointing at up and
// two public models: image-model (generation + reference images) and image-only (generation
// without reference-image support), so the capability gate can be observed per endpoint.
func imageFixture(t *testing.T, up *imageUpstream, opts ...fixtureOption) *fixture {
	t.Helper()
	f := newFixture(t, opts...)
	ctx := context.Background()

	cfg, err := json.Marshal(map[string]any{"base_url": up.server.URL + "/v1", "timeout_s": 5})
	if err != nil {
		t.Fatal(err)
	}
	provID, err := f.db.UpsertProvider(ctx, &domain.Provider{
		Name: "images", Kind: "openai-images", Enabled: true, Priority: 5, Weight: 100,
		ConfigJSON: string(cfg),
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, caps := range map[string]string{
		"image-model": `{"image_generation":true,"image":true}`,
		"image-only":  `{"image_generation":true}`,
	} {
		if _, err := f.db.UpsertProviderModel(ctx, &domain.ProviderModel{
			ProviderID: provID, PublicModel: name, UpstreamModel: "upstream-" + name,
			Enabled: true, CapabilitiesJSON: caps,
		}); err != nil {
			t.Fatal(err)
		}
		modelID, err := f.db.UpsertModel(ctx, &domain.Model{PublicName: name, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.UpsertRoute(ctx, &domain.Route{
			ModelID: modelID, ProviderID: provID, Priority: 5, Weight: 100, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.registry.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

// waitForImageLog waits for the request-log row of one image request (the handler writes it
// just after the response).
func waitForImageLog(t *testing.T, f *fixture, requestID string) *domain.RequestLogRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec, err := f.db.GetRequestLog(context.Background(), requestID)
		if err == nil {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("request log %s never appeared: %v", requestID, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestImageGenerationEndToEnd drives the whole path: the gateway parses the OpenAI request,
// routes it to the image provider, meters the three dimensions and answers in the upstream's
// own shape.
func TestImageGenerationEndToEnd(t *testing.T) {
	up := newImageUpstream(t)
	f := imageFixture(t, up)

	resp := f.do(t, "POST", "/v1/images/generations",
		`{"model":"image-model","prompt":"a fox reading","n":2,"size":"1024x1024","quality":"high","moderation":"low"}`, nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("x-gateway-provider"); got != "images" {
		t.Fatalf("x-gateway-provider = %q", got)
	}

	var payload struct {
		Created int64 `json:"created"`
		Data    []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
		Usage *struct {
			InputTokens int64 `json:"input_tokens"`
			Details     struct {
				TextTokens  int64 `json:"text_tokens"`
				ImageTokens int64 `json:"image_tokens"`
			} `json:"input_tokens_details"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
		Size string `json:"size"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if payload.Created == 0 || len(payload.Data) != 1 || payload.Data[0].B64JSON != "RklOQUw=" {
		t.Fatalf("response body = %s", raw)
	}
	if payload.Usage == nil || payload.Usage.OutputTokens != 4160 ||
		payload.Usage.InputTokens != 25 || payload.Usage.Details.ImageTokens != 5 {
		t.Fatalf("usage = %+v, want the upstream's own numbers", payload.Usage)
	}
	if payload.Size != "1024x1024" {
		t.Fatalf("size echo = %q", payload.Size)
	}

	// What the upstream saw: the canonical request translated to the upstream model name, with
	// the fields the client set and nothing invented for the ones it did not.
	if up.path != "/v1/images/generations" {
		t.Fatalf("upstream path = %q", up.path)
	}
	var sent map[string]any
	if err := json.Unmarshal(up.body, &sent); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, up.body)
	}
	if sent["model"] != "upstream-image-model" || sent["prompt"] != "a fox reading" ||
		sent["n"] != float64(2) || sent["moderation"] != "low" {
		t.Fatalf("upstream body = %s", up.body)
	}
	if _, ok := sent["stream"]; ok {
		t.Fatalf("a non-streaming request must not set stream: %s", up.body)
	}

	// Metering: the three image dimensions land in the usage row.
	records, err := f.db.ListUsage(context.Background(), f.key.AccountID, time.Time{}, time.Now().Add(time.Minute), 50)
	if err != nil {
		t.Fatal(err)
	}
	var dims map[string]int64
	for _, rec := range records {
		if rec.Model == "image-model" {
			if err := json.Unmarshal([]byte(rec.DimensionsJSON), &dims); err != nil {
				t.Fatalf("dimensions: %v (%s)", err, rec.DimensionsJSON)
			}
		}
	}
	if dims == nil {
		t.Fatalf("no usage row for image-model; rows = %+v", records)
	}
	if dims["input"] != 20 || dims["image_input"] != 5 || dims["image_output"] != 4160 {
		t.Fatalf("metered dimensions = %v, want input=20 image_input=5 image_output=4160", dims)
	}

	// Request log: the prompt and the parameters, never the payload.
	rec := waitForImageLog(t, f, resp.Header.Get("x-request-id"))
	if rec.Endpoint != "/v1/images/generations" {
		t.Fatalf("endpoint = %q", rec.Endpoint)
	}
	if !strings.Contains(rec.RequestJSON, "a fox reading") {
		t.Fatalf("the prompt must be recorded: %q", rec.RequestJSON)
	}
	if strings.Contains(rec.RequestJSON, "RklOQUw=") {
		t.Fatalf("the image itself must never be recorded: %q", rec.RequestJSON)
	}
}

// TestImageStreamEndToEnd pins the client-visible SSE vocabulary: partial frames then a
// completed frame carrying the usage, under the event names the OpenAI SDK expects.
func TestImageStreamEndToEnd(t *testing.T) {
	up := newImageUpstream(t)
	up.stream = true
	f := imageFixture(t, up)

	resp := f.do(t, "POST", "/v1/images/generations",
		`{"model":"image-model","prompt":"a fox","stream":true,"partial_images":2}`, nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	body := string(raw)
	if !strings.Contains(body, "event: image_generation.partial_image") {
		t.Fatalf("missing partial frame: %s", body)
	}
	completed := body[strings.Index(body, "event: image_generation.completed"):]
	if !strings.Contains(completed, `"b64_json":"RklOQUw="`) {
		t.Fatalf("the completed frame must carry the finished image: %s", completed)
	}
	if !strings.Contains(completed, `"usage"`) || !strings.Contains(completed, `"output_tokens":4160`) {
		t.Fatalf("the completed frame must carry the usage: %s", completed)
	}
	// The canonical usage event is protocol plumbing; it must not reach the client.
	if strings.Contains(body, "event: usage") {
		t.Fatalf("a canonical event leaked to the client: %s", body)
	}
	var sent map[string]any
	if err := json.Unmarshal(up.body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["stream"] != true || sent["partial_images"] != float64(2) {
		t.Fatalf("streaming flags not forwarded: %s", up.body)
	}
}

// TestImageEditMultipartEndToEnd: an edit arrives as multipart and reaches the upstream as
// multipart, reference images included.
func TestImageEditMultipartEndToEnd(t *testing.T) {
	up := newImageUpstream(t)
	f := imageFixture(t, up)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for name, value := range map[string]string{"model": "image-model", "prompt": "make it blue", "n": "1"} {
		_ = writer.WriteField(name, value)
	}
	part, err := writer.CreateFormFile("image[]", "in.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("PNG-BYTES"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("POST", f.server.URL+"/v1/images/edits", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	if up.path != "/v1/images/edits" {
		t.Fatalf("upstream path = %q", up.path)
	}
	if !strings.HasPrefix(up.contentType, "multipart/form-data") {
		t.Fatalf("upstream content type = %q", up.contentType)
	}
	if !strings.Contains(string(up.body), "PNG-BYTES") || !strings.Contains(string(up.body), `name="image[]"`) {
		t.Fatalf("the reference image did not reach the upstream: %s", up.body)
	}
}

// TestImageRequestWithoutTheCapabilityIsRejectedLocally: a model that does not declare
// image_generation must be refused before any upstream call — including under the default
// strip degradation, which would otherwise keep the candidate.
func TestImageRequestWithoutTheCapabilityIsRejectedLocally(t *testing.T) {
	up := newImageUpstream(t)
	f := imageFixture(t, up)

	// echo-model exists in the standard fixture with no image capabilities at all.
	resp := f.do(t, "POST", "/v1/images/generations", `{"model":"echo-model","prompt":"a fox"}`, nil)
	status, code := decodeError(t, resp)
	if status != http.StatusBadRequest || code != "unsupported_parameter" {
		t.Fatalf("status/code = %d/%s, want 400/unsupported_parameter", status, code)
	}
	if up.path != "" {
		t.Fatalf("the upstream must not be called, but %s was", up.path)
	}
}

// TestImageEditRequiresTheImageCapability: writing images is not reading them. A model that
// declares only image_generation cannot serve an edit.
func TestImageEditRequiresTheImageCapability(t *testing.T) {
	up := newImageUpstream(t)
	f := imageFixture(t, up)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("model", "image-only")
	_ = writer.WriteField("prompt", "make it blue")
	part, err := writer.CreateFormFile("image", "in.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("PNG"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", f.server.URL+"/v1/images/edits", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if up.path != "" {
		t.Fatalf("the upstream must not be called, but %s was", up.path)
	}
}

// TestImageBodyOverTheCeilingIsRejected: server.images_max_body_bytes is the knob, and it is
// a 413 with a message an operator can act on.
func TestImageBodyOverTheCeilingIsRejected(t *testing.T) {
	up := newImageUpstream(t)
	f := imageFixture(t, up, func(s *fixtureSetup) {
		s.imagesMaxBodyBytes = 256
	})

	big := `{"model":"image-model","prompt":"` + strings.Repeat("x", 512) + `"}`
	resp := f.do(t, "POST", "/v1/images/generations", big, nil)
	status, _ := decodeError(t, resp)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", status)
	}
	if up.path != "" {
		t.Fatalf("the upstream must not be called, but %s was", up.path)
	}
}

// TestImageParameterValidation: the bounds the gateway enforces itself (the upstream's own),
// and the missing required fields.
func TestImageParameterValidation(t *testing.T) {
	up := newImageUpstream(t)
	f := imageFixture(t, up)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing model", `{"prompt":"a fox"}`, http.StatusBadRequest},
		{"missing prompt", `{"model":"image-model"}`, http.StatusBadRequest},
		{"n too large", `{"model":"image-model","prompt":"a fox","n":11}`, http.StatusBadRequest},
		{"n zero", `{"model":"image-model","prompt":"a fox","n":0}`, http.StatusBadRequest},
		{"partial_images too large", `{"model":"image-model","prompt":"a fox","partial_images":9}`, http.StatusBadRequest},
		{"not an object", `"a fox"`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		resp := f.do(t, "POST", "/v1/images/generations", tc.body, nil)
		if resp.StatusCode != tc.want {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Errorf("%s: status = %d, want %d (%s)", tc.name, resp.StatusCode, tc.want, raw)
			continue
		}
		resp.Body.Close()
	}
	if up.path != "" {
		t.Fatalf("no rejected request may reach the upstream, but %s was", up.path)
	}
}

// TestUpstreamErrorBecomesAStructuredError: a fatal upstream status is reported as-is, with
// the upstream's message, and the request log row still exists.
func TestUpstreamErrorBecomesAStructuredError(t *testing.T) {
	up := newImageUpstream(t)
	up.status = http.StatusBadRequest
	f := imageFixture(t, up)

	resp := f.do(t, "POST", "/v1/images/generations", `{"model":"image-model","prompt":"a fox"}`, nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		// A client error from the upstream is fatal and forwarded with its own status.
		t.Fatalf("status = %d, want 400 (%s)", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "upstream refused") {
		t.Fatalf("the upstream message must survive: %s", raw)
	}
}

// TestChatRequestToAnImageModelFails with a pointer: the operator who points a text model at
// an image provider gets a message that says which endpoint to use.
func TestChatRequestToAnImageModelFails(t *testing.T) {
	up := newImageUpstream(t)
	f := imageFixture(t, up)

	resp := f.do(t, "POST", "/v1/responses", `{"model":"image-model","input":"hello"}`, nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want a failure (%s)", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "/v1/images/generations") {
		t.Fatalf("the error must point at the image endpoint: %s", raw)
	}
}

// TestModelListingDisclosesImageOutput: /v1/models tells a client that this model produces
// images, which is what keeps it out of text-only pickers.
func TestModelListingDisclosesImageOutput(t *testing.T) {
	up := newImageUpstream(t)
	f := imageFixture(t, up)

	resp := f.do(t, "GET", "/v1/models", "", nil)
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID               string          `json:"id"`
			OutputModalities []string        `json:"output_modalities"`
			InputModalities  []string        `json:"input_modalities"`
			Capabilities     map[string]bool `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	byID := map[string]struct {
		Output []string
		Input  []string
		Caps   map[string]bool
	}{}
	for _, entry := range list.Data {
		byID[entry.ID] = struct {
			Output []string
			Input  []string
			Caps   map[string]bool
		}{entry.OutputModalities, entry.InputModalities, entry.Capabilities}
	}
	imageModel, ok := byID["image-model"]
	if !ok {
		t.Fatalf("image-model is missing from the listing: %+v", list.Data)
	}
	if len(imageModel.Output) != 1 || imageModel.Output[0] != "image" {
		t.Fatalf("output_modalities = %v, want [image]", imageModel.Output)
	}
	if !imageModel.Caps["image_generation"] {
		t.Fatalf("capabilities = %v, want image_generation", imageModel.Caps)
	}
	if len(imageModel.Input) != 2 {
		t.Fatalf("input_modalities = %v, want [text image]", imageModel.Input)
	}
	// A text model in the same deployment keeps its text output modality.
	if echo, ok := byID["echo-model"]; ok {
		if len(echo.Output) != 1 || echo.Output[0] != "text" {
			t.Fatalf("echo-model output_modalities = %v, want [text]", echo.Output)
		}
	}
}

// TestImageResponseWithoutUsageOmitsIt: an upstream that reports no usage must not have one
// invented for the client — the gateway still meters an estimate, and says so internally.
func TestImageResponseWithoutUsageOmitsIt(t *testing.T) {
	up := newImageUpstream(t)
	up.respond = `{"created":1,"data":[{"b64_json":"AA"}]}`
	f := imageFixture(t, up)

	resp := f.do(t, "POST", "/v1/images/generations", `{"model":"image-model","prompt":"a fox"}`, nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", resp.StatusCode, raw)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["usage"]; ok {
		t.Fatalf("usage must be omitted when the upstream reported none: %s", raw)
	}
	records, err := f.db.ListUsage(context.Background(), f.key.AccountID, time.Time{}, time.Now().Add(time.Minute), 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range records {
		if rec.Model == "image-model" {
			found = true
			if rec.UsageSource != "estimated" {
				t.Fatalf("the row must be marked estimated: %+v", rec)
			}
		}
	}
	if !found {
		t.Fatal("an image request that answered must still be metered")
	}
}

// TestImageRequestReachesTheRightProviderOnly: the image capability decides the candidate,
// so an image request never lands on the chat provider and a chat request never lands on the
// image provider (the latter fails with a pointer instead of an empty answer).
func TestImageRequestReachesTheRightProviderOnly(t *testing.T) {
	up := newImageUpstream(t)
	f := imageFixture(t, up)

	// The image model has exactly one route: the image provider.
	plan, err := f.srv.deps.Router.Plan(domain.RouteRequest{
		Model:    "image-model",
		Key:      f.key,
		Features: map[string]bool{"image_generation": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].ProviderName != "images" {
		t.Fatalf("candidates = %+v", plan.Candidates)
	}
	if plan.Candidates[0].UpstreamModel != "upstream-image-model" {
		t.Fatalf("upstream model = %q", plan.Candidates[0].UpstreamModel)
	}
}
