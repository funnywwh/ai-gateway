package sshworkspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

// The mailbox is the whole control channel between a tenant's dsh and the gateway: a
// request directory and a reply directory inside the account's own DSH home.
//
// Why a file and not an endpoint: the tenant worker shares the host's network namespace, so
// an HTTP channel to the gateway was possible — but it would have needed a listening surface
// and a per-tenant credential, while the filesystem already gives exactly the right
// authority for free. Each account can write only its own DSH home (it is the only tree its
// sandbox binds), so "ask for a mount" cannot be turned into "ask for someone else's mount",
// and there is nothing to authenticate.
const (
	// RequestOpen asks the gateway to mount a remote directory for this account.
	RequestOpen = "open"
	// RequestClose asks the gateway to detach one of this account's mounts.
	RequestClose = "close"
)

// requestIDRE bounds a request id to something that is safe as a file name everywhere.
var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Request is one operation a tenant asks the gateway to perform.
type Request struct {
	ID         string    `json:"id"`
	Op         string    `json:"op"`
	Host       string    `json:"host,omitempty"`
	Remote     string    `json:"remote,omitempty"`
	Mountpoint string    `json:"mountpoint,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Reply is what the gateway leaves behind for the tenant's UI to read.
type Reply struct {
	ID         string `json:"id"`
	OK         bool   `json:"ok"`
	Code       string `json:"code,omitempty"`
	Error      string `json:"error,omitempty"`
	Mountpoint string `json:"mountpoint,omitempty"`
	Remote     string `json:"remote,omitempty"`
	Host       string `json:"host,omitempty"`
	// Restarted reports that the account's worker was restarted to make the change visible:
	// the profile binds mount points at worker start, so a mount set cannot change under a
	// running sandbox.
	Restarted bool      `json:"restarted,omitempty"`
	Lazy      bool      `json:"lazy,omitempty"`
	At        time.Time `json:"at"`
}

// RequestDir is where a tenant leaves requests.
func RequestDir(dshHome string) string { return filepath.Join(dshHome, "ssh-requests") }

// ReplyDir is where the gateway leaves replies.
func ReplyDir(dshHome string) string { return filepath.Join(dshHome, "ssh-replies") }

// ValidateRequest rejects a request that must not be acted on: an unknown operation, an
// unusable id (it becomes a file name), or a missing operand for the operation.
func ValidateRequest(req Request) error {
	if !requestIDRE.MatchString(req.ID) {
		return Errorf(CodeInvalidPath, "request id %q is not usable", req.ID)
	}
	switch req.Op {
	case RequestOpen:
		if err := ValidateHostSpec(req.Host); err != nil {
			return err
		}
		return ValidateRemotePath(req.Remote)
	case RequestClose:
		if !filepath.IsAbs(req.Mountpoint) {
			return Errorf(CodeInvalidPath, "mount point %q is not absolute", req.Mountpoint)
		}
		return nil
	default:
		return Errorf(CodeInvalidPath, "unsupported operation %q", req.Op)
	}
}

// WriteRequest stores one request atomically at 0600.
func WriteRequest(dshHome string, req Request) error {
	if err := ValidateRequest(req); err != nil {
		return err
	}
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now().UTC()
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	dir := RequestDir(dshHome)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return securefile.WriteAtomic(filepath.Join(dir, req.ID+".json"), append(data, '\n'), 0o600)
}

// ReadRequests lists a tenant's pending requests in creation order. A single unreadable or
// malformed file is skipped with its error reported through the returned slice of problems,
// so one bad file can never wedge the whole mailbox.
func ReadRequests(dshHome string) (requests []Request, problems []error) {
	entries, err := os.ReadDir(RequestDir(dshHome))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{err}
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(RequestDir(dshHome), entry.Name())
		data, err := securefile.ReadLimitedRegular(path, 64<<10)
		if err != nil {
			problems = append(problems, fmt.Errorf("reading %s: %w", path, err))
			continue
		}
		var req Request
		if err := json.Unmarshal(data, &req); err != nil {
			problems = append(problems, fmt.Errorf("parsing %s: %w", path, err))
			continue
		}
		if err := ValidateRequest(req); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", path, err))
			continue
		}
		requests = append(requests, req)
	}
	sort.SliceStable(requests, func(i, j int) bool {
		if requests[i].CreatedAt.Equal(requests[j].CreatedAt) {
			return requests[i].ID < requests[j].ID
		}
		return requests[i].CreatedAt.Before(requests[j].CreatedAt)
	})
	return requests, problems
}

// RemoveRequest deletes one handled request. A missing file is not an error: the consumer
// and the UI may race, and both outcomes mean "no longer pending".
func RemoveRequest(dshHome, id string) error {
	if !requestIDRE.MatchString(id) {
		return Errorf(CodeInvalidPath, "request id %q is not usable", id)
	}
	err := securefile.RemoveFile(filepath.Join(RequestDir(dshHome), id+".json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// WriteReply records the outcome of one request for the tenant's UI.
func WriteReply(dshHome string, reply Reply) error {
	if !requestIDRE.MatchString(reply.ID) {
		return Errorf(CodeInvalidPath, "reply id %q is not usable", reply.ID)
	}
	if reply.At.IsZero() {
		reply.At = time.Now().UTC()
	}
	data, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	dir := ReplyDir(dshHome)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return securefile.WriteAtomic(filepath.Join(dir, reply.ID+".json"), append(data, '\n'), 0o600)
}

// ReadReplies lists the replies a tenant has not acknowledged yet.
func ReadReplies(dshHome string) ([]Reply, error) {
	entries, err := os.ReadDir(ReplyDir(dshHome))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	replies := make([]Reply, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := securefile.ReadLimitedRegular(filepath.Join(ReplyDir(dshHome), entry.Name()), 64<<10)
		if err != nil {
			continue
		}
		var reply Reply
		if err := json.Unmarshal(data, &reply); err != nil {
			continue
		}
		replies = append(replies, reply)
	}
	sort.SliceStable(replies, func(i, j int) bool {
		if replies[i].At.Equal(replies[j].At) {
			return replies[i].ID < replies[j].ID
		}
		return replies[i].At.Before(replies[j].At)
	})
	return replies, nil
}

// RemoveReply forgets one reply the UI has shown.
func RemoveReply(dshHome, id string) error {
	if !requestIDRE.MatchString(id) {
		return Errorf(CodeInvalidPath, "reply id %q is not usable", id)
	}
	err := securefile.RemoveFile(filepath.Join(ReplyDir(dshHome), id+".json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
