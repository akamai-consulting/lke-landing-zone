package main

import (
	"net/url"
	"strings"
	"testing"
)

// SigV4's canonical path is not Go's escaped path. Go leaves $&+,;=:@ unescaped in
// paths; SigV4 requires everything outside the unreserved set encoded, with `/`
// preserved. A mismatch makes the signature fail and this probe report "could not
// classify" — an encryption verdict caused by a bug in the checker.
func TestS3EscapePath(t *testing.T) {
	for in, want := range map[string]string{
		"/bucket/plain.txt":           "/bucket/plain.txt",
		"/b/docker/registry/v2/blobs": "/b/docker/registry/v2/blobs",
		"/b/has space":                "/b/has%20space",
		"/b/plus+sign":                "/b/plus%2Bsign",
		"/b/amp&and=eq":               "/b/amp%26and%3Deq",
		"/b/colon:sep":                "/b/colon%3Asep",
		"/b/tilde~dash-dot._ok":       "/b/tilde~dash-dot._ok",
		"/b/pct%20already":            "/b/pct%2520already",
	} {
		if got := s3EscapePath(in); got != want {
			t.Errorf("s3EscapePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Slashes must survive as separators or every key becomes one flat segment and the
// canonical request describes a different object than the one requested.
func TestS3EscapePathPreservesSeparators(t *testing.T) {
	got := s3EscapePath("/bucket/a/b/c")
	if strings.Count(got, "/") != 4 {
		t.Errorf("separators were escaped: %q", got)
	}
}

// The escaped form must be a valid encoding of the raw path, or net/url silently
// discards RawPath and re-escapes by its own rules — sending bytes that were never
// signed. This is the invariant the fix depends on.
func TestS3EscapePathRoundTripsThroughURL(t *testing.T) {
	for _, raw := range []string{"/b/plain", "/b/has space", "/b/plus+sign", "/b/colon:sep"} {
		u := &url.URL{Scheme: "https", Host: "h"}
		u.Path = raw
		u.RawPath = s3EscapePath(raw)
		if got := u.EscapedPath(); got != u.RawPath {
			t.Errorf("net/url rejected RawPath for %q: emitted %q, signed %q — the wire and the "+
				"signature would disagree", raw, got, u.RawPath)
		}
	}
}
