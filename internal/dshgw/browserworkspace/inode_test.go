package browserworkspace

import (
	"context"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// This specifically guards the zero-TTL lookup bug found by the kernel test:
// fresh inodes on every lookup orphan existing handles when a directory moves.
func TestLookupPreservesInodeTree(t *testing.T) {
	var requests []Request
	value := Value{Kind: "directory"}
	root := testNode(BackendFunc(func(_ context.Context, r Request) (Response, error) {
		requests = append(requests, r)
		return Response{OK: true, Value: value}, nil
	}))
	ctx := context.Background()
	dir, e := root.Lookup(ctx, "a", &fuse.EntryOut{})
	if e != 0 {
		t.Fatal(e)
	}
	root.AddChild("a", dir, true)
	again, e := root.Lookup(ctx, "a", &fuse.EntryOut{})
	if e != 0 || again != dir {
		t.Fatalf("directory identity changed: %v", e)
	}
	dn := dir.Operations().(*node)
	value = Value{Kind: "file", Size: 1}
	file, e := dn.Lookup(ctx, "f", &fuse.EntryOut{})
	if e != 0 {
		t.Fatal(e)
	}
	dir.AddChild("f", file, true)
	again, e = dn.Lookup(ctx, "f", &fuse.EntryOut{})
	if e != 0 || again != file {
		t.Fatalf("file identity changed: %v", e)
	}
	if e := root.Rename(ctx, "a", root, "b", 0); e != 0 {
		t.Fatal(e)
	}
	// rawBridge performs this after a successful NodeRenamer call.
	if !root.MvChild("a", &root.Inode, "b", true) {
		t.Fatal("move")
	}
	fn := file.Operations().(*node)
	in := &fuse.SetAttrIn{}
	in.Valid = fuse.FATTR_SIZE
	in.Size = 1
	if e := fn.Setattr(ctx, nil, in, &fuse.AttrOut{}); e != 0 {
		t.Fatal(e)
	}
	last := requests[len(requests)-2]
	if last.Op != OpTruncate || last.Path != "b/f" {
		t.Fatalf("old path after rename: %+v", last)
	}
	if e := root.Rename(ctx, "b", root, "c", 1); e != syscall.ENOTSUP {
		t.Fatal(e)
	}
	// A changed type must not reuse a stale inode of the opposite kind.
	value = Value{Kind: "directory"}
	changed, e := dn.Lookup(ctx, "f", &fuse.EntryOut{})
	if e != 0 || changed == file {
		t.Fatalf("stale type reused: %v", e)
	}
}
