package browserworkspace

import (
	"context"
	"strings"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func invalidPortableNames() []string {
	names := []string{"bad\xff", "a:b", strings.Repeat("x", 256), strings.Repeat("中", 86), "a/b", `a\b`, ".", "..", ""}
	for c := byte(0); c < 0x20; c++ {
		names = append(names, "a"+string([]byte{c})+"b")
	}
	return append(names, "a\x7fb")
}
func TestPortableNamesBeforeBackendAndKernel(t *testing.T) {
	calls := 0
	value := Value{Kind: "file"}
	n := testNode(BackendFunc(func(context.Context, Request) (Response, error) {
		calls++
		return Response{OK: true, Value: value}, nil
	}))
	for _, name := range invalidPortableNames() {
		before := calls
		if _, e := n.Lookup(context.Background(), name, &fuse.EntryOut{}); e != syscall.ENOENT {
			t.Fatalf("lookup %q: %v", name, e)
		}
		if _, _, _, e := n.Create(context.Background(), name, 0, 0600, &fuse.EntryOut{}); e != syscall.EINVAL {
			t.Fatalf("create %q: %v", name, e)
		}
		if calls != before {
			t.Fatalf("invalid name reached backend: %q", name)
		}
		value = Value{Entries: []Entry{{Name: name, Kind: "file"}}}
		stream, e := n.Readdir(context.Background())
		if e != syscall.EIO || stream != nil {
			t.Fatalf("invalid backend name exposed: %q: %v", name, e)
		}
	}
	// A literal replacement character is valid and must never alias invalid bytes.
	for _, name := range []string{"中文.txt", "valid\ufffd", strings.Repeat("x", 255), strings.Repeat("中", 85)} {
		if !validName(name) {
			t.Fatalf("rejected valid name %q", name)
		}
	}
}
func TestRequestValidatesWholeRelativePath(t *testing.T) {
	calls := 0
	n := testNode(BackendFunc(func(context.Context, Request) (Response, error) { calls++; return Response{OK: true}, nil }))
	invalid := append(invalidPortableNames(), "/absolute", "a//b", "a/../b", "a/./b", "a/", strings.Repeat("a/", 2048)+"a")
	for _, p := range invalid {
		if p == "" || p == "a/b" {
			continue
		} // Root and a valid multi-component path.
		for _, req := range []Request{{Op: OpRead, Path: p}, {Op: OpRename, Path: "ok", Target: p}} {
			if _, e := n.req(context.Background(), req); e != syscall.EINVAL {
				t.Fatalf("accepted %+v: %v", req, e)
			}
		}
	}
	if _, e := n.req(context.Background(), Request{Op: OpRename, Path: "ok"}); e != syscall.EINVAL {
		t.Fatal(e)
	}
	if calls != 0 {
		t.Fatal("invalid path reached backend")
	}
	// Exactly 4096 bytes with individually valid components is accepted.
	maxPath := strings.Repeat("a/", 2047) + "ab"
	for _, p := range []string{"", "中文/valid\ufffd", maxPath} {
		if _, e := n.req(context.Background(), Request{Op: OpStat, Path: p}); e != 0 {
			t.Fatalf("valid %q: %v", p, e)
		}
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}
