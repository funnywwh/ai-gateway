package browserworkspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type MountOptions struct {
	Timeout time.Duration
	Debug   bool
}

func MountFS(mountpoint string, backend Backend) (*fuse.Server, error) {
	return MountFSWithOptions(mountpoint, backend, MountOptions{Timeout: 30 * time.Second})
}
func MountFSWithOptions(mountpoint string, backend Backend, opts MountOptions) (*fuse.Server, error) {
	if backend == nil || mountpoint == "" || !path.IsAbs(mountpoint) {
		return nil, fmt.Errorf("browserworkspace: invalid mountpoint or backend")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	root := &node{backend: backend, timeout: opts.Timeout}
	// No attribute, positive/negative entry or data caching: a disconnected browser
	// must not appear to provide live files. Existing directory streams are snapshots.
	noCache := time.Duration(0)
	nfs := fs.NewNodeFS(root, &fs.Options{MountOptions: fuse.MountOptions{Debug: opts.Debug, Name: "browser-workspace", FsName: "dshgw-browser-workspace"}, NullPermissions: true, EntryTimeout: &noCache, AttrTimeout: &noCache, NegativeTimeout: &noCache})
	srv, err := fuse.NewServer(nfs, mountpoint, &fuse.MountOptions{Debug: opts.Debug, Name: "browser-workspace", FsName: "dshgw-browser-workspace"})
	if err != nil {
		return nil, err
	}
	go srv.Serve()
	if err := srv.WaitMount(); err != nil {
		_ = srv.Unmount()
		return nil, err
	}
	return srv, nil
}

var (
	_ fs.NodeLookuper  = (*node)(nil)
	_ fs.NodeGetattrer = (*node)(nil)
	_ fs.NodeReaddirer = (*node)(nil)
	_ fs.NodeReader    = (*node)(nil)
	_ fs.NodeWriter    = (*node)(nil)
	_ fs.NodeCreater   = (*node)(nil)
	_ fs.NodeMkdirer   = (*node)(nil)
	_ fs.NodeRenamer   = (*node)(nil)
	_ fs.NodeUnlinker  = (*node)(nil)
	_ fs.NodeRmdirer   = (*node)(nil)
	_ fs.NodeFlusher   = (*node)(nil)
	_ fs.NodeOpener    = (*node)(nil)
	_ fs.NodeSetattrer = (*node)(nil)
)

type node struct {
	fs.Inode
	backend Backend
	timeout time.Duration
}

// go-fuse updates the inode tree after a successful Rename. Deriving paths
// from that tree also updates nested, already-open children of moved directories.
func (n *node) currentPath() string { return n.Path(n.Root()) }
func (n *node) req(ctx context.Context, r Request) (Response, syscall.Errno) {
	// Validate before JSON marshaling: invalid UTF-8 otherwise becomes U+FFFD,
	// potentially aliasing a different, valid browser filename.
	if !validRelativePath(r.Path) || !validRelativePath(r.Target) || (r.Op == OpRename && r.Target == "") {
		return Response{}, syscall.EINVAL
	}
	c, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	r.ID = newID()
	resp, err := n.backend.Call(c, r)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Response{}, syscall.ETIMEDOUT
		}
		if errors.Is(err, context.Canceled) {
			return Response{}, syscall.EINTR
		}
		return Response{}, syscall.EIO
	}
	if !resp.OK {
		return resp, codeErr(resp.Error)
	}
	return resp, 0
}
func newID() string {
	b := make([]byte, 8)
	if _, e := rand.Read(b); e != nil {
		return "fuse"
	}
	return hex.EncodeToString(b)
}
func codeErr(e *RemoteError) syscall.Errno {
	if e == nil {
		return syscall.EIO
	}
	switch strings.ToUpper(e.Code) {
	case "ENOENT":
		return syscall.ENOENT
	case "EEXIST":
		return syscall.EEXIST
	case "ENOTDIR":
		return syscall.ENOTDIR
	case "EISDIR":
		return syscall.EISDIR
	case "ENOTEMPTY":
		return syscall.ENOTEMPTY
	case "EACCES":
		return syscall.EACCES
	case "EPERM":
		return syscall.EPERM
	case "EINVAL":
		return syscall.EINVAL
	case "ENOSPC":
		return syscall.ENOSPC
	case "EROFS":
		return syscall.EROFS
	case "EOVERFLOW":
		return syscall.EOVERFLOW
	case "EFBIG":
		return syscall.EFBIG
	case "EXDEV":
		return syscall.EXDEV
	case "EBUSY":
		return syscall.EBUSY
	case "ETIMEDOUT":
		return syscall.ETIMEDOUT
	case "EINTR":
		return syscall.EINTR
	case "ENOTSUP", "EOPNOTSUPP":
		return syscall.ENOTSUP
	default:
		return syscall.EIO
	}
}

// Keep the portable path subset aligned with the browser's checkRelativePath.
// Limits are UTF-8 bytes, not rune counts or JavaScript UTF-16 code units.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\:") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
func validRelativePath(p string) bool {
	if len(p) > 4096 || !utf8.ValidString(p) {
		return false
	}
	if p == "" {
		return true
	}
	for _, name := range strings.Split(p, "/") {
		if !validName(name) {
			return false
		}
	}
	return true
}
func child(p, name string) string {
	if p == "" {
		return name
	}
	return path.Join(p, name)
}
func validValue(v Value) bool { return (v.Kind == "file" || v.Kind == "directory") && v.Size >= 0 }
func fill(a *fuse.Attr, v Value) {
	a.Mode = fuse.S_IFREG | 0o644
	if v.Kind == "directory" {
		a.Mode = fuse.S_IFDIR | 0o755
	}
	a.Size = uint64(v.Size)
	if v.LastModified > 0 {
		a.Mtime = uint64(v.LastModified / 1000)
		a.Mtimensec = uint32((v.LastModified % 1000) * 1e6)
	}
}
func (n *node) inode(ctx context.Context, name string, v Value) *fs.Inode {
	mode := uint32(fuse.S_IFREG)
	if v.Kind == "directory" {
		mode = fuse.S_IFDIR
	}
	// Preserve inode identity across zero-TTL lookups so rename updates open handles.
	if existing := n.GetChild(name); existing != nil && existing.Mode() == mode {
		return existing
	}
	return n.NewInode(ctx, &node{backend: n.backend, timeout: n.timeout}, fs.StableAttr{Mode: mode})
}
func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if !validName(name) {
		return nil, syscall.ENOENT
	}
	r, e := n.req(ctx, Request{Op: OpStat, Path: child(n.currentPath(), name)})
	if e != 0 {
		return nil, e
	}
	if !validValue(r.Value) {
		return nil, syscall.EIO
	}
	fill(&out.Attr, r.Value)
	return n.inode(ctx, name, r.Value), 0
}
func (n *node) Open(ctx context.Context, _ uint32) (fs.FileHandle, uint32, syscall.Errno) {
	var out fuse.AttrOut
	e := n.Getattr(ctx, nil, &out)
	if e == 0 && out.Mode&syscall.S_IFMT != syscall.S_IFREG {
		e = syscall.EISDIR
	}
	// The kernel performs O_TRUNC through Setattr (atomic O_TRUNC is not negotiated).
	return nil, fuse.FOPEN_DIRECT_IO, e
}
func (n *node) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	r, e := n.req(ctx, Request{Op: OpStat, Path: n.currentPath()})
	if e != 0 {
		return e
	}
	if !validValue(r.Value) {
		return syscall.EIO
	}
	fill(&out.Attr, r.Value)
	return 0
}
func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	r, e := n.req(ctx, Request{Op: OpList, Path: n.currentPath()})
	if e != 0 {
		return nil, e
	}
	x := make([]fuse.DirEntry, 0, len(r.Value.Entries))
	seen := map[string]bool{}
	for _, v := range r.Value.Entries {
		k := uint32(fuse.S_IFREG)
		if v.Kind == "directory" {
			k = fuse.S_IFDIR
		} else if v.Kind != "file" {
			return nil, syscall.EIO
		}
		if !validName(v.Name) || seen[v.Name] {
			return nil, syscall.EIO
		}
		seen[v.Name] = true
		x = append(x, fuse.DirEntry{Name: v.Name, Mode: k})
	}
	return fs.NewListDirStream(x), 0
}
func (n *node) Read(ctx context.Context, _ fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off < 0 {
		return nil, syscall.EINVAL
	}
	r, e := n.req(ctx, Request{Op: OpRead, Path: n.currentPath(), Offset: off, Size: int64(len(dest))})
	if e != 0 {
		return nil, e
	}
	if len(r.Value.Data) > len(dest) || uint64(r.Value.Bytes) > uint64(len(r.Value.Data)) {
		return nil, syscall.EIO
	}
	count := len(r.Value.Data)
	if r.Value.Bytes != 0 {
		count = int(r.Value.Bytes)
	}
	return fuse.ReadResultData(r.Value.Data[:count]), 0
}
func (n *node) Write(ctx context.Context, _ fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	if off < 0 {
		return 0, syscall.EINVAL
	}
	r, e := n.req(ctx, Request{Op: OpWrite, Path: n.currentPath(), Offset: off, Data: data, Size: int64(len(data))})
	if e != 0 {
		return 0, e
	}
	if uint64(r.Value.Bytes) > uint64(len(data)) {
		return 0, syscall.EIO
	}
	return r.Value.Bytes, 0
}
func (n *node) Flush(ctx context.Context, _ fs.FileHandle) syscall.Errno {
	_, e := n.req(ctx, Request{Op: OpFlush, Path: n.currentPath()})
	return e
}
func (n *node) Setattr(ctx context.Context, _ fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	// Check ALL requested mutations before changing anything, including combined
	// size+mode requests. FSA cannot persist POSIX ownership, modes or timestamps.
	// Synthetic modes never contain suid/sgid; the kernel kill-privilege hint is safe.
	supported := uint32(fuse.FATTR_SIZE | fuse.FATTR_FH | fuse.FATTR_LOCKOWNER | fuse.FATTR_KILL_SUIDGID)
	if in.Valid & ^supported != 0 {
		return syscall.ENOTSUP
	}
	if sz, ok := in.GetSize(); ok {
		if sz > math.MaxInt64 {
			return syscall.EOVERFLOW
		}
		if _, e := n.req(ctx, Request{Op: OpTruncate, Path: n.currentPath(), Size: int64(sz)}); e != 0 {
			return e
		}
	}
	return n.Getattr(ctx, nil, out)
}
func (n *node) Create(ctx context.Context, name string, flags uint32, _ uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return n.create(ctx, name, Request{Op: OpCreate, Exclusive: flags&syscall.O_EXCL != 0, Truncate: flags&syscall.O_TRUNC != 0}, out)
}
func (n *node) Mkdir(ctx context.Context, name string, _ uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	i, _, _, e := n.create(ctx, name, Request{Op: OpMkdir}, out)
	return i, e
}
func (n *node) create(ctx context.Context, name string, req Request, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	if !validName(name) {
		return nil, nil, 0, syscall.EINVAL
	}
	req.Path = child(n.currentPath(), name)
	r, e := n.req(ctx, req)
	if e != 0 {
		return nil, nil, 0, e
	}
	want := "file"
	if req.Op == OpMkdir {
		want = "directory"
	}
	if !validValue(r.Value) || r.Value.Kind != want {
		return nil, nil, 0, syscall.EIO
	}
	fill(&out.Attr, r.Value)
	return n.inode(ctx, name, r.Value), nil, fuse.FOPEN_DIRECT_IO, 0
}
func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	if !validName(name) {
		return syscall.EINVAL
	}
	_, e := n.req(ctx, Request{Op: OpUnlink, Path: child(n.currentPath(), name)})
	return e
}
func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	if !validName(name) {
		return syscall.EINVAL
	}
	_, e := n.req(ctx, Request{Op: OpRmdir, Path: child(n.currentPath(), name)})
	return e
}
func (n *node) Rename(ctx context.Context, name string, p fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if flags != 0 {
		return syscall.ENOTSUP
	}
	if !validName(name) || !validName(newName) {
		return syscall.EINVAL
	}
	q, ok := p.(*node)
	if !ok || q.Root() != n.Root() {
		return syscall.EXDEV
	}
	_, e := n.req(ctx, Request{Op: OpRename, Path: child(n.currentPath(), name), Target: child(q.currentPath(), newName)})
	return e
}
