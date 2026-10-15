package baosnap

// store.go — the backup bucket, over SigV4.
//
// WHY NOT internal/shared/objstore AS-IS. Its two request helpers were built for
// probes, and both truncate: S3SignedRequest reads at most 8 KiB of a response and
// S3ObjectRequest at most 1 MiB. A LIST of a month of snapshots is past 8 KiB, and
// a snapshot is past 1 MiB — a truncated listing would make retention and restore
// see a subset of the bucket and report it as the whole thing. The signing chain
// (s3sig) and the path/query escapers (objstore) are reused verbatim; only the
// body handling is new.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/s3sig"
)

// Store is one bucket under one key.
type Store struct {
	AccessKey, SecretKey string
	// Endpoint is the OBJ endpoint: a bare host (`us-ord-10.linodeobjects.com`, as
	// the backup secret stores it) or a URL. A bare host means https.
	Endpoint string
	Bucket   string
	// VirtualHosted addresses the bucket as <bucket>.<endpoint> instead of
	// <endpoint>/<bucket>. The in-cluster producer sets it, and the reason is
	// load-bearing: the objProxy SSE-C gateway is switched on by a CoreDNS rewrite
	// of the EXACT endpoint host, cluster-wide. Path-style traffic from the backup
	// pod would be captured by it and every snapshot written SSE-C under a key that
	// lives in OpenBao — so a lost OpenBao would take the means of reading its own
	// backups with it. The virtual-hosted name is not rewritten, so backups go
	// straight to Linode whether or not the gateway is on. (They need no SSE-C: a
	// snapshot is already ciphertext under the barrier.) Bucket labels here are
	// dot-free by validation, so the endpoint's wildcard certificate covers them.
	VirtualHosted bool
	// HTTP is the transport; nil means a client with a generous timeout.
	HTTP *http.Client
	// now is a seam for signing-date tests.
	now func() time.Time
}

// listPageCap bounds a LIST walk: 50 pages of 1000 keys is far beyond any
// retention this bucket is configured for, and a bound means a runaway bucket
// is an error rather than an unbounded loop.
const listPageCap = 50

func (s Store) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (s Store) base() (scheme, host string) {
	e := strings.TrimRight(strings.TrimSpace(s.Endpoint), "/")
	scheme = "https"
	if strings.HasPrefix(e, "http://") {
		scheme = "http"
	}
	return scheme, s3sig.Host(e)
}

// do signs and sends one request. body may be nil. The caller owns resp.Body.
func (s Store) do(ctx context.Context, method, key, query string, body []byte) (*http.Response, error) {
	if s.AccessKey == "" || s.SecretKey == "" || s.Bucket == "" || s.Endpoint == "" {
		return nil, fmt.Errorf("backup store is missing its key, bucket or endpoint")
	}
	scheme, host := s.base()
	region := s3sig.Region(s.Endpoint)
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])

	path := "/" + s.Bucket
	if key != "" {
		path += "/" + key
	}
	if s.VirtualHosted {
		host = s.Bucket + "." + host
		path = "/" + key
	}
	escaped := objstore.S3EscapePath(path)
	canonQuery := objstore.S3CanonicalQuery(query)
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		method, escaped, canonQuery, canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")
	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, s3sig.SHA256Hex(canonicalRequest),
	}, "\n")
	sig := hex.EncodeToString(s3sig.HMACSHA256(s3sig.SigningKey(s.SecretKey, dateStamp, region, "s3"), stringToSign))

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, scheme+"://"+host, rdr)
	if err != nil {
		return nil, err
	}
	// The SAME escaped bytes are signed and sent (see objstore.S3SignedRequest).
	req.URL.Path = path
	req.URL.RawPath = escaped
	req.URL.RawQuery = canonQuery
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.AccessKey, scope, signedHeaders, sig))
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	return s.client().Do(req)
}

// errFrom turns a non-2xx response into an error carrying the S3 code, which is
// what tells a wrong key (AccessDenied) from a wrong bucket (NoSuchBucket).
func errFrom(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	code := resp.Header.Get("x-amz-error-code")
	if code == "" {
		code = s3sig.ErrorCode(string(b))
	}
	if code == "" {
		code = "no S3 error code"
	}
	return fmt.Errorf("%s: HTTP %d (%s)", op, resp.StatusCode, code)
}

// Put writes body at key.
func (s Store) Put(ctx context.Context, key string, body []byte) error {
	resp, err := s.do(ctx, http.MethodPut, key, "", body)
	if err != nil {
		return fmt.Errorf("put %s/%s: %w", s.Bucket, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return errFrom("put "+s.Bucket+"/"+key, resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Get reads key, refusing anything over MaxSnapshotBytes.
func (s Store) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.do(ctx, http.MethodGet, key, "", nil)
	if err != nil {
		return nil, fmt.Errorf("get %s/%s: %w", s.Bucket, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, errFrom("get "+s.Bucket+"/"+key, resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxSnapshotBytes+1))
	if err != nil {
		return nil, fmt.Errorf("get %s/%s: %w", s.Bucket, key, err)
	}
	if len(b) > MaxSnapshotBytes {
		return nil, fmt.Errorf("get %s/%s: object exceeds %d bytes", s.Bucket, key, MaxSnapshotBytes)
	}
	return b, nil
}

// HashObject streams key and returns its sha256 and length — the producer's
// read-back, which proves the bucket returns what was written without holding a
// second copy of the snapshot in memory.
func (s Store) HashObject(ctx context.Context, key string) (string, int64, error) {
	resp, err := s.do(ctx, http.MethodGet, key, "", nil)
	if err != nil {
		return "", 0, fmt.Errorf("read back %s/%s: %w", s.Bucket, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", 0, errFrom("read back "+s.Bucket+"/"+key, resp)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, MaxSnapshotBytes+1))
	if err != nil {
		return "", n, fmt.Errorf("read back %s/%s: %w", s.Bucket, key, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// GetStatus issues a plain signed GET for key — no SSE-C headers — and returns
// only the status code and S3 error code, discarding the body. It is the
// consumer-side encryption probe for buckets written SSE-C (the velero bucket):
// an SSE-C object cannot be read without its key, so a 400 proves the object is
// encrypted and a 200 proves it is plaintext. The probe never holds the
// customer key. A transport error is returned as such, never as a status.
func (s Store) GetStatus(ctx context.Context, key string) (int, string, error) {
	resp, err := s.do(ctx, http.MethodGet, key, "", nil)
	if err != nil {
		return 0, "", fmt.Errorf("get %s/%s: %w", s.Bucket, key, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	code := resp.Header.Get("x-amz-error-code")
	if code == "" {
		code = s3sig.ErrorCode(string(b))
	}
	return resp.StatusCode, code, nil
}

// Delete removes key. A 404 is success — the object is gone either way.
func (s Store) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, key, "", nil)
	if err != nil {
		return fmt.Errorf("delete %s/%s: %w", s.Bucket, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return errFrom("delete "+s.Bucket+"/"+key, resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

type listResult struct {
	Contents []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// List returns every object under prefix, walking all pages. A page cap that is
// reached is an ERROR, not a short answer: retention and restore both make
// decisions over "the whole bucket", and a silently partial listing is the
// truncation this file exists to avoid.
func (s Store) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for page := 0; page < listPageCap; page++ {
		q := "list-type=2&max-keys=1000&prefix=" + objstore.S3EscapeQueryComponent(prefix)
		if token != "" {
			q += "&continuation-token=" + objstore.S3EscapeQueryComponent(token)
		}
		resp, err := s.do(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", s.Bucket, err)
		}
		if resp.StatusCode/100 != 2 {
			err := errFrom("list "+s.Bucket, resp)
			resp.Body.Close()
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", s.Bucket, err)
		}
		var lr listResult
		if err := xml.Unmarshal(body, &lr); err != nil {
			// Unparseable is "could not tell", never "empty".
			return nil, fmt.Errorf("list %s: unparseable ListObjectsV2 response: %v", s.Bucket, err)
		}
		for _, c := range lr.Contents {
			t, err := time.Parse(time.RFC3339, c.LastModified)
			if err != nil {
				t = time.Time{}
			}
			out = append(out, Object{Key: c.Key, Size: c.Size, LastModified: t})
		}
		if !lr.IsTruncated {
			return out, nil
		}
		if lr.NextContinuationToken == "" {
			return nil, fmt.Errorf("list %s: truncated page with no continuation token", s.Bucket)
		}
		token = lr.NextContinuationToken
	}
	return nil, fmt.Errorf("list %s: more than %d pages — refusing to act on a partial listing", s.Bucket, listPageCap)
}

// WaitUsable polls a cheap LIST until the key answers, bounded by budget.
//
// A FRESHLY MINTED LINODE KEY IS NOT IMMEDIATELY USABLE: it propagates to the
// object-storage gateways asynchronously, and the first requests answer 403
// AccessDenied — indistinguishable, from the error alone, from a key scoped to the
// wrong bucket (the teardown drain hit this; see drain_obj_buckets.go). Only the
// restore path mints one, so only it calls this.
func (s Store) WaitUsable(ctx context.Context, budget, interval time.Duration) error {
	deadline := time.Now().Add(budget)
	var last error
	for {
		resp, err := s.do(ctx, http.MethodGet, "", "list-type=2&max-keys=1&prefix="+objstore.S3EscapeQueryComponent(Prefix), nil)
		if err == nil {
			if resp.StatusCode/100 == 2 {
				resp.Body.Close()
				return nil
			}
			last = errFrom("list "+s.Bucket, resp)
			resp.Body.Close()
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("key never became usable within %s: %w", budget, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// FieldsToStore builds a Store from the backup secret's field set, naming every
// missing field rather than the first — an operator fixing the secret by hand
// should not have to iterate.
func FieldsToStore(f map[string]string) (Store, error) {
	var missing []string
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "BUCKET", "ENDPOINT"} {
		if strings.TrimSpace(f[k]) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return Store{}, fmt.Errorf("%s is missing %s — it is written by `llz ci mint-bootstrap-objkeys` and rotated by the reconciler's linode-creds lane",
			KeyPath, strings.Join(missing, ", "))
	}
	return Store{
		AccessKey: f["AWS_ACCESS_KEY_ID"], SecretKey: f["AWS_SECRET_ACCESS_KEY"],
		Bucket: f["BUCKET"], Endpoint: f["ENDPOINT"],
	}, nil
}

// Describe is a short human form of a snapshot for listings.
func Describe(s Snapshot, now time.Time) string {
	age := now.Sub(s.Taken).Round(time.Minute)
	size := "?"
	if s.Size > 0 {
		size = strconv.FormatInt(s.Size, 10) + " B"
	}
	return fmt.Sprintf("%s  taken %s (%s ago)  cluster %s  %s",
		s.Key, s.Taken.UTC().Format(time.RFC3339), age, s.ClusterID, size)
}
