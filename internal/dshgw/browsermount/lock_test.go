package browsermount

import (
	"testing"
)

func TestBrowserStateLockIsExclusive(t *testing.T) {
	root := t.TempDir()
	first := NewWithState(nil, nil, root)
	if err := first.AcquireLock(); err != nil { t.Fatal(err) }
	defer first.ReleaseLock()
	second := NewWithState(nil, nil, root)
	if err := second.CleanupStale(); err == nil { t.Fatal("second service acquired browser state lock") }
}
