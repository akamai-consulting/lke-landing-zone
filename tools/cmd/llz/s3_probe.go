package main

// s3_probe.go — validity probe for the Terraform-state Object Storage keys
// (TF_STATE_ACCESS_KEY / TF_STATE_SECRET_KEY). Unlike the Linode/GitHub/GHCR
// tokens, S3 credentials can't be checked with a plain Bearer GET — they need an
// AWS SigV4-signed request. There's no AWS SDK in this module, so SigV4 is
// implemented here (the signing chain is unit-tested against AWS's documented
// example vector). The probe HEADs the state bucket and classifies by the S3
// error code: only InvalidAccessKeyId / SignatureDoesNotMatch mean the CREDENTIAL
// is bad — a 2xx, a NoSuchBucket, or an AccessDenied all prove the key
// authenticated (the request was signed and accepted; the rest is about the
// resource/permissions, not the key's validity).

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// s3BucketProbe issues a SigV4-signed HEAD of bucket at the OBJ endpoint and
// returns the HTTP status plus the S3 <Code> (from the error body, "" on 2xx).
// Package var so tests exercise probeS3Pair/classifyS3 without network. code 0 =
// unreachable.
var s3BucketProbe = func(accessKey, secretKey, endpoint, bucket string) (code int, s3Code string, err error) {
	host := s3Host(endpoint)
	region := s3Region(endpoint)
	client := &http.Client{Timeout: 20 * time.Second}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256Hex("") // empty body

	canonicalURI := "/" + bucket
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		http.MethodHead, canonicalURI, "", canonicalHeaders, signedHeaders, payloadHash,
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
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+canonicalURI, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", auth)
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	// A HEAD carries no body; fall back to the header S3 sets for the error code.
	s3c := resp.Header.Get("x-amz-error-code")
	if s3c == "" {
		if body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096)); len(body) > 0 {
			s3c = s3ErrorCode(string(body))
		}
	}
	return resp.StatusCode, s3c, nil
}

// probeS3Pair validates the OBJ key pair against its bucket. Both keys, the
// endpoint, and the bucket must be known; otherwise it can't sign a request.
func probeS3Pair(accessKey, secretKey, endpoint, bucket string) tokenValidity {
	const name = "TF_STATE_ACCESS_KEY"
	if accessKey == "" || secretKey == "" {
		return tokenValidity{name, vSkipped, "not cached — gather the OBJ keys locally to probe"}
	}
	if endpoint == "" || bucket == "" {
		return tokenValidity{name, vSkipped, "TF_STATE_ENDPOINT/TF_STATE_BUCKET unknown — can't sign a probe"}
	}
	code, s3Code, err := s3BucketProbe(accessKey, secretKey, endpoint, bucket)
	if err != nil {
		code = 0
	}
	status, detail := classifyS3(code, s3Code)
	return tokenValidity{name, status, detail}
}

// classifyS3 maps an S3 response to a credential-validity verdict. The key
// AUTHENTICATED on 2xx, NoSuchBucket (404), or AccessDenied — those are about the
// bucket/permissions, not the credential. Only InvalidAccessKeyId /
// SignatureDoesNotMatch mean the CREDENTIAL itself is bad. Pure (unit-tested).
func classifyS3(code int, s3Code string) (validityStatus, string) {
	switch {
	case code == 0:
		return vUnreachable, "OBJ endpoint unreachable — could not verify"
	case s3Code == "InvalidAccessKeyId" || s3Code == "SignatureDoesNotMatch":
		return vInvalid, fmt.Sprintf("S3 credentials rejected (%s) — rotate the state-bucket key", s3Code)
	case code/100 == 2:
		return vValid, "valid (authenticates to Object Storage)"
	case code == http.StatusNotFound:
		return vValid, "valid (authenticated; state bucket not found — check TF_STATE_BUCKET)"
	case code == http.StatusForbidden:
		// Authenticated but not authorized for this bucket (a mis-scoped key). The
		// CREDENTIAL is valid; flag the scope as a warning.
		return vWarn, "valid but not authorized for this bucket (AccessDenied) — check the key's bucket scope"
	default:
		return vUnreachable, fmt.Sprintf("unexpected S3 response %d — could not verify", code)
	}
}

// ── SigV4 primitives (unit-tested against AWS's documented example) ────────────

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// sigV4SigningKey derives the AWS SigV4 signing key (the documented HMAC chain).
func sigV4SigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

// s3Host strips the scheme from an OBJ endpoint → us-ord-1.linodeobjects.com.
func s3Host(endpoint string) string {
	h := strings.TrimPrefix(endpoint, "https://")
	h = strings.TrimPrefix(h, "http://")
	return strings.TrimRight(h, "/")
}

// s3Region derives the SigV4 region from a Linode OBJ endpoint: the leading host
// label (us-ord-1.linodeobjects.com → us-ord-1), which is the region Linode's
// S3-compatible gateway signs against. Falls back to us-east-1 (the Ceph default)
// when the host isn't a recognizable linodeobjects.com endpoint.
func s3Region(endpoint string) string {
	h := s3Host(endpoint)
	if i := strings.Index(h, ".linodeobjects.com"); i > 0 {
		return h[:i]
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		return h[:i]
	}
	return "us-east-1"
}

var s3ErrCodeRe = regexp.MustCompile(`<Code>([^<]+)</Code>`)

// s3ErrorCode extracts the <Code> element from an S3 error XML body ("" if none).
func s3ErrorCode(body string) string {
	if m := s3ErrCodeRe.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}
