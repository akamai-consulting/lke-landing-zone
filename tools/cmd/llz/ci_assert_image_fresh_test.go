package main

import (
	"strings"
	"testing"
)

func TestRunAssertImageFresh(t *testing.T) {
	const sha = "0d634d7d54a138314be21d0891c376fbae99519a"
	cases := []struct {
		name, baked, ref string
		wantErr          string // substring; "" means no error
	}{
		{"dev matches full sha", "dev-" + sha, sha, ""},
		{"dev matches short ref", "dev-" + sha, sha[:12], ""},
		{"dev short build matches full ref", "dev-" + sha[:12], sha, ""},
		{"dev sha mismatch", "dev-" + sha, "7ec07dc7929384cf393bbde98002d7089097e673", "image/template skew"},
		{"dev vs branch ref skips", "dev-" + sha, "main", ""},
		{"unstamped dev skips", "dev", sha, ""},
		{"empty version skips", "", sha, ""},
		{"release tag matches", "v1.2.3", "v1.2.3", ""},
		{"release tag mismatch", "v1.2.3", "v1.2.4", "image/template skew"},
		{"release vs sha skips", "v1.2.3", sha, ""},
		{"empty ref errors", "dev-" + sha, "", "--template-ref is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runAssertImageFresh(tc.baked, tc.ref)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("runAssertImageFresh(%q,%q) = %v, want nil", tc.baked, tc.ref, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runAssertImageFresh(%q,%q) = %v, want error containing %q", tc.baked, tc.ref, err, tc.wantErr)
			}
		})
	}
}

func TestAssertImageFreshCmdWiring(t *testing.T) {
	c := ciAssertImageFreshCmd()
	if c.Use != "assert-image-fresh --template-ref <ref>" {
		t.Errorf("Use = %q", c.Use)
	}
	if c.Flags().Lookup("template-ref") == nil {
		t.Error("missing --template-ref flag")
	}
}
