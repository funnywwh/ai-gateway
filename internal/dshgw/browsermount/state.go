package browsermount

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// AcquireLock serializes startup cleanup and instance lifetime. Configure state,
// registry and acquire this lock before serving; ReleaseLock is owner-only.
func (s *Service) AcquireLock() error {
	if s.recordDir == "" || s.lock != nil {
		return nil
	}
	if err := s.ensureRecordDir(); err != nil {
		return err
	}
	fd, err := syscall.Open(filepath.Join(filepath.Dir(s.recordDir), "browser-mounts.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "browser-mounts.lock")
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		_ = f.Close()
		if err != nil {
			return err
		}
		return errors.New("browser state lock must be a private regular file")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return fmt.Errorf("browser workspace already served: %w", err)
	}
	s.lock = f
	return nil
}
func (s *Service) ReleaseLock() error {
	if s.lock == nil {
		return nil
	}
	err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	if closeErr := s.lock.Close(); err == nil {
		err = closeErr
	}
	s.lock = nil
	return err
}
func (s *Service) ensureRecordDir() error {
	if err := os.MkdirAll(s.recordDir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(s.recordDir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 || !noSymlinkAncestors(s.recordDir) {
		return errors.New("browser records must be a private non-symlink directory")
	}
	return nil
}

type mountRecord struct{ ID, Tenant, Workspace, Path, State string }

func (s *Service) recordPath(id string) string { return filepath.Join(s.recordDir, id+".json") }
func (s *Service) writeRecord(r mountRecord) error {
	if s.recordDir == "" {
		return nil
	}
	if err := s.ensureRecordDir(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.recordDir, ".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		var b []byte
		b, err = json.Marshal(r)
		if err == nil {
			_, err = tmp.Write(append(b, '\n'))
		}
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, s.recordPath(r.ID))
	}
	if err == nil {
		err = syncDirectory(s.recordDir)
	}
	return err
}
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func mountInfoPath(path string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, v := range fields {
			if v == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || len(fields) <= sep+2 {
			continue
		}
		if unescapeMount(fields[4]) == path && unescapeMount(fields[sep+2]) == browserFSName && fields[sep+1] == "fuse.browser-workspace" {
			return true, nil
		}
	}
	return false, sc.Err()
}
func noSymlinkAncestors(path string) bool {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if p == "/" {
			return true
		}
	}
}
func validRecordPath(r mountRecord) bool {
	if len(r.ID) != 48 || filepath.Base(r.ID) != r.ID {
		return false
	}
	if _, err := hex.DecodeString(r.ID); err != nil {
		return false
	}
	if r.Tenant == "" || (r.State != "preparing" && r.State != "ready") {
		return false
	}
	if !filepath.IsAbs(r.Path) || filepath.Clean(r.Path) != r.Path || !filepath.IsAbs(r.Workspace) || filepath.Clean(r.Workspace) != r.Workspace || r.Workspace == "/" || !noSymlinkAncestors(r.Workspace) || !noSymlinkAncestors(filepath.Join(r.Workspace, "browser")) {
		return false
	}
	return filepath.Dir(r.Path) == filepath.Join(r.Workspace, "browser") && filepath.Base(r.Path) == r.ID
}

// CleanupStale is STARTUP ONLY, with no old worker namespaces alive (workers
// must use die-with-parent). It verifies filename, registry workspace, canonical
// path and the kernel FUSE source/type before lazy detach. It never operates on
// a foreign mount or follows a record symlink. Invalid records are retained and
// reported, not silently treated as successfully cleaned.
func (s *Service) CleanupStale() error {
	if s.recordDir == "" {
		return nil
	}
	if err := s.AcquireLock(); err != nil {
		return err
	}
	ents, err := os.ReadDir(s.recordDir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		entry := filepath.Join(s.recordDir, e.Name())
		reject := func() { errs = append(errs, fmt.Errorf("reject unsafe browser mount record %s", e.Name())) }
		st, er := os.Lstat(entry)
		if er != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 16384 {
			reject()
			continue
		}
		b, er := os.ReadFile(entry)
		if er != nil {
			errs = append(errs, er)
			continue
		}
		var r mountRecord
		if json.Unmarshal(b, &r) != nil || !validRecordPath(r) || e.Name() != r.ID+".json" {
			reject()
			continue
		}
		if s.registry == nil {
			errs = append(errs, errors.New("stale cleanup requires tenant registry"))
			continue
		}
		t, ok := s.registry.Get(r.Tenant)
		if !ok || t.Workspace != r.Workspace {
			reject()
			continue
		}
		mounted, er := mountInfoPath(r.Path)
		if er != nil {
			errs = append(errs, er)
			continue
		}
		if mounted {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			er = exec.CommandContext(ctx, "fusermount3", "-u", "-z", r.Path).Run()
			if er != nil {
				er = exec.CommandContext(ctx, "fusermount", "-u", "-z", r.Path).Run()
			}
			cancel()
			if er != nil {
				errs = append(errs, fmt.Errorf("unmount %s: %w", r.Path, er))
				continue
			}
		}
		// Also clean a record whose mount vanished in a crash/previous retry. Refuse
		// a symlink or nonempty/foreign mount rather than deleting arbitrary content.
		if st, er := os.Lstat(r.Path); er == nil && (!st.IsDir() || st.Mode()&os.ModeSymlink != 0) {
			reject()
			continue
		} else if er != nil && !os.IsNotExist(er) {
			errs = append(errs, er)
			continue
		}
		if er = removeAbsentOK(r.Path); er != nil {
			errs = append(errs, er)
			continue
		}
		if er = removeAbsentOK(entry); er != nil {
			errs = append(errs, er)
		}
	}
	return errors.Join(errs...)
}
