// Package browserworkspace mounts an explicitly authorized browser directory using FUSE.
package browserworkspace

import "context"

type Operation string

const (
	OpRead     Operation = "read"
	OpWrite    Operation = "write"
	OpList     Operation = "list"
	OpStat     Operation = "stat"
	OpCreate   Operation = "create"
	OpMkdir    Operation = "mkdir"
	OpUnlink   Operation = "unlink"
	OpRmdir    Operation = "rmdir"
	OpTruncate Operation = "truncate"
	OpRename   Operation = "rename"
	OpFlush    Operation = "flush"
)

// Request paths are canonical relative slash-separated paths; empty means root.
// Exclusive and Truncate apply only to create. FSA cannot guarantee exclusive
// creation against other browser tabs or native programs (see README.md).
type Request struct {
	ID        string    `json:"id"`
	Tenant    string    `json:"tenant,omitempty"`
	Op        Operation `json:"op"`
	Path      string    `json:"path"`
	Offset    int64     `json:"offset,omitempty"`
	Size      int64     `json:"size,omitempty"`
	Data      []byte    `json:"data,omitempty"`
	Target    string    `json:"target,omitempty"`
	Exclusive bool      `json:"exclusive,omitempty"`
	Truncate  bool      `json:"truncate,omitempty"`
}
// Entry is one directory entry of a `list` answer. Size and LastModified are part of the
// client's answer — its executor reports every entry's metadata — and MUST stay declared:
// the respond payload is decoded with DisallowUnknownFields, so an undeclared field makes
// the whole listing answer fail to decode. The browser's reply is then dropped, the
// pending readdir waits out the FUSE timeout, and every `ls` of a mounted directory fails
// with ETIMEDOUT. The FUSE adapter itself needs only Name and Kind.
type Entry struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Size         int64  `json:"size,omitempty"`
	LastModified int64  `json:"lastModified,omitempty"`
}
type Value struct {
	Kind         string  `json:"kind,omitempty"`
	Size         int64   `json:"size,omitempty"`
	LastModified int64   `json:"lastModified,omitempty"`
	Entries      []Entry `json:"entries,omitempty"`
	Data         []byte  `json:"data,omitempty"`
	Bytes        uint32  `json:"bytes,omitempty"`
}
type RemoteError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RemoteError) Error() string { return e.Code + ": " + e.Message }

type Response struct {
	ID    string       `json:"id,omitempty"`
	OK    bool         `json:"ok"`
	Value Value        `json:"value"`
	Error *RemoteError `json:"error,omitempty"`
}

// Backend must honor context cancellation/deadlines. Timeout does not prove a
// mutating browser operation was rolled back: its completion can be ambiguous.
type Backend interface {
	Call(context.Context, Request) (Response, error)
}
type BackendFunc func(context.Context, Request) (Response, error)

func (f BackendFunc) Call(c context.Context, r Request) (Response, error) { return f(c, r) }
