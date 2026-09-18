package config

import "testing"

func TestBrowserWorkspacesSwitch(t *testing.T) {
	for _, tc := range []struct {
		name, doc     string
		enabled, fail bool
	}{
		{"default", baseCfg, false, false},
		{"enabled", baseCfg + "browser_workspaces:\n  enabled: true\n", true, false},
		{"requires plugin", "directory_picker: browse\nbrowser_workspaces:\n  enabled: true\n", true, true},
		{"seed collision", baseCfg + "browser_workspaces:\n  enabled: true\nworkspace_seed: [browser]\n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tc.doc))
			if tc.fail {
				if err == nil {
					t.Fatal("accepted invalid browser config")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.BrowserWorkspaces.Enabled != tc.enabled {
				t.Fatal("wrong enabled value")
			}
		})
	}
}
