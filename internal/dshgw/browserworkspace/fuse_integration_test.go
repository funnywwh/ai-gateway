package browserworkspace

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// diskBackend is deliberately a test-only POSIX backend. Passing these tests
// validates the FUSE adapter, NOT availability of POSIX features in browser FSA.
type diskBackend struct {
	root    string
	offline atomic.Bool
	stalled atomic.Bool
}

func (b *diskBackend) Call(ctx context.Context, r Request) (Response, error) {
	if b.offline.Load() {
		return Response{}, errors.New("browser disconnected")
	}
	if b.stalled.Load() {
		<-ctx.Done()
		return Response{}, ctx.Err()
	}
	p := filepath.Join(b.root, filepath.FromSlash(r.Path))
	v := Value{}
	var err error
	stat := func() {
		var st os.FileInfo
		st, err = os.Stat(p)
		if err == nil {
			v.Kind = "file"
			if st.IsDir() {
				v.Kind = "directory"
			}
			v.Size = st.Size()
			v.LastModified = st.ModTime().UnixMilli()
		}
	}
	switch r.Op {
	case OpStat:
		stat()
	case OpCreate:
		flags := os.O_CREATE | os.O_RDWR
		if r.Exclusive {
			flags |= os.O_EXCL
		}
		if r.Truncate {
			flags |= os.O_TRUNC
		}
		var f *os.File
		f, err = os.OpenFile(p, flags, 0600)
		if err == nil {
			err = f.Close()
		}
		if err == nil {
			stat()
		}
	case OpMkdir:
		err = os.Mkdir(p, 0700)
		if err == nil {
			stat()
		}
	case OpRead:
		var f *os.File
		f, err = os.Open(p)
		if err == nil {
			defer f.Close()
			v.Data = make([]byte, int(r.Size))
			var n int
			n, err = f.ReadAt(v.Data, r.Offset)
			v.Data = v.Data[:n]
			v.Bytes = uint32(n)
			if errors.Is(err, io.EOF) {
				err = nil
			}
		}
	case OpWrite:
		var f *os.File
		f, err = os.OpenFile(p, os.O_WRONLY, 0)
		if err == nil {
			defer f.Close()
			var n int
			n, err = f.WriteAt(r.Data, r.Offset)
			v.Bytes = uint32(n)
		}
	case OpTruncate:
		err = os.Truncate(p, r.Size)
	case OpRename:
		err = os.Rename(p, filepath.Join(b.root, filepath.FromSlash(r.Target)))
	case OpUnlink:
		err = syscall.Unlink(p)
	case OpRmdir:
		err = syscall.Rmdir(p)
	case OpList:
		var entries []os.DirEntry
		entries, err = os.ReadDir(p)
		for _, e := range entries {
			k := "file"
			if e.IsDir() {
				k = "directory"
			}
			v.Entries = append(v.Entries, Entry{Name: e.Name(), Kind: k})
		}
	case OpFlush:
		_, err = os.Stat(p)
	default:
		err = syscall.ENOTSUP
	}
	if err != nil {
		code := "EIO"
		for _, c := range []string{"ENOENT", "EEXIST", "ENOTDIR", "EISDIR", "ENOTEMPTY", "EACCES", "ENOTSUP"} {
			if errors.Is(err, codeErr(&RemoteError{Code: c})) {
				code = c
				break
			}
		}
		return Response{Error: &RemoteError{Code: code, Message: err.Error()}}, nil
	}
	return Response{OK: true, Value: v}, nil
}

// Opt-in real kernel test. All backing files and mountpoints are temporary;
// cleanup unmounts before testing removes their directories, even on failure.
func TestRealFUSEMount(t *testing.T) {
	if os.Getenv("BROWSERWORKSPACE_FUSE_TEST") != "1" {
		t.Skip("set BROWSERWORKSPACE_FUSE_TEST=1")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Fatal(err)
	}
	mount := t.TempDir()
	b := &diskBackend{root: t.TempDir()}
	srv, err := MountFSWithOptions(mount, b, MountOptions{Timeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b.offline.Store(false)
		b.stalled.Store(false)
		if err := srv.Unmount(); err != nil {
			t.Errorf("unmount: %v", err)
		}
		srv.Wait()
	})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	expect := func(err error, want syscall.Errno) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	}
	p := func(s string) string { return filepath.Join(mount, s) }
	read := func(s, want string) {
		t.Helper()
		data, err := os.ReadFile(p(s))
		must(err)
		if string(data) != want {
			t.Fatalf("%s: got %q want %q", s, data, want)
		}
	}
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	must(err)
	if !strings.Contains(string(mountinfo), " - fuse.browser-workspace dshgw-browser-workspace ") {
		t.Fatal("mount identity missing from mountinfo")
	}
	must(os.Mkdir(p("a"), 0700))
	must(os.Mkdir(p("a/nested"), 0700))
	must(os.WriteFile(p("a/nested/file"), []byte("hello world"), 0600))
	read("a/nested/file", "hello world")
	f, err := os.OpenFile(p("a/nested/file"), os.O_RDWR, 0)
	must(err)
	defer f.Close()
	must(os.Rename(p("a/nested/file"), p("a/nested/moved")))
	must(f.Truncate(5))
	read("a/nested/moved", "hello")
	_, err = f.WriteAt([]byte("!"), 5)
	must(err)
	read("a/nested/moved", "hello!")
	must(os.Rename(p("a"), p("b")))
	_, err = f.WriteAt([]byte("X"), 0)
	must(err)
	must(f.Truncate(4))
	read("b/nested/moved", "Xell")
	data := make([]byte, 4)
	_, err = f.ReadAt(data, 0)
	must(err)
	if string(data) != "Xell" {
		t.Fatal(string(data))
	}
	must(os.Truncate(p("b/nested/moved"), 2))
	read("b/nested/moved", "Xe")
	expect(os.Chmod(p("b/nested/moved"), 0400), syscall.ENOTSUP)
	expect(os.Chtimes(p("b/nested/moved"), time.Now(), time.Now()), syscall.ENOTSUP)
	excl, err := os.OpenFile(p("b/nested/moved"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if excl != nil {
		excl.Close()
	}
	expect(err, syscall.EEXIST)
	// Existing O_CREATE without O_TRUNC preserves content; O_TRUNC must truncate.
	plain, err := os.OpenFile(p("b/nested/moved"), os.O_CREATE|os.O_WRONLY, 0600)
	must(err)
	must(plain.Close())
	read("b/nested/moved", "Xe")
	must(os.WriteFile(p("b/nested/moved"), []byte("z"), 0600))
	read("b/nested/moved", "z")
	// Cached positive/negative lookups and file data must observe external changes.
	_, err = os.Stat(p("external"))
	expect(err, syscall.ENOENT)
	must(os.WriteFile(filepath.Join(b.root, "external"), []byte("first"), 0600))
	read("external", "first")
	must(os.WriteFile(filepath.Join(b.root, "external"), []byte("changed"), 0600))
	read("external", "changed")
	must(os.Remove(filepath.Join(b.root, "external")))
	_, err = os.Stat(p("external"))
	expect(err, syscall.ENOENT)
	expect(os.Remove(p("b")), syscall.ENOTEMPTY)
	b.offline.Store(true)
	_, err = f.ReadAt(data, 0)
	expect(err, syscall.EIO)
	_, err = os.Stat(p("b/nested/moved"))
	expect(err, syscall.EIO)
	_, err = os.ReadDir(p("b"))
	expect(err, syscall.EIO)
	b.offline.Store(false)
	b.stalled.Store(true)
	start := time.Now()
	_, err = f.ReadAt(data, 0)
	expect(err, syscall.ETIMEDOUT)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
	b.stalled.Store(false)
	must(f.Close())
	must(os.Remove(p("b/nested/moved")))
	must(os.Remove(p("b/nested")))
	must(os.Remove(p("b")))
	entries, err := os.ReadDir(mount)
	must(err)
	if len(entries) != 0 {
		t.Fatal(entries)
	}
}
