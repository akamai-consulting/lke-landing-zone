package main

import (
	"encoding/hex"
	"testing"
)

// TestSigV4SigningKey checks the HMAC derivation against AWS's documented example
// (docs "Examples of how to derive a signing key for Signature Version 4").
func TestSigV4SigningKey(t *testing.T) {
	key := sigV4SigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20150830", "us-east-1", "iam")
	got := hex.EncodeToString(key)
	want := "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"
	if got != want {
		t.Errorf("sigV4SigningKey = %s, want %s", got, want)
	}
}

func TestSha256Hex_EmptyPayload(t *testing.T) {
	// The well-known SHA256 of the empty string, used as the SigV4 payload hash.
	if got := sha256Hex(""); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("sha256Hex(\"\") = %s", got)
	}
}

func TestS3RegionAndHost(t *testing.T) {
	cases := []struct{ endpoint, host, region string }{
		{"https://us-ord-1.linodeobjects.com", "us-ord-1.linodeobjects.com", "us-ord-1"},
		{"https://us-ord-1.linodeobjects.com/", "us-ord-1.linodeobjects.com", "us-ord-1"},
		{"http://nl-ams-1.linodeobjects.com", "nl-ams-1.linodeobjects.com", "nl-ams-1"},
		{"https://minio.example.com", "minio.example.com", "minio"},
		{"weird", "weird", "us-east-1"},
	}
	for _, tc := range cases {
		if h := s3Host(tc.endpoint); h != tc.host {
			t.Errorf("s3Host(%q) = %q, want %q", tc.endpoint, h, tc.host)
		}
		if r := s3Region(tc.endpoint); r != tc.region {
			t.Errorf("s3Region(%q) = %q, want %q", tc.endpoint, r, tc.region)
		}
	}
}

func TestS3ErrorCode(t *testing.T) {
	body := `<?xml version="1.0"?><Error><Code>SignatureDoesNotMatch</Code><Message>...</Message></Error>`
	if got := s3ErrorCode(body); got != "SignatureDoesNotMatch" {
		t.Errorf("s3ErrorCode = %q", got)
	}
	if got := s3ErrorCode("no xml here"); got != "" {
		t.Errorf("s3ErrorCode(none) = %q, want empty", got)
	}
}

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
