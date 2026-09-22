package browsermount

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// DetachedGuard protects offline CLI lifecycle operations. Only the serving
// process owns the browser connection and can safely detach a live mount.
// A separate process must never archive or recursively remove its contents.
// It intentionally does not advertise stale FUSE mounts as usable workspaces.
type DetachedGuard struct{ Registry *registry.Registry }

func (g *DetachedGuard) MountsFor(string) []string { return nil }

// AttachedMounts refuses to guess: this process does not own the browser connection, so it
// cannot tell an attached mount from a stale entry, and DetachTenant below is where it says so.
func (g *DetachedGuard) AttachedMounts(string) []string { return nil }

// DetachTenant is the logout half for a process that does not own the mounts (M76). The offline
// guard's whole point is that only the serving process may touch a live mount, so this is the
// same refusal as DropTenant.
func (g *DetachedGuard) DetachTenant(ctx context.Context, tenant string) error {
	return g.DropTenant(ctx, tenant)
}
func (g *DetachedGuard) DropTenant(_ context.Context, tenant string) error {
	excluded, err := g.BrowserBackupExclusions(tenant)
	if err != nil {
		return err
	}
	if len(excluded) > 0 {
		return fmt.Errorf("tenant %s has a browser mount; close it through the running gateway before offline lifecycle operations", tenant)
	}
	return nil
}

// BrowserBackupExclusions excludes only actually mounted remote content when
// the feature is off. Ordinary directories named browser must still be backed up.
func (g *DetachedGuard) BrowserBackupExclusions(tenant string) ([]string, error) {
	t, ok := g.Registry.Get(tenant)
	if !ok {
		return nil, fmt.Errorf("unknown tenant %s", tenant)
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("cannot verify browser mounts: %w", err)
	}
	defer f.Close()
	root := filepath.Join(t.Workspace, "browser")
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 1<<20)
	var found bool
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 6 {
			continue
		}
		mp := unescapeMount(fields[4])
		if mp == root || strings.HasPrefix(mp, root+string(filepath.Separator)) {
			found = true
		}
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	if found {
		return []string{root}, nil
	}
	return nil, nil
}
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if c, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(c))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
