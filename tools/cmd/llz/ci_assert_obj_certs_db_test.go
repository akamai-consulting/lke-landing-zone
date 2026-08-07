package main

// Tests for the three coverage gates found in the post-PR functional pass:
// assert-obj-roundtrip, assert-certificates and assert-database.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/objenc"
)

// ── assert-obj-roundtrip ─────────────────────────────────────────────────────

func objSecretJSON(access, secret string) []byte {
	b, _ := json.Marshal(map[string]any{"data": map[string]string{
		"AWS_ACCESS_KEY_ID":     base64.StdEncoding.EncodeToString([]byte(access)),
		"AWS_SECRET_ACCESS_KEY": base64.StdEncoding.EncodeToString([]byte(secret)),
	}})
	return b
}

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

func certListJSON(items ...string) []byte {
	return []byte(`{"items":[` + strings.Join(items, ",") + `]}`)
}

func certItemJSON(ns, name, ready, notAfter, reason string) string {
	return `{"metadata":{"name":"` + name + `","namespace":"` + ns + `"},
	  "status":{"notAfter":"` + notAfter + `","conditions":[{"type":"Ready","status":"` + ready + `","reason":"` + reason + `","message":"m"}]}}`
}
