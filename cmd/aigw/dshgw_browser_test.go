package main

import "testing"

func TestBuildDshgwChildBrowserSwitch(t *testing.T) {
	cfg, binary := childFixture(t)
	for _, enabled := range []bool{false, true, false} {
		cfg.Dshgw.BrowserWorkspaces.Enabled = enabled
		child, err := buildDshgwChild(cfg, binary)
		if err != nil {
			t.Fatal(err)
		}
		browser := child.config.BrowserWorkspaces
		if enabled && (browser == nil || !browser.Enabled) {
			t.Fatal("enabled browser config not passed to child")
		}
		if !enabled && browser != nil {
			t.Fatal("disabled block should be omitted")
		}
	}
}
