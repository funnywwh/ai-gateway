package pluginapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
)

// Environment variables the host sets on a plugin process.
const (
	EnvProtocol    = "GW_PLUGIN_PROTOCOL"
	EnvInstance    = "GW_PLUGIN_INSTANCE"
	EnvConfig      = "GW_PLUGIN_CONFIG"
	EnvCredentials = "GW_PLUGIN_CREDENTIALS"
	EnvStateDir    = "GW_PLUGIN_STATE_DIR"
)

// CredentialsFileName is where the host hands credentials to a plugin
// (inside GW_PLUGIN_STATE_DIR, mode 0600). Preferred over the environment
// because environment variables are readable by same-UID processes and are size-bounded.
const CredentialsFileName = "credentials.json"

// Serve runs the plugin protocol loop until stdin closes or a shutdown frame arrives.
//
// Typical plugin main:
//
//	func main() {
//	    if err := pluginapi.Serve(&MyProvider{}); err != nil {
//	        fmt.Fprintln(os.Stderr, "plugin:", err)
//	        os.Exit(1)
//	    }
//	}
func Serve(p Provider) error {
	if p == nil {
		return fmt.Errorf("pluginapi: nil provider")
	}
	info := p.Info()
	if info.Name == "" {
		return fmt.Errorf("pluginapi: Info().Name must not be empty")
	}

	if creds := loadCredentials(p.StateDir()); len(creds) > 0 {
		p.SetCredentials(creds)
	}

	enc := NewEncoder(os.Stdout)
	hs := Handshake{
		Type:         FrameHandshake,
		Protocol:     ProtocolVersion,
		Name:         info.Name,
		Version:      info.Version,
		Capabilities: info.Capabilities,
	}
	if sp, ok := p.(SchemaProvider); ok {
		hs.ConfigSchema = sp.ConfigSchema()
		hs.CredentialsSchema = sp.CredentialsSchema()
	}
	if err := enc.Write(hs); err != nil {
		return fmt.Errorf("pluginapi: write handshake: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()

	srv := &server{provider: p, enc: enc, active: map[string]context.CancelFunc{}}
	dec := NewDecoder(os.Stdin)
	for {
		frame, err := dec.ReadFrame()
		if err != nil {
			break // EOF or protocol error: stop serving
		}
		if srv.handle(ctx, frame) {
			break
		}
	}
	srv.wg.Wait()
	return nil
}

// loadCredentials reads credentials.json from the plugin state dir (if present).
func loadCredentials(stateDir string) map[string]string {
	if stateDir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(stateDir, CredentialsFileName))
	if err != nil {
		return nil
	}
	var creds map[string]string
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil
	}
	return creds
}

type server struct {
	provider Provider
	enc      *Encoder

	mu     sync.Mutex
	active map[string]context.CancelFunc
	wg     sync.WaitGroup
}

// handle processes one inbound frame; it returns true when the plugin must stop.
func (s *server) handle(ctx context.Context, frame *Frame) bool {
	switch frame.Method {
	case MethodPing:
		_ = s.enc.Write(Frame{Type: FramePong})
		return false
	case MethodShutdown:
		s.cancelAll()
		return true
	case MethodCancel:
		var params CancelParams
		if err := DecodeParams(frame.Params, &params); err == nil {
			s.cancel(params.ID)
		}
		return false
	case MethodInfo:
		return s.dispatch(frame, func(context.Context) (any, error) {
			return s.provider.Info(), nil
		})
	case MethodListModels:
		return s.dispatch(frame, func(reqCtx context.Context) (any, error) {
			return s.provider.ListModels(reqCtx)
		})
	case MethodHealth:
		return s.dispatch(frame, func(reqCtx context.Context) (any, error) {
			if err := s.provider.Health(reqCtx); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		})
	case MethodAction:
		var params ActionParams
		if err := DecodeParams(frame.Params, &params); err != nil {
			_ = s.writeError(frame.ID, NewError("bad_params", err.Error()))
			return false
		}
		return s.dispatch(frame, func(reqCtx context.Context) (any, error) {
			raw, err := s.provider.RunAction(reqCtx, params.Name, params.Input)
			if err != nil {
				return nil, err
			}
			if len(raw) == 0 {
				return ActionStatus{Status: "ok"}, nil
			}
			return json.RawMessage(raw), nil
		})
	case MethodComplete:
		var req Request
		if err := DecodeParams(frame.Params, &req); err != nil {
			_ = s.writeError(frame.ID, NewError("bad_params", err.Error()))
			return false
		}
		return s.dispatch(frame, func(reqCtx context.Context) (any, error) {
			return s.provider.Complete(reqCtx, &req)
		})
	case MethodStream:
		var req Request
		if err := DecodeParams(frame.Params, &req); err != nil {
			_ = s.writeError(frame.ID, NewError("bad_params", err.Error()))
			return false
		}
		return s.dispatchStream(frame.ID, &req)
	default:
		if frame.ID != "" {
			_ = s.writeError(frame.ID, NewError("unsupported_method", "unknown method: "+frame.Method))
		}
		return false
	}
}

func (s *server) dispatch(frame *Frame, fn func(context.Context) (any, error)) bool {
	id := frame.ID
	reqCtx, cancel := s.register(id)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.unregister(id)
		defer cancel()
		if id == "" {
			return
		}
		result, err := s.callSafely(reqCtx, fn)
		if err != nil {
			_ = s.writeError(id, toProtocolError(err))
			return
		}
		raw, err := EncodeParams(result)
		if err != nil {
			_ = s.writeError(id, NewError("encode_error", err.Error()))
			return
		}
		_ = s.enc.Write(Frame{ID: id, Type: FrameResult, Result: raw})
	}()
	return false
}

func (s *server) dispatchStream(id string, req *Request) bool {
	reqCtx, cancel := s.register(id)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.unregister(id)
		defer cancel()
		if id == "" {
			return
		}
		// The plugin states why generation stopped with a finish event; the SDK turns
		// it into the end frame so the host can tell a complete answer from a
		// truncated one. A plugin that emits no finish event keeps the legacy
		// reading ("stop").
		finishReason := "stop"
		emit := func(ev Event) error {
			if ev.Type == EventFinish {
				if ev.Reason != "" {
					finishReason = ev.Reason
				}
				return nil // terminal marker: reported in the end frame, not as an event
			}
			return s.enc.Write(Frame{ID: id, Type: FrameEvent, Event: &ev})
		}
		err := s.streamSafely(reqCtx, req, emit)
		if err != nil {
			protoErr := toProtocolError(err)
			if reqCtx.Err() != nil {
				// Cancelled by the host: close the stream as partial rather than as an error.
				end := StreamEnd{Partial: true, FinishReason: "cancelled"}
				_ = s.writeEnd(id, end)
				return
			}
			_ = s.writeError(id, protoErr)
			return
		}
		_ = s.writeEnd(id, StreamEnd{FinishReason: finishReason})
	}()
	return false
}

func (s *server) callSafely(ctx context.Context, fn func(context.Context) (any, error)) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("plugin panic: %v", r)
		}
	}()
	return fn(ctx)
}

func (s *server) streamSafely(ctx context.Context, req *Request, emit func(Event) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("plugin panic: %v", r)
		}
	}()
	return s.provider.Stream(ctx, req, emit)
}

func (s *server) writeEnd(id string, end StreamEnd) error {
	raw, err := EncodeParams(end)
	if err != nil {
		return err
	}
	return s.enc.Write(Frame{ID: id, Type: FrameEnd, Result: raw})
}

func (s *server) writeError(id string, e *Error) error {
	return s.enc.Write(Frame{ID: id, Type: FrameError, Error: e})
}

func (s *server) register(id string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	if id == "" {
		return ctx, cancel
	}
	s.mu.Lock()
	s.active[id] = cancel
	s.mu.Unlock()
	return ctx, cancel
}

func (s *server) unregister(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	delete(s.active, id)
	s.mu.Unlock()
}

func (s *server) cancel(id string) {
	s.mu.Lock()
	cancel, ok := s.active[id]
	s.mu.Unlock()
	if ok {
		cancel()
	}
}

func (s *server) cancelAll() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for _, c := range s.active {
		cancels = append(cancels, c)
	}
	s.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// toProtocolError maps a Go error onto the wire error payload.
func toProtocolError(err error) *Error {
	if e, ok := IsError(err); ok {
		return e
	}
	if err == nil {
		return NewError("unknown", "unknown error")
	}
	return NewError("plugin_error", err.Error())
}
