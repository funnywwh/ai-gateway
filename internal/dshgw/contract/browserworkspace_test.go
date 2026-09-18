package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
)

// Real DSH host plugin discovery and client asset publication. A temporary home
// and random loopback port isolate this from every existing user session. This
// checks the boot wire and HTTP assets, not browser UI activation or filesystem IO.
func TestBrowserWorkspacePluginRealDSH(t *testing.T) {
	if os.Getenv("DSHGW_BROWSER_DSH_TEST") != "1" {
		t.Skip("set DSHGW_BROWSER_DSH_TEST=1")
	}
	root, node := os.Getenv("DSHGW_DSH_ROOT"), os.Getenv("DSHGW_NODE")
	if root == "" || node == "" {
		t.Fatal("DSHGW_DSH_ROOT and DSHGW_NODE are required")
	}
	_, source, _, _ := runtime.Caller(0)
	plugin := filepath.Join(filepath.Dir(source), "..", "..", "..", "cmd", "dshgw", "plugin", "browser-workspace", "index.js")
	plugin, err := filepath.Abs(plugin)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	live, err := startDshWithSetup(ctx, config.DshRuntime{NodeBin: node, BinJS: filepath.Join(root, "lib", "bin.js")}, func(home, workspace string) error {
		// dsh web initializes/loads its web profile, not a patch at DSH_HOME's
		// root. Its initProfile preserves an existing profile patch file.
		profile := filepath.Join(home, "profiles", "web")
		if err := os.MkdirAll(profile, 0700); err != nil {
			return err
		}
		patch := fmt.Sprintf("- insert:\n    - id: browser-workspace\n      name: %q\n", (&url.URL{Scheme: "file", Path: plugin}).String())
		return os.WriteFile(filepath.Join(profile, "cordis.patch.yml"), []byte(patch), 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer live.close()
	origin, cookie, err := exchange(ctx, live.URL)
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(path string) (string, string) {
		t.Helper()
		ref, err := url.Parse(path)
		if err != nil || !strings.HasPrefix(path, "/") || ref == nil || ref.IsAbs() || ref.Host != "" {
			t.Fatalf("invalid same-origin advertised URL %q: %v", path, err)
		}
		u := origin.ResolveReference(ref)
		req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: HTTP %d: %s", path, resp.StatusCode, body)
		}
		return string(body), resp.Header.Get("Content-Type")
	}
	page, _ := fetch("/")
	// DSH injects JSON through globalThis["__DSH_BOOT__"]. Only bootstrap
	// modules have script-src tags: application/lazy modules are advertised in
	// entries[].url. Reading that graph proves the host loader discovered the
	// package's dsh.client declaration; a guessed asset URL would not prove it.
	match := regexp.MustCompile(`(?s)<script>\s*globalThis\["__DSH_BOOT__"\]\s*=\s*(.*?)\s*</script>`).FindStringSubmatch(page)
	if len(match) != 2 {
		t.Fatal("DSH homepage does not advertise a __DSH_BOOT__ manifest")
	}
	var boot struct {
		Entries []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
			Rev string `json:"rev"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimSpace(match[1]), ";")), &boot); err != nil {
		t.Fatalf("decode DSH boot manifest: %v", err)
	}
	const pluginID = "dshgw-browser-workspace"
	found := 0
	var ids []string
	for _, entry := range boot.Entries {
		ids = append(ids, entry.ID)
		if entry.ID != pluginID {
			continue
		}
		found++
		if !strings.HasPrefix(entry.URL, "/plugins/") || entry.Rev == "" {
			t.Fatalf("plugin has invalid advertised bundle URL/revision: %+v", entry)
		}
		body, contentType := fetch(entry.URL)
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || (mediaType != "text/javascript" && mediaType != "application/javascript") {
			t.Fatalf("advertised plugin URL %s served %q instead of JavaScript", entry.URL, contentType)
		}
		if !strings.Contains(body, "__ModuleLoader__.load") || !strings.Contains(body, pluginID) || !strings.Contains(body, "showDirectoryPicker") {
			t.Fatalf("advertised plugin URL %s did not serve the browser workspace ModuleLoader bundle", entry.URL)
		}
		t.Logf("DSH discovered %s and served its advertised bundle %s", pluginID, entry.URL)
	}
	if found != 1 {
		t.Fatalf("want exactly one %s in DSH boot entries, got %d; discovered: %v (check profiles/web/cordis.patch.yml and package dsh.client/exports)", pluginID, found, ids)
	}
}
