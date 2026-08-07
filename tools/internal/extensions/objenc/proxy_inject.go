package objenc

// objproxy_inject.go — the SSE-C header injection rules, as a pure function.
//
// WHY THIS EXISTS AT ALL. Linode Object Storage implements exactly one
// server-side encryption mode, SSE-C (measured: SSE-S3 is 400 InvalidArgument and
// PutBucketEncryption is 501 NotImplemented — see the linode_object_storage_bucket
// entries in ci_at_rest_guard.go). Neither writer that puts data there can emit
// SSE-C: Loki's SSEConfig accepts only SSE-KMS and SSE-S3 and hard-errors on
// anything else, and distribution's S3 driver exposes only `encrypt` and `keyid`.
// A proxy can, so the encryption is reachable without forking either app.
//
// THE PROXY OWNS NO CRYPTO. It adds headers; Linode does the encryption. Object
// bodies pass through untouched, which is what keeps this small enough to trust on
// the path of every image pull and every log write.
//
// NO RE-SIGNING. Measured against Linode: SSE-C headers are honoured even when
// they are NOT in the request's SigV4 SignedHeaders. That is the fact that makes
// this a header filter instead of a terminating, re-signing S3 proxy — the
// client's signature stays valid because we change neither the body, the method,
// the path, nor the Host. It is ALSO why callers must reach us at the real
// endpoint hostname (via DNS) rather than at a Service name: rewriting Host would
// invalidate the signature we deliberately do not recompute.
//
// THE RULE THAT IS NOT OBVIOUS. Blanket injection is correct for ordinary objects
// AND for every multipart request including CompleteMultipartUpload (measured —
// Complete does not reject the headers). It is NOT sufficient for a server-side
// COPY: copying an ENCRYPTED SOURCE additionally needs the
// x-amz-copy-source-server-side-encryption-customer-* trio, and destination
// headers alone fail with
//
//	400 InvalidArgument: Requests specifying Server Side Encryption with
//	Customer provided keys must provide a valid encryption algorithm.
//
// Harbor's registry reaches that path: apl-core sets multipartcopythresholdsize to
// 5GiB, so distribution issues a server-side COPY for blobs above that size. Rare,
// not impossible, and it fails closed — which is why the copy-source case is
// handled here rather than left as a known gap.

import (
	"crypto/md5" //nolint:gosec // SSE-C's key checksum header is MD5 by S3 API definition, not a security choice
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

// SSE-C request headers, spelled as the S3 API defines them.
const (
	hdrSSECAlgorithm = "x-amz-server-side-encryption-customer-algorithm"
	hdrSSECKey       = "x-amz-server-side-encryption-customer-key"
	hdrSSECKeyMD5    = "x-amz-server-side-encryption-customer-key-MD5"

	hdrCopySource          = "x-amz-copy-source"
	hdrCopySrcSSECAlgo     = "x-amz-copy-source-server-side-encryption-customer-algorithm"
	hdrCopySrcSSECKey      = "x-amz-copy-source-server-side-encryption-customer-key"
	hdrCopySrcSSECKeyMD5   = "x-amz-copy-source-server-side-encryption-customer-key-MD5"
	sseCustomerAlgorithm   = "AES256"
	sseCustomerKeyRawBytes = 32 // AES-256
)

// ssecKey is the encryption key in the two encodings the headers need, derived
// once at startup so the hot path does no crypto at all.
type ssecKey struct {
	b64    string // base64(key)
	md5B64 string // base64(md5(key)) — S3's key-integrity check
}

// newSSECKey validates and pre-encodes the raw key. SSE-C is AES-256, so the key
// is exactly 32 bytes; a short key is rejected here rather than at the first PUT,
// because a proxy that starts and then fails every write looks like an outage
// somewhere else entirely.
func newSSECKey(raw []byte) (ssecKey, error) {
	if len(raw) != sseCustomerKeyRawBytes {
		return ssecKey{}, fmt.Errorf(
			"SSE-C key must be exactly %d raw bytes (AES-256), got %d — generate one with `openssl rand 32`",
			sseCustomerKeyRawBytes, len(raw))
	}
	sum := md5.Sum(raw) //nolint:gosec // required by the S3 SSE-C contract
	return ssecKey{
		b64:    base64.StdEncoding.EncodeToString(raw),
		md5B64: base64.StdEncoding.EncodeToString(sum[:]),
	}, nil
}

// wantsSSEC reports whether a request addresses an OBJECT and uses a verb that
// carries object content, which is the only place SSE-C headers belong.
//
// Deliberately conservative. Bucket-level operations (ListObjectsV2,
// GetBucketLocation, the multi-object DeleteObjects POST) and DELETE were verified
// to work WITHOUT these headers, and were never verified to work WITH them —
// adding headers to a request shape nobody tested is how a proxy turns a working
// listing into a 400 that looks like a bucket problem. Object DELETE needs no key
// either: removing ciphertext does not require reading it.
func wantsSSEC(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost:
	default:
		return false
	}
	return objectKeyFromPath(path) != ""
}

// objectKeyFromPath extracts the object key from a path-style S3 request
// (/<bucket>/<key...>), returning "" for bucket-level or root requests.
//
// Path-style is the only shape this sees: apl-core renders
// `regionendpoint: https://<region>.linodeobjects.com` for Harbor and the same
// host for Loki, and the S3 clients in this platform address buckets by path
// (REGISTRY_STORAGE_S3_FORCEPATHSTYLE / s3ForcePathStyle). A virtual-hosted
// request would carry the bucket in the Host header and arrive here as /<key>,
// which this would read as a bucket-level request and skip — failing CLOSED
// (unencrypted) rather than breaking, so it is asserted by the proxy at startup
// instead of guessed at here.
func objectKeyFromPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	slash := strings.IndexByte(trimmed, '/')
	if slash < 0 || slash == len(trimmed)-1 {
		return "" // "/bucket", "/bucket/" or "/" — no object
	}
	return trimmed[slash+1:]
}

// injectSSEC applies the encryption headers to one outbound request. It reports
// whether it injected, so the proxy can count encrypted vs passed-through
// requests rather than asserting a posture it has not measured.
//
// Client-supplied SSE-C headers are OVERWRITTEN, not merged. The key is the
// proxy's to choose: a client that pinned its own would write objects this
// deployment could never read back, and one that echoed a key it should not have
// would be worse. Set() (not Add()) is what makes that true.
func injectSSEC(h http.Header, method, path string, key ssecKey) bool {
	if !wantsSSEC(method, path) {
		// Strip anything a client sent on a shape we deliberately leave alone, so
		// a stray header cannot reach the upstream unreviewed.
		stripSSEC(h)
		return false
	}
	h.Set(hdrSSECAlgorithm, sseCustomerAlgorithm)
	h.Set(hdrSSECKey, key.b64)
	h.Set(hdrSSECKeyMD5, key.md5B64)

	// A server-side COPY reads an ENCRYPTED source, and the source needs its own
	// key trio. Without these the request fails 400 even though the destination
	// headers above are present and correct.
	if h.Get(hdrCopySource) != "" {
		h.Set(hdrCopySrcSSECAlgo, sseCustomerAlgorithm)
		h.Set(hdrCopySrcSSECKey, key.b64)
		h.Set(hdrCopySrcSSECKeyMD5, key.md5B64)
	} else {
		h.Del(hdrCopySrcSSECAlgo)
		h.Del(hdrCopySrcSSECKey)
		h.Del(hdrCopySrcSSECKeyMD5)
	}
	return true
}

// stripSSEC removes every SSE-C header from a request.
func stripSSEC(h http.Header) {
	for _, k := range []string{
		hdrSSECAlgorithm, hdrSSECKey, hdrSSECKeyMD5,
		hdrCopySrcSSECAlgo, hdrCopySrcSSECKey, hdrCopySrcSSECKeyMD5,
	} {
		h.Del(k)
	}
}

// redactSSEC removes the KEY headers (not the algorithm/MD5 markers) from a header
// set destined for a log line. The key is the whole secret: with it, every object in
// the bucket is readable; without it, Linode cannot recover them at all.
//
// NOTHING IN THE PROXY LOGS HEADERS TODAY — this is reached only from tests, and
// that is deliberate rather than an oversight. The proxy's ErrorHandler prints
// method and path and nothing else, so there is no live path to protect. This
// exists so that the first person to add request logging has the safe spelling
// already sitting here, instead of reaching for `%v` on an http.Header that carries
// the one unrecoverable secret in the system.
func redactSSEC(h http.Header) http.Header {
	out := h.Clone()
	for _, k := range []string{hdrSSECKey, hdrCopySrcSSECKey} {
		if out.Get(k) != "" {
			out.Set(k, "<redacted>")
		}
	}
	return out
}
