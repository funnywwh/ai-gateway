package pluginapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed is returned when the plugin process is gone.
var ErrClosed = errors.New("pluginapi: plugin connection closed")

// ErrAbandoned is returned when a call was abandoned locally (cancel/close).
var ErrAbandoned = errors.New("pluginapi: call abandoned")

// Client drives a plugin process over a pair of byte streams. It performs the
// handshake, dispatches frames by request id, applies backpressure with a bounded
// per-call buffer and surfaces credentials notifications.
type Client struct {
	enc *Encoder
	hs  Handshake

	mu    sync.Mutex
	calls map[string]*call
	next  atomic.Int64

	done   chan struct{}
	errMu  sync.Mutex
	err    error
	closed atomic.Bool

	credMu  sync.Mutex
	onCreds func(creds map[string]string, reason string)
}

type call struct {
	ch      chan *Frame
	abandon chan struct{}
}

// bufferSize bounds how many frames a plugin may run ahead of the host.
// When it fills, the plugin's emit blocks: that is the backpressure primitive
// used by the in-flight throttle policy.
const bufferSize = 8

// NewClient performs the handshake over (r, w) and returns a ready client.
func NewClient(r io.Reader, w io.Writer, handshakeTimeout time.Duration) (*Client, error) {
	c := &Client{
		enc:   NewEncoder(w),
		calls: map[string]*call{},
		done:  make(chan struct{}),
	}
	dec := NewDecoder(r)
	ready := make(chan error, 1)
	go c.readLoop(dec, ready)

	if handshakeTimeout <= 0 {
		handshakeTimeout = 3 * time.Second
	}
	select {
	case err := <-ready:
		if err != nil {
			return nil, err
		}
		return c, nil
	case <-time.After(handshakeTimeout):
		c.finish(fmt.Errorf("pluginapi: handshake timeout after %s", handshakeTimeout))
		return nil, fmt.Errorf("pluginapi: handshake timeout after %s", handshakeTimeout)
	}
}

// Handshake returns the negotiated handshake.
func (c *Client) Handshake() Handshake { return c.hs }

// Closed reports whether the connection is gone.
func (c *Client) Closed() bool { return c.closed.Load() }

// Err returns the terminal error, if any.
func (c *Client) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// OnCredentials registers the handler for provider.credentials notifications.
func (c *Client) OnCredentials(fn func(creds map[string]string, reason string)) {
	c.credMu.Lock()
	c.onCreds = fn
	c.credMu.Unlock()
}

func (c *Client) readLoop(dec *Decoder, ready chan<- error) {
	line, err := dec.ReadLine()
	if err != nil {
		ready <- fmt.Errorf("pluginapi: read handshake: %w", err)
		c.finish(err)
		return
	}
	var hs Handshake
	if err := json.Unmarshal(line, &hs); err != nil {
		err = fmt.Errorf("pluginapi: decode handshake: %w", err)
		ready <- err
		c.finish(err)
		return
	}
	if hs.Type != FrameHandshake {
		err = fmt.Errorf("pluginapi: first frame must be a handshake, got %q", hs.Type)
		ready <- err
		c.finish(err)
		return
	}
	if hs.Protocol != ProtocolVersion {
		err = fmt.Errorf("pluginapi: protocol mismatch (plugin=%d host=%d)", hs.Protocol, ProtocolVersion)
		ready <- err
		c.finish(err)
		return
	}
	c.hs = hs
	ready <- nil

	for {
		frame, err := dec.ReadFrame()
		if err != nil {
			c.finish(err)
			return
		}
		c.dispatch(frame)
	}
}

func (c *Client) dispatch(frame *Frame) {
	if frame.Type == FrameNotify && frame.Method == MethodCredentials {
		var params CredentialsParams
		if err := DecodeParams(frame.Params, &params); err == nil {
			c.credMu.Lock()
			handler := c.onCreds
			c.credMu.Unlock()
			if handler != nil {
				handler(params.Credentials, params.Reason)
			}
		}
		return
	}
	if frame.Type == FramePong {
		return
	}

	c.mu.Lock()
	cl, ok := c.calls[frame.ID]
	c.mu.Unlock()
	if !ok {
		return // late frame for an abandoned call
	}
	select {
	case cl.ch <- frame:
	case <-cl.abandon:
	case <-c.done:
	}
}

func (c *Client) finish(err error) {
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
	if c.closed.CompareAndSwap(false, true) {
		close(c.done)
	}
}

func (c *Client) newCall() (string, *call) {
	id := fmt.Sprintf("%d", c.next.Add(1))
	cl := &call{ch: make(chan *Frame, bufferSize), abandon: make(chan struct{})}
	c.mu.Lock()
	c.calls[id] = cl
	c.mu.Unlock()
	return id, cl
}

func (c *Client) dropCall(id string, cl *call) {
	c.mu.Lock()
	delete(c.calls, id)
	c.mu.Unlock()
	close(cl.abandon)
}

func (c *Client) terminalErr() error {
	if err := c.Err(); err != nil {
		if errors.Is(err, io.EOF) {
			return ErrClosed
		}
		return err
	}
	return ErrClosed
}

// ListModels asks the plugin for its upstream model list.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var out []ModelInfo
	err := c.unary(ctx, MethodListModels, nil, &out)
	return out, err
}

// Health probes upstream reachability.
func (c *Client) Health(ctx context.Context) error {
	var out map[string]any
	return c.unary(ctx, MethodHealth, nil, &out)
}

// Complete performs a non-streaming call.
func (c *Client) Complete(ctx context.Context, req *Request) (*Response, error) {
	var out Response
	if err := c.unary(ctx, MethodComplete, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RunAction executes one interactive plugin action.
func (c *Client) RunAction(ctx context.Context, name string, input json.RawMessage) (json.RawMessage, error) {
	id, cl := c.newCall()
	defer c.dropCall(id, cl)

	params, err := EncodeParams(ActionParams{Name: name, Input: input})
	if err != nil {
		return nil, err
	}
	if err := c.enc.Write(Frame{ID: id, Method: MethodAction, Params: params}); err != nil {
		return nil, err
	}
	select {
	case frame := <-cl.ch:
		if frame.Type == FrameError {
			return nil, frame.Error
		}
		return frame.Result, nil
	case <-ctx.Done():
		c.Cancel(id, "context_cancelled")
		return nil, ctx.Err()
	case <-cl.abandon:
		return nil, ErrAbandoned
	case <-c.done:
		return nil, c.terminalErr()
	}
}

// Stream performs a streaming call, invoking emit for every increment.
// It returns the terminal stream payload (finish reason / partial flag).
func (c *Client) Stream(ctx context.Context, req *Request, emit func(Event) error) (*StreamEnd, error) {
	id, cl := c.newCall()
	defer c.dropCall(id, cl)

	params, err := EncodeParams(req)
	if err != nil {
		return nil, err
	}
	if err := c.enc.Write(Frame{ID: id, Method: MethodStream, Params: params}); err != nil {
		return nil, err
	}

	for {
		select {
		case frame := <-cl.ch:
			switch frame.Type {
			case FrameEvent:
				if frame.Event != nil {
					if err := emit(*frame.Event); err != nil {
						c.Cancel(id, "emit_failed")
						return nil, err
					}
				}
			case FrameError:
				return nil, frame.Error
			case FrameEnd:
				var end StreamEnd
				if len(frame.Result) > 0 {
					if err := json.Unmarshal(frame.Result, &end); err != nil {
						return nil, fmt.Errorf("pluginapi: decode stream end: %w", err)
					}
				}
				return &end, nil
			}
		case <-ctx.Done():
			c.Cancel(id, "context_cancelled")
			return nil, ctx.Err()
		case <-cl.abandon:
			return nil, ErrAbandoned
		case <-c.done:
			return nil, c.terminalErr()
		}
	}
}

func (c *Client) unary(ctx context.Context, method string, req *Request, out any) error {
	id, cl := c.newCall()
	defer c.dropCall(id, cl)

	var params json.RawMessage
	if req != nil {
		encoded, err := EncodeParams(req)
		if err != nil {
			return err
		}
		params = encoded
	}
	if err := c.enc.Write(Frame{ID: id, Method: method, Params: params}); err != nil {
		return err
	}

	select {
	case frame := <-cl.ch:
		switch frame.Type {
		case FrameError:
			return frame.Error
		case FrameResult:
			if out == nil || len(frame.Result) == 0 {
				return nil
			}
			if err := json.Unmarshal(frame.Result, out); err != nil {
				return fmt.Errorf("pluginapi: decode result of %s: %w", method, err)
			}
			return nil
		default:
			return fmt.Errorf("pluginapi: unexpected frame type %q for %s", frame.Type, method)
		}
	case <-ctx.Done():
		c.Cancel(id, "context_cancelled")
		return ctx.Err()
	case <-cl.abandon:
		return ErrAbandoned
	case <-c.done:
		return c.terminalErr()
	}
}

// Cancel asks the plugin to abort an in-flight call.
func (c *Client) Cancel(id, reason string) {
	if c.closed.Load() {
		return
	}
	params, err := EncodeParams(CancelParams{ID: id, Reason: reason})
	if err != nil {
		return
	}
	_ = c.enc.Write(Frame{Method: MethodCancel, Params: params})
}

// Ping performs a heartbeat round trip.
func (c *Client) Ping(ctx context.Context) error {
	if c.closed.Load() {
		return c.terminalErr()
	}
	return c.enc.Write(Frame{Method: MethodPing})
}

// Shutdown asks the plugin to stop gracefully.
func (c *Client) Shutdown() error {
	if c.closed.Load() {
		return nil
	}
	return c.enc.Write(Frame{Method: MethodShutdown})
}

// Close marks the connection closed (the host is responsible for killing the process).
func (c *Client) Close() {
	c.finish(ErrClosed)
}
