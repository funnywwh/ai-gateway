package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// Config remains a leaf package, so its YAML DTO validates independently of
// domain. Pin their accepted values together to prevent future schema drift.
func TestBootstrapReasoningValidationMatchesDomain(t *testing.T) {
	for _, mode := range []string{"", "default", "force", "inherit", "invalid"} {
		for _, effort := range []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max", "invalid"} {
			configErr := (BootstrapModelReasoning{Mode: mode, Effort: effort}).Validate()
			domainErr := (domain.ModelReasoning{Mode: mode, Effort: effort}).Validate()
			if (configErr == nil) != (domainErr == nil) {
				t.Fatalf("validation drift for %q/%q: config=%v domain=%v", mode, effort, configErr, domainErr)
			}
		}
	}
}

func TestBootstrapModelReasoningLoadsFromYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `bootstrap:
  models:
    - public_name: test-model
      reasoning:
        mode: force
        effort: high
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Bootstrap.Models[0].Reasoning
	if got == nil || got.Mode != "force" || got.Effort != "high" {
		t.Fatalf("reasoning = %+v", got)
	}
}

func TestBootstrapModelReasoningRejectsInvalidYAML(t *testing.T) {
	cases := []struct {
		name      string
		reasoning string
	}{
		{"unknown key", "mode: force\n        effort: high\n        typo: ignored"},
		{"bad mode", "mode: inherit\n        effort: high"},
		{"bad effort", "mode: force\n        effort: extreme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "bootstrap:\n  models:\n    - public_name: test-model\n      reasoning:\n        " + tc.reasoning + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load accepted invalid bootstrap reasoning")
			}
		})
	}
}

// TestBootstrapAccountNameValidationMatchesDomain pins the second restated rule: the
// configuration DTO trims and measures an account name exactly like domain.NormalizeAccountName,
// so YAML cannot accept a name the store would later rewrite or refuse.
func TestBootstrapAccountNameValidationMatchesDomain(t *testing.T) {
	cases := []string{
		"internal", "ops@example.com", "北京研发", "  ops@example.com  ", "\u3000运维组\u3000",
		"客户 A 组 🚀", "acme\xff", "\xc3\x28",
		strings.Repeat("a", 64), strings.Repeat("中", 64),
		strings.Repeat("a", 65), strings.Repeat("中", 65),
		"", "   ", "\u3000 \t\n",
	}
	for _, name := range cases {
		configErr := validateBootstrapAccountName(name)
		_, domainErr := domain.NormalizeAccountName(name)
		if (configErr == nil) != (domainErr == nil) {
			t.Errorf("account name validation drift for %q: config=%v domain=%v", name, configErr, domainErr)
		}
	}
	if MaxBootstrapAccountNameRunes != domain.MaxAccountNameRunes {
		t.Errorf("rune limits drifted apart: config=%d domain=%d",
			MaxBootstrapAccountNameRunes, domain.MaxAccountNameRunes)
	}
}
