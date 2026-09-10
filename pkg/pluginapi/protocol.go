package pluginapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Frame types.
const (
	FrameHandshake = "handshake"
	FrameResult    = "result"
	FrameEvent     = "event"
	FrameEnd       = "end"
	FrameError     = "error"
	FramePong      = "pong"
	FrameNotify    = "notify"
)

// Methods understood by Serve / Client.
const (
	MethodInfo        = "provider.info"
	MethodListModels  = "provider.list_models"
	MethodComplete    = "provider.complete"
	MethodStream      = "provider.stream"
	MethodHealth      = "provider.health"
	MethodAction      = "provider.action"
	MethodCancel      = "provider.cancel"
	MethodPing        = "ping"
	MethodShutdown    = "shutdown"
	MethodCredentials = "provider.credentials"
)

// MaxFrameBytes bounds a single NDJSON frame.
const MaxFrameBytes = 8 << 20

// newlineByte terminates every NDJSON frame. It is written as a byte slice
// because this repository generates Go source without backslash escapes.
var newlineByte = []byte{0x0A}

// Frame is the envelope for every wire message.
type Frame struct {
	ID     string          `json:"id,omitempty"`
	Type   string          `json:"type,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
	Event  *Event          `json:"event,omitempty"`
}

// Handshake is the first frame a plugin writes to stdout.
type Handshake struct {
	Type              string          `json:"type"`
	Protocol          int             `json:"protocol"`
	Name              string          `json:"name"`
	Version           string          `json:"version"`
	Capabilities      Capabilities    `json:"capabilities"`
	ConfigSchema      json.RawMessage `json:"config_schema,omitempty"`
	CredentialsSchema json.RawMessage `json:"credentials_schema,omitempty"`
}

// StreamEnd terminates a stream frame sequence.
type StreamEnd struct {
	Usage        Usage  `json:"usage"`
	FinishReason string `json:"finish_reason,omitempty"`
	Partial      bool   `json:"partial,omitempty"`
}

// CancelParams is the payload of provider.cancel.
type CancelParams struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// CredentialsParams is the payload of the provider.credentials notification.
type CredentialsParams struct {
	Credentials map[string]string `json:"credentials"`
	Reason      string            `json:"reason,omitempty"`
}

// ActionParams is the payload of provider.action.
type ActionParams struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ActionStatus is the result of provider.action.
type ActionStatus struct {
	Status  string `json:"status"` // ok|pending_user|error
	Message string `json:"message,omitempty"`
	URL     string `json:"url,omitempty"`
	Code    string `json:"code,omitempty"`
	PollMS  int    `json:"poll_ms,omitempty"`
	State   string `json:"state,omitempty"`
}

// Encoder writes NDJSON frames. It is safe for concurrent use.
type Encoder struct {
	mu sync.Mutex
	w  *bufio.Writer
}

// NewEncoder wraps w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: bufio.NewWriterSize(w, 32*1024)}
}

// Write encodes v as one line and flushes it.
func (e *Encoder) Write(v any) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("pluginapi: encode frame: %w", err)
	}
	if len(data) > MaxFrameBytes {
		return fmt.Errorf("pluginapi: frame too large (%d bytes)", len(data))
	}
	if _, err := e.w.Write(data); err != nil {
		return err
	}
	if _, err := e.w.Write(newlineByte); err != nil {
		return err
	}
	return e.w.Flush()
}

// Decoder reads NDJSON frames.
type Decoder struct {
	sc *bufio.Scanner
}

// NewDecoder wraps r with a bounded line scanner.
func NewDecoder(r io.Reader) *Decoder {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), MaxFrameBytes)
	return &Decoder{sc: sc}
}

// ReadFrame returns the next frame, or io.EOF when the stream ends.
func (d *Decoder) ReadFrame() (*Frame, error) {
	line, err := d.ReadLine()
	if err != nil {
		return nil, err
	}
	var fr Frame
	if err := json.Unmarshal(line, &fr); err != nil {
		return nil, fmt.Errorf("pluginapi: decode frame: %w", err)
	}
	return &fr, nil
}

// ReadLine returns the next non-empty raw line (used for the handshake frame,
// whose fields live at the top level rather than inside Frame).
func (d *Decoder) ReadLine() ([]byte, error) {
	for d.sc.Scan() {
		line := d.sc.Bytes()
		if len(line) == 0 {
			continue
		}
		out := make([]byte, len(line))
		copy(out, line)
		return out, nil
	}
	if err := d.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// EncodeParams marshals params into a frame payload.
func EncodeParams(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("pluginapi: encode params: %w", err)
	}
	return data, nil
}

// DecodeParams unmarshals frame params into v.
func DecodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}
