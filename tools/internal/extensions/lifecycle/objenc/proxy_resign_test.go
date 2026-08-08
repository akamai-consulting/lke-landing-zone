package objenc

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/s3sig"
)

const testAKID = "AKIAEXAMPLEKEYID"

func testCreds() objProxyCreds {
	return objProxyCreds{AccessKeyID: testAKID, SecretAccessKey: "s3cr3t-example-key"}
}

// signAsClient signs the request the way a real S3 client would, so the fixture
// carries a signature the proxy can VERIFY. Before verification existed these tests
// used Signature=deadbeef and passed — which is precisely the hole: naming the
// access key id was accepted as authorization.
func signAsClient(t *testing.T, r *http.Request, c objProxyCreds, host string) {
	t.Helper()
	const dateStamp, region = "20260803", "us-ord"
	amzDate := dateStamp + "T210000Z"
	r.Header.Set("X-Amz-Date", amzDate)
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonical := "host:" + host + "\n" +
		"x-amz-content-sha256:" + r.Header.Get("X-Amz-Content-Sha256") + "\n" +
		"x-amz-date:" + amzDate + "\n"
	sh := strings.Join(signed, ";")
	cr := strings.Join([]string{
		r.Method, objstore.S3EscapePath(r.URL.Path), objstore.S3CanonicalQuery(r.URL.RawQuery), canonical, sh,
		r.Header.Get("X-Amz-Content-Sha256"),
	}, "\n")
	scope := dateStamp + "/" + region + "/s3/aws4_request"
	sts := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, s3sig.SHA256Hex(cr)}, "\n")
	sig := hex.EncodeToString(s3sig.HMACSHA256(s3sig.SigningKey(c.SecretAccessKey, dateStamp, region, "s3"), sts))
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKeyID+"/"+scope+
		", SignedHeaders="+sh+", Signature="+sig)
}

// awsChunked builds the framing the SDK emits: one data chunk, a terminal chunk, and
// a CRC32 trailer.
func awsChunked(payload string) string {
	return fmt.Sprintf("%x\r\n%s\r\n0\r\nx-amz-checksum-crc32:115vKg==\r\n\r\n", len(payload), payload)
}

func trailerRequest(t *testing.T, payload string) *http.Request {
	t.Helper()
	body := awsChunked(payload)
	r := httptest.NewRequest(http.MethodPut, "https://us-ord-10.linodeobjects.com/bucket/key", strings.NewReader(body))
	r.Header.Set("X-Amz-Content-Sha256", sha256StreamingUnsignedTrailer)
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(payload)))
	r.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	r.Header.Set("X-Amz-Sdk-Checksum-Algorithm", "CRC32")
	r.Header.Set("X-Amz-Date", "20260803T210000Z")
	r.ContentLength = int64(len(body))
	signAsClient(t, r, testCreds(), "us-ord-10.linodeobjects.com")
	return r
}

// THE FIX. Linode's Ceph rejects aws-chunked-with-trailer outright, so the request
// has to leave the proxy in a form it accepts: raw body, no framing headers, a real
// payload hash, and a signature over the result.
func TestResignConvertsTheFramingLinodeRejects(t *testing.T) {
	prev := objProxyResignNow
	t.Cleanup(func() { objProxyResignNow = prev })
	objProxyResignNow = func() time.Time { return time.Date(2026, 8, 3, 21, 0, 0, 0, time.UTC) }

	const payload = "chunk-bytes-that-must-survive"
	r := trailerRequest(t, payload)
	old := r.Header.Get("Authorization")

	done, err := resignForUpstream(r, testCreds(), "us-ord-10.linodeobjects.com")
	if err != nil || !done {
		t.Fatalf("re-sign should have applied: done=%v err=%v", done, err)
	}

	got, _ := io.ReadAll(r.Body)
	if string(got) != payload {
		t.Errorf("body = %q, want the DE-CHUNKED payload %q — a mis-parse here writes a corrupt object", got, payload)
	}
	if r.ContentLength != int64(len(payload)) {
		t.Errorf("ContentLength = %d, want %d", r.ContentLength, len(payload))
	}
	for _, h := range []string{"Content-Encoding", "X-Amz-Decoded-Content-Length", "X-Amz-Trailer", "X-Amz-Sdk-Checksum-Algorithm"} {
		if v := r.Header.Get(h); v != "" {
			t.Errorf("%s survived as %q — the body is no longer chunked, so the request contradicts itself", h, v)
		}
	}
	if h := r.Header.Get("X-Amz-Content-Sha256"); h == sha256StreamingUnsignedTrailer || len(h) != 64 {
		t.Errorf("x-amz-content-sha256 = %q, want a real payload hash", h)
	}
	if r.Header.Get("Authorization") == old {
		t.Error("Authorization unchanged — the signed headers changed, so the old signature cannot still be valid")
	}
	if !strings.Contains(r.Header.Get("Authorization"), "Credential="+testAKID+"/") {
		t.Errorf("re-signed under a different identity: %q", r.Header.Get("Authorization"))
	}
}

// Identity is preserved or the request is left alone. Re-signing someone else's
// request as us would change who the upstream believes is writing.
func TestResignRefusesAnotherIdentity(t *testing.T) {
	r := trailerRequest(t, "x")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=SOMEONEELSE/20260803/us-ord/s3/aws4_request, Signature=x")
	before := r.Header.Get("Authorization")

	done, err := resignForUpstream(r, testCreds(), "us-ord-10.linodeobjects.com")
	if err != nil || done {
		t.Fatalf("a foreign access key must be passed through: done=%v err=%v", done, err)
	}
	if r.Header.Get("Authorization") != before {
		t.Error("the request was modified despite not being ours to repair")
	}
}

// With no credentials the proxy must behave exactly as it did before #397 — this is
// what keeps "the proxy never re-signs" true for every deployment that has not
// opted in.
func TestResignIsInertWithoutCredentials(t *testing.T) {
	r := trailerRequest(t, "x")
	before := r.Header.Get("Content-Encoding")
	done, err := resignForUpstream(r, objProxyCreds{}, "us-ord-10.linodeobjects.com")
	if err != nil || done {
		t.Fatalf("no credentials must mean no re-signing: done=%v err=%v", done, err)
	}
	if r.Header.Get("Content-Encoding") != before {
		t.Error("the request was rewritten with no credentials configured")
	}
}

// Ordinary requests are the overwhelming majority and must not be touched at all.
func TestResignLeavesNormalRequestsAlone(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "https://us-ord-10.linodeobjects.com/b/k", strings.NewReader("plain"))
	r.Header.Set("X-Amz-Content-Sha256", s3sig.SHA256Hex("plain"))
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+testAKID+"/20260803/us-ord/s3/aws4_request, Signature=x")

	done, err := resignForUpstream(r, testCreds(), "us-ord-10.linodeobjects.com")
	if err != nil || done {
		t.Fatalf("a normally-framed request must pass through untouched: done=%v err=%v", done, err)
	}
}

// A body we decoded wrongly would be written as a CORRUPT object that every
// downstream checksum accepts. The client's own decoded-length is the cross-check,
// and disagreeing with it must abort the rewrite rather than forward a guess.
func TestResignRefusesWhenTheDecodedLengthDisagrees(t *testing.T) {
	r := trailerRequest(t, "twelve bytes")
	r.Header.Set("X-Amz-Decoded-Content-Length", "999")

	done, err := resignForUpstream(r, testCreds(), "us-ord-10.linodeobjects.com")
	if done || err == nil {
		t.Fatalf("a length mismatch must abort, not forward: done=%v err=%v", done, err)
	}
	if !strings.Contains(err.Error(), "decoded") {
		t.Errorf("the error must name the mismatch: %v", err)
	}
}

func TestDecodeAWSChunkedHandlesMultipleChunks(t *testing.T) {
	body := "5\r\nhello\r\n6\r\n world\r\n0\r\nx-amz-checksum-crc32:abc=\r\n\r\n"
	got, err := decodeAWSChunked(strings.NewReader(body), 11)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world" {
		t.Errorf("decoded %q, want %q", got, "hello world")
	}
}

func TestParseObjProxyCredsAcceptsEnvStyle(t *testing.T) {
	c := parseObjProxyCreds([]byte("AWS_ACCESS_KEY_ID=AK123\nAWS_SECRET_ACCESS_KEY=SK456\n"))
	if c.AccessKeyID != "AK123" || c.SecretAccessKey != "SK456" || !c.usable() {
		t.Errorf("parsed %+v", c)
	}
}

// End to end through the real proxy: what the upstream RECEIVES must be the repaired
// request, with the SSE-C headers still applied on top. The two features have to
// compose — re-signing that dropped the encryption headers would silently write
// plaintext, which is the one outcome this whole component exists to prevent.
func TestProxyResignsAndStillInjectsSSEC(t *testing.T) {
	prev := objProxyResignNow
	t.Cleanup(func() { objProxyResignNow = prev })
	objProxyResignNow = func() time.Time { return time.Date(2026, 8, 3, 21, 0, 0, 0, time.UTC) }

	var gotBody []byte
	var gotHdr http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHdr = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	host := strings.TrimPrefix(upstream.URL, "http://")
	c := &objProxyCounters{}
	rp, err := buildObjProxy(ProxyOpts{Upstream: host, Creds: testCreds()}, testKey(t), c)
	if err != nil {
		t.Fatal(err)
	}
	rp.Transport = upstream.Client().Transport
	od := rp.Director
	rp.Director = func(r *http.Request) { od(r); r.URL.Scheme = "http" }

	const payload = "loki-chunk-payload"
	req := trailerRequest(t, payload)
	req.Host = host
	// Re-sign for the host this proxy actually fronts: verification recomputes the
	// signature over the upstream it was built with, and the default fixture signs
	// for the real Linode endpoint.
	signAsClient(t, req, testCreds(), host)
	rp.ServeHTTP(httptest.NewRecorder(), req)

	if string(gotBody) != payload {
		t.Errorf("upstream received %q, want the de-chunked %q", gotBody, payload)
	}
	if gotHdr.Get("Content-Encoding") == "aws-chunked" {
		t.Error("upstream still received aws-chunked framing — this is the exact request Linode 403s")
	}
	if gotHdr.Get(hdrSSECKey) == "" || gotHdr.Get(hdrSSECAlgorithm) != "AES256" {
		t.Error("SSE-C headers missing on the re-signed request — it would be written in PLAINTEXT")
	}
	if c.resigned.Load() != 1 {
		t.Errorf("resigned counter = %d, want 1", c.resigned.Load())
	}
	if !bytes.Contains([]byte(gotHdr.Get("Authorization")), []byte(testAKID)) {
		t.Errorf("upstream saw Authorization %q", gotHdr.Get("Authorization"))
	}
}

// ── defects found in code review ────────────────────────────────────────────

// A FAILED repair must leave the request byte-identical, because the caller's
// fallback forwards it. The first version decoded straight from r.Body, so a
// failure drained it and the proxy wrote an EMPTY object upstream — a silent write
// of nothing, strictly worse than the 403 the repair exists to avoid.
func TestFailedResignLeavesTheBodyIntactForThePassThrough(t *testing.T) {
	const payload = "twelve bytes"
	r := trailerRequest(t, payload)
	r.Header.Set("X-Amz-Decoded-Content-Length", "999") // forces a decode failure

	done, err := resignForUpstream(r, testCreds(), "us-ord-10.linodeobjects.com")
	if done || err == nil {
		t.Fatalf("expected a refused repair: done=%v err=%v", done, err)
	}
	got, _ := io.ReadAll(r.Body)
	if string(got) != awsChunked(payload) {
		t.Errorf("body after a failed repair = %d bytes, want the original %d — the caller forwards this, "+
			"so a drained body writes an empty object", len(got), len(awsChunked(payload)))
	}
}

// Repairing means buffering the whole object. This is a DaemonSet with a 512Mi limit
// serving every pod on its node, so an unbounded buffer turns one large upload into
// an object-storage outage for the whole node — worse than the write it would fix.
func TestResignRefusesPayloadsTooLargeToBufferSafely(t *testing.T) {
	r := trailerRequest(t, "small on the wire")
	r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(int64(objProxyResignMaxBody)+1))
	before, _ := io.ReadAll(io.NopCloser(strings.NewReader(awsChunked("small on the wire"))))

	done, err := resignForUpstream(r, testCreds(), "us-ord-10.linodeobjects.com")
	if done {
		t.Fatal("an oversized payload must NOT be buffered")
	}
	if err == nil || !strings.Contains(err.Error(), "repair limit") {
		t.Errorf("the refusal must name the limit: %v", err)
	}
	// And it must not have touched the body on its way to refusing.
	got, _ := io.ReadAll(r.Body)
	if len(got) != len(before) {
		t.Errorf("body was consumed by a refused repair: %d bytes left, want %d", len(got), len(before))
	}
}

// The length header is mandatory in this framing; without it there is no bound to
// check before reading, so the repair declines rather than reading an unknown size.
func TestResignRefusesWithoutADeclaredLength(t *testing.T) {
	r := trailerRequest(t, "x")
	r.Header.Del("X-Amz-Decoded-Content-Length")
	done, err := resignForUpstream(r, testCreds(), "us-ord-10.linodeobjects.com")
	if done || err == nil {
		t.Fatalf("missing decoded-length must refuse: done=%v err=%v", done, err)
	}
}

// The canonical form must be used BY the signer, not merely exist. Parameter order
// is the client's choice and the canonical form is order-independent, so the same
// multipart PUT written two ways must produce the SAME signature. Testing
// objstore.S3CanonicalQuery alone leaves the call site free to keep signing RawQuery.
func TestResignSignsTheCanonicalQueryNotTheArrivalOrder(t *testing.T) {
	prev := objProxyResignNow
	t.Cleanup(func() { objProxyResignNow = prev })
	objProxyResignNow = func() time.Time { return time.Date(2026, 8, 3, 21, 0, 0, 0, time.UTC) }

	sign := func(query string) string {
		body := awsChunked("part-bytes")
		r := httptest.NewRequest(http.MethodPut,
			"https://us-ord-10.linodeobjects.com/b/k?"+query, strings.NewReader(body))
		r.Header.Set("X-Amz-Content-Sha256", sha256StreamingUnsignedTrailer)
		r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len("part-bytes")))
		r.ContentLength = int64(len(body))
		signAsClient(t, r, testCreds(), "us-ord-10.linodeobjects.com")
		done, err := resignForUpstream(r, testCreds(), "us-ord-10.linodeobjects.com")
		if err != nil || !done {
			t.Fatalf("re-sign should have applied for %q: done=%v err=%v", query, done, err)
		}
		return r.Header.Get("Authorization")
	}

	if a, b := sign("partNumber=1&uploadId=ZZZ"), sign("uploadId=ZZZ&partNumber=1"); a != b {
		t.Errorf("signature depends on the client's parameter ORDER, so whichever order the SDK happens to "+
			"use will 403:\n  %s\n  %s", a, b)
	}
}

// A ROTATED credential must be picked up without restarting the pod.
// secret/obj/platform is rotated by `llz ci rotate-linode-creds`; reading it once at
// startup meant the proxy would keep re-signing with the retired key, 403ing every
// repaired write until every pod happened to restart — the outage caused by the
// thing that exists to prevent it. Same shape as the serving cert that was loaded
// once and expired at day 90.
func TestCredsLoaderPicksUpARotation(t *testing.T) {
	prevStat, prevRead := objProxyStat, objProxyReadFile
	t.Cleanup(func() { objProxyStat, objProxyReadFile = prevStat, prevRead })

	mod := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	content := []byte("AWS_ACCESS_KEY_ID=OLD\nAWS_SECRET_ACCESS_KEY=OLDSEC\n")
	objProxyStat = func(string) (os.FileInfo, error) { return fakeFileInfo{mod}, nil }
	objProxyReadFile = func(string) ([]byte, error) { return content, nil }

	l := &credsLoader{path: "/etc/llz/obj-creds/credentials"}
	if got := l.get(); got.AccessKeyID != "OLD" {
		t.Fatalf("first read = %+v", got)
	}

	// Rotation: new content AND a new mtime, as a Secret volume swap produces.
	content = []byte("AWS_ACCESS_KEY_ID=NEW\nAWS_SECRET_ACCESS_KEY=NEWSEC\n")
	mod = mod.Add(time.Minute)
	if got := l.get(); got.AccessKeyID != "NEW" || got.SecretAccessKey != "NEWSEC" {
		t.Errorf("after rotation = %+v, want the NEW pair — a stale key 403s every repaired write", got)
	}
}

// An unchanged file must not be re-read on every request; the repair runs on every
// Loki chunk flush.
func TestCredsLoaderCachesWhileTheFileIsUnchanged(t *testing.T) {
	prevStat, prevRead := objProxyStat, objProxyReadFile
	t.Cleanup(func() { objProxyStat, objProxyReadFile = prevStat, prevRead })

	reads := 0
	mod := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	objProxyStat = func(string) (os.FileInfo, error) { return fakeFileInfo{mod}, nil }
	objProxyReadFile = func(string) ([]byte, error) {
		reads++
		return []byte("AWS_ACCESS_KEY_ID=A\nAWS_SECRET_ACCESS_KEY=B\n"), nil
	}

	l := &credsLoader{path: "p"}
	for i := 0; i < 5; i++ {
		l.get()
	}
	if reads != 1 {
		t.Errorf("read the credential %d times for an unchanged file, want 1", reads)
	}
}

// A transient read failure must not drop the capability: the last known-good key is
// the best guess available, and the upstream rejects it if it is wrong.
func TestCredsLoaderKeepsLastGoodOnReadFailure(t *testing.T) {
	prevStat, prevRead := objProxyStat, objProxyReadFile
	t.Cleanup(func() { objProxyStat, objProxyReadFile = prevStat, prevRead })

	mod := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	objProxyStat = func(string) (os.FileInfo, error) { return fakeFileInfo{mod}, nil }
	objProxyReadFile = func(string) ([]byte, error) { return []byte("AWS_ACCESS_KEY_ID=A\nAWS_SECRET_ACCESS_KEY=B\n"), nil }
	l := &credsLoader{path: "p"}
	l.get()

	mod = mod.Add(time.Minute)
	objProxyReadFile = func(string) ([]byte, error) { return nil, errNotFound }
	if got := l.get(); got.AccessKeyID != "A" {
		t.Errorf("a transient read error dropped the credential: %+v", got)
	}
}

type fakeFileInfo struct{ mod time.Time }

func (f fakeFileInfo) Name() string       { return "credentials" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o440 }
func (f fakeFileInfo) ModTime() time.Time { return f.mod }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

// The Director must consult credsFn PER REQUEST, not capture a value once.
//
// This is a silent-regression shape: if the wiring is reordered so credsFn is nil
// when buildObjProxy runs, resolveCreds falls back to the static pair and
// re-signing still WORKS — it just stops picking up rotations, which nothing else
// would reveal until a rotation broke every write.
func TestProxyConsultsTheCredentialLoaderPerRequest(t *testing.T) {
	prev := objProxyResignNow
	t.Cleanup(func() { objProxyResignNow = prev })
	objProxyResignNow = func() time.Time { return time.Date(2026, 8, 3, 21, 0, 0, 0, time.UTC) }

	// Record the key id AND whether the framing was actually removed. The key id
	// alone cannot tell a re-signed request from a passed-through one: a
	// pass-through still carries the CLIENT's key id, so asserting on it only would
	// pass even with re-signing disabled entirely.
	type observed struct {
		akid    string
		chunked bool
	}
	var seen []observed
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, observed{
			akid:    authCredentialKeyID(r.Header.Get("Authorization")),
			chunked: r.Header.Get("Content-Encoding") == "aws-chunked",
		})
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")

	current := objProxyCreds{AccessKeyID: "KEY-ONE", SecretAccessKey: "s1"}
	c := &objProxyCounters{}
	// creds is deliberately left ZERO: the static fallback must not be what makes
	// this pass, or a regression to it would go unnoticed.
	rp, err := buildObjProxy(ProxyOpts{
		Upstream: host,
		CredsFn:  func() objProxyCreds { return current },
	}, testKey(t), c)
	if err != nil {
		t.Fatal(err)
	}
	rp.Transport = upstream.Client().Transport
	od := rp.Director
	rp.Director = func(r *http.Request) { od(r); r.URL.Scheme = "http" }

	send := func(akid string) {
		body := awsChunked("payload")
		r := httptest.NewRequest(http.MethodPut, "https://ignored/bucket/key", strings.NewReader(body))
		r.Host = host
		r.Header.Set("X-Amz-Content-Sha256", sha256StreamingUnsignedTrailer)
		r.Header.Set("Content-Encoding", "aws-chunked")
		r.Header.Set("X-Amz-Decoded-Content-Length", "7")
		r.ContentLength = int64(len(body))
		signAsClient(t, r, current, host)
		rp.ServeHTTP(httptest.NewRecorder(), r)
	}

	send("KEY-ONE")
	current = objProxyCreds{AccessKeyID: "KEY-TWO", SecretAccessKey: "s2"} // rotation
	send("KEY-TWO")

	if len(seen) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(seen))
	}
	for i, o := range seen {
		if o.chunked {
			t.Errorf("request %d still carried aws-chunked framing — it was not re-signed at all, so this "+
				"test would pass on the key id alone while proving nothing", i)
		}
	}
	if seen[0].akid != "KEY-ONE" || seen[1].akid != "KEY-TWO" {
		t.Errorf("upstream saw %v — the second request must be signed with the ROTATED key, or the loader "+
			"was captured once and rotations are invisible", seen)
	}
}

// THE SIGNING-ORACLE HOLE. Re-signing replaces the caller's Authorization with one
// minted from the real secret key. If the only check is "the Credential names our
// access key id", then possessing that id — which is not secret, and which the proxy
// prints at startup — is enough to have arbitrary objects written under the
// platform's credential. The NetworkPolicy admits every namespace on :8443, so that
// is any pod in the cluster.
//
// Verifying the caller's own signature first restores the property the gateway had
// before re-signing existed: it grants no access the caller did not already have, it
// only TRANSLATES a request they could already have made.
func TestResignRefusesARequestWhoseSignatureDoesNotVerify(t *testing.T) {
	r := trailerRequest(t, "attacker-payload")
	// Same access key id, forged signature — the exact shape a signing oracle serves.
	auth := r.Header.Get("Authorization")
	r.Header.Set("Authorization", auth[:strings.Index(auth, "Signature=")]+"Signature=deadbeefdeadbeef")

	done, err := resignForUpstream(r, testCreds(), "us-ord-10.linodeobjects.com")
	if done {
		t.Fatal("a forged signature was re-signed with the real key — the proxy is a signing oracle")
	}
	if err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("the refusal must name the reason: %v", err)
	}
}

// And a caller that DID hold the secret is still served — the check must not break
// the repair it protects.
func TestResignAcceptsAProperlySignedRequest(t *testing.T) {
	prev := objProxyResignNow
	t.Cleanup(func() { objProxyResignNow = prev })
	objProxyResignNow = func() time.Time { return time.Date(2026, 8, 3, 21, 0, 0, 0, time.UTC) }

	r := trailerRequest(t, "legitimate-payload")
	done, err := resignForUpstream(r, testCreds(), "us-ord-10.linodeobjects.com")
	if err != nil || !done {
		t.Fatalf("a correctly signed request must still be repaired: done=%v err=%v", done, err)
	}
}

// SigV4 canonicalisation, in the direction that matters. A verifier that rejects a
// correctly-signed request does not fail safe in any useful sense: the write is
// forwarded unrepaired, Linode 403s it, and #397 returns wearing a different error.
func TestCanonicalHeaderValueMatchesSigV4(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		in         []string
	}{
		{"repeated headers join with commas", "a,b", []string{"a", "b"}},
		{"internal runs collapse", "attempt=1; max=3", []string{"attempt=1;   max=3"}},
		{"leading and trailing trimmed", "v", []string{"  v  "}},
		{"tabs are whitespace too", "a b", []string{"a\tb"}},
		{"single value unchanged", "AES256", []string{"AES256"}},
	} {
		if got := canonicalHeaderValue(tc.in); got != tc.want {
			t.Errorf("%s: canonicalHeaderValue(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// End to end: a request signed over a DUPLICATED header must still verify. Taking
// only the first value would refuse it, and the refusal path forwards the request
// unrepaired — so this presents as Loki being unable to write, not as a proxy bug.
func TestResignVerifiesARequestSignedOverDuplicateHeaders(t *testing.T) {
	prev := objProxyResignNow
	t.Cleanup(func() { objProxyResignNow = prev })
	objProxyResignNow = func() time.Time { return time.Date(2026, 8, 3, 21, 0, 0, 0, time.UTC) }

	const host = "us-ord-10.linodeobjects.com"
	body := awsChunked("dup-header-payload")
	r := httptest.NewRequest(http.MethodPut, "https://"+host+"/bucket/key", strings.NewReader(body))
	r.Header.Set("X-Amz-Content-Sha256", sha256StreamingUnsignedTrailer)
	r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len("dup-header-payload")))
	r.Header.Add("X-Amz-Meta-Tag", "first")
	r.Header.Add("X-Amz-Meta-Tag", "second")
	r.ContentLength = int64(len(body))

	// Sign including the duplicated header, as a spec-compliant client would.
	const dateStamp, region = "20260803", "us-ord"
	amzDate := dateStamp + "T210000Z"
	r.Header.Set("X-Amz-Date", amzDate)
	sh := "host;x-amz-content-sha256;x-amz-date;x-amz-meta-tag"
	canonical := "host:" + host + "\n" +
		"x-amz-content-sha256:" + sha256StreamingUnsignedTrailer + "\n" +
		"x-amz-date:" + amzDate + "\n" +
		"x-amz-meta-tag:first,second\n"
	cr := strings.Join([]string{r.Method, objstore.S3EscapePath(r.URL.Path), "", canonical, sh, sha256StreamingUnsignedTrailer}, "\n")
	scope := dateStamp + "/" + region + "/s3/aws4_request"
	sts := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, s3sig.SHA256Hex(cr)}, "\n")
	sig := hex.EncodeToString(s3sig.HMACSHA256(s3sig.SigningKey(testCreds().SecretAccessKey, dateStamp, region, "s3"), sts))
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+testAKID+"/"+scope+", SignedHeaders="+sh+", Signature="+sig)

	done, err := resignForUpstream(r, testCreds(), host)
	if err != nil || !done {
		t.Fatalf("a correctly signed request with a duplicated header must verify: done=%v err=%v", done, err)
	}
}

// errNotFound stands in for kubectl's "NotFound" exit. A local copy of package
// main's errRetrofitNotFound: a one-line sentinel, and a test fixture cannot cross
// a package boundary.
var errNotFound = errors.New("Error from server (NotFound)")
