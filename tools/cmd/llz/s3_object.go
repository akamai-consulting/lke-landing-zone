package main

// s3_object.go — a SigV4-signed PUT / GET / DELETE of a single object, so a gate
// can prove a bucket is WRITABLE by the credential its consumer actually holds.
//
// s3_probe.go already signs a HEAD to answer "is this key valid?". That is a
// question about the CREDENTIAL, and it deliberately treats NoSuchBucket and
// AccessDenied as proof the key authenticated. Exactly the right call there, and
// exactly the wrong one for asking "can Loki write its chunks?" — a bucket that
// 404s and a bucket that refuses writes are both fatal to the consumer and both
// pass that classification.
//
// So this is a different question with a different verdict, sharing the signing
// chain rather than re-deriving it.

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// s3ObjectRequest performs one SigV4-signed request against <endpoint>/<bucket>/<key>
// and returns the status, the S3 error code, and the body.
//
// Package var so the round-trip logic is testable without network.
var s3ObjectRequest = func(method, accessKey, secretKey, endpoint, bucket, key string, payload []byte) (int, string, []byte, error) {
	host := s3Host(endpoint)
	region := s3Region(endpoint)

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256Hex(string(payload))

	canonicalURI := "/" + bucket + "/" + key
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		method, canonicalURI, "", canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex(canonicalRequest),
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(sigV4SigningKey(secretKey, dateStamp, region, "s3"), stringToSign))
	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+host+canonicalURI, body)
	if err != nil {
		return 0, "", nil, err
	}
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", auth)
	if payload != nil {
		req.ContentLength = int64(len(payload))
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	s3c := resp.Header.Get("x-amz-error-code")
	if s3c == "" && len(respBody) > 0 {
		s3c = s3ErrorCode(string(respBody))
	}
	return resp.StatusCode, s3c, respBody, nil
}

// s3RoundTripResult is the outcome of a write→read→delete cycle.
type s3RoundTripResult struct {
	Wrote    bool
	ReadBack bool
	Cleaned  bool
	FailWhy  string
}

// OK reports a proven writable bucket.
func (r s3RoundTripResult) OK() bool { return r.Wrote && r.ReadBack }

// s3ObjectRoundTrip writes a probe object, reads it back, verifies the bytes, and
// deletes it.
//
// READING BACK IS NOT REDUNDANT. A PUT that returns 200 against the wrong
// endpoint is the whole failure this exists to catch: Linode's gen-1 and gen-2
// object storage are DISJOINT namespaces reachable at different hosts, so a
// bucket can accept a write on one while every consumer 404s against the other.
// Fetching the exact bytes back through the same endpoint is what distinguishes
// "the write landed where the consumer will look" from "a write landed
// somewhere".
//
// Cleanup is best-effort: a probe object left behind is a few bytes of litter,
// while failing the gate because a DELETE did not land would report a write
// problem that is not one.
func s3ObjectRoundTrip(accessKey, secretKey, endpoint, bucket, key string, payload []byte) s3RoundTripResult {
	var r s3RoundTripResult

	code, s3c, _, err := s3ObjectRequest(http.MethodPut, accessKey, secretKey, endpoint, bucket, key, payload)
	switch {
	case err != nil:
		r.FailWhy = fmt.Sprintf("PUT %s/%s failed at the transport (%v) — the endpoint is unreachable from here", bucket, key, err)
		return r
	case code < 200 || code >= 300:
		r.FailWhy = fmt.Sprintf("PUT %s/%s returned HTTP %d%s — %s",
			bucket, key, code, s3CodeSuffix(s3c), explainS3Write(code, s3c, bucket, endpoint))
		return r
	}
	r.Wrote = true

	code, s3c, got, err := s3ObjectRequest(http.MethodGet, accessKey, secretKey, endpoint, bucket, key, nil)
	switch {
	case err != nil:
		r.FailWhy = fmt.Sprintf("the object was written but GET %s/%s failed at the transport (%v)", bucket, key, err)
		return r
	case code < 200 || code >= 300:
		r.FailWhy = fmt.Sprintf("the PUT succeeded but reading it back returned HTTP %d%s. "+
			"A write that cannot be read at the SAME endpoint is the disjoint-namespace failure: Linode's "+
			"object-storage generations are separate namespaces on different hosts, so the bucket can accept "+
			"a write on one while every consumer looks at the other", code, s3CodeSuffix(s3c))
		return r
	case !bytes.Equal(got, payload):
		r.FailWhy = fmt.Sprintf("the object read back from %s/%s does not match what was written "+
			"(%d bytes out, %d back) — this endpoint is not serving the object that was just stored",
			bucket, key, len(payload), len(got))
		return r
	}
	r.ReadBack = true

	if code, _, _, err := s3ObjectRequest(http.MethodDelete, accessKey, secretKey, endpoint, bucket, key, nil); err == nil && code < 300 {
		r.Cleaned = true
	}
	return r
}

func s3CodeSuffix(s3Code string) string {
	if s3Code == "" {
		return ""
	}
	return " (" + s3Code + ")"
}

// explainS3Write turns an S3 write failure into the thing to go and check.
func explainS3Write(code int, s3Code, bucket, endpoint string) string {
	switch s3Code {
	case "NoSuchBucket":
		return fmt.Sprintf("bucket %q does not exist AT %s. It may exist at a DIFFERENT endpoint — that is the "+
			"obj-cluster split: a full cluster id like us-ord-10 stripped to the region us-ord puts the bucket on "+
			"gen-1 while consumers address gen-2, and a bucket census by label passes throughout because it asks "+
			"the Linode API rather than this endpoint", bucket, endpoint)
	case "AccessDenied":
		return "the credential authenticated but is not permitted to write here — check the object-storage key's " +
			"bucket access, which is scoped per bucket and is not implied by the key existing"
	case "InvalidAccessKeyId", "SignatureDoesNotMatch":
		return "the credential itself is rejected — it has been revoked or rotated out from under this consumer " +
			"without the Secret being re-synced"
	case "":
		if code == 301 || code == 307 {
			return "the endpoint redirected, which means this bucket lives in a different region/generation than " +
				"the host it was addressed at"
		}
	}
	return "check the bucket, the endpoint and the key's permissions"
}
