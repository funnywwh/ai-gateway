package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/pkg/pluginapi"
)

// Bounds the gateway enforces before an upstream ever sees the request. They are the
// upstream's own documented limits: refusing here keeps a request that cannot succeed from
// uploading tens of megabytes first, and it is the only place a clear error can be produced
// for "17 reference images".
const (
	maxImageInputs     = 16
	maxImagesPerCall   = 10
	maxPartialImages   = 3
	imageBodyTooLargeP = "payload_too_large"
)

// imageJSONFields are the fields this package models. Everything else a client sends is
// carried in ImageRequest.Extra and forwarded verbatim: the images API keeps growing
// (moderation, output_compression, input_fidelity, whatever comes next), and a provider that
// understands a field should be able to receive it without the core knowing it.
var imageJSONFields = map[string]bool{
	"model": true, "prompt": true, "n": true, "size": true, "quality": true,
	"background": true, "output_format": true, "output_compression": true,
	"moderation": true, "response_format": true, "input_fidelity": true,
	"user": true, "stream": true, "partial_images": true,
}

// imageCall is one parsed image request plus the fact that decides the response shape.
//
// Stream is deliberately NOT a field of the canonical ImageRequest: which protocol method the
// host calls (provider.images vs provider.images.stream) already says whether the provider
// streams, so a second copy could only ever disagree with the call that carries it.
type imageCall struct {
	req    *pluginapi.ImageRequest
	stream bool
}

// parseImageGenerate decodes a JSON body of POST /v1/images/generations.
func parseImageGenerate(body []byte) (*imageCall, *domain.APIError) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, domain.ErrInvalidRequest("the request body must be a JSON object: " + err.Error())
	}
	req := &pluginapi.ImageRequest{Op: pluginapi.ImageOpGenerate}
	stream, apiErr := fillImageRequest(req, fields)
	if apiErr != nil {
		return nil, apiErr
	}
	return &imageCall{req: req, stream: stream}, nil
}

// parseImageEdit decodes a multipart/form-data body of POST /v1/images/edits.
//
// The upstream accepts up to 16 reference images under the field name `image`. Three
// spellings of that name reach this handler in practice — the official Node SDK sends
// `image[]`, the Python SDK and hand-written curl send `image` repeatedly, and some clients
// index them as `image[0]` — so all three are accepted and normalised into one list. Parts
// are read into memory: the body was already bounded by server.images_max_body_bytes.
func parseImageEdit(r *http.Request, body []byte) (*imageCall, *domain.APIError) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, domain.ErrInvalidRequest("an image edit must be sent as multipart/form-data with a boundary")
	}

	req := &pluginapi.ImageRequest{Op: pluginapi.ImageOpEdit}
	fields := map[string]json.RawMessage{}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err != nil {
			break // io.EOF ends the loop; a malformed tail is caught by the checks below
		}
		value, readErr := readPart(part, maxImagePartBytes)
		if readErr != nil {
			return nil, readErr
		}
		name := part.FormName()
		if part.FileName() == "" {
			// A text field: keep it in the same raw-JSON form the JSON path produces, so
			// both request shapes feed one validation path.
			fields[name] = mustJSON(string(value))
			continue
		}
		image := pluginapi.ImageInput{
			Name: part.FileName(), MIME: part.Header.Get("Content-Type"), Data: value,
		}
		switch {
		case isImageField(name):
			if len(req.Input) >= maxImageInputs {
				return nil, domain.ErrInvalidRequest(
					fmt.Sprintf("an edit accepts at most %d reference images", maxImageInputs))
			}
			req.Input = append(req.Input, image)
		case name == "mask":
			req.Mask = &image
		default:
			return nil, domain.ErrInvalidRequest("unexpected file field " + name + " (expected image, image[] or mask)")
		}
	}
	stream, apiErr := fillImageRequest(req, fields)
	if apiErr != nil {
		return nil, apiErr
	}
	if len(req.Input) == 0 {
		return nil, domain.ErrInvalidRequest("an edit needs at least one reference image in the `image` field")
	}
	return &imageCall{req: req, stream: stream}, nil
}

// maxImagePartBytes bounds one reference image. It is the upstream's documented per-image
// ceiling, and it is also what keeps a single part from outgrowing the body limit.
const maxImagePartBytes = 50 << 20

// readPart reads one form part, refusing a part that is larger than the per-image ceiling.
func readPart(part *multipart.Part, limit int64) ([]byte, *domain.APIError) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(part); err != nil {
		return nil, domain.ErrInvalidRequest("reading a form part failed: " + err.Error())
	}
	if int64(buf.Len()) > limit {
		return nil, domain.ErrInvalidRequest(fmt.Sprintf("one form part exceeds %d bytes", limit))
	}
	return buf.Bytes(), nil
}

// isImageField accepts the three spellings of the reference-image field.
func isImageField(name string) bool {
	if name == "image" {
		return true
	}
	trimmed := strings.TrimSuffix(name, "[]")
	if trimmed != name {
		return true
	}
	if strings.HasPrefix(name, "image[") && strings.HasSuffix(name, "]") {
		_, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "image["), "]"))
		return err == nil
	}
	return false
}

// fillImageRequest decodes the shared fields of both request shapes, validates them and reports
// whether the client asked for a streaming answer.
//
// Validation stops at the bounds the gateway itself must honour (a missing model or prompt,
// a count outside the upstream's range): a value this gateway has an opinion about but the
// upstream does too — size, quality, output_format — is passed through, because rejecting a
// value some upstream accepts would turn a working request into a support ticket.
func fillImageRequest(req *pluginapi.ImageRequest, fields map[string]json.RawMessage) (bool, *domain.APIError) {
	if err := decodeImageString(fields, "model", &req.Model); err != nil {
		return false, err
	}
	if err := decodeImageString(fields, "prompt", &req.Prompt); err != nil {
		return false, err
	}
	for name, target := range map[string]*string{
		"size": &req.Size, "quality": &req.Quality, "background": &req.Background,
		"output_format": &req.OutputFormat, "moderation": &req.Moderation,
		"input_fidelity": &req.InputFidelity, "response_format": &req.ResponseFormat,
		"user": &req.User,
	} {
		if err := decodeImageString(fields, name, target); err != nil {
			return false, err
		}
	}

	req.N = 1
	if raw, ok := fields["n"]; ok && string(raw) != "null" {
		count, err := decodeImageInt(raw, "n")
		if err != nil {
			return false, err
		}
		req.N = count
	}
	if req.N < 1 || req.N > maxImagesPerCall {
		return false, domain.ErrInvalidRequest(fmt.Sprintf("n must be between 1 and %d", maxImagesPerCall)).WithParam("n")
	}

	if raw, ok := fields["partial_images"]; ok && string(raw) != "null" {
		partial, err := decodeImageInt(raw, "partial_images")
		if err != nil {
			return false, err
		}
		req.PartialImages = partial
	}
	if req.PartialImages < 0 || req.PartialImages > maxPartialImages {
		return false, domain.ErrInvalidRequest(
			fmt.Sprintf("partial_images must be between 0 and %d", maxPartialImages)).WithParam("partial_images")
	}

	stream := false
	if raw, ok := fields["stream"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &stream); err != nil {
			return false, domain.ErrInvalidRequest("stream must be a boolean").WithParam("stream")
		}
	}

	if raw, ok := fields["output_compression"]; ok && string(raw) != "null" {
		compression, err := decodeImageInt(raw, "output_compression")
		if err != nil {
			return false, err
		}
		req.OutputCompression = &compression
	}

	if strings.TrimSpace(req.Model) == "" {
		return false, domain.ErrInvalidRequest("model is required").WithParam("model")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return false, domain.ErrInvalidRequest("prompt is required").WithParam("prompt")
	}

	for name, raw := range fields {
		if imageJSONFields[name] {
			continue
		}
		if req.Extra == nil {
			req.Extra = map[string]json.RawMessage{}
		}
		req.Extra[name] = append(json.RawMessage(nil), raw...)
	}
	return stream, nil
}

// decodeImageString reads one optional string field. A JSON null is "not set" rather than an
// error: clients send nulls for optional parameters routinely.
func decodeImageString(fields map[string]json.RawMessage, name string, target *string) *domain.APIError {
	raw, ok := fields[name]
	if !ok || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return domain.ErrInvalidRequest(name + " must be a string").WithParam(name)
	}
	return nil
}

// decodeImageInt reads one integer field. Multipart text parts arrive as JSON strings, which
// is why the string form is accepted here as well as the number form.
func decodeImageInt(raw json.RawMessage, name string) (int, *domain.APIError) {
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		parsed, convErr := strconv.Atoi(strings.TrimSpace(text))
		if convErr != nil {
			return 0, domain.ErrInvalidRequest(name + " must be an integer").WithParam(name)
		}
		return parsed, nil
	}
	return 0, domain.ErrInvalidRequest(name + " must be an integer").WithParam(name)
}
