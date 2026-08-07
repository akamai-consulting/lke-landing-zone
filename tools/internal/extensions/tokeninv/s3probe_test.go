package tokeninv

import (
	"testing"
)

func TestClassifyS3(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		s3Code string
		want   ValidityStatus
	}{
		{"ok", 200, "", VValid},
		{"bucket gone but authed", 404, "NoSuchBucket", VValid},
		{"authed, wrong scope", 403, "AccessDenied", VWarn},
		{"bad key id", 403, "InvalidAccessKeyId", VInvalid},
		{"bad signature", 403, "SignatureDoesNotMatch", VInvalid},
		{"unreachable", 0, "", VUnreachable},
		{"weird", 500, "InternalError", VUnreachable},
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

	if tv := ProbeS3Pair("", "", "https://x.linodeobjects.com", "b"); tv.Status != VSkipped {
		t.Errorf("no keys: status %v, want VSkipped", tv.Status)
	}
	if tv := ProbeS3Pair("ak", "sk", "", "b"); tv.Status != VSkipped {
		t.Errorf("no endpoint: status %v, want VSkipped", tv.Status)
	}
	if called {
		t.Error("S3BucketProbe should not run without full inputs")
	}

	// Full inputs → probe runs → classified.
	S3BucketProbe = func(_, _, _, _ string) (int, string, error) { return 403, "InvalidAccessKeyId", nil }
	if tv := ProbeS3Pair("ak", "sk", "https://x.linodeobjects.com", "b"); tv.Status != VInvalid {
		t.Errorf("bad key: status %v, want VInvalid", tv.Status)
	}
}
