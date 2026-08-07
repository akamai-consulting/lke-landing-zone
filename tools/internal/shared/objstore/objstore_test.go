package objstore

// Tests for the S3 wire layer, AND THE SECOND TIME THIS SET HAS BEEN FILED UNDER
// THE WRONG OWNER.
//
// The round-trip tests started in cmd/llz/ci_assert_obj_certs_db_test.go, a file
// named for three unrelated subjects. Extracting assert-objstore moved them to
// internal/extensions/objenc, whose header recorded the lesson: "the tests were
// filed by the command that happened to exercise the code, not by the code they
// test." That was right, and it was still one hop short -- the code they test was
// never objenc's either. It was the S3 wire layer that objenc happened to contain.
//
// TestSampleReturnsNewestFirst and TestCanonicalQuerySortsAndEncodesForSigning
// arrive from a different direction: each sat in a file (assert_test.go,
// proxy_resign_test.go) that is mostly about something which correctly stayed
// behind. A file is not a unit of ownership, and checking each Test individually
// rather than moving files wholesale is what separated these two from the sixteen
// beside them that genuinely test the re-signer and the encryption assertion.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// THE FIXTURE CAME WITH THEM, and needed no rewriting for once: seamObjS3 swaps
// S3ObjectRequest, which is this package's seam now. A helper that has to be
// reduced to a minimal local copy is the usual sign a test was moved further than
// the code it exercises; this one moving intact is the sign the split was cut in
// the right place.
// seamObjS3 replaces the signed HTTP layer with a fake object store.
type fakeObjStore struct {
	objects   map[string][]byte
	putStatus int
	putCode   string
	getStatus int
	// corrupt makes GET return different bytes than were written — the
	// wrong-endpoint signature.
	corrupt bool
	deletes int
}

func seamObjS3(t *testing.T, f *fakeObjStore) {
	orig := S3ObjectRequest
	t.Cleanup(func() { S3ObjectRequest = orig })
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	S3ObjectRequest = func(method, _, _, _, bucket, key string, payload []byte) (int, string, []byte, error) {
		full := bucket + "/" + key
		switch method {
		case http.MethodPut:
			if f.putStatus != 0 && f.putStatus/100 != 2 {
				return f.putStatus, f.putCode, nil, nil
			}
			f.objects[full] = payload
			return 200, "", nil, nil
		case http.MethodGet:
			if f.getStatus != 0 && f.getStatus/100 != 2 {
				return f.getStatus, "", nil, nil
			}
			b, ok := f.objects[full]
			if !ok {
				return 404, "NoSuchKey", nil, nil
			}
			if f.corrupt {
				return 200, "", []byte("different bytes entirely"), nil
			}
			return 200, "", b, nil
		case http.MethodDelete:
			f.deletes++
			delete(f.objects, full)
			return 204, "", nil, nil
		}
		return 400, "", nil, nil
	}
}

func TestS3ObjectRoundTrip(t *testing.T) {
	f := &fakeObjStore{}
	seamObjS3(t, f)
	r := S3ObjectRoundTrip("AK", "SK", "us-ord-10.linodeobjects.com", "b", "k", []byte("payload"))
	if !r.OK() || !r.Cleaned {
		t.Fatalf("a healthy bucket must round-trip and clean up: %+v", r)
	}
	if f.deletes != 1 {
		t.Errorf("the probe object must be deleted, got %d deletes", f.deletes)
	}
}

// A write that cannot be read back at the SAME endpoint is the disjoint-namespace
// failure, and it is the reason the gate reads back at all.
// A write that cannot be read back at the SAME endpoint is the disjoint-namespace
// failure, and it is the reason the gate reads back at all.
func TestS3ObjectRoundTripFailsWhenReadBackDiffers(t *testing.T) {
	f := &fakeObjStore{corrupt: true}
	seamObjS3(t, f)
	r := S3ObjectRoundTrip("AK", "SK", "ep", "b", "k", []byte("payload"))
	if r.OK() {
		t.Fatal("bytes that come back different must fail — a PUT can succeed against the wrong endpoint")
	}
	if !r.Wrote || r.ReadBack {
		t.Errorf("the verdict should record the write succeeded and the read did not: %+v", r)
	}
}

func TestS3ObjectRoundTripNoSuchBucketExplainsTheSplit(t *testing.T) {
	seamObjS3(t, &fakeObjStore{putStatus: 404, putCode: "NoSuchBucket"})
	r := S3ObjectRoundTrip("AK", "SK", "us-ord.linodeobjects.com", "llz-loki-chunks", "k", []byte("x"))
	if r.OK() {
		t.Fatal("NoSuchBucket must fail")
	}
	// The failure has to name the obj-cluster split, or the operator goes looking
	// at the Linode console, sees the bucket, and concludes the gate is wrong.
	if !strings.Contains(r.FailWhy, "gen-1") || !strings.Contains(r.FailWhy, "by label") {
		t.Errorf("the failure must explain that the bucket may exist at a different endpoint, got %q", r.FailWhy)
	}
}

// SigV4 signs query parameters SORTED and RFC3986-encoded. A multipart PUT carries
// ?partNumber=N&uploadId=…, and signing them in arrival order yields a signature the
// upstream cannot reproduce — so multipart uploads would 403 while single-part
// writes worked, which reads as flaky object storage rather than as a bug here.
func TestCanonicalQuerySortsAndEncodesForSigning(t *testing.T) {
	if got := S3CanonicalQuery("uploadId=ZZZ&partNumber=1"); got != "partNumber=1&uploadId=ZZZ" {
		t.Errorf("canonical query = %q, want the SORTED form", got)
	}
	// A space is %20 in SigV4, not the '+' net/url's Encode would produce.
	if got := S3CanonicalQuery("k=a b"); got != "k=a%20b" {
		t.Errorf("space encoded as %q, want %%20 — '+' is a different byte to the signer", got)
	}
	if got := S3CanonicalQuery("flag&b=2"); got != "b=2&flag=" {
		t.Errorf("valueless key rendered %q, want `flag=`", got)
	}
	if got := S3CanonicalQuery(""); got != "" {
		t.Errorf("empty query = %q", got)
	}
}

// Plain LIST order is lexicographic, so on a bucket with history the whole sample is
// drawn from the OLDEST keys — every one of them pre-cutover, and the check then has
// nothing to judge no matter how much fresh data exists. Newest-first is what makes
// the sample relevant.
func TestSampleReturnsNewestFirst(t *testing.T) {
	base := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	body := ""
	for i, name := range []string{"aaa-oldest", "mmm-middle", "zzz-newest"} {
		body += fmt.Sprintf("<Contents><Key>%s</Key><LastModified>%s</LastModified></Contents>",
			name, base.Add(time.Duration(i)*time.Hour).Format(time.RFC3339))
	}
	prev := S3SignedRequest
	S3SignedRequest = func(_, _, _, _, _, _ string) (int, string, error) { return 200, body, nil }
	t.Cleanup(func() { S3SignedRequest = prev })

	got, err := SampleObjectKeys("ak", "sk", "e", "b", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "zzz-newest" || got[1].Key != "mmm-middle" {
		t.Errorf("sample = %+v, want the two NEWEST — lexicographic order samples only the oldest keys, "+
			"which on a reused bucket are all pre-cutover and unjudgeable", got)
	}
}

// S3EscapePath came across at 0%, which is how the bug its own comment describes
// got shipped once already: concatenating the raw path is correct for exactly as
// long as every key is hex and slashes, and the failure it produces is a SIGNATURE
// mismatch reported as "could not classify" by an encryption gate. The checker
// would be wrong and the finding would point at the proxy.
func TestS3EscapePath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain/key.txt", "plain/key.txt"},
		{"a-b_c.d~e", "a-b_c.d~e"},
		// Slashes stay literal: they are path separators in the canonical request,
		// not data.
		{"/bucket/key", "/bucket/key"},
		{"has space", "has%20space"},
		// Go's url escaping leaves these alone in paths; SigV4 requires them encoded,
		// which is the whole reason this is hand-rolled.
		{"$&+,;=:@", "%24%26%2B%2C%3B%3D%3A%40"},
		{"pct%sign", "pct%25sign"},
		{"plus+key", "plus%2Bkey"},
		{"", ""},
	} {
		if got := S3EscapePath(tc.in); got != tc.want {
			t.Errorf("S3EscapePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ExplainS3Write is the remediation text an operator reads at 2am. The NoSuchBucket
// case is the one that earns the table: it is the obj-cluster generation split,
// where a bucket census by label passes while every write fails, because the census
// asks the Linode API and the write asks this endpoint.
func TestExplainS3Write(t *testing.T) {
	for _, tc := range []struct {
		name, s3Code, want string
		code               int
	}{
		{name: "no such bucket names the endpoint split", s3Code: "NoSuchBucket", want: "obj-cluster split"},
		{name: "access denied points at per-bucket scope", s3Code: "AccessDenied", want: "scoped per bucket"},
		{name: "bad key reads as rotated-out", s3Code: "InvalidAccessKeyId", want: "revoked or rotated"},
		{name: "signature mismatch is the same cause", s3Code: "SignatureDoesNotMatch", want: "revoked or rotated"},
		{name: "301 with no code is a region redirect", code: 301, want: "different region/generation"},
		{name: "307 likewise", code: 307, want: "different region/generation"},
		{name: "unknown falls back to the checklist", code: 500, want: "check the bucket"},
		{name: "unrecognised s3 code falls back too", s3Code: "TeapotError", want: "check the bucket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExplainS3Write(tc.code, tc.s3Code, "llz-loki", "https://us-ord-10.linodeobjects.com")
			if !strings.Contains(got, tc.want) {
				t.Errorf("ExplainS3Write(%d, %q) = %q\n  want it to mention %q", tc.code, tc.s3Code, got, tc.want)
			}
		})
	}
	// The bucket and endpoint are interpolated, not described — an explanation that
	// cannot name which bucket at which host is not actionable.
	got := ExplainS3Write(404, "NoSuchBucket", "llz-loki", "https://us-ord-10.linodeobjects.com")
	for _, want := range []string{"llz-loki", "us-ord-10"} {
		if !strings.Contains(got, want) {
			t.Errorf("NoSuchBucket explanation omits %q: %s", want, got)
		}
	}
}

func TestS3CodeSuffix(t *testing.T) {
	if got := s3CodeSuffix(""); got != "" {
		t.Errorf("empty code must add nothing, got %q", got)
	}
	if got := s3CodeSuffix("NoSuchKey"); got != " (NoSuchKey)" {
		t.Errorf("s3CodeSuffix = %q", got)
	}
}
