package main

import (
	"testing"
)

func TestClassifyS3(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		s3Code string
		want   validityStatus
	}{
		{"ok", 200, "", vValid},
		{"bucket gone but authed", 404, "NoSuchBucket", vValid},
		{"authed, wrong scope", 403, "AccessDenied", vWarn},
		{"bad key id", 403, "InvalidAccessKeyId", vInvalid},
		{"bad signature", 403, "SignatureDoesNotMatch", vInvalid},
		{"unreachable", 0, "", vUnreachable},
		{"weird", 500, "InternalError", vUnreachable},
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
	orig := s3BucketProbe
	t.Cleanup(func() { s3BucketProbe = orig })
	called := false
	s3BucketProbe = func(_, _, _, _ string) (int, string, error) { called = true; return 200, "", nil }

	if tv := probeS3Pair("", "", "https://x.linodeobjects.com", "b"); tv.status != vSkipped {
		t.Errorf("no keys: status %v, want vSkipped", tv.status)
	}
	if tv := probeS3Pair("ak", "sk", "", "b"); tv.status != vSkipped {
		t.Errorf("no endpoint: status %v, want vSkipped", tv.status)
	}
	if called {
		t.Error("s3BucketProbe should not run without full inputs")
	}

	// Full inputs → probe runs → classified.
	s3BucketProbe = func(_, _, _, _ string) (int, string, error) { return 403, "InvalidAccessKeyId", nil }
	if tv := probeS3Pair("ak", "sk", "https://x.linodeobjects.com", "b"); tv.status != vInvalid {
		t.Errorf("bad key: status %v, want vInvalid", tv.status)
	}
}
