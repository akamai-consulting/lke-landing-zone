package objenc

// s3_ssec_probe.go — is this object actually encrypted?
//
// THE TRICK, and why this needs no key. An SSE-C object cannot be read without its
// key: Linode answers a keyless GET/HEAD with 400. So a HEAD carrying NO SSE-C
// headers separates the two states by itself —
//
//	400  the object is encrypted (the server is demanding the key we withheld)
//	200  the object is PLAINTEXT (it handed over metadata to an unkeyed caller)
//
// which means the gate that proves encryption never has to hold the encryption key.
// That matters more than it looks: this check runs in CI, and a check that needed
// the SSE-C key would put the one unrecoverable secret in this system onto a runner
// in order to assert a property that can be observed without it.
//
// Measured against Linode (us-ord-10, scratch bucket, since deleted):
//   - PUT with SSE-C then HEAD with the key      → 200
//   - the same object HEAD with no key           → 400
//   - a plaintext object HEAD with no key        → 200

import (
	"fmt"
	"net/http"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/s3sig"
)

// ssecVerdict is what the probe learned about one object.
type ssecVerdict int

const (
	ssecEncrypted ssecVerdict = iota // 400 — the server demanded a key
	ssecPlaintext                    // 200 — readable with no key at all
	ssecAbsent                       // 404 — nothing there to judge
	ssecUnknown                      // anything else: auth failure, network, 5xx
)

func (v ssecVerdict) String() string {
	switch v {
	case ssecEncrypted:
		return "encrypted"
	case ssecPlaintext:
		return "PLAINTEXT"
	case ssecAbsent:
		return "absent"
	default:
		return "unknown"
	}
}

// ObjectSSECProbe HEADs one object WITHOUT SSE-C headers and classifies the answer.
var ObjectSSECProbe = func(accessKey, secretKey, endpoint, bucket, key string) (ssecVerdict, string) {
	code, body, err := objstore.S3SignedRequest(http.MethodHead, accessKey, secretKey, endpoint,
		"/"+bucket+"/"+key, "")
	if err != nil {
		return ssecUnknown, err.Error()
	}
	switch code {
	case http.StatusBadRequest:
		// The server refused to serve it to a caller with no key.
		return ssecEncrypted, "HTTP 400 — the server demanded the SSE-C key we deliberately withheld"
	case http.StatusOK:
		return ssecPlaintext, "HTTP 200 — the object was served to a caller holding NO encryption key"
	case http.StatusNotFound:
		return ssecAbsent, "HTTP 404"
	default:
		return ssecUnknown, fmt.Sprintf("HTTP %d (%s)", code, s3sig.ErrorCode(body))
	}
}
