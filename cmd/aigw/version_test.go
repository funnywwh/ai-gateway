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
		shape    string
		encoding string
		want     []string
	}{
		{"minified", "gzip", []string{"console minified", "transfer gzip"}},
		{"source", "identity", []string{"console source", "transfer identity"}},
	}
	for _, tc := range cases {
		t.Run(tc.shape+"-"+tc.encoding, func(t *testing.T) {
			restoreAssets, restoreEncoding := uiAssets, uiEncoding
			uiAssets, uiEncoding = tc.shape, tc.encoding
			t.Cleanup(func() { uiAssets, uiEncoding = restoreAssets, restoreEncoding })

			got := versionLine()
			for _, want := range append(tc.want, version, revision, date) {
				if !strings.Contains(got, want) {
					t.Errorf("versionLine() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

// TestVersionLineDefaultEncodingIsIdentity is the second half of "the default cannot
// overstate the binary": a hand build serves the assets it embedded, uncompressed, and
// saying "gzip" there would send whoever reads the release record looking for a feature
// that is not in the file.
func TestVersionLineDefaultEncodingIsIdentity(t *testing.T) {
	if uiEncoding != "identity" {
		t.Fatalf("uiEncoding default = %q, want %q: only `make build` may claim gzip", uiEncoding, "identity")
	}
	if got := versionLine(); !strings.Contains(got, "transfer identity") {
		t.Errorf("versionLine() = %q, want the unset default to read %q", got, "transfer identity")
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
