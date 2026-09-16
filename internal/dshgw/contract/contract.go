package contract

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
)

type Check struct {
	Name     string
	Required bool
	Timeout  time.Duration
	Run      func(context.Context) error
}
type Result struct {
	Name     string        `json:"name"`
	Required bool          `json:"required"`
	OK       bool          `json:"ok"`
	Skipped  bool          `json:"skipped,omitempty"`
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
}
type Report struct {
	OK      bool     `json:"ok"`
	Results []Result `json:"results"`
}

var ErrSkipped = errors.New("contract check skipped")

func Run(ctx context.Context, checks []Check) Report {
	report := Report{OK: true}
	for _, check := range checks {
		started := time.Now()
		timeout := check.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		checkCtx, cancel := context.WithTimeout(ctx, timeout)
		err := check.Run(checkCtx)
		cancel()
		result := Result{Name: check.Name, Required: check.Required, OK: err == nil, Skipped: errors.Is(err, ErrSkipped), Duration: time.Since(started)}
		if err != nil && !result.Skipped {
			result.Error = err.Error()
		}
		if err != nil && check.Required {
			report.OK = false
		}
		report.Results = append(report.Results, result)
	}
	return report
}

var startupRE = regexp.MustCompile(`^dsh web: (http://127\.0\.0\.1:[0-9]+/\?token=[A-Za-z0-9_-]{43})(?: \(LAN: .+\))?$`)

type liveDsh struct {
	cmd       *exec.Cmd
	wait      <-chan error
	URL       string
	Home      string
	Workspace string
}

func startDshWithSetup(ctx context.Context, rt config.DshRuntime, setup func(string, string) error) (*liveDsh, error) {
	if rt.NodeBin == "" || rt.BinJS == "" {
		return nil, errors.New("dsh node_bin and bin_js are required")
	}
	home, err := os.MkdirTemp("", "dshgw-contract-home-")
	if err != nil {
		return nil, err
	}
	workspace, err := os.MkdirTemp("", "dshgw-contract-work-")
	if err != nil {
		_ = os.RemoveAll(home)
		return nil, err
	}
	adopted := false
	defer func() {
		if !adopted {
			_ = os.RemoveAll(home)
			_ = os.RemoveAll(workspace)
		}
	}()
	if setup != nil {
		if err := setup(home, workspace); err != nil {
			_ = os.RemoveAll(home)
			_ = os.RemoveAll(workspace)
			return nil, err
		}
	}
	cmd := exec.CommandContext(ctx, rt.NodeBin, rt.BinJS, "web", "--port", "0", "--no-open")
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "HOME="+workspace, "DSH_HOME="+home)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	lines := make(chan string, 1)
	read := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			if startupRE.MatchString(line) {
				select {
				case lines <- line:
				default:
				}
			}
		}
	}
	go read(stdout)
	go read(stderr)
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait(); close(wait) }()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-lines:
			match := startupRE.FindStringSubmatch(line)
			if len(match) == 2 {
				adopted = true
				return &liveDsh{cmd: cmd, wait: wait, URL: match[1], Home: home, Workspace: workspace}, nil
			}
		case err := <-wait:
			_ = os.RemoveAll(home)
			_ = os.RemoveAll(workspace)
			return nil, fmt.Errorf("dsh exited before startup URL: %v", err)
		case <-timer.C:
			_ = cmd.Process.Kill()
			<-wait
			return nil, errors.New("timed out waiting for dsh startup URL")
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-wait
			return nil, ctx.Err()
		}
	}
}
func startDsh(ctx context.Context, rt config.DshRuntime) (*liveDsh, error) {
	return startDshWithSetup(ctx, rt, nil)
}

func (l *liveDsh) close() {
	if l == nil {
		return
	}
	_ = l.cmd.Process.Signal(os.Interrupt)
	select {
	case <-l.wait:
	case <-time.After(2 * time.Second):
		_ = l.cmd.Process.Kill()
		<-l.wait
	}
	_ = os.RemoveAll(l.Home)
	_ = os.RemoveAll(l.Workspace)
}
func exchange(ctx context.Context, raw string) (*url.URL, *http.Cookie, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, nil, err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Host = u.Host
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, errors.New("dsh contract token exchange request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		return nil, nil, fmt.Errorf("token exchange status %d, want 303", resp.StatusCode)
	}
	var auth []*http.Cookie
	for _, cookie := range resp.Cookies() {
		if strings.HasPrefix(cookie.Name, "dsh-auth-") && cookie.Value != "" {
			auth = append(auth, cookie)
		}
	}
	if len(auth) != 1 {
		return nil, nil, fmt.Errorf("token exchange returned %d dsh auth cookies", len(auth))
	}
	return u, auth[0], nil
}

func baseDshChecks(rt config.DshRuntime) []Check {
	return []Check{
		{Name: "cli-version-and-web-flags", Required: true, Timeout: 15 * time.Second, Run: func(ctx context.Context) error {
			for _, args := range [][]string{{rt.BinJS, "--version"}, {rt.BinJS, "web", "--help"}} {
				out, err := exec.CommandContext(ctx, rt.NodeBin, args...).CombinedOutput()
				if err != nil {
					return fmt.Errorf("dsh CLI %v: %w: %s", args[1:], err, out)
				}
				if len(out) == 0 {
					return errors.New("dsh CLI returned empty output")
				}
			}
			return nil
		}},
		{Name: "exact-startup-url", Required: true, Timeout: 40 * time.Second, Run: func(ctx context.Context) error {
			live, err := startDsh(ctx, rt)
			if err != nil {
				return err
			}
			defer live.close()
			if !startupRE.MatchString("dsh web: " + live.URL) {
				return fmt.Errorf("unexpected URL %q", live.URL)
			}
			return nil
		}},
		{Name: "token-exchange-cookie-authority", Required: true, Timeout: 40 * time.Second, Run: func(ctx context.Context) error {
			live, err := startDsh(ctx, rt)
			if err != nil {
				return err
			}
			defer live.close()
			_, _, err = exchange(ctx, live.URL)
			return err
		}},
		{Name: "host-origin-fence", Required: true, Timeout: 40 * time.Second, Run: func(ctx context.Context) error {
			live, err := startDsh(ctx, rt)
			if err != nil {
				return err
			}
			defer live.close()
			u, cookie, err := exchange(ctx, live.URL)
			if err != nil {
				return err
			}
			client := &http.Client{}
			request := func(host, origin string) (int, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+u.Host+"/api", nil)
				if err != nil {
					return 0, err
				}
				req.Host = host
				if origin != "" {
					req.Header.Set("Origin", origin)
				}
				req.AddCookie(cookie)
				resp, err := client.Do(req)
				if err != nil {
					return 0, err
				}
				defer resp.Body.Close()
				return resp.StatusCode, nil
			}
			status, err := request(u.Host, "")
			if err != nil {
				return err
			}
			if status == http.StatusForbidden || status == http.StatusUnauthorized {
				return fmt.Errorf("valid Host with auth cookie returned %d", status)
			}
			status, err = request("example.invalid", "")
			if err != nil {
				return err
			}
			if status != http.StatusForbidden {
				return fmt.Errorf("external Host returned %d, want 403", status)
			}
			status, err = request(u.Host, "https://example.invalid")
			if err != nil {
				return err
			}
			if status != http.StatusForbidden {
				return fmt.Errorf("external Origin returned %d, want 403", status)
			}
			return nil
		}},
		{Name: "fresh-home-bootstrap", Required: true, Timeout: 40 * time.Second, Run: func(ctx context.Context) error {
			live, err := startDsh(ctx, rt)
			if err != nil {
				return err
			}
			defer live.close()
			for _, path := range []string{"profiles/web/package.json", "profiles/web/cordis.yml"} {
				if _, err := os.Stat(filepath.Join(live.Home, path)); err != nil {
					return fmt.Errorf("bootstrap missing %s: %w", path, err)
				}
			}
			return nil
		}},
		{Name: "credentials-schema-and-mode", Required: true, Timeout: 40 * time.Second, Run: func(ctx context.Context) error {
			live, err := startDshWithSetup(ctx, rt, func(home, _ string) error {
				credentials := []byte("version: 1\nrefs:\n  AIGW_API_KEY: sk-contract-dummy\nrecords: {}\n")
				settings := []byte("llm-pi-ai:\n  providers:\n    aigw:\n      apiKeyEnv: AIGW_API_KEY\n      api: openai-responses\n      baseURL: http://127.0.0.1:1/v1\n      models:\n        - id: contract-model\n          name: contract-model\n")
				if err := os.WriteFile(filepath.Join(home, ".credentials.yaml"), credentials, 0o600); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(home, "settings.yaml"), settings, 0o600)
			})
			if err != nil {
				return err
			}
			defer live.close()
			path := filepath.Join(live.Home, ".credentials.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			if info.Mode().Perm() != 0o600 {
				return fmt.Errorf("credentials mode %o, want 600", info.Mode().Perm())
			}
			var root map[string]any
			if err := yaml.Unmarshal(data, &root); err != nil {
				return err
			}
			keys := make([]string, 0, len(root))
			for key := range root {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			joined := strings.Join(keys, ",")
			if joined != "records,refs,version" {
				return fmt.Errorf("credentials keys are %v", keys)
			}
			return nil
		}},
		{Name: "sandbox-unavailable-fails-closed", Required: true, Timeout: 15 * time.Second, Run: func(ctx context.Context) error {
			root := filepath.Dir(filepath.Dir(rt.BinJS))
			script := `import {SANDBOX_UNAVAILABLE,SandboxUnavailableError} from ` + fmt.Sprintf("%q", filepath.ToSlash(filepath.Join(root, "node_modules/@deepseek-ai/dsh-sandbox/lib/index.js"))) + `;const e=new SandboxUnavailableError('workspace-write');if(e.code!==SANDBOX_UNAVAILABLE||!e.message.includes('refusing to run the command unconfined'))process.exit(1)`
			out, err := exec.CommandContext(ctx, rt.NodeBin, "--input-type=module", "-e", script).CombinedOutput()
			if err != nil {
				return fmt.Errorf("sandbox fail-closed contract: %w: %s", err, out)
			}
			return nil
		}},
	}
}

func AigwChecks(baseURL, key string) []Check {
	checks := []Check{{Name: "aigw-models-auth-fence", Required: true, Timeout: 10 * time.Second, Run: func(ctx context.Context) error {
		u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/v1/models")
		if err != nil {
			return err
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			return fmt.Errorf("unauthenticated /v1/models returned %d, want 401", resp.StatusCode)
		}
		return nil
	}}}
	if key != "" {
		checks = append(checks, Check{Name: "aigw-key-model-list", Required: true, Timeout: 10 * time.Second, Run: func(ctx context.Context) error {
			client := &aigw.Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 10 * time.Second}}
			_, err := client.ValidateKey(ctx, key)
			return err
		}})
		checks = append(checks, Check{Name: "aigw-key-header-equivalence", Required: true, Timeout: 20 * time.Second, Run: func(ctx context.Context) error {
			return checkAPIKeyHeader(ctx, baseURL, key)
		}})
	}
	return checks
}
