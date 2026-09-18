package browsermount

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// Uses a real kernel FUSE mount and the actual HTTP reverse carrier. The peer
// simulates browser file handles in Go; this is not Chromium permission testing.
func TestRealReverseHTTPFUSE(t *testing.T) {
	if os.Getenv("BROWSERWORKSPACE_FUSE_TEST") != "1" {
		t.Skip("set BROWSERWORKSPACE_FUSE_TEST=1")
	}
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "bytes.bin"), []byte{0, 255, 13, 10, 128}, 0600); err != nil {
		t.Fatal(err)
	}
	tenant := registry.Tenant{Name: "browser-e2e", Workspace: t.TempDir()}
	service := New(func(context.Context, registry.Tenant) error { return nil })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { service.ServeTenant(w, r, tenant, "owner") }))
	defer server.Close()
	call := func(ctx context.Context, endpoint string, p any) (json.RawMessage, error) {
		body, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/browser-workspace/"+endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var result struct {
			OK    bool
			Value json.RawMessage
			Error struct{ Message string }
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, err
		}
		if !result.OK {
			return nil, fmt.Errorf("remote: %s", result.Error.Message)
		}
		return result.Value, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := call(ctx, "open", map[string]any{"name": "local", "writable": true})
	if err != nil {
		t.Fatal(err)
	}
	var opened struct{ Token, Mountpoint string }
	if err := json.Unmarshal(raw, &opened); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := service.DropTenant(context.Background(), tenant.Name); err != nil {
			t.Error(err)
		}
	}()
	peerCtx, stopPeer := context.WithCancel(ctx)
	defer stopPeer()
	done := make(chan error, 1)
	go func() {
		for {
			raw, err := call(peerCtx, "poll", map[string]string{"token": opened.Token})
			if err != nil {
				done <- err
				return
			}
			var batch struct{ Requests []fs.Request }
			if err := json.Unmarshal(raw, &batch); err != nil {
				done <- err
				return
			}
			for _, r := range batch.Requests {
				response := localOperation(local, r)
				if _, err := call(peerCtx, "respond", map[string]any{"token": opened.Token, "id": r.ID, "result": response}); err != nil {
					done <- err
					return
				}
			}
		}
	}()
	defer func() { stopPeer(); <-done }()
	if _, err := call(ctx, "activate", map[string]string{"token": opened.Token}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(opened.Mountpoint, "bytes.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte{0, 255, 13, 10, 128}) {
		t.Fatal(got)
	}
	if err := os.Mkdir(filepath.Join(opened.Mountpoint, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte{255, 0, 32, 3}
	if err := os.WriteFile(filepath.Join(opened.Mountpoint, "dir", "new.bin"), data, 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(local, "dir", "new.bin"))
	if err != nil || !bytes.Equal(data, actual) {
		t.Fatal(actual, err)
	}
	if err := os.Truncate(filepath.Join(opened.Mountpoint, "dir", "new.bin"), 2); err != nil {
		t.Fatal(err)
	}
	actual, err = os.ReadFile(filepath.Join(local, "dir", "new.bin"))
	if err != nil || len(actual) != 2 {
		t.Fatal(actual, err)
	}
	if err := os.Remove(filepath.Join(opened.Mountpoint, "dir", "new.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(opened.Mountpoint, "dir")); err != nil {
		t.Fatal(err)
	}
}
func localOperation(root string, r fs.Request) fs.Response {
	path := filepath.Join(root, filepath.FromSlash(r.Path))
	var v fs.Value
	var err error
	switch r.Op {
	case "stat":
		var info os.FileInfo
		info, err = os.Stat(path)
		if err == nil {
			v.Size = info.Size()
			v.Kind = "file"
			if info.IsDir() {
				v.Kind = "directory"
			}
			v.LastModified = info.ModTime().UnixMilli()
		}
	case "list":
		var entries []os.DirEntry
		entries, err = os.ReadDir(path)
		for _, e := range entries {
			kind := "file"
			if e.IsDir() {
				kind = "directory"
			}
			v.Entries = append(v.Entries, fs.Entry{Name: e.Name(), Kind: kind})
		}
	case "read":
		var data []byte
		data, err = os.ReadFile(path)
		if err == nil && r.Offset < int64(len(data)) {
			end := min(int64(len(data)), r.Offset+r.Size)
			v.Data = data[r.Offset:end]
			v.Bytes = uint32(len(v.Data))
		}
	case "write":
		var f *os.File
		f, err = os.OpenFile(path, os.O_WRONLY, 0600)
		if err == nil {
			var n int
			n, err = f.WriteAt(r.Data, r.Offset)
			v.Bytes = uint32(n)
			_ = f.Close()
		}
	case "create":
		var f *os.File
		f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			_ = f.Close()
			v.Kind = "file"
		}
	case "mkdir":
		err = os.Mkdir(path, 0700)
		v.Kind = "directory"
	case "unlink", "rmdir":
		err = os.Remove(path)
	case "truncate":
		err = os.Truncate(path, r.Size)
	case "flush":
	default:
		return fs.Response{Error: &fs.RemoteError{Code: "ENOTSUP"}}
	}
	if err != nil {
		code := "EIO"
		if os.IsNotExist(err) {
			code = "ENOENT"
		}
		if os.IsExist(err) {
			code = "EEXIST"
		}
		return fs.Response{Error: &fs.RemoteError{Code: code, Message: err.Error()}}
	}
	return fs.Response{OK: true, Value: v}
}
