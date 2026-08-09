package s3sig

// s3sig_test.go — moved with the signing chain. Region parsing is the case worth
// keeping close: a wrong region produces a signature the server rejects with a
// message about credentials, which reads as "wrong key" and is not.

import (
	"encoding/hex"
	"testing"
)

func TestS3ErrorCode(t *testing.T) {
	body := `<?xml version="1.0"?><Error><Code>SignatureDoesNotMatch</Code><Message>...</Message></Error>`
	if got := ErrorCode(body); got != "SignatureDoesNotMatch" {
		t.Errorf("s3ErrorCode = %q", got)
	}
	if got := ErrorCode("no xml here"); got != "" {
		t.Errorf("ErrorCode(none) = %q, want empty", got)
	}
}

// TestSigV4SigningKey checks the HMAC derivation against AWS's documented example
// (docs "Examples of how to derive a signing key for Signature Version 4").
func TestSigV4SigningKey(t *testing.T) {
	key := SigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20150830", "us-east-1", "iam")
	got := hex.EncodeToString(key)
	want := "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"
	if got != want {
		t.Errorf("sigV4SigningKey = %s, want %s", got, want)
	}
}

func TestSha256Hex_EmptyPayload(t *testing.T) {
	// The well-known SHA256 of the empty string, used as the SigV4 payload hash.
	if got := SHA256Hex(""); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("SHA256Hex(\"\") = %s", got)
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
		if h := Host(tc.endpoint); h != tc.host {
			t.Errorf("Host(%q) = %q, want %q", tc.endpoint, h, tc.host)
		}
		if r := Region(tc.endpoint); r != tc.region {
			t.Errorf("Region(%q) = %q, want %q", tc.endpoint, r, tc.region)
		}
	}
}
