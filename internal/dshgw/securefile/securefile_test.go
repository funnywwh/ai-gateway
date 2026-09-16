package securefile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadLimitedRegular(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "data")
	if err := os.WriteFile(path, []byte("abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadLimitedRegular(path, 4); err != nil || string(data) != "abcd" {
		t.Fatalf("read = %q, %v", data, err)
	}
	for _, limit := range []int64{-1, 0, 3} {
		if _, err := ReadLimitedRegular(path, limit); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{link, root, fifo} {
		if _, err := ReadLimitedRegular(invalid, 4); err == nil {
			t.Fatalf("accepted non-regular path %s", invalid)
		}
	}
}

func TestAtomicWritePreservesOwnershipAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "data")
	if err := WriteAtomic(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if err := WriteAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	ownerBefore, ownerAfter := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if ownerBefore.Uid != ownerAfter.Uid || ownerBefore.Gid != ownerAfter.Gid || after.Mode().Perm() != 0o600 {
		t.Fatalf("ownership or mode changed: before=%+v after=%+v", ownerBefore, ownerAfter)
	}
	data, err := ReadLimitedRegular(path, 3)
	if err != nil || string(data) != "new" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

func TestSymlinkParentCannotRedirectPrivilegedOperations(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "untrusted")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "data"), []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLimitedRegular(filepath.Join(link, "data"), 32); err == nil {
		t.Fatal("read followed symlink parent")
	}
	if err := WriteAtomic(filepath.Join(link, "new", "data"), []byte("bad"), 0o600); err == nil {
		t.Fatal("write followed symlink parent")
	}
	if err := WithLock(filepath.Join(link, "new.lock"), func() error { t.Fatal("lock followed symlink parent"); return nil }); err == nil {
		t.Fatal("lock accepted symlink parent")
	}
	if err := RemoveFile(filepath.Join(link, "data")); err == nil {
		t.Fatal("unlink followed symlink parent")
	}
	data, _ := os.ReadFile(filepath.Join(outside, "data"))
	if string(data) != "sentinel" {
		t.Fatalf("outside data changed: %q", data)
	}
	for _, name := range []string{"new", "new.lock"} {
		if _, err := os.Stat(filepath.Join(outside, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created outside parent: %s (%v)", name, err)
		}
	}
}

func TestAtomicWriteRejectsLeafSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(link, []byte("bad"), 0o600); err == nil {
		t.Fatal("accepted leaf symlink")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "sentinel" {
		t.Fatalf("target changed: %q", data)
	}
}
