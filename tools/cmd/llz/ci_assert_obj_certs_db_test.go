package main

// Tests for the three coverage gates found in the post-PR functional pass:
// assert-obj-roundtrip, assert-certificates and assert-database.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/objenc"
)

// ── assert-obj-roundtrip ─────────────────────────────────────────────────────

func TestExplainS3Write(t *testing.T) {
	for _, tc := range []struct {
		code string
		want string
	}{
		{"AccessDenied", "not permitted to write"},
		{"InvalidAccessKeyId", "revoked or rotated"},
		{"SignatureDoesNotMatch", "revoked or rotated"},
	} {
		if got := objenc.ExplainS3Write(403, tc.code, "b", "e"); !strings.Contains(got, tc.want) {
			t.Errorf("%s: expected %q in %q", tc.code, tc.want, got)
		}
	}
}
