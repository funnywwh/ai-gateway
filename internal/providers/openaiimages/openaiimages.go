// Package openaiimages implements a builtin provider for upstreams that speak the OpenAI
// Images API natively: POST /images/generations (JSON) and POST /images/edits (multipart),
// with the GPT image models' optional streaming mode.
//
// It is a provider of its own rather than a mode of openai-chat or openai-responses because
// the Images API shares neither the request nor the response shape with them: there is no
// instructions/input/tools, and the answer is an image rather than a message. A provider that
// has to ask "which kind of call is this" in every method would be the alternative.
package openaiimages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/providers/httpx"
	"github.com/winger/ai-gateway/pkg/pluginapi"
	"github.com/winger/ai-gateway/pkg/providerkit"
)

// defaultTimeoutS is the HTTP client timeout an empty timeout_s falls back to.
//
// It is longer than the chat providers' 120s on purpose: an image generation runs for tens
// of seconds, and a high-quality one at a large size can exceed a minute. The upstream
// answer is the only thing this timeout bounds; the client's own patience is its business.
const defaultTimeoutS = 300

// maxResponseBytes caps one non-streaming response for the same reason the plugin protocol
// caps a frame: a multi-image answer is base64, and 10 images at a megabyte each leave a
// small-looking ceiling behind. It matches MaxFrameBytes so a provider's answer can always
// cross the protocol.
const maxResponseBytes = pluginapi.MaxFrameBytes

// Config is the provider configuration.
type Config struct {
	BaseURL  string            `json:"base_url"`
	APIKey   string            `json:"api_key"`
	Headers  map[string]string `json:"headers"`
	TimeoutS int               `json:"timeout_s"`
	Models   []ModelConfig     `json:"models"`
}

// ModelConfig declares one upstream image model.
//
// Capabilities is free-form like every other provider's, but two keys matter here:
// image_generation (this model serves the Images API) and image (it accepts image input,
// which is what an edit needs).
type ModelConfig struct {
	Public       string          `json:"public"`
	Upstream     string          `json:"upstream"`
	Capabilities map[string]bool `json:"capabilities"`
}

// Provider talks to an upstream Images API.
type Provider struct {
	name     string
	cfg      Config
	client   *http.Client
	stateDir string

	mu    sync.RWMutex
	creds map[string]string
}

// New builds the provider from config JSON and credentials.
func New(name, configJSON, stateDir string, creds map[string]string) (*Provider, error) {
	cfg := Config{TimeoutS: defaultTimeoutS}
	if strings.TrimSpace(configJSON) != "" {
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return nil, fmt.Errorf("openai-images: bad config: %w", err)
		}
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("openai-images: base_url is required")
	}
	if cfg.TimeoutS <= 0 {
		cfg.TimeoutS = defaultTimeoutS
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.APIKey == "" && creds != nil {
		cfg.APIKey = creds["api_key"]
	}
	return &Provider{
		name:     name,
		cfg:      cfg,
		client:   httpx.ClientWithTimeout(time.Duration(cfg.TimeoutS) * time.Second),
		stateDir: stateDir,
		creds:    creds,
	}, nil
}

// Info reports capabilities.
func (p *Provider) Info() pluginapi.Info {
	return pluginapi.Info{
		Name: "openai-images", Version: "0.1.0",
		Capabilities: pluginapi.Capabilities{
			Images: true, ListModels: true, Health: true, UsageDimensions: true,
		},
	}
}

// StateDir returns the state directory.
func (p *Provider) StateDir() string { return p.stateDir }

// SetCredentials receives rotated credentials.
func (p *Provider) SetCredentials(creds map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creds = creds
	if creds != nil && creds["api_key"] != "" {
		p.cfg.APIKey = creds["api_key"]
	}
}

// ListModels returns the configured catalogue. The images API has no model listing endpoint
// worth trusting (a relay that answers /models answers with chat models too), so the
// catalogue is declared rather than discovered — and declaring it is what lets the console's
// "refresh models" turn it into provider-model rows with the right capabilities.
func (p *Provider) ListModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	out := make([]pluginapi.ModelInfo, 0, len(p.cfg.Models))
	for _, m := range p.cfg.Models {
		upstream := m.Upstream
		if upstream == "" {
			upstream = m.Public
		}
		out = append(out, pluginapi.ModelInfo{
			ID: m.Public, UpstreamModel: upstream, Capabilities: m.Capabilities,
		})
	}
	return out, nil
}

// Health probes upstream reachability through GET /models.
//
// A relay that does not implement /models reports a probe failure while the data path works
// fine; the console shows the error verbatim, which is the honest answer for a manual probe.
func (p *Provider) Health(ctx context.Context) error {
	req, err := httpx.NewRequest(ctx, http.MethodGet, p.cfg.BaseURL+"/models", nil)
	if err != nil {
		return pluginapi.NewRetryableError("bad_request", err.Error(), 0)
	}
	p.applyHeaders(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return pluginapi.NewRetryableError("upstream_unreachable", err.Error(), 0)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return httpx.ErrorFromResponse(resp)
	}
	return nil
}

// Actions returns no interactive actions.
func (p *Provider) Actions() []pluginapi.Action { return nil }

// RunAction is unsupported.
func (p *Provider) RunAction(ctx context.Context, name string, in json.RawMessage) (json.RawMessage, error) {
	return nil, pluginapi.NewError("unknown_action", "unknown action: "+name)
}

// imageOnlyError is what a chat-shaped call gets: this provider serves the Images API, and
// saying so beats an empty answer or a confusing "no such endpoint".
func imageOnlyError(model string) *pluginapi.Error {
	return pluginapi.NewError(pluginapi.CodeImageModelOnly,
		"openai-images: "+model+" serves the Images API; use POST /v1/images/generations "+
			"(or /v1/images/edits) instead of the Responses API")
}

// Complete rejects a chat-shaped request.
func (p *Provider) Complete(ctx context.Context, req *pluginapi.Request) (*pluginapi.Response, error) {
	return nil, imageOnlyError(modelOf(req))
}

// Stream rejects a chat-shaped request.
func (p *Provider) Stream(ctx context.Context, req *pluginapi.Request, emit func(pluginapi.Event) error) error {
	return imageOnlyError(modelOf(req))
}

func modelOf(req *pluginapi.Request) string {
	if req == nil || strings.TrimSpace(req.Model) == "" {
		return "this provider"
	}
	return req.Model
}

// Images performs one non-streaming image request.
func (p *Provider) Images(ctx context.Context, req *pluginapi.ImageRequest) (*pluginapi.ImageResponse, error) {
	wire, err := p.generate(ctx, req)
	if err != nil {
		return nil, err
	}
	return wire.toResponse(req.Prompt), nil
}

// usageWire mirrors the images API usage object (GPT image models only).
type usageWire struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	Details      *struct {
		TextTokens  int64 `json:"text_tokens"`
		ImageTokens int64 `json:"image_tokens"`
	} `json:"input_tokens_details,omitempty"`
}

// dimensions maps the upstream usage onto the gateway's metered dimensions.
//
// The split matters for money, not for curiosity: the upstream prices image input and image
// output at their own rates, and folding them into the text dimensions is what made
// scripts/official-pricing.sh record "the one place we under-count". A relay that reports no
// detail still gets priced through the documented fallbacks (image_input → input_cache_miss,
// image_output → output).
func (u *usageWire) dimensions() pluginapi.Usage {
	dims := map[string]int64{}
	if u == nil {
		return pluginapi.Usage{Dimensions: dims}
	}
	if u.Details != nil {
		dims["input"] = u.Details.TextTokens
		dims["image_input"] = u.Details.ImageTokens
	} else {
		dims["input"] = u.InputTokens
	}
	dims["image_output"] = u.OutputTokens
	return pluginapi.Usage{Dimensions: dims}
}

// imageWire is one entry of the response's data array.
type imageWire struct {
	URL           string `json:"url"`
	B64JSON       string `json:"b64_json"`
	RevisedPrompt string `json:"revised_prompt"`
}

// imagesResponseWire mirrors the parts of the images response the gateway needs.
type imagesResponseWire struct {
	Created      int64       `json:"created"`
	Data         []imageWire `json:"data"`
	Usage        *usageWire  `json:"usage"`
	Size         string      `json:"size"`
	Quality      string      `json:"quality"`
	Background   string      `json:"background"`
	OutputFormat string      `json:"output_format"`
	Error        *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

func (w *imagesResponseWire) toResponse(prompt string) *pluginapi.ImageResponse {
	usage := w.Usage.dimensions()
	if len(usage.Dimensions) == 0 {
		// An upstream that reports nothing (relays do this) still costs something: meter the
		// prompt as an estimate and say it is one, rather than logging a free request.
		usage = pluginapi.Usage{
			Dimensions: map[string]int64{"input": providerkit.EstimateTokens(prompt, 0)},
			Estimated:  true,
		}
	}
	out := &pluginapi.ImageResponse{
		Created: w.Created, Usage: usage,
		Size: w.Size, Quality: w.Quality, Background: w.Background, OutputFormat: w.OutputFormat,
	}
	for _, img := range w.Data {
		out.Data = append(out.Data, pluginapi.Image{
			URL: img.URL, B64JSON: img.B64JSON, RevisedPrompt: img.RevisedPrompt,
		})
	}
	return out
}

// generate performs a full (non-streaming) generation or edit and returns the decoded body.
func (p *Provider) generate(ctx context.Context, req *pluginapi.ImageRequest) (*imagesResponseWire, error) {
	httpReq, err := p.buildRequest(ctx, req, false)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, pluginapi.NewRetryableError("upstream_timeout", err.Error(), 504)
		}
		return nil, pluginapi.NewRetryableError("upstream_unreachable", err.Error(), 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, httpx.ErrorFromResponse(resp)
	}

	var wire imagesResponseWire
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&wire); err != nil {
		return nil, pluginapi.NewRetryableError("upstream_bad_response", err.Error(), 502)
	}
	if wire.Error != nil {
		return nil, pluginapi.NewError("upstream_error", wire.Error.Message)
	}
	return &wire, nil
}

// buildRequest renders the upstream request: the JSON body for a generation, multipart for
// an edit, and the SSE-accepting variant when stream is asked for.
func (p *Provider) buildRequest(ctx context.Context, req *pluginapi.ImageRequest, stream bool) (*http.Request, error) {
	if req == nil {
		return nil, pluginapi.NewError("bad_request", "openai-images: nil request")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, pluginapi.NewError("bad_request", "openai-images: prompt is required")
	}

	var (
		body        io.Reader
		contentType string
		path        string
	)
	edit := req.Op == pluginapi.ImageOpEdit
	if edit {
		if len(req.Input) == 0 {
			return nil, pluginapi.NewError("bad_request", "openai-images: an edit needs at least one reference image")
		}
		raw, ct, err := multipartBody(req)
		if err != nil {
			return nil, err
		}
		body, contentType, path = bytes.NewReader(raw), ct, "/images/edits"
	} else {
		raw, err := jsonBody(req, stream)
		if err != nil {
			return nil, err
		}
		body, contentType, path = bytes.NewReader(raw), "application/json", "/images/generations"
	}

	httpReq, err := httpx.NewRequest(ctx, http.MethodPost, p.cfg.BaseURL+path, body)
	if err != nil {
		return nil, pluginapi.NewError("bad_request", err.Error())
	}
	p.applyHeaders(httpReq)
	httpReq.Header.Set("Content-Type", contentType)
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	return httpReq, nil
}

// jsonBody renders the JSON request body. Only fields the client actually set are sent: an
// empty string is not the same request as an absent one (the upstream's own default may be
// what the client wants), and the same rule is what keeps Extra from inventing values.
func jsonBody(req *pluginapi.ImageRequest, stream bool) ([]byte, error) {
	payload := map[string]any{"model": req.Model, "prompt": req.Prompt}
	if req.N > 0 {
		payload["n"] = req.N
	}
	setIfNotEmpty(payload, "size", req.Size)
	setIfNotEmpty(payload, "quality", req.Quality)
	setIfNotEmpty(payload, "background", req.Background)
	setIfNotEmpty(payload, "output_format", req.OutputFormat)
	setIfNotEmpty(payload, "moderation", req.Moderation)
	setIfNotEmpty(payload, "input_fidelity", req.InputFidelity)
	setIfNotEmpty(payload, "response_format", req.ResponseFormat)
	setIfNotEmpty(payload, "user", req.User)
	if req.OutputCompression != nil {
		payload["output_compression"] = *req.OutputCompression
	}
	if stream {
		payload["stream"] = true
		if req.PartialImages > 0 {
			payload["partial_images"] = req.PartialImages
		}
	}
	for key, value := range req.Extra {
		if _, exists := payload[key]; exists {
			continue
		}
		payload[key] = value
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, pluginapi.NewError("encode_error", err.Error())
	}
	return out, nil
}

func setIfNotEmpty(payload map[string]any, key, value string) {
	if strings.TrimSpace(value) != "" {
		payload[key] = value
	}
}

// multipartBody renders an edit request as multipart/form-data.
//
// Reference images are written as repeated "image[]" fields, which is what the official SDKs
// send; the gateway accepts three spellings on its own edge and normalises them into
// ImageRequest.Input, so only one spelling ever leaves here.
func multipartBody(req *pluginapi.ImageRequest) ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	for _, field := range []struct{ name, value string }{
		{"model", req.Model}, {"prompt", req.Prompt}, {"size", req.Size},
		{"quality", req.Quality}, {"background", req.Background},
		{"output_format", req.OutputFormat}, {"moderation", req.Moderation},
		{"input_fidelity", req.InputFidelity}, {"response_format", req.ResponseFormat},
		{"user", req.User},
	} {
		if strings.TrimSpace(field.value) == "" {
			continue
		}
		if err := writer.WriteField(field.name, field.value); err != nil {
			return nil, "", pluginapi.NewError("encode_error", err.Error())
		}
	}
	if req.N > 0 {
		if err := writer.WriteField("n", strconv.Itoa(req.N)); err != nil {
			return nil, "", pluginapi.NewError("encode_error", err.Error())
		}
	}
	if req.OutputCompression != nil {
		if err := writer.WriteField("output_compression", strconv.Itoa(*req.OutputCompression)); err != nil {
			return nil, "", pluginapi.NewError("encode_error", err.Error())
		}
	}
	if err := writeFilePart(writer, "image[]", req.Input, "image.png"); err != nil {
		return nil, "", err
	}
	if req.Mask != nil {
		if err := writeFilePart(writer, "mask", []pluginapi.ImageInput{*req.Mask}, "mask.png"); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", pluginapi.NewError("encode_error", err.Error())
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

// writeFilePart writes images under one form field name, keeping the media type the client
// declared (an upstream that re-encodes by content type would otherwise be told
// application/octet-stream for a PNG).
func writeFilePart(writer *multipart.Writer, field string, images []pluginapi.ImageInput, fallbackName string) error {
	for _, img := range images {
		name := img.Name
		if strings.TrimSpace(name) == "" {
			name = fallbackName
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, name))
		contentType := img.MIME
		if strings.TrimSpace(contentType) == "" {
			contentType = "application/octet-stream"
		}
		header.Set("Content-Type", contentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return pluginapi.NewError("encode_error", err.Error())
		}
		if _, err := part.Write(img.Data); err != nil {
			return pluginapi.NewError("encode_error", err.Error())
		}
	}
	return nil
}

// streamFrame mirrors one SSE event of the streaming images API. Both event families
// (image_generation.* for a generation, image_edit.* for an edit) share the payload shape.
type streamFrame struct {
	Type         string     `json:"type"`
	B64JSON      string     `json:"b64_json"`
	CreatedAt    int64      `json:"created_at"`
	Size         string     `json:"size"`
	Quality      string     `json:"quality"`
	Background   string     `json:"background"`
	OutputFormat string     `json:"output_format"`
	PartialIndex int        `json:"partial_image_index"`
	Usage        *usageWire `json:"usage"`
	Error        *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

// ImagesStream performs one streaming image request.
//
// The upstream answers text/event-stream with partial images and a terminal completed event;
// this translates them into the canonical events and ends with the usage and finish events
// the host expects from any stream. A stream that ends without that terminal event is a
// failure, never a finished answer: half a picture is not a picture.
func (p *Provider) ImagesStream(ctx context.Context, req *pluginapi.ImageRequest, emit func(pluginapi.Event) error) error {
	if req == nil {
		return pluginapi.NewError("bad_request", "openai-images: nil request")
	}
	httpReq, err := p.buildRequest(ctx, req, true)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return pluginapi.NewRetryableError("upstream_timeout", err.Error(), 504)
		}
		return pluginapi.NewRetryableError("upstream_unreachable", err.Error(), 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpx.ErrorFromResponse(resp)
	}

	// A partial image is a whole base64 PNG on one SSE data line, so the reader's line
	// buffer has to be the same order as the protocol's frame ceiling.
	reader := providerkit.NewSSEReader(resp.Body, pluginapi.MaxFrameBytes)
	estimator := providerkit.NewCharEstimator(0)
	partials := 0
	completed := false
	var usage *pluginapi.Usage

	for {
		var frame streamFrame
		ev, err := reader.NextJSON(&frame)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return pluginapi.NewRetryableError("upstream_stream_error", err.Error(), 502)
		}
		if ev.Name == "done" {
			break
		}
		if frame.Error != nil {
			return pluginapi.NewRetryableError("upstream_stream_error", frame.Error.Message, 502)
		}

		switch frame.Type {
		case "image_generation.partial_image", "image_edit.partial_image":
			partials++
			estimator.Add(frame.B64JSON)
			if err := emit(pluginapi.Event{Type: pluginapi.EventImagePartial, Image: &pluginapi.ImageEvent{
				B64JSON: frame.B64JSON, PartialIndex: frame.PartialIndex, Created: frame.CreatedAt,
				Size: frame.Size, Quality: frame.Quality, Background: frame.Background,
				OutputFormat: frame.OutputFormat,
			}}); err != nil {
				return err
			}
		case "image_generation.completed", "image_edit.completed":
			completed = true
			estimator.Add(frame.B64JSON)
			u := frame.Usage.dimensions()
			usage = &u
			if err := emit(pluginapi.Event{Type: pluginapi.EventImageCompleted, Image: &pluginapi.ImageEvent{
				B64JSON: frame.B64JSON, Created: frame.CreatedAt, Size: frame.Size,
				Quality: frame.Quality, Background: frame.Background, OutputFormat: frame.OutputFormat,
			}}); err != nil {
				return err
			}
		case "error":
			message := "the upstream reported an error mid-stream"
			if frame.Error != nil && frame.Error.Message != "" {
				message = frame.Error.Message
			}
			return pluginapi.NewRetryableError("upstream_stream_error", message, 502)
		}
	}

	if !completed {
		return pluginapi.NewRetryableError("upstream_stream_incomplete",
			fmt.Sprintf("the upstream stream ended before the image was finished (%d partial frames)", partials), 502)
	}
	if usage == nil {
		// No usage in the stream: report an estimate rather than nothing, and say it is one.
		estimated := pluginapi.Usage{
			Dimensions: map[string]int64{"input": providerkit.EstimateTokens(req.Prompt, 0)},
			Estimated:  true,
		}
		usage = &estimated
	}
	if err := emit(pluginapi.Event{Type: pluginapi.EventUsage, Usage: usage}); err != nil {
		return err
	}
	return emit(pluginapi.Event{Type: pluginapi.EventFinish, Reason: pluginapi.ReasonStop})
}

func (p *Provider) applyHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	p.mu.RLock()
	apiKey := p.cfg.APIKey
	headers := p.cfg.Headers
	p.mu.RUnlock()

	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
}
