package pluginapi

import (
	"context"
	"encoding/json"
)

// Image operations. The gateway picks one from the endpoint the client called
// (POST /v1/images/generations or POST /v1/images/edits) and it is what a provider
// branches on: the same two upstream paths, two different event families.
const (
	ImageOpGenerate = "generate"
	ImageOpEdit     = "edit"
)

// ImageRequest is the canonical image-generation request.
//
// It is a separate shape from Request rather than a variant of it on purpose: a
// canonical chat request is built around instructions/input/tools, and an image request
// shares almost none of it. Folding the two together would make every provider branch on
// "which kind of request is this" in every method it implements.
//
// The gateway normalises the fields a provider may rely on: Op is always set, N is at
// least 1, PartialImages is within 0..3, and an edit always carries at least one Input.
// Parameters this package does not model (the upstream images API keeps growing: moderation,
// output_compression, input_fidelity, …) travel in Extra verbatim — a provider that
// understands them merges them into the upstream body instead of the core deciding what to
// discard.
type ImageRequest struct {
	Model string `json:"model"`
	// Op is ImageOpGenerate or ImageOpEdit.
	Op     string `json:"op"`
	Prompt string `json:"prompt"`
	// N is the number of images to produce (>= 1).
	N int `json:"n,omitempty"`
	// Size, Quality, Background, OutputFormat, Moderation and InputFidelity are the
	// upstream's own spellings, forwarded verbatim: validating them here would only be able
	// to disagree with the upstream that actually enforces them.
	Size           string `json:"size,omitempty"`
	Quality        string `json:"quality,omitempty"`
	Background     string `json:"background,omitempty"`
	OutputFormat   string `json:"output_format,omitempty"`
	Moderation     string `json:"moderation,omitempty"`
	InputFidelity  string `json:"input_fidelity,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	User           string `json:"user,omitempty"`
	// OutputCompression is a pointer because 0 is a legal value (no compression).
	OutputCompression *int `json:"output_compression,omitempty"`
	PartialImages     int  `json:"partial_images,omitempty"`
	// Input carries the reference images of an edit (0..16). Empty for a generation.
	Input []ImageInput `json:"input,omitempty"`
	// Mask is an optional edit mask. A mask without Input is meaningless and the gateway
	// never produces one.
	Mask *ImageInput `json:"mask,omitempty"`
	// Extra carries client fields this package does not model, and it travels on the wire as
	// a real field (unlike Request.Extra, which is host-side only): a plugin provider has no
	// other way to see them. A JSON-speaking provider merges them into the upstream body; a
	// multipart one has nowhere to put them and drops them (documented in docs/api-images.md
	// rather than silently lost).
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// ImageInput is one image on its way to the upstream: a reference image or a mask.
// Data is base64 on the wire (encoding/json's []byte encoding), which is what keeps a
// binary payload inside the NDJSON frame protocol.
type ImageInput struct {
	Name string `json:"name,omitempty"`
	// MIME is the media type as the client declared it ("image/png", …). Empty when the
	// client did not say, in which case a provider must not assume one.
	MIME string `json:"mime,omitempty"`
	Data []byte `json:"data"`
}

// Image is one produced image.
//
// Which of URL / B64JSON is set is the upstream's choice (GPT image models always answer
// base64, dall-e-style relays may answer url). The gateway forwards whichever arrived;
// it never fetches a URL to re-encode it.
type Image struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// ImageResponse is the canonical non-streaming image response.
//
// The echo fields (Size/Quality/Background/OutputFormat) are what the client-visible
// response repeats back: the upstream's answer to "what did you actually produce", which
// is not always what was asked for (the model may pick a size, a relay may re-encode).
// Empty when the upstream did not say.
type ImageResponse struct {
	Created      int64   `json:"created"`
	Data         []Image `json:"data"`
	Usage        Usage   `json:"usage"`
	Size         string  `json:"size,omitempty"`
	Quality      string  `json:"quality,omitempty"`
	Background   string  `json:"background,omitempty"`
	OutputFormat string  `json:"output_format,omitempty"`
}

// ImageEvent is the payload of an image streaming event.
//
// Partial frames carry B64JSON + PartialIndex; the terminal frame carries the final
// B64JSON. Usage does NOT travel here: it is reported through the stream's usage event,
// exactly like a chat stream, so there is one place where a stream's metered quantity
// comes from.
type ImageEvent struct {
	B64JSON string `json:"b64_json,omitempty"`
	// PartialIndex is the 0-based index of a partial image.
	PartialIndex int    `json:"partial_image_index,omitempty"`
	Created      int64  `json:"created,omitempty"`
	Size         string `json:"size,omitempty"`
	Quality      string `json:"quality,omitempty"`
	Background   string `json:"background,omitempty"`
	OutputFormat string `json:"output_format,omitempty"`
}

// ImageEvent types.
const (
	// EventImagePartial is one intermediate image of a streaming generation. Emitting them
	// is optional (partial_images may be 0): a stream may go straight to
	// EventImageCompleted.
	EventImagePartial = "image.partial"
	// EventImageCompleted is the finished image of a streaming generation. It is the
	// payload the client sees as image_generation.completed / image_edit.completed.
	EventImageCompleted = "image.completed"
)

// ImageProvider is optionally implemented by plugins that serve image models.
//
// It is a separate interface rather than two more methods on Provider because Provider is
// what every existing plugin implements: adding methods to it would break every one of them
// at compile time. A plugin declares `images: true` in its handshake, implements this
// interface, and the host routes image requests to it. The host type-asserts, so a plugin
// that declares the capability without implementing the interface fails loudly at request
// time instead of crashing the process.
type ImageProvider interface {
	// Images performs one non-streaming image request.
	Images(ctx context.Context, req *ImageRequest) (*ImageResponse, error)
	// ImagesStream performs one streaming image request; emit blocks under backpressure.
	// A plugin whose upstream cannot stream should answer with a non-retryable error and
	// code "images_stream_unsupported", which the gateway reports as a client-visible
	// "retry without stream" rather than as an upstream failure.
	ImagesStream(ctx context.Context, req *ImageRequest, emit func(Event) error) error
}
