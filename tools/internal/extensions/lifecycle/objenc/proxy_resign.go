package objenc

// proxy_resign.go — the fix for #397.
//
// THE BUG. Recent aws-sdk-go-v2 sends PutObject as `content-encoding: aws-chunked`
// with a trailing CRC32 (`x-amz-content-sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER`,
// `x-amz-trailer: x-amz-checksum-crc32`). Linode Object Storage is Ceph RGW and
// rejects that framing with 403 AccessDenied. Loki 3.7.2 uses that SDK, so it cannot
// write a single chunk — measured on three clusters, with every other health signal
// color.Green throughout.
//
// WHY THE FIX LANDS HERE AND NOT IN LOKI. Loki is apl-core's workload on a managed
// App Platform. Its legacy S3 client exposes no checksum knob (the `send_content_md5`
// option exists only on the thanos path, and these clusters run
// use_thanos_objstore: false), and it does NOT take the setting from the
// environment — a Kyverno mutation setting AWS_REQUEST_CHECKSUM_CALCULATION was
// built, confirmed present on the ingester, and the pod still failed every flush and
// wrote nothing. The proxy is already on the path for every object-storage request
// and is this repo's code, which makes it the one place LLZ can act.
//
// WHY IT CAN BE DONE AT ALL. In this framing the PAYLOAD IS UNSIGNED — SigV4 covers
// the headers and the literal string STREAMING-UNSIGNED-PAYLOAD-TRAILER stands in
// for the body hash. So the body may be rewritten freely. The headers may not:
// content-encoding, content-length, x-amz-decoded-content-length and x-amz-trailer
// are all in SignedHeaders, so removing the framing means re-signing.
//
// WHICH IS WHY THIS IS OPT-IN AND IDENTITY-PRESERVING. Re-signing needs a secret
// key, so the proxy holds one — a real increase in what a compromised proxy could
// do, and not something to switch on by accident. It therefore:
//
//   - does nothing at all unless credentials are configured (--creds-file);
//   - re-signs ONLY requests in the broken framing, passing everything else through
//     byte-for-byte as before, so the "never re-signs" property still holds for the
//     traffic that does not need it;
//   - re-signs as the SAME access key the client used, refusing when the client's
//     key is not the one we hold. It repairs a request; it never launders one
//     identity into another.

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/s3sig"
)

// The payload markers that mean "aws-chunked framing, body hash not signed".
const (
	sha256StreamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	sha256StreamingSignedTrailer   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
)

// objProxyCreds is the key pair the proxy re-signs with. Empty = feature off.
type objProxyCreds struct {
	AccessKeyID     string
	SecretAccessKey string
}

func (c objProxyCreds) usable() bool { return c.AccessKeyID != "" && c.SecretAccessKey != "" }

// needsResign reports whether this request is in the framing Linode rejects.
func needsResign(h http.Header) bool {
	switch h.Get("X-Amz-Content-Sha256") {
	case sha256StreamingUnsignedTrailer, sha256StreamingSignedTrailer:
		return true
	}
	return false
}

// authCredentialKeyID pulls the access key id out of an Authorization header:
//
//	AWS4-HMAC-SHA256 Credential=<AKID>/<date>/<region>/s3/aws4_request, SignedHeaders=…
//
// Returns "" when the header is absent or not in that shape, which callers treat as
// "not ours to touch" rather than as an error.
func authCredentialKeyID(auth string) string {
	i := strings.Index(auth, "Credential=")
	if i < 0 {
		return ""
	}
	rest := auth[i+len("Credential="):]
	if j := strings.IndexAny(rest, "/,"); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// parseAuthorization pulls the fields SigV4 verification needs out of an
// Authorization header. Returns ok=false for anything not in the expected shape,
// which callers treat as "not ours to repair".
func parseAuthorization(auth string) (keyID, scope, signedHeaders, signature string, ok bool) {
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return "", "", "", "", false
	}
	for _, part := range strings.Split(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 "), ",") {
		k, v, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch k {
		case "Credential":
			id, sc, cut := strings.Cut(v, "/")
			if !cut {
				return "", "", "", "", false
			}
			keyID, scope = id, sc
		case "SignedHeaders":
			signedHeaders = v
		case "Signature":
			signature = v
		}
	}
	ok = keyID != "" && scope != "" && signedHeaders != "" && signature != ""
	return keyID, scope, signedHeaders, signature, ok
}

// verifyClientSigV4 recomputes the CLIENT's signature and compares it.
//
// WITHOUT THIS THE PROXY IS A SIGNING ORACLE. Re-signing replaces the caller's
// Authorization with one minted from the real secret key, so if the only admission
// check is "the Credential names our access key id", then possessing that ID —
// which is not a secret, and which this process prints at startup — is enough to
// have arbitrary objects written under the platform's credential. The NetworkPolicy
// admits every namespace on :8443, so that is any pod in the cluster.
//
// Verifying first restores the property the gateway had before re-signing existed:
// the proxy grants no access the caller did not already have. It only TRANSLATES a
// request the caller could already have made, out of a framing Linode rejects and
// into one it accepts.
//
// The payload hash is the literal streaming marker, exactly as the client signed it
// — the body is unsigned in this framing, which is what makes the repair possible
// and equally what makes this check necessary.
func verifyClientSigV4(r *http.Request, secretKey, host string) error {
	auth := r.Header.Get("Authorization")
	_, scope, signedHeaders, want, ok := parseAuthorization(auth)
	if !ok {
		return fmt.Errorf("the Authorization header is not a parseable AWS4-HMAC-SHA256 credential")
	}
	parts := strings.Split(scope, "/")
	if len(parts) != 4 {
		return fmt.Errorf("credential scope %q is not <date>/<region>/<service>/aws4_request", scope)
	}
	dateStamp, region, service := parts[0], parts[1], parts[2]

	var canonical strings.Builder
	for _, h := range strings.Split(signedHeaders, ";") {
		switch h {
		case "host":
			// The client signed the endpoint NAME; DNS is what sent it here, and the
			// Director has already rewritten r.Host to the same upstream.
			canonical.WriteString("host:" + host + "\n")
		case "content-length":
			// net/http hoists Content-Length out of Header into the field.
			canonical.WriteString("content-length:" + strconv.FormatInt(r.ContentLength, 10) + "\n")
		default:
			canonical.WriteString(h + ":" + canonicalHeaderValue(r.Header.Values(h)) + "\n")
		}
	}
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	canonicalRequest := strings.Join([]string{
		r.Method, objstore.S3EscapePath(r.URL.Path), objstore.S3CanonicalQuery(r.URL.RawQuery),
		canonical.String(), signedHeaders, payloadHash,
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", r.Header.Get("X-Amz-Date"), scope, s3sig.SHA256Hex(canonicalRequest),
	}, "\n")
	got := hex.EncodeToString(s3sig.HMACSHA256(s3sig.SigningKey(secretKey, dateStamp, region, service), stringToSign))
	// Constant-time: this is a signature comparison, and a timing oracle here would
	// hand back exactly what the check exists to withhold.
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return fmt.Errorf("the request's own SigV4 signature does not verify — refusing to re-sign it")
	}
	return nil
}

// canonicalHeaderValue renders a header the way SigV4 does: repeated headers are
// joined with commas in the order sent, and each value is trimmed with sequential
// internal spaces collapsed to one.
//
// Getting this wrong is a FALSE NEGATIVE, which is the dangerous direction here. A
// verifier that rejects a correctly-signed request does not merely fail closed — it
// refuses to repair Loki's writes, they go upstream in the framing Linode rejects,
// and #397 comes straight back wearing a different error. http.Header.Get returns
// only the FIRST value, so a duplicated signed header would have failed every time.
func canonicalHeaderValue(values []string) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strings.Join(strings.Fields(v), " "))
	}
	return strings.Join(parts, ",")
}

// decodeAWSChunked unwraps aws-chunked framing to the raw payload.
//
//	<hex-len>[;chunk-signature=…]\r\n <data> \r\n … 0[;…]\r\n <trailers> \r\n
//
// The chunk-signature parameter is ignored: it authenticates the STREAM to the
// upstream, and this request is not going upstream in this form. Trailers are
// dropped with the framing — the CRC32 they carry is exactly what the server will
// not accept, and the payload's integrity on the re-signed request is covered by the
// x-amz-content-sha256 computed over the decoded bytes.
func decodeAWSChunked(r io.Reader, expected int64) ([]byte, error) {
	br := bufio.NewReader(r)
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("reading chunk header: %w", err)
		}
		header := strings.TrimRight(line, "\r\n")
		if i := strings.IndexByte(header, ';'); i >= 0 {
			header = header[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(header), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("chunk length %q is not hex: %w", header, err)
		}
		if n == 0 {
			break // terminal chunk; whatever follows is trailers, deliberately discarded
		}
		if _, err := io.CopyN(&out, br, n); err != nil {
			return nil, fmt.Errorf("reading %d chunk bytes: %w", n, err)
		}
		// Each chunk is followed by CRLF. Consume it; a short read here means the
		// framing is malformed and guessing past it would corrupt the object.
		if _, err := br.Discard(2); err != nil {
			return nil, fmt.Errorf("chunk terminator: %w", err)
		}
	}
	// x-amz-decoded-content-length is the client's own statement of the payload size.
	// Disagreeing with it means we mis-parsed, and forwarding a body we decoded wrong
	// would write a CORRUPT object that every checksum downstream would accept.
	if expected >= 0 && int64(out.Len()) != expected {
		return nil, fmt.Errorf("decoded %d bytes but x-amz-decoded-content-length says %d", out.Len(), expected)
	}
	return out.Bytes(), nil
}

// resignForUpstream rewrites a trailer-framed request in place as a plain signed PUT.
//
// Returns false (leaving the request untouched) whenever it is not ours to repair —
// no credentials, a different access key, or an unparseable body. Passing the
// original through is always safe: it is what the proxy did before this existed, and
// the upstream's own answer is a truer verdict than a rewrite we are unsure of.
func resignForUpstream(r *http.Request, creds objProxyCreds, upstreamHost string) (bool, error) {
	if !creds.usable() || !needsResign(r.Header) {
		return false, nil
	}
	if kid := authCredentialKeyID(r.Header.Get("Authorization")); kid != creds.AccessKeyID {
		// Someone else's identity. Repairing it would mean signing as us on their
		// behalf, which changes who the upstream thinks is writing.
		return false, nil
	}
	// PROVE THE CALLER ALREADY HELD THE SECRET before minting a signature for them.
	// Naming our access key id is not authorization — see verifyClientSigV4.
	if err := verifyClientSigV4(r, creds.SecretAccessKey, upstreamHost); err != nil {
		return false, err
	}
	// SIZE GATE, BEFORE THE BODY IS TOUCHED. Repairing means buffering the whole
	// object, and this is a DaemonSet with a 512Mi limit serving every pod on the
	// node — a few concurrent large uploads would OOM it and take object storage down
	// for the whole node, which is far worse than the write this would have fixed.
	// Over the cap the request passes through unrepaired: it fails upstream exactly
	// as it does today, which is the status quo rather than a new failure.
	//
	// It also refuses when the length is absent: that header is mandatory in this
	// framing, and without it there is no bound to check before reading.
	v := r.Header.Get("X-Amz-Decoded-Content-Length")
	if v == "" {
		return false, fmt.Errorf("no x-amz-decoded-content-length: cannot bound the buffer before reading")
	}
	declared, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return false, fmt.Errorf("x-amz-decoded-content-length %q: %w", v, err)
	}
	if declared < 0 || declared > objProxyResignMaxBody {
		return false, fmt.Errorf("payload %d bytes exceeds the %d-byte repair limit; forwarded unrepaired",
			declared, objProxyResignMaxBody)
	}

	// BUFFER THE RAW BODY FIRST so a failure can put it back. Decoding straight from
	// r.Body drained it, and the caller's "forward it unrepaired" fallback then sent
	// an EMPTY object upstream — a silent write of nothing, which is worse than the
	// 403 this repair exists to avoid.
	// +1 so a body AT the cap is distinguishable from one OVER it. io.LimitReader
	// returns a short read with NO error, so without the extra byte a truncated read
	// is indistinguishable from a complete one — and restore() would then hand the
	// caller a TRUNCATED prefix, which the proxy forwards as a complete object. A
	// silently corrupt write is worse than any failure this repair prevents.
	const readCap = objProxyResignMaxBody*2 + 1
	raw, err := io.ReadAll(io.LimitReader(r.Body, readCap))
	_ = r.Body.Close()
	restore := func() { r.Body = io.NopCloser(bytes.NewReader(raw)) }
	if err != nil {
		restore()
		return false, fmt.Errorf("reading body: %w", err)
	}
	if int64(len(raw)) == readCap {
		// We hold a prefix, not the body. There is nothing faithful to forward, so
		// fail the request rather than write a truncated object: replace the body
		// with a reader that errors, which makes the transport abort and the
		// ErrorHandler return 502.
		r.Body = io.NopCloser(errReader{fmt.Errorf("obj-proxy: request body exceeded the %d-byte repair limit", objProxyResignMaxBody)})
		r.ContentLength = 0
		return false, fmt.Errorf("body exceeded %d bytes despite a declared length of %d — refusing to forward a truncated object",
			readCap, declared)
	}
	body, err := decodeAWSChunked(bytes.NewReader(raw), declared)
	if err != nil {
		restore()
		return false, err
	}

	// Drop the framing and every checksum header that came with it. Leaving any of
	// them makes the request self-contradictory: the body is no longer chunked.
	for _, h := range []string{"Content-Encoding", "X-Amz-Decoded-Content-Length", "X-Amz-Trailer",
		"X-Amz-Sdk-Checksum-Algorithm", "X-Amz-Checksum-Crc32", "X-Amz-Checksum-Crc32c",
		"X-Amz-Checksum-Sha1", "X-Amz-Checksum-Sha256", "X-Amz-Checksum-Algorithm"} {
		r.Header.Del(h)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))

	payloadHash := s3sig.SHA256Hex(string(body))
	now := objProxyResignNow().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	region := s3sig.Region(upstreamHost)

	// A MINIMAL signed set — host, payload hash, date — for the same reason the
	// SSE-C headers are added outside SignedHeaders: everything not signed is still
	// sent and still honoured, and signing fewer headers is fewer things that can
	// disagree between what we canonicalise and what net/http finally puts on the
	// wire (Accept-Encoding and Content-Length being the classic offenders).
	canonicalHeaders := "host:" + upstreamHost + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	escapedPath := objstore.S3EscapePath(r.URL.Path)
	// The CANONICAL query, not the raw one. SigV4 signs parameters sorted by name and
	// RFC3986-encoded; the client sends them in whatever order it likes. A multipart
	// PUT carries `?partNumber=N&uploadId=…`, and signing them in arrival order
	// produced a signature the upstream could not reproduce — so every multipart
	// upload would 403 while single-part writes worked, which is the kind of partial
	// failure that gets diagnosed as "flaky object storage".
	// The wire still carries the original order: the server canonicalises before
	// verifying, so the two need not match.
	canonicalRequest := strings.Join([]string{
		r.Method, escapedPath, objstore.S3CanonicalQuery(r.URL.RawQuery), canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")
	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, s3sig.SHA256Hex(canonicalRequest),
	}, "\n")
	signature := hex.EncodeToString(s3sig.HMACSHA256(s3sig.SigningKey(creds.SecretAccessKey, dateStamp, region, "s3"), stringToSign))

	// Signed and sent must be the same bytes; see objstore.S3SignedRequest for why RawPath is
	// set alongside Path rather than trusting Go to re-escape identically.
	r.URL.RawPath = escapedPath
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	r.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKeyID, scope, signedHeaders, signature))
	return true, nil
}

// errReader fails every read. Used to abort a request whose body we consumed but
// cannot faithfully reproduce — the transport surfaces it as an upstream error
// instead of writing a partial object.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// objProxyResignMaxBody bounds what the repair will buffer. Loki's chunks are
// ~1.5MB, so this clears them by more than an order of magnitude while staying far
// under the pod's 512Mi limit even with several requests in flight.
const objProxyResignMaxBody = 32 << 20 // 32 MiB

// credsLoader serves the re-signing credential, RE-READING it when the file changes.
//
// Reading once at startup was a latent outage. secret/obj/platform is rotated by
// `llz ci rotate-linode-creds`, ESO republishes the Secret within its 5m refresh,
// and a proxy holding the retired key would then re-sign every repaired write with
// a credential the upstream has revoked — 403 on every Loki flush, caused by the
// component that exists to prevent exactly that, until every pod happened to
// restart. It is the same shape as the serving certificate that was loaded once and
// expired at day 90.
//
// Kubernetes swaps a Secret volume atomically, so ModTime is a reliable trigger and
// the file is never observed half-written. A read error keeps the last known-good
// value rather than dropping the capability on a transient — the previous key is
// still the best guess available, and the upstream will reject it if it is not.
type credsLoader struct {
	path string
	mu   sync.Mutex
	cur  objProxyCreds
	mod  time.Time
}

func (l *credsLoader) get() objProxyCreds {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, err := objProxyStat(l.path)
	if err != nil {
		return l.cur
	}
	if !st.ModTime().After(l.mod) && l.cur.usable() {
		return l.cur
	}
	raw, err := objProxyReadFile(l.path)
	if err != nil {
		return l.cur
	}
	next := parseObjProxyCreds(raw)
	if !next.usable() {
		// A truncated or half-populated projection: keep what works.
		return l.cur
	}
	if next.AccessKeyID != l.cur.AccessKeyID && l.cur.AccessKeyID != "" {
		fmt.Printf("obj-proxy: re-signing credential rotated to access key %s\n", next.AccessKeyID)
	}
	l.cur, l.mod = next, st.ModTime()
	return l.cur
}

// Seams for the reload path — a rotation is precisely what cannot be rehearsed
// against a real cluster in a unit test.
var (
	objProxyStat     = os.Stat
	objProxyReadFile = os.ReadFile
)

// objProxyResignNow is the clock seam — a signature is only valid within the
// upstream's clock skew window, so tests need to pin it.
var objProxyResignNow = time.Now

// parseObjProxyCreds reads an access key / secret pair from a mounted file. Accepts
// `key=value` lines so the same file shape works whether ESO writes an env-style
// blob or a two-key Secret projected as one file.
func parseObjProxyCreds(b []byte) objProxyCreds {
	var c objProxyCreds
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(k)) {
		case "AWS_ACCESS_KEY_ID", "ACCESS_KEY_ID", "ACCESSKEYID":
			c.AccessKeyID = strings.TrimSpace(v)
		case "AWS_SECRET_ACCESS_KEY", "SECRET_ACCESS_KEY", "SECRETACCESSKEY":
			c.SecretAccessKey = strings.TrimSpace(v)
		}
	}
	return c
}
