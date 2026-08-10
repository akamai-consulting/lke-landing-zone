package objenc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/md5" //nolint:gosec // mirrors the SSE-C key-checksum contract under test
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) ssecKey {
	t.Helper()
	k, err := newSSECKey([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The key encodings are what Linode checks; getting the MD5 wrong is rejected at
// every request with a message about the algorithm, not about the checksum.
func TestNewSSECKeyEncodings(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef")
	k := testKey(t)
	if want := base64.StdEncoding.EncodeToString(raw); k.b64 != want {
		t.Errorf("b64 = %q, want %q", k.b64, want)
	}
	sum := md5.Sum(raw) //nolint:gosec // see above
	if want := base64.StdEncoding.EncodeToString(sum[:]); k.md5B64 != want {
		t.Errorf("md5B64 = %q, want %q", k.md5B64, want)
	}
}

// SSE-C is AES-256. A wrong-length key must fail at STARTUP: a proxy that starts
// and then 400s every write reads as an outage in Loki or Harbor, not here.
func TestNewSSECKeyRejectsWrongLength(t *testing.T) {
	for _, raw := range []string{"", "short", strings.Repeat("x", 31), strings.Repeat("x", 33)} {
		if _, err := newSSECKey([]byte(raw)); err == nil {
			t.Errorf("a %d-byte key must be rejected", len(raw))
		}
	}
}

// THE TEST THAT WAS MISSING. Every bug on this component has lived at a boundary
// where both sides were individually tested and disagreed about the contract. Here
// the seeder wrote base64 (a KV value is a JSON string; raw AES bytes contain NULs
// and do not survive one) and the proxy demanded 32 raw bytes — so every pod
// CrashLoopBackOff'd in the first e2e run that got as far as starting the process,
// while both unit tests passed.
//
// This drives the SEEDER's real output through the PROXY's real reader.
func TestSSECKeyRoundTripsFromSeederToProxy(t *testing.T) {
	d := testDeps(t)
	var seeded map[string]string
	prevPut, prevGet, prevGen := d.KVPut, d.KVGet, newSSECKeyMaterial
	d.KVPut = func(_ string, f map[string]string) error { seeded = f; return nil }
	d.KVGet = func(string, string) (string, KVVerdict) { return "", KVAbsent }
	t.Cleanup(func() { d.KVPut, d.KVGet, newSSECKeyMaterial = prevPut, prevGet, prevGen })

	if err := seedSSECKeyInto(d); err != nil {
		t.Fatal(err)
	}
	if seeded["key"] == "" {
		t.Fatal("the seeder wrote no key")
	}

	// ESO materialises the KV string verbatim into the Secret, so the file on disk
	// is exactly what was seeded.
	material, err := parseSSECKeyFile([]byte(seeded["key"]))
	if err != nil {
		t.Fatalf("the proxy cannot read what the seeder wrote: %v", err)
	}
	if _, err := newSSECKey(material); err != nil {
		t.Fatalf("the seeded key is not a usable SSE-C key: %v", err)
	}
}

// The kubelet and editors add trailing newlines to mounted files.
func TestParseSSECKeyFileToleratesTrailingNewlines(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	for _, suffix := range []string{"", "\n", "\r\n"} {
		got, err := parseSSECKeyFile([]byte(b64 + suffix))
		if err != nil {
			t.Errorf("suffix %q: %v", suffix, err)
			continue
		}
		if len(got) != 32 {
			t.Errorf("suffix %q: decoded %d bytes, want 32", suffix, len(got))
		}
	}
}

// A raw key written by hand still works, and a wrong-length one fails with a
// message naming both accepted forms rather than a bare length complaint.
// A 32-char ASCII key from the base64 alphabet decodes cleanly as base64 (to 24
// bytes). Decoding SUCCEEDING is not evidence the input was base64 — only decoding
// to exactly 32 bytes is — so this must be read as a raw key, not rejected.
func TestParseSSECKeyFileRawKeyThatLooksLikeBase64(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef") // valid base64, decodes to 24
	got, err := parseSSECKeyFile(raw)
	if err != nil {
		t.Fatalf("a raw key drawn from the base64 alphabet must still be read as raw: %v", err)
	}
	if string(got) != string(raw) {
		t.Errorf("got %q, want the raw bytes back", got)
	}
}

func TestParseSSECKeyFileFormsAndErrors(t *testing.T) {
	if got, err := parseSSECKeyFile([]byte("0123456789abcdef0123456789abcdef")); err != nil || len(got) != 32 {
		t.Errorf("a raw 32-byte key must be accepted: %v", err)
	}
	for _, bad := range []string{"short", base64.StdEncoding.EncodeToString([]byte("too-short"))} {
		_, err := parseSSECKeyFile([]byte(bad))
		if err == nil {
			t.Errorf("%q must be rejected", bad)
			continue
		}
		if !strings.Contains(err.Error(), "base64") || !strings.Contains(err.Error(), "raw") {
			t.Errorf("the error must name BOTH accepted forms, got: %v", err)
		}
	}
}

// Object writes and reads get the headers.
func TestInjectSSECOnObjectRequests(t *testing.T) {
	k := testKey(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/bucket/chunk-0001"},
		{http.MethodGet, "/bucket/chunk-0001"},
		{http.MethodHead, "/bucket/deep/nested/key"},
		{http.MethodPost, "/bucket/blob"}, // multipart create/complete
	} {
		h := http.Header{}
		if !injectSSEC(h, tc.method, tc.path, k) {
			t.Errorf("%s %s: expected injection", tc.method, tc.path)
			continue
		}
		if h.Get(hdrSSECAlgorithm) != "AES256" || h.Get(hdrSSECKey) != k.b64 || h.Get(hdrSSECKeyMD5) != k.md5B64 {
			t.Errorf("%s %s: headers not set correctly: %v", tc.method, tc.path, redactSSEC(h))
		}
	}
}

// Bucket-level and DELETE requests were verified to work WITHOUT these headers and
// never verified WITH them. Injecting there risks turning a working listing into a
// 400 that looks like a bucket problem.
func TestInjectSSECSkipsBucketLevelAndDelete(t *testing.T) {
	k := testKey(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/bucket"},           // ListObjectsV2
		{http.MethodGet, "/bucket/"},          // trailing slash, still no key
		{http.MethodGet, "/"},                 // service-level
		{http.MethodPost, "/bucket"},          // DeleteObjects
		{http.MethodDelete, "/bucket/object"}, // removing ciphertext needs no key
	} {
		h := http.Header{}
		if injectSSEC(h, tc.method, tc.path, k) {
			t.Errorf("%s %s: must NOT inject", tc.method, tc.path)
		}
		if h.Get(hdrSSECAlgorithm) != "" || h.Get(hdrSSECKey) != "" {
			t.Errorf("%s %s: headers leaked onto a skipped shape", tc.method, tc.path)
		}
	}
}

// THE measured rule. A server-side COPY of an encrypted source needs the
// copy-source trio as well; destination headers alone fail with
// "400 InvalidArgument: ... must provide a valid encryption algorithm".
func TestInjectSSECAddsCopySourceHeaders(t *testing.T) {
	k := testKey(t)
	h := http.Header{}
	h.Set(hdrCopySource, "/bucket/src")
	if !injectSSEC(h, http.MethodPut, "/bucket/dst", k) {
		t.Fatal("a copy is still an object write")
	}
	if h.Get(hdrCopySrcSSECAlgo) != "AES256" ||
		h.Get(hdrCopySrcSSECKey) != k.b64 ||
		h.Get(hdrCopySrcSSECKeyMD5) != k.md5B64 {
		t.Errorf("copy-source headers missing — this is the case that fails 400 Upstream: %v", redactSSEC(h))
	}
}

// The mirror image: a plain PUT must not carry copy-source headers, including when
// a previous handler or a client put them there.
func TestInjectSSECStripsStaleCopySourceHeaders(t *testing.T) {
	k := testKey(t)
	h := http.Header{}
	h.Set(hdrCopySrcSSECAlgo, "AES256")
	h.Set(hdrCopySrcSSECKey, "someone-elses-key")
	if !injectSSEC(h, http.MethodPut, "/bucket/dst", k) {
		t.Fatal("expected injection")
	}
	if h.Get(hdrCopySrcSSECKey) != "" || h.Get(hdrCopySrcSSECAlgo) != "" {
		t.Error("copy-source headers must be removed when there is no x-amz-copy-source")
	}
}

// The key is the proxy's to choose. A client that pinned its own would write
// objects this deployment could never read back.
func TestInjectSSECOverridesClientSuppliedKey(t *testing.T) {
	k := testKey(t)
	h := http.Header{}
	h.Set(hdrSSECKey, "attacker-chosen-key")
	h.Set(hdrSSECAlgorithm, "AES128")
	injectSSEC(h, http.MethodPut, "/bucket/obj", k)
	if h.Get(hdrSSECKey) != k.b64 {
		t.Error("client-supplied key must be overwritten, not merged")
	}
	if got := h.Values(hdrSSECKey); len(got) != 1 {
		t.Errorf("Set must replace, not append: %d values", len(got))
	}
	if h.Get(hdrSSECAlgorithm) != "AES256" {
		t.Error("client-supplied algorithm must be overwritten")
	}
}

// A skipped shape must not carry a client's SSE-C headers through to the upstream
// unreviewed either.
func TestInjectSSECStripsHeadersOnSkippedShapes(t *testing.T) {
	k := testKey(t)
	h := http.Header{}
	h.Set(hdrSSECKey, "client-supplied")
	h.Set(hdrSSECAlgorithm, "AES256")
	if injectSSEC(h, http.MethodDelete, "/bucket/obj", k) {
		t.Fatal("DELETE must not inject")
	}
	if h.Get(hdrSSECKey) != "" || h.Get(hdrSSECAlgorithm) != "" {
		t.Error("client SSE-C headers must be stripped on shapes we deliberately skip")
	}
}

// The key must never reach a log line: with it every object is readable, and
// Linode keeps no copy to fall back on.
func TestRedactSSECHidesKeysOnly(t *testing.T) {
	k := testKey(t)
	h := http.Header{}
	h.Set(hdrCopySource, "/b/src")
	injectSSEC(h, http.MethodPut, "/b/dst", k)
	r := redactSSEC(h)
	for _, hide := range []string{hdrSSECKey, hdrCopySrcSSECKey} {
		if r.Get(hide) != "<redacted>" {
			t.Errorf("%s must be redacted, got %q", hide, r.Get(hide))
		}
	}
	if r.Get(hdrSSECKeyMD5) != k.md5B64 {
		t.Error("the MD5 marker is not secret and is the only way to tell WHICH key was used")
	}
	if h.Get(hdrSSECKey) != k.b64 {
		t.Error("redactSSEC must not mutate the original header set")
	}
}

func TestUpstreamHostOnly(t *testing.T) {
	for in, want := range map[string]string{
		"us-ord-10.linodeobjects.com":          "us-ord-10.linodeobjects.com",
		"https://us-ord-10.linodeobjects.com":  "us-ord-10.linodeobjects.com",
		"http://us-ord-10.linodeobjects.com/":  "us-ord-10.linodeobjects.com",
		"https://us-ord-10.linodeobjects.com/": "us-ord-10.linodeobjects.com",
	} {
		if got := upstreamHostOnly(in); got != want {
			t.Errorf("upstreamHostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

// End to end through the real ReverseProxy: headers arrive at the upstream, the
// body is forwarded byte-for-byte, and Host is preserved (which is what keeps the
// client's SigV4 signature valid without re-signing).
func TestObjProxyForwardsWithHeadersAndIntactBody(t *testing.T) {
	var gotKey, gotAlgo, gotHost string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get(hdrSSECKey)
		gotAlgo = r.Header.Get(hdrSSECAlgorithm)
		gotHost = r.Host
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	k := testKey(t)
	c := &objProxyCounters{}
	// Point the proxy at the httptest server's host; the scheme is overridden to
	// https by the Director, so swap the transport for the test's plain one.
	rp, err := buildObjProxy(ProxyOpts{Upstream: u.Host}, k, c)
	if err != nil {
		t.Fatal(err)
	}
	rp.Transport = upstream.Client().Transport
	origDirector := rp.Director
	rp.Director = func(r *http.Request) { origDirector(r); r.URL.Scheme = "http" }

	body := strings.Repeat("layer-bytes", 1000)
	req := httptest.NewRequest(http.MethodPut, "http://ignored/bucket/blob", strings.NewReader(body))
	req.Host = u.Host
	rp.ServeHTTP(httptest.NewRecorder(), req)

	if gotAlgo != "AES256" || gotKey != k.b64 {
		t.Errorf("upstream did not receive the SSE-C headers (algo=%q)", gotAlgo)
	}
	if string(gotBody) != body {
		t.Errorf("body corrupted: got %d bytes, want %d", len(gotBody), len(body))
	}
	if gotHost != u.Host {
		t.Errorf("Host = %q, want %q — rewriting it would invalidate the client's SigV4 signature", gotHost, u.Host)
	}
	if c.injected.Load() != 1 || c.total.Load() != 1 {
		t.Errorf("counters wrong: total=%d injected=%d", c.total.Load(), c.injected.Load())
	}
}

// writeKeypair emits a self-signed cert/key pair with the given serial, so a test
// can tell one generation from the next.
func writeKeypair(t *testing.T, dir string, serial int64) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "obj-proxy-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(crand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// THE 90-day fuse. cert-manager rotates the Secret at day 60 and the kubelet
// updates the mount; a server that loaded its keypair at boot keeps presenting the
// ORIGINAL leaf until it expires at day 90 and then fails every handshake — with
// the pod still Ready, because liveness is on the separate plaintext health port.
// GetCertificate must re-read from disk, so a rotation is picked up on the next
// handshake rather than at the next restart that nothing triggers.
func TestObjProxyServingCertIsRotatable(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeKeypair(t, dir, 1001)
	cfg := objProxyServingTLS(certPath, keyPath)

	first, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	firstLeaf, err := x509.ParseCertificate(first.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	// cert-manager renews in place: same paths, new material.
	writeKeypair(t, dir, 2002)

	second, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	secondLeaf, err := x509.ParseCertificate(second.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	if firstLeaf.SerialNumber.Cmp(secondLeaf.SerialNumber) == 0 {
		t.Fatal("GetCertificate returned the SAME leaf after rotation — the keypair is " +
			"cached at startup, so this proxy stops serving when the original expires")
	}
	if secondLeaf.SerialNumber.Int64() != 2002 {
		t.Errorf("serving the wrong generation: serial = %d, want 2002", secondLeaf.SerialNumber.Int64())
	}
}

// Explicit, not inherited from the Go version in use.
func TestObjProxyServingTLSMinVersion(t *testing.T) {
	cfg := objProxyServingTLS("/nonexistent", "/nonexistent")
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2 — a transport-posture component must state it", cfg.MinVersion)
	}
}

// THE loop the startup check cannot see. With cluster DNS in the pod, the endpoint
// resolves to the obj-proxy SERVICE, whose ClusterIP is not a local address — so
// assertNotSelf passes and requests loop through kube-proxy, possibly hitting a
// DIFFERENT pod each hop so no single process ever sees itself. The marker header
// catches it on the first returning request.
func TestObjProxyRefusesALoopedRequest(t *testing.T) {
	c := &objProxyCounters{}
	rp, err := buildObjProxy(ProxyOpts{Upstream: "example.invalid"}, testKey(t), c)
	if err != nil {
		t.Fatal(err)
	}
	h := objProxyHandlerFor(rp, c)

	req := httptest.NewRequest(http.MethodPut, "http://ignored/bucket/obj", nil)
	req.Header.Set(objProxyLoopHeader, "1") // already been through a proxy
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusLoopDetected {
		t.Fatalf("a looped request must be refused with 508, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dnsPolicy: Default") {
		t.Error("the response must name the misconfiguration, not just say 'loop'")
	}
	if c.loops.Load() != 1 {
		t.Errorf("loops counter = %d, want 1 — this is the signal to alert on", c.loops.Load())
	}
	if c.total.Load() != 0 {
		t.Error("a refused loop must not be counted as a proxied request")
	}
}

// The marker must be SET on the way out, or the guard above can never fire.
func TestObjProxyMarksOutboundRequests(t *testing.T) {
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(objProxyLoopHeader)
	}))
	defer upstream.Close()
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := &objProxyCounters{}
	rp, err := buildObjProxy(ProxyOpts{Upstream: u.Host}, testKey(t), c)
	if err != nil {
		t.Fatal(err)
	}
	rp.Transport = upstream.Client().Transport
	orig := rp.Director
	rp.Director = func(r *http.Request) { orig(r); r.URL.Scheme = "http" }

	req := httptest.NewRequest(http.MethodPut, "http://ignored/bucket/obj", strings.NewReader("x"))
	req.Host = u.Host
	objProxyHandlerFor(rp, c).ServeHTTP(httptest.NewRecorder(), req)

	if got != "1" {
		t.Errorf("outbound request carried %q, want the loop marker — without it the guard is inert", got)
	}
}

// A virtual-host-style request (bucket.<endpoint>/<key>) puts the object key
// somewhere injectSSEC does not look: a single-segment key reads as a bucket-level
// operation and gets NO SSE-C headers, so the object is written in PLAINTEXT while
// every signal stays color.Green. Nothing routes such requests here today — the CoreDNS
// rewrite is an exact match — but broadening that rewrite is a natural-looking
// change that would arm this silently. It must fail closed instead.
func TestObjProxyRefusesVirtualHostStyleAddressing(t *testing.T) {
	c := &objProxyCounters{}
	rp, err := buildObjProxy(ProxyOpts{Upstream: "us-ord-10.linodeobjects.com"}, testKey(t), c)
	if err != nil {
		t.Fatal(err)
	}
	h := objProxyHandlerForHost(rp, c, "us-ord-10.linodeobjects.com")

	req := httptest.NewRequest(http.MethodPut, "https://ignored/chunk", strings.NewReader("x"))
	req.Host = "platform-loki-chunks-e2e.us-ord-10.linodeobjects.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMisdirectedRequest {
		t.Errorf("status = %d, want 421 — forwarding this writes an unencrypted object", w.Code)
	}
	if c.misdirected.Load() != 1 {
		t.Errorf("misdirected counter = %d, want 1", c.misdirected.Load())
	}
}

// The normal path-style request must be unaffected by that guard.
func TestObjProxyAcceptsThePathStyleHostItFronts(t *testing.T) {
	c := &objProxyCounters{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")

	rp, err := buildObjProxy(ProxyOpts{Upstream: host}, testKey(t), c)
	if err != nil {
		t.Fatal(err)
	}
	rp.Transport = upstream.Client().Transport
	od := rp.Director
	rp.Director = func(r *http.Request) { od(r); r.URL.Scheme = "http" }

	req := httptest.NewRequest(http.MethodPut, "https://ignored/bucket/key", strings.NewReader("x"))
	req.Host = host
	w := httptest.NewRecorder()
	objProxyHandlerForHost(rp, c, host).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("path-style request rejected with %d", w.Code)
	}
}
