package main

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
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
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

// s3SignedRequest issues a SigV4-signed request and returns status + body. Kept
// separate from s3BucketProbe (which only ever HEADs a bucket root) so object-level
// probes do not have to re-derive the signing chain.
// s3EscapePath URI-encodes a path the way SigV4 requires: every byte outside the
// unreserved set percent-encoded, with `/` preserved as the segment separator.
//
// Concatenating the raw path (the first revision) is correct only while every key
// happens to be hex and slashes. A key with a space, a `+`, a `%` or any non-ASCII
// byte produces a canonical request that does not match what is sent, the signature
// fails, and this probe reports "could not classify" — which reads as an ENCRYPTION
// problem on a gate whose whole job is to answer that question. The bug would be in
// the checker, and the finding would point at the proxy.
//
// Go's own url escaping is not usable here: it leaves `$&+,;=:@` unescaped in paths,
// while SigV4's canonical form requires them encoded.
func s3EscapePath(path string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		ch := path[i]
		switch {
		case ch == '/':
			b.WriteByte(ch)
		case strings.IndexByte(unreserved, ch) >= 0:
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

var s3SignedRequest = func(method, accessKey, secretKey, endpoint, path, query string) (int, string, error) {
	host := s3Host(endpoint)
	region := s3Region(endpoint)

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256Hex("")

	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	// The SAME escaped form is signed and sent. Deriving them separately is how a
	// canonical request drifts from the wire request.
	escapedPath := s3EscapePath(path)
	canonicalRequest := strings.Join([]string{
		method, escapedPath, query, canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex(canonicalRequest),
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(sigV4SigningKey(secretKey, dateStamp, region, "s3"), stringToSign))
	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://"+host, nil)
	if err != nil {
		return 0, "", err
	}
	// Path carries the decoded form and RawPath the escaped one. net/url emits
	// RawPath verbatim when it is a valid encoding of Path, so the bytes on the wire
	// are exactly the bytes that were signed — which assigning a pre-escaped string
	// to Path alone would NOT guarantee, since Go would re-escape it by its own rules.
	req.URL.Path = path
	req.URL.RawPath = escapedPath
	req.URL.RawQuery = query
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", auth)

	resp, err := (&http.Client{Timeout: 25 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return resp.StatusCode, string(body), nil
}

var s3ListKeyRe = regexp.MustCompile(`<Key>([^<]+)</Key>`)

// s3SampleObjectKeys returns up to max object keys from the bucket.
//
// SAMPLING PROVES FAILURE, NOT SUCCESS — and the first revision of this got that
// backwards by fetching exactly ONE key. That is adequate for a bucket that should
// be uniformly encrypted from its first write, and badly misleading for a bucket
// mid-migration: with a thousand plaintext objects among ten thousand, a
// single-object sample reports green nine times out of ten.
//
// So the caller samples a set and fails on ANY plaintext in it. A clean sample is
// evidence, not proof; a dirty one is proof. That asymmetry is stated in the
// finding text rather than hidden, because the number that matters to an auditor
// is "how many did you look at", and a checker that implies full coverage from a
// sample is worse than one that admits the bound.
var s3SampleObjectKeys = func(accessKey, secretKey, endpoint, bucket string, max int) ([]string, error) {
	if max < 1 {
		max = 1
	}
	code, body, err := s3SignedRequest(http.MethodGet, accessKey, secretKey, endpoint,
		"/"+bucket, fmt.Sprintf("list-type=2&max-keys=%d", max))
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("listing %s returned HTTP %d (%s)", bucket, code, s3ErrorCode(body))
	}
	var keys []string
	for _, m := range s3ListKeyRe.FindAllStringSubmatch(body, -1) {
		keys = append(keys, m[1])
	}
	return keys, nil
}

// s3ObjectSSECProbe HEADs one object WITHOUT SSE-C headers and classifies the answer.
var s3ObjectSSECProbe = func(accessKey, secretKey, endpoint, bucket, key string) (ssecVerdict, string) {
	code, body, err := s3SignedRequest(http.MethodHead, accessKey, secretKey, endpoint,
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
		return ssecUnknown, fmt.Sprintf("HTTP %d (%s)", code, s3ErrorCode(body))
	}
}
