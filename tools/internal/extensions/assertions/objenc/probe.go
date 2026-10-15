package objenc

// probe.go — is this object actually encrypted?
//
// S3 SAYS SO ON HEAD. An object stored under server-side encryption answers a HEAD
// with `x-amz-server-side-encryption: AES256` (SSE-S3); a plaintext object answers
// with no such header at all. So one HEAD, signed with the consumer's own key,
// separates the two states — and it needs no key material beyond the read
// credential the consumer already holds.
//
// Linode Object Storage applies SSE-S3 BY DEFAULT on E2 and E3 endpoints (rolled out
// 6 Oct – 16 Nov 2026). Nothing the writer sends turns it on, which is exactly why
// this is checked on objects rather than on config: there is no config to check.
// E0/E1 endpoints are not covered, and objects written before enablement stay
// plaintext until rewritten.
//
// A 400 on a HEAD with valid credentials is what an SSE-C object answers when the
// caller withholds its key. LLZ no longer writes SSE-C, but buckets that ran the
// retired obj-proxy gateway still hold such objects, and they are encrypted — so
// they classify as such rather than as unknown.

import (
	"fmt"
	"net/http"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/s3sig"
)

// sseVerdict is what the probe learned about one object.
type sseVerdict int

const (
	sseEncrypted   sseVerdict = iota // 200 with x-amz-server-side-encryption set
	ssePlaintext                     // 200 with no SSE header — stored in the clear
	sseCustomerKey                   // 400 — an SSE-C object written by the retired gateway
	sseAbsent                        // 404 — nothing there to judge
	sseUnknown                       // anything else: auth failure, network, 5xx
)

func (v sseVerdict) String() string {
	switch v {
	case sseEncrypted:
		return "encrypted"
	case ssePlaintext:
		return "PLAINTEXT"
	case sseCustomerKey:
		return "customer-key"
	case sseAbsent:
		return "absent"
	default:
		return "unknown"
	}
}

// sseHeader is where S3 reports an object's at-rest encryption on HEAD.
const sseHeader = "x-amz-server-side-encryption"

// ObjectSSEProbe HEADs one object with the consumer's credential and classifies the answer.
var ObjectSSEProbe = func(accessKey, secretKey, endpoint, bucket, key string) (sseVerdict, string) {
	code, hdr, body, err := objstore.S3HeadObject(accessKey, secretKey, endpoint, bucket, key)
	if err != nil {
		return sseUnknown, err.Error()
	}
	return classifySSE(code, hdr, body)
}

// classifySSE is the pure half of the probe, so the verdict table is testable
// without a network.
func classifySSE(code int, hdr http.Header, body string) (sseVerdict, string) {
	switch code {
	case http.StatusOK:
		if alg := hdr.Get(sseHeader); alg != "" {
			return sseEncrypted, "HTTP 200, " + sseHeader + ": " + alg
		}
		return ssePlaintext, "HTTP 200 with no " + sseHeader + " header — the object is stored in the clear"
	case http.StatusBadRequest:
		return sseCustomerKey, "HTTP 400 — an SSE-C object, readable only with the customer key"
	case http.StatusNotFound:
		return sseAbsent, "HTTP 404"
	default:
		return sseUnknown, fmt.Sprintf("HTTP %d (%s)", code, s3sig.ErrorCode(body))
	}
}
