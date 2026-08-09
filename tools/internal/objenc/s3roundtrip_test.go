package objenc

// s3roundtrip_test.go — tests for S3ObjectRoundTrip, which is defined HERE.
//
// THEY WERE NEVER PACKAGE MAIN'S, and neither were they the assertion's. They
// lived in cmd/llz/ci_assert_obj_certs_db_test.go — a file named for three
// unrelated subjects — and drive objenc.S3ObjectRoundTrip through the
// S3ObjectRequest seam this package owns. Extracting assert-objstore surfaced them
// because the shared fixture moved; the FIRST classification put them with the
// assertion, which was wrong for the same reason the original filename was: the
// tests were filed by the command that happened to exercise the code, not by the
// code they test.
//
// Fifth stranded-test find, and the first where the answer was a THIRD package —
// neither the one they came from nor the one being extracted.

import (
	"net/http"
	"strings"
	"testing"
)

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
