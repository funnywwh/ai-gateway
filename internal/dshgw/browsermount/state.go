package browsermount

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// mountRecord is one durable mount. Persistent marks a mount whose directory key came from
// the client, which makes the mount point the stable virtual path of that local directory
// rather than per-mount scratch: startup cleanup must release the mount but leave the empty
// directory in place, exactly as a disconnect does.
type mountRecord struct {
	ID, Tenant, Workspace, Path, State string
	Persistent                         bool
}

// recordPath names the record file. The account is part of the FILE NAME because the key is
// now normally the local directory's own name: "Downloads" is unique inside one account but
// not across the gateway, and a shared file name would let one account's mount overwrite
// another's record. Tenant names are [a-z][a-z0-9-]*, so the first dot is unambiguous.
func (s *Service) recordPath(tenant, id string) string {
	return filepath.Join(s.recordDir, tenant+"."+id+".json")
}

// recordFileName accepts both the current "<tenant>.<id>.json" and the "<id>.json" written
// before a key could be a directory name. The record CONTENT is validated either way, so
// refusing the older name would only leave an upgraded gateway's stale mount uncleanable.
func recordFileName(name string, r mountRecord) bool {
	return name == r.Tenant+"."+r.ID+".json" || name == r.ID+".json"
}
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
		err = os.Rename(name, s.recordPath(r.Tenant, r.ID))
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
	// The id is one safe path segment: the local directory's own name, or the 32/48 hex id a
	// client that predates directory names still sends. Base is checked again here because a
	// record is a file this process must be able to trust after a crash.
	if !validDirectoryKey(r.ID) || filepath.Base(r.ID) != r.ID {
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
		if json.Unmarshal(b, &r) != nil || !validRecordPath(r) || !recordFileName(e.Name(), r) {
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
			// The same forced ladder the live teardown uses (M76): a lazy detach, and an abort of
			// the FUSE connection when even that does not remove the entry. Startup is exactly
			// where a mount held by a foreign namespace is found still attached, so a plain
			// `-u -z` here would leave the mount (and the record) behind on every restart.
			if err := s.forceDetach(r.Path); err != nil {
				errs = append(errs, err)
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
		// A stable mount point is released only by an explicit purge: leaving the empty
		// directory is what keeps the virtual path of that local directory (and the DSH
		// workspace entry pointing at it) in place across a gateway restart.
		if !r.Persistent {
			if er = removeAbsentOK(r.Path); er != nil {
				errs = append(errs, er)
				continue
			}
		}
		if er = removeAbsentOK(entry); er != nil {
			errs = append(errs, er)
		}
	}
	return errors.Join(errs...)
}
