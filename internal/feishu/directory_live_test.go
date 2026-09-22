package feishu

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"gopkg.in/yaml.v3"
)

// A live test against the real tenant. It never runs in CI or `go test ./...`: it is
// skipped unless FEISHU_LIVE_CONFIG names a config file whose feishu block is enabled.
// Use it while wiring up the two contact permissions in the Feishu console:
//
//	FEISHU_LIVE_CONFIG=config.yaml go test ./internal/feishu/ -run TestLiveDirectory -v
//
// The assertion is deliberately loose — at least one department and one person, and names
// may be missing when the two contact permissions are not granted yet (that is exactly the
// state this test exists to observe).
func TestLiveDirectory(t *testing.T) {
	path := os.Getenv("FEISHU_LIVE_CONFIG")
	if path == "" {
		t.Skip("FEISHU_LIVE_CONFIG is not set; skipping the live directory read")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var file struct {
		Feishu config.Feishu `yaml:"feishu"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if !file.Feishu.Enabled || file.Feishu.AppID == "" {
		t.Skipf("%s has no enabled feishu block", path)
	}
	if file.Feishu.TimeoutS <= 0 {
		file.Feishu.TimeoutS = 10
	}

	got, err := New(file.Feishu).Directory(context.Background(), DirectoryOptions{})
	if err != nil {
		t.Fatalf("live directory read: %v", err)
	}
	t.Logf("departments=%d users=%d names_available=%v truncated=%v fetched_at=%s",
		len(got.Departments), len(got.Users), got.NamesAvailable, got.Truncated,
		got.FetchedAt.Format(time.RFC3339))
	if len(got.Users) == 0 {
		t.Fatal("the live tenant reported no people at all")
	}
	for _, department := range got.Departments[:min(3, len(got.Departments))] {
		t.Logf("dept depth=%d parent=%q id=%s name=%q", department.Depth, department.ParentID, department.ID, department.Name)
	}
	if !got.NamesAvailable {
		t.Log("names unavailable: add 「获取部门基础信息」/「获取用户基本信息」 and publish a new app version (docs/feishu.md §5c.1)")
	}
}
