package pluginhost

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	binaryMu      sync.Mutex
	binaryCache   []string
	binaryCacheAt time.Time
)

// ResolveBinary maps a plugin name onto a discovered executable path.
//
// Matching order: exact basename, "aigw-provider-<name>", then any executable whose
// basename contains the name. When nothing matches the name itself is returned so the
// error surfaces at Start time with a useful message.
func (h *Host) ResolveBinary(name string) string {
	h.mu.Lock()
	dir, extra := h.cfg.Dir, h.cfg.Extra
	h.mu.Unlock()

	binaryMu.Lock()
	defer binaryMu.Unlock()

	if time.Since(binaryCacheAt) > 30*time.Second {
		cache := make([]string, 0, 8)
		if dir != "" {
			if entries, err := filepath.Glob(filepath.Join(dir, "*")); err == nil {
				cache = append(cache, entries...)
			}
		}
		cache = append(cache, extra...)
		binaryCache, binaryCacheAt = cache, time.Now()
	}

	best := ""
	for _, path := range binaryCache {
		base := filepath.Base(path)
		switch {
		case base == name:
			return path
		case base == "aigw-provider-"+name:
			best = path
		case best == "" && strings.Contains(base, name):
			best = path
		}
	}
	if best != "" {
		return best
	}
	return name
}
