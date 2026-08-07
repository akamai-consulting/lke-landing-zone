package tokeninv

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

func TestClassifyS3(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		s3Code string
		want   tokenprobe.ValidityStatus
	}{
		{"ok", 200, "", tokenprobe.VValid},
		{"bucket gone but authed", 404, "NoSuchBucket", tokenprobe.VValid},
		{"authed, wrong scope", 403, "AccessDenied", tokenprobe.VWarn},
		{"bad key id", 403, "InvalidAccessKeyId", tokenprobe.VInvalid},
		{"bad signature", 403, "SignatureDoesNotMatch", tokenprobe.VInvalid},
		{"unreachable", 0, "", tokenprobe.VUnreachable},
		{"weird", 500, "InternalError", tokenprobe.VUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := classifyS3(tc.code, tc.s3Code); got != tc.want {
				t.Errorf("classifyS3(%d,%q) = %v, want %v", tc.code, tc.s3Code, got, tc.want)
			}
		})
	}
}

func TestProbeS3Pair_SkipsWithoutInputs(t *testing.T) {
	orig := S3BucketProbe
	t.Cleanup(func() { S3BucketProbe = orig })
	called := false
	S3BucketProbe = func(_, _, _, _ string) (int, string, error) { called = true; return 200, "", nil }

	if tv := ProbeS3Pair("", "", "https://x.linodeobjects.com", "b"); tv.Status != tokenprobe.VSkipped {
		t.Errorf("no keys: status %v, want tokenprobe.VSkipped", tv.Status)
	}
	if tv := ProbeS3Pair("ak", "sk", "", "b"); tv.Status != tokenprobe.VSkipped {
		t.Errorf("no endpoint: status %v, want tokenprobe.VSkipped", tv.Status)
	}
	if called {
		t.Error("S3BucketProbe should not run without full inputs")
	}

	// Full inputs → probe runs → classified.
	S3BucketProbe = func(_, _, _, _ string) (int, string, error) { return 403, "InvalidAccessKeyId", nil }
	if tv := ProbeS3Pair("ak", "sk", "https://x.linodeobjects.com", "b"); tv.Status != tokenprobe.VInvalid {
		t.Errorf("bad key: status %v, want tokenprobe.VInvalid", tv.Status)
	}
}
