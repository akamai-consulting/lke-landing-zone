// Package s3sig is the SigV4 signing and endpoint arithmetic every S3 caller in
// this repo shares: the HMAC chain, the derived signing key, the payload hash, and
// how to get a host and a region out of a Linode Object Storage endpoint.
//
// IT BECAME A PACKAGE ON THE NINTH EXTRACTION, when internal/objenc needed it — the
// SSE-C probe signs its own HEAD requests. Before that the helpers were private to
// s3_probe.go and called by four other files in package main by accident of
// proximity.
//
// Region parsing is the part worth having in one place. A Linode endpoint encodes
// its region in the hostname, and getting it wrong produces a signature the server
// rejects with a message about credentials — a failure that reads as "wrong key"
// and is not.
package s3sig

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// SHA256Hex is the payload hash SigV4 signs over. Moved here with the rest of the
// signing chain; regenroot.go used it for an unrelated digest and now imports it.
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}

func HMACSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// SigningKey derives the AWS SigV4 signing key (the documented HMAC chain).
func SigningKey(secret, dateStamp, region, service string) []byte {
	kDate := HMACSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := HMACSHA256(kDate, region)
	kService := HMACSHA256(kRegion, service)
	return HMACSHA256(kService, "aws4_request")
}

// Host strips the scheme from an OBJ endpoint → us-ord-1.linodeobjects.com.
func Host(endpoint string) string {
	h := strings.TrimPrefix(endpoint, "https://")
	h = strings.TrimPrefix(h, "http://")
	return strings.TrimRight(h, "/")
}

// Region derives the SigV4 region from a Linode OBJ endpoint: the leading host
// label (us-ord-1.linodeobjects.com → us-ord-1), which is the region Linode's
// S3-compatible gateway signs against. Falls back to us-east-1 (the Ceph default)
// when the host isn't a recognizable linodeobjects.com endpoint.
func Region(endpoint string) string {
	h := Host(endpoint)
	if i := strings.Index(h, ".linodeobjects.com"); i > 0 {
		return h[:i]
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		return h[:i]
	}
	return "us-east-1"
}

var errCodeRe = regexp.MustCompile(`<Code>([^<]+)</Code>`)

// ErrorCode extracts the <Code> element from an S3 error XML body ("" if none).
func ErrorCode(body string) string {
	if m := errCodeRe.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}
