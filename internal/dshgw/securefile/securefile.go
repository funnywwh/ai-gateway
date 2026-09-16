// Package securefile provides anchored atomic replacement and advisory locks.
// Linux openat with O_NOFOLLOW pins each directory component before walking the
// next one: root lifecycle commands must not follow tenant-controlled symlinks.
package securefile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// openParent resolves each parent through an already-open directory descriptor.
// Never run MkdirAll before this walk: it could create directories outside a
// tenant root by following an attacker-controlled ancestor symlink.
func openParent(path string, create bool) (*os.File, string, error) {
	if path == "" {
		return nil, "", errors.New("empty file path")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	if absolute == "/" {
		return nil, "", errors.New("file path must not be filesystem root")
	}
	dir, err := os.Open("/")
	if err != nil {
		return nil, "", err
	}
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(absolute), "/"), "/") {
		if part == "" {
			continue
		}
		flags := syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
		fd, openErr := syscall.Openat(int(dir.Fd()), part, flags, 0)
		if errors.Is(openErr, syscall.ENOENT) && create {
			if mkdirErr := syscall.Mkdirat(int(dir.Fd()), part, 0o750); mkdirErr != nil && !errors.Is(mkdirErr, syscall.EEXIST) {
				_ = dir.Close()
				return nil, "", fmt.Errorf("create parent of %s: %w", path, mkdirErr)
			}
			fd, openErr = syscall.Openat(int(dir.Fd()), part, flags, 0)
		}
		_ = dir.Close()
		if openErr != nil {
			return nil, "", fmt.Errorf("open parent of %s without symlinks: %w", path, openErr)
		}
		dir = os.NewFile(uintptr(fd), part)
	}
	return dir, filepath.Base(absolute), nil
}

func openRegularAt(dir *os.File, name string, flags int, mode fs.FileMode) (*os.File, error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	return file, nil
}

// OpenRegular opens a regular file without following leaf or ancestor symlinks.
// The caller must close it. O_NONBLOCK prevents a malicious FIFO from hanging a
// privileged command before the descriptor's type can be checked.
func OpenRegular(path string, flags int, mode fs.FileMode) (*os.File, error) {
	dir, name, err := openParent(path, flags&os.O_CREATE != 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	file, err := openRegularAt(dir, name, flags, mode)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return file, nil
}

// RemoveFile unlinks a leaf through its pinned, symlink-free parent directory.
func RemoveFile(path string) error {
	dir, name, err := openParent(path, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	return syscall.Unlinkat(int(dir.Fd()), name)
}

// CheckPermissions rejects non-regular files, symlinks, and permission bits
// outside allowed. Numeric ownership is checked separately by doctor.
func CheckPermissions(path string, allowed fs.FileMode) error {
	file, err := OpenRegular(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if extra := info.Mode().Perm() &^ allowed.Perm(); extra != 0 {
		return fmt.Errorf("%s mode %04o is broader than %04o", path, info.Mode().Perm(), allowed.Perm())
	}
	return nil
}

// WriteAtomic pins the destination directory, writes and fsyncs a new regular
// file, renames within that same directory, then fsyncs the directory. Existing
// UID/GID are preserved; symlink targets and symlink parents are rejected.
func WriteAtomic(path string, data []byte, mode fs.FileMode) (err error) {
	dir, name, err := openParent(path, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	uid, gid := -1, -1
	old, openErr := openRegularAt(dir, name, os.O_RDONLY, 0)
	if openErr == nil {
		info, statErr := old.Stat()
		_ = old.Close()
		if statErr != nil {
			return statErr
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(stat.Uid), int(stat.Gid)
		}
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", path, openErr)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tempName := "." + name + ".tmp-" + hex.EncodeToString(nonce[:])
	temp, err := openRegularAt(dir, tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		_ = temp.Close()
		_ = syscall.Unlinkat(int(dir.Fd()), tempName)
	}()
	if uid >= 0 {
		if err := temp.Chown(uid, gid); err != nil {
			return err
		}
	}
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := syscall.Renameat(int(dir.Fd()), tempName, int(dir.Fd()), name); err != nil {
		return err
	}
	if err := dir.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

// WithLock serializes operators that mutate dshgw state.
func WithLock(path string, fn func() error) error {
	file, err := OpenRegular(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

// CheckOwnership resolves the configured OS identity and verifies its numeric
// owner. It accepts directories too, but never a leaf symlink.
func CheckOwnership(path, owner, group string) error {
	account, err := user.Lookup(owner)
	if err != nil {
		return err
	}
	gr, err := user.LookupGroup(group)
	if err != nil {
		return err
	}
	wantUID, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return err
	}
	wantGID, err := strconv.ParseUint(gr.Gid, 10, 32)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("file ownership is unavailable on this platform")
	}
	if uint64(stat.Uid) != wantUID || uint64(stat.Gid) != wantGID {
		return fmt.Errorf("%s owner is %d:%d, want %s:%s", path, stat.Uid, stat.Gid, owner, group)
	}
	return nil
}

// ReadLimitedRegular uses a single symlink-free descriptor and enforces the
// bound while reading as well as before allocation (the file may grow).
func ReadLimitedRegular(path string, max int64) ([]byte, error) {
	if max < 0 || max == int64(^uint64(0)>>1) {
		return nil, errors.New("invalid read limit")
	}
	file, err := OpenRegular(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > max {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, max)
	}
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, max)
	}
	return data, nil
}
