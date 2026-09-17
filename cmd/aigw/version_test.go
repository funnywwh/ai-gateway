package main

import (
	"strings"
	"testing"
)

// TestVersionLineNamesTheConsoleShape pins the one line scripts/release.sh copies into a
// release record. "Which shape of console assets is in this binary?" used to be answerable
// only by `strings bin/aigw | grep -c renderShell`, so the answer is now part of the
// build identity that gets written down — which makes this line a contract, not decoration.
func TestVersionLineNamesTheConsoleShape(t *testing.T) {
	cases := []struct {
		shape string
		want  string
	}{
		{"minified", "console minified"},
		{"source", "console source"},
	}
	for _, tc := range cases {
		t.Run(tc.shape, func(t *testing.T) {
			restore := uiAssets
			uiAssets = tc.shape
			t.Cleanup(func() { uiAssets = restore })

			got := versionLine()
			if !strings.Contains(got, tc.want) {
				t.Errorf("versionLine() = %q, want it to contain %q", got, tc.want)
			}
			for _, part := range []string{version, revision, date} {
				if !strings.Contains(got, part) {
					t.Errorf("versionLine() = %q, want it to contain %q", got, part)
				}
			}
		})
	}
}

// TestVersionLineDefaultShapeIsTheLoudSide keeps the default on the side that cannot lie
// about a release: `make build` sets uiAssets next to the -overlay flag that creates
// the minified mirror, so a hand build (which is what the default describes) must report
// "source". The reverse mistake — claiming minified while carrying readable code — is the
// one an operator would never catch.
func TestVersionLineDefaultShapeIsTheLoudSide(t *testing.T) {
	if uiAssets != "source" {
		t.Fatalf("uiAssets default = %q, want %q: only `make build` may claim minified", uiAssets, "source")
	}
	if got := versionLine(); !strings.Contains(got, "console source") {
		t.Errorf("versionLine() = %q, want the unset default to read %q", got, "console source")
	}
}
