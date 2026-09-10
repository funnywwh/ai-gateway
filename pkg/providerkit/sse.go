package providerkit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// SSEEvent is one decoded server-sent event.
type SSEEvent struct {
	Name string
	Data []byte
}

// SSEReader decodes text/event-stream payloads line by line.
type SSEReader struct {
	sc      *bufio.Scanner
	pending *SSEEvent
}

// NewSSEReader wraps r (maxLine <= 0 means 1 MiB).
func NewSSEReader(r io.Reader, maxLine int) *SSEReader {
	if maxLine <= 0 {
		maxLine = 1 << 20
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	return &SSEReader{sc: sc}
}

// Next returns the next event, or io.EOF when the stream ends.
// The literal payload "[DONE]" is returned as an event with Data == "DONE"
// and Name == "done" so callers can terminate cleanly.
func (r *SSEReader) Next() (*SSEEvent, error) {
	if r.pending != nil {
		ev := r.pending
		r.pending = nil
		return ev, nil
	}

	var (
		name string
		data bytes.Buffer
		seen bool
	)
	for r.sc.Scan() {
		line := strings.TrimRight(r.sc.Text(), crString)
		if line == "" {
			if !seen {
				continue // keep-alive or stray blank line
			}
			payload := trimTrailingLF(data.Bytes())
			if string(payload) == doneSentinel {
				return &SSEEvent{Name: "done", Data: []byte("DONE")}, nil
			}
			return &SSEEvent{Name: name, Data: payload}, nil
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / heartbeat
		}
		seen = true
		field, value := splitSSEField(line)
		switch field {
		case "event":
			name = value
		case "data":
			data.WriteString(value)
			data.WriteByte(byteLF)
		default:
			// "id" and "retry" are not used by the gateway.
		}
	}
	if err := r.sc.Err(); err != nil {
		return nil, err
	}
	if seen {
		payload := trimTrailingLF(data.Bytes())
		if string(payload) == doneSentinel {
			return &SSEEvent{Name: "done", Data: []byte("DONE")}, nil
		}
		return &SSEEvent{Name: name, Data: payload}, nil
	}
	return nil, io.EOF
}

// NextJSON decodes the next event payload into v (skipping the [DONE] sentinel).
func (r *SSEReader) NextJSON(v any) (*SSEEvent, error) {
	for {
		ev, err := r.Next()
		if err != nil {
			return nil, err
		}
		if ev.Name == "done" {
			return ev, nil
		}
		if len(ev.Data) == 0 {
			continue
		}
		if err := json.Unmarshal(ev.Data, v); err != nil {
			return nil, fmt.Errorf("providerkit: decode SSE data: %w", err)
		}
		return ev, nil
	}
}

const (
	byteLF         = 0x0A
	carriageReturn = 0x0D
)

// crString is the carriage-return character as a string (escape-free construction).
var crString = string([]byte{carriageReturn})

// doneSentinel is the OpenAI-style stream terminator.
const doneSentinel = "[DONE]"

func splitSSEField(line string) (string, string) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return line, ""
	}
	field := line[:idx]
	value := line[idx+1:]
	value = strings.TrimPrefix(value, " ")
	return field, value
}

// trimTrailingLF strips trailing newline bytes without using escape sequences.
func trimTrailingLF(b []byte) []byte {
	end := len(b)
	for end > 0 && (b[end-1] == byteLF || b[end-1] == carriageReturn) {
		end--
	}
	out := make([]byte, end)
	copy(out, b[:end])
	return out
}
