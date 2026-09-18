package browserworkspace

import (
	"context"
	"errors"
	"math"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func testNode(b Backend) *node {
	n := &node{backend: b, timeout: 20 * time.Millisecond}
	fs.NewNodeFS(n, &fs.Options{})
	return n
}
func TestLookupValidation(t *testing.T) {
	calls := 0
	value := Value{Kind: "file"}
	n := testNode(BackendFunc(func(context.Context, Request) (Response, error) {
		calls++
		return Response{OK: true, Value: value}, nil
	}))
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "a\x00b", "a\nb"} {
		if _, e := n.Lookup(context.Background(), name, &fuse.EntryOut{}); e != syscall.ENOENT {
			t.Fatalf("%q: %v", name, e)
		}
	}
	if calls != 0 {
		t.Fatal("invalid names reached backend")
	}
	for _, v := range []Value{{}, {Kind: "symlink"}, {Kind: "file", Size: -1}} {
		value = v
		if _, e := n.Lookup(context.Background(), "x", &fuse.EntryOut{}); e != syscall.EIO {
			t.Fatalf("%+v: %v", v, e)
		}
		if e := n.Getattr(context.Background(), nil, &fuse.AttrOut{}); e != syscall.EIO {
			t.Fatalf("%+v: %v", v, e)
		}
	}
	value = Value{Kind: "directory"}
	out := &fuse.EntryOut{}
	i, e := n.Lookup(context.Background(), "x", out)
	if e != 0 || i.Mode() != fuse.S_IFDIR || out.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		t.Fatalf("%v %+v", e, out)
	}
}
func TestCreateFlagsAndNames(t *testing.T) {
	var got Request
	n := testNode(BackendFunc(func(_ context.Context, r Request) (Response, error) {
		got = r
		return Response{OK: true, Value: Value{Kind: "file"}}, nil
	}))
	_, _, flags, e := n.Create(context.Background(), "x", syscall.O_CREAT|syscall.O_EXCL|syscall.O_TRUNC|syscall.O_RDWR, 0600, &fuse.EntryOut{})
	if e != 0 || flags != fuse.FOPEN_DIRECT_IO || !got.Exclusive || !got.Truncate || got.Path != "x" {
		t.Fatalf("%+v %v %v", got, flags, e)
	}
	for _, name := range []string{"..", "a/b", "a\nb"} {
		if _, _, _, e := n.Create(context.Background(), name, 0, 0600, &fuse.EntryOut{}); e != syscall.EINVAL {
			t.Fatal(e)
		}
		if _, e := n.Mkdir(context.Background(), name, 0700, &fuse.EntryOut{}); e != syscall.EINVAL {
			t.Fatal(e)
		}
		if e := n.Unlink(context.Background(), name); e != syscall.EINVAL {
			t.Fatal(e)
		}
		if e := n.Rmdir(context.Background(), name); e != syscall.EINVAL {
			t.Fatal(e)
		}
	}
}
func TestSetattrRejectsBeforeMutation(t *testing.T) {
	var requests []Request
	n := testNode(BackendFunc(func(_ context.Context, r Request) (Response, error) {
		requests = append(requests, r)
		return Response{OK: true, Value: Value{Kind: "file", Size: 4}}, nil
	}))
	for _, flag := range []uint32{fuse.FATTR_MODE, fuse.FATTR_UID, fuse.FATTR_GID, fuse.FATTR_ATIME, fuse.FATTR_MTIME, fuse.FATTR_CTIME, fuse.FATTR_ATIME_NOW, fuse.FATTR_MTIME_NOW, 1 << 31} {
		for _, sizeFlag := range []uint32{0, fuse.FATTR_SIZE} {
			in := &fuse.SetAttrIn{}
			in.Valid = flag | sizeFlag
			in.Size = 4
			if e := n.Setattr(context.Background(), nil, in, &fuse.AttrOut{}); e != syscall.ENOTSUP {
				t.Fatalf("%x: %v", in.Valid, e)
			}
		}
	}
	if len(requests) != 0 {
		t.Fatal(requests)
	}
	in := &fuse.SetAttrIn{}
	in.Valid = fuse.FATTR_SIZE
	in.Size = math.MaxUint64
	if e := n.Setattr(context.Background(), nil, in, &fuse.AttrOut{}); e != syscall.EOVERFLOW {
		t.Fatal(e)
	}
	in.Size = 4
	in.Valid |= fuse.FATTR_KILL_SUIDGID
	out := &fuse.AttrOut{}
	if e := n.Setattr(context.Background(), nil, in, out); e != 0 || out.Size != 4 || out.Mode&syscall.S_IFMT != syscall.S_IFREG {
		t.Fatalf("%v %+v", e, out)
	}
	if len(requests) != 2 || requests[0].Op != OpTruncate || requests[1].Op != OpStat {
		t.Fatal(requests)
	}
}
func TestErrnoAndDeadline(t *testing.T) {
	for code, want := range map[string]syscall.Errno{"ENOENT": syscall.ENOENT, "EEXIST": syscall.EEXIST, "ENOTDIR": syscall.ENOTDIR, "EISDIR": syscall.EISDIR, "ENOTEMPTY": syscall.ENOTEMPTY, "EACCES": syscall.EACCES, "EPERM": syscall.EPERM, "EINVAL": syscall.EINVAL, "ENOSPC": syscall.ENOSPC, "EROFS": syscall.EROFS, "EOVERFLOW": syscall.EOVERFLOW, "EFBIG": syscall.EFBIG, "EXDEV": syscall.EXDEV, "EBUSY": syscall.EBUSY, "ETIMEDOUT": syscall.ETIMEDOUT, "EINTR": syscall.EINTR, "ENOTSUP": syscall.ENOTSUP, "EOPNOTSUPP": syscall.ENOTSUP, "unknown": syscall.EIO} {
		if got := codeErr(&RemoteError{Code: code}); got != want {
			t.Fatalf("%s: %v", code, got)
		}
	}
	if codeErr(nil) != syscall.EIO {
		t.Fatal("missing error accepted")
	}
	n := testNode(BackendFunc(func(ctx context.Context, _ Request) (Response, error) { <-ctx.Done(); return Response{}, ctx.Err() }))
	if _, e := n.req(context.Background(), Request{}); e != syscall.ETIMEDOUT {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := n.req(ctx, Request{}); e != syscall.EINTR {
		t.Fatal(e)
	}
	n.backend = BackendFunc(func(context.Context, Request) (Response, error) { return Response{}, errors.New("offline") })
	if _, e := n.req(context.Background(), Request{}); e != syscall.EIO {
		t.Fatal(e)
	}
}
func TestMalformedResponses(t *testing.T) {
	v := Value{}
	n := testNode(BackendFunc(func(context.Context, Request) (Response, error) { return Response{OK: true, Value: v}, nil }))
	for _, entries := range [][]Entry{{{Name: "..", Kind: "file"}}, {{Name: "x", Kind: "link"}}, {{Name: "x", Kind: "file"}, {Name: "x", Kind: "file"}}} {
		v = Value{Entries: entries}
		if _, e := n.Readdir(context.Background()); e != syscall.EIO {
			t.Fatal(e)
		}
	}
	for _, value := range []Value{{Data: []byte("long")}, {Data: []byte("a"), Bytes: 2}} {
		v = value
		if _, e := n.Read(context.Background(), nil, make([]byte, 2), 0); e != syscall.EIO {
			t.Fatal(e)
		}
	}
	v = Value{Bytes: 3}
	if _, e := n.Write(context.Background(), nil, []byte("a"), 0); e != syscall.EIO {
		t.Fatal(e)
	}
	if _, e := n.Read(context.Background(), nil, nil, -1); e != syscall.EINVAL {
		t.Fatal(e)
	}
	if _, e := n.Write(context.Background(), nil, nil, -1); e != syscall.EINVAL {
		t.Fatal(e)
	}
	v = Value{Kind: "directory"}
	if _, _, _, e := n.Create(context.Background(), "x", 0, 0, &fuse.EntryOut{}); e != syscall.EIO {
		t.Fatal(e)
	}
}
