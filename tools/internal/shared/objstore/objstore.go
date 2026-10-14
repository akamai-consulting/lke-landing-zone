// Package objstore is the S3 wire layer: signed requests, listing, and a
// write/read-back round trip. It sits directly on internal/shared/s3sig, which
// does the SigV4 signing and nothing else.
//
// IT CAME OUT OF internal/extensions/objenc, WHICH IS WHERE IT WAS WRITTEN AND NOT
// WHERE IT BELONGED, and the split is the clearest instance of a rule this repo
// had been breaking 41 extensions deep: nothing in extensions/ should be imported
// by extensions/. `objenc` was imported by four peers -- assert-objstore,
// assert-observability, credential-rotate and teardown -- which reads as a popular
// capability and was nothing of the kind. Three of the four used ONLY this: how to
// escape an S3 path, how to sign a request, how to walk a LIST, how to prove a
// bucket is writable. None of them wanted object ENCRYPTION, which is what objenc
// is about.
//
// THE COST OF LEAVING IT WAS NOT STYLISTIC. The model says `Always` is a default
// rather than a constant -- an instance with no object storage must be able to
// turn `assert-objstore` off. While assert-objstore imported objenc for a path
// escaper, turning object storage off was a compile-time impossibility rather than
// a configuration flag, and the headline promise of the declaration model was
// undeliverable for reasons no declaration recorded.
//
// WHAT STAYED BEHIND is the part that is genuinely about encryption: the SSE-C
// verdict (`is this object encrypted`), the re-signing proxy, and the Harbor blob
// walk. Those ask a question ABOUT objects; this package only knows how to talk to
// the store.
//
// TWO QUESTIONS, NOT ONE, and the distinction came across with the code because
// it is the reason both halves exist. S3SignedRequest signs a HEAD to answer "is
// this key valid?", and deliberately treats NoSuchBucket and AccessDenied as proof
// the key authenticated -- exactly right for a credential check, and exactly wrong
// for "can Loki write its chunks?", where a bucket that 404s and a bucket that
// refuses writes are both fatal to the consumer and both pass that classification.
// S3ObjectRoundTrip is the second question: PUT, read back, compare, delete. They
// share the signing chain rather than re-deriving it.
//
// TWO SEAMS ARE EXPORTED THAT WERE NOT BEFORE -- S3SignedRequest and
// S3EscapeQueryComponent. Both have real non-test callers left behind in objenc's
// proxy re-signer, so this is a seam crossing a package line, not an export bought
// to satisfy a test.
package objstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/s3sig"
)

// S3SignedRequest issues a SigV4-signed request and returns status + body. Kept
// separate from s3BucketProbe (which only ever HEADs a bucket root) so object-level
// probes do not have to re-derive the signing chain.
// S3EscapePath URI-encodes a path the way SigV4 requires: every byte outside the
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
func S3EscapePath(path string) string {
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

var S3SignedRequest = func(method, accessKey, secretKey, endpoint, path, query string) (int, string, error) {
	host := s3sig.Host(endpoint)
	region := s3sig.Region(endpoint)

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := s3sig.SHA256Hex("")

	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	// The SAME escaped form is signed and sent. Deriving them separately is how a
	// canonical request drifts from the wire request.
	escapedPath := S3EscapePath(path)
	canonicalRequest := strings.Join([]string{
		method, escapedPath, query, canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, s3sig.SHA256Hex(canonicalRequest),
	}, "\n")
	signature := hex.EncodeToString(s3sig.HMACSHA256(s3sig.SigningKey(secretKey, dateStamp, region, "s3"), stringToSign))
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

// One <Contents> entry: the key and when it was written. LastModified is what lets
// the caller tell "written since the gateway went live" from "predates it", which is
// the difference between a breach and a bucket with history.
var s3ListEntryRe = regexp.MustCompile(`(?s)<Key>([^<]+)</Key>.*?<LastModified>([^<]+)</LastModified>`)

var s3ListTokenRe = regexp.MustCompile(`<NextContinuationToken>([^<]+)</NextContinuationToken>`)

// ObjectRef is one listed object.
type ObjectRef struct {
	Key          string
	LastModified time.Time
	// Bucket is filled by the CALLER, not the lister — one sample can span several
	// buckets, and a finding that cannot name which one is not actionable.
	Bucket string
}

// s3SamplePageCap bounds the LIST walk. Ten pages of 1000 is enough to find recent
// writes in any bucket this gate looks at, and bounded so a large bucket cannot turn
// one check into thousands of requests. The bound is reported, never silent.
const s3SamplePageCap = 10

// SampleObjectKeys returns up to max object keys from the bucket.
//
// SAMPLING PROVES FAILURE, NOT SUCCESS — and the first revision of this got that
// backwards by fetching exactly ONE key. That is adequate for a bucket that should
// be uniformly encrypted from its first write, and badly misleading for a bucket
// mid-migration: with a thousand plaintext objects among ten thousand, a
// single-object sample reports color.Green nine times out of ten.
//
// So the caller samples a set and fails on ANY plaintext in it. A clean sample is
// evidence, not proof; a dirty one is proof. That asymmetry is stated in the
// finding text rather than hidden, because the number that matters to an auditor
// is "how many did you look at", and a checker that implies full coverage from a
// sample is worse than one that admits the bound.
var SampleObjectKeys = func(accessKey, secretKey, endpoint, bucket string, max int) ([]ObjectRef, error) {
	if max < 1 {
		max = 1
	}
	var refs []ObjectRef
	token := ""
	for page := 0; page < s3SamplePageCap; page++ {
		query := "list-type=2&max-keys=1000"
		if token != "" {
			// Canonical query order is by key, and `continuation-token` sorts before
			// `list-type`/`max-keys` — send it in the order it is signed or SigV4 fails.
			//
			// RFC3986 escaping, not url.QueryEscape: the two agree on the base64
			// alphabet a continuation token actually uses (+, /, =) and disagree on a
			// space, which QueryEscape renders `+` where SigV4 demands %20. Today's
			// tokens contain no spaces, so this was correct by the luck of the
			// alphabet rather than by construction — and a signature that is right
			// only for the inputs seen so far is the kind that breaks on a bucket big
			// enough to need the second page.
			query = "continuation-token=" + S3EscapeQueryComponent(token) + "&" + query
		}
		code, body, err := S3SignedRequest(http.MethodGet, accessKey, secretKey, endpoint, "/"+bucket, query)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, fmt.Errorf("listing %s returned HTTP %d (%s)", bucket, code, s3sig.ErrorCode(body))
		}
		for _, m := range s3ListEntryRe.FindAllStringSubmatch(body, -1) {
			ts, err := time.Parse(time.RFC3339, m[2])
			if err != nil {
				// An unparseable timestamp must not silently become the zero time —
				// that would sort it oldest and quietly exclude it from the sample.
				// Treat it as brand new so it IS judged.
				ts = time.Now()
			}
			refs = append(refs, ObjectRef{Key: m[1], LastModified: ts})
		}
		t := s3ListTokenRe.FindStringSubmatch(body)
		if t == nil {
			break
		}
		token = t[1]
	}
	// NEWEST FIRST, then take max. Plain LIST order is lexicographic, which for a
	// bucket with history means the sample is drawn entirely from the oldest keys —
	// the ones written before the gateway existed. Sampling those and calling them a
	// breach is how this check reported PLAINTEXT on a cluster where every write
	// since the cutover was correctly encrypted.
	sort.Slice(refs, func(i, j int) bool { return refs[i].LastModified.After(refs[j].LastModified) })
	if len(refs) > max {
		refs = refs[:max]
	}
	return refs, nil
}

// S3CanonicalQuery renders a query string in SigV4 canonical form: every parameter
// URI-encoded, sorted by name, valueless keys kept as `k=`.
//
// net/url is not usable here — url.Values.Encode() sorts correctly but escapes with
// QueryEscape, which encodes a space as `+` where SigV4 requires `%20`.
func S3CanonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	type kv struct{ k, v string }
	var pairs []kv
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		dk, err := url.QueryUnescape(k)
		if err != nil {
			dk = k
		}
		dv, err := url.QueryUnescape(v)
		if err != nil {
			dv = v
		}
		pairs = append(pairs, kv{S3EscapeQueryComponent(dk), S3EscapeQueryComponent(dv)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.k+"="+p.v)
	}
	return strings.Join(out, "&")
}

// S3EscapeQueryComponent is RFC3986 escaping: unreserved characters pass, everything
// else becomes %XX. Unlike the path escaper, `/` is NOT exempt in a query component.
func S3EscapeQueryComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// S3ObjectRequest performs one SigV4-signed request against <endpoint>/<bucket>/<key>
// and returns the status, the S3 error code, and the body.
//
// Package var so the round-trip logic is testable without network.
var S3ObjectRequest = func(method, accessKey, secretKey, endpoint, bucket, key string, payload []byte) (int, string, []byte, error) {
	host := s3sig.Host(endpoint)
	region := s3sig.Region(endpoint)

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := s3sig.SHA256Hex(string(payload))

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
		"AWS4-HMAC-SHA256", amzDate, scope, s3sig.SHA256Hex(canonicalRequest),
	}, "\n")
	signature := hex.EncodeToString(s3sig.HMACSHA256(s3sig.SigningKey(secretKey, dateStamp, region, "s3"), stringToSign))
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
		s3c = s3sig.ErrorCode(string(respBody))
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

// S3ObjectRoundTrip writes a probe object, reads it back, verifies the bytes, and
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
func S3ObjectRoundTrip(accessKey, secretKey, endpoint, bucket, key string, payload []byte) s3RoundTripResult {
	var r s3RoundTripResult

	code, s3c, _, err := S3ObjectRequest(http.MethodPut, accessKey, secretKey, endpoint, bucket, key, payload)
	switch {
	case err != nil:
		r.FailWhy = fmt.Sprintf("PUT %s/%s failed at the transport (%v) — the endpoint is unreachable from here", bucket, key, err)
		return r
	case code < 200 || code >= 300:
		r.FailWhy = fmt.Sprintf("PUT %s/%s returned HTTP %d%s — %s",
			bucket, key, code, s3CodeSuffix(s3c), ExplainS3Write(code, s3c, bucket, endpoint))
		return r
	}
	r.Wrote = true

	code, s3c, got, err := S3ObjectRequest(http.MethodGet, accessKey, secretKey, endpoint, bucket, key, nil)
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

	if code, _, _, err := S3ObjectRequest(http.MethodDelete, accessKey, secretKey, endpoint, bucket, key, nil); err == nil && code < 300 {
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

// ExplainS3Write turns an S3 write failure into the thing to go and check.
func ExplainS3Write(code int, s3Code, bucket, endpoint string) string {
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
