package teardown

// drain_obj_buckets.go — `llz ci drain-obj-buckets`: empty the data buckets
// WITHOUT deleting them.
//
// WHY IT EXISTS. SSE-C keys are per-object and every cluster mints its own
// (`llz ci seed-ssec-key` generates one whenever OpenBao has none, which on a fresh
// cluster is always). So objects a destroyed cluster left behind are not stale —
// they are permanently unreadable by its successor. Loki's index-gateway does not
// skip an object it cannot decrypt; it fails the whole table and retries, which
// degrades queries far enough to time out callers while every write-path check stays
// color.Green. See #397's follow-up.
//
// WHY EMPTY AND NOT DELETE. Destroying the buckets also works, and it was the first
// attempt. Linode does not release the bucket NAME promptly: a recreate six minutes
// after a successful destroy still failed with
//
//	[400] The bucket 'platform-loki-chunks-e2e' already exists
//
// so deleting them just moves the failure to the next provision. Emptying gets the
// property that matters — no objects from a previous key generation — and leaves
// nothing to re-create.
//
// IT IS DESTRUCTIVE AND SAYS SO. On a real deployment these buckets hold the logs
// and the registry; this is for a lane that owns its data. The workflow gates it
// behind an opt-in input, and this verb additionally refuses without --yes.

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content-MD5 is mandated by the S3 DeleteObjects API
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/harborauth"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/s3sig"
)

func RunDrainObjBuckets(region string, yes bool) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	if !yes {
		return fmt.Errorf("drain-obj-buckets deletes every log chunk and registry blob in %q — pass --yes", region)
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return err
	}
	e, ok := lz.Env(region)
	if !ok {
		return fmt.Errorf("no deployment %q in the spec", region)
	}
	endpoint := clusterspec.ObjEndpointHost(e.Cluster.ObjectStorage.Cluster)
	if endpoint == "" {
		return fmt.Errorf("deployment %q declares no cluster.objectStorage.cluster", region)
	}
	buckets := []string{
		clusterspec.ObjLokiChunksBucket(lz.ObjLabelPrefix(), region),
		clusterspec.ObjHarborRegistryBucket(lz.ObjLabelPrefix(), region),
	}

	// The SAME credential the consumers use, read from the cluster's own Secret, is
	// NOT available here: the cluster is already destroyed when this runs. So it
	// mints a short-lived key scoped to exactly these buckets and revokes it on the
	// way out — the same shape as the drain in the object-storage destroy job.
	ak, sk, cleanup, err := drainObjMintKey(region, endpoint, buckets)
	if err != nil {
		return fmt.Errorf("minting a scoped drain key: %w", err)
	}
	defer cleanup()

	// A FRESHLY MINTED KEY IS NOT IMMEDIATELY USABLE. Linode propagates it to the
	// object-storage gateways asynchronously, so the first request after the mint
	// answers 403 AccessDenied — indistinguishable, from the error alone, from a key
	// scoped to the wrong buckets. Draining by hand hid this: the mint and the first
	// LIST were separate commands seconds apart, and it worked every time.
	//
	// So wait for the key to become usable before concluding anything, and report the
	// wait rather than folding it into a generic failure.
	if err := waitObjKeyUsable(ak, sk, endpoint, buckets); err != nil {
		return fmt.Errorf("the scoped drain key never became usable: %w", err)
	}

	var failed []string
	for _, b := range buckets {
		if b == "" {
			continue
		}
		n, err := drainOneBucket(ak, sk, endpoint, b)
		if err != nil {
			// Report and keep going: leaving ONE bucket full still poisons the next
			// run, so a partial drain must be visible rather than masked by the first
			// failure.
			fmt.Fprintf(os.Stderr, "::warning::draining %s failed after %d object(s): %v\n", b, n, err)
			failed = append(failed, b)
			continue
		}
		fmt.Printf("drained %s: %d object(s) deleted.\n", b, n)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not empty: %s — the next cluster will inherit unreadable objects", strings.Join(failed, ", "))
	}
	return nil
}

// drainObjMintKey mints a short-lived object-storage key scoped to exactly these
// buckets and returns a revoke function. The cluster is already destroyed when this
// runs, so the consumers' own credential is not reachable — and a broad key would
// hand this verb far more reach than "empty these two buckets" needs.
//
// Revocation is deferred by the caller rather than left to the workflow, so the key
// does not outlive the process even when the drain fails.
var drainObjMintKey = func(region, endpoint string, buckets []string) (string, string, func(), error) {
	token := os.Getenv("LINODE_API_TOKEN")
	if token == "" {
		return "", "", nil, fmt.Errorf("LINODE_API_TOKEN must be set")
	}
	objCluster := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"), ".linodeobjects.com")
	var want []string
	for _, b := range buckets {
		if b != "" {
			want = append(want, b)
		}
	}
	api := credrotate.TempObjkeyLinodeClient(token)
	m, err := api.CreateObjectStorageKeyBuckets(context.Background(),
		"llz-drain-"+region, objCluster, want, "read_write")
	if err != nil {
		return "", "", nil, err
	}
	id, _ := cli.AsUint64(m["id"])
	ak, sk := cli.AsString(m["access_key"]), cli.AsString(m["secret_key"])
	if ak == "" || sk == "" {
		return "", "", nil, fmt.Errorf("the Linode API returned no usable key pair")
	}
	return ak, sk, func() {
		if err := api.DeleteObjectStorageKey(context.Background(), id); err != nil {
			// A leaked scoped key is a real finding, not noise: it grants
			// read_write on the data buckets until someone revokes it by hand.
			fmt.Fprintf(os.Stderr, "::warning::could not revoke temporary drain key %d (%v) — revoke it manually\n", id, err)
		}
	}, nil
}

// waitObjKeyUsable polls until a freshly minted key can LIST, or the budget runs
// out. Bounded: a key that is genuinely mis-scoped must fail, not hang.
func waitObjKeyUsable(ak, sk, endpoint string, buckets []string) error {
	var probe string
	for _, b := range buckets {
		if b != "" {
			probe = b
			break
		}
	}
	if probe == "" {
		return nil
	}
	deadline := drainNow().Add(objKeyReadyBudget)
	var last error
	for attempt := 1; ; attempt++ {
		if _, err := objstore.SampleObjectKeys(ak, sk, endpoint, probe, 1); err == nil {
			if attempt > 1 {
				fmt.Printf("drain key became usable after %d attempt(s).\n", attempt)
			}
			return nil
		} else {
			last = err
		}
		if !drainNow().Before(deadline) {
			return fmt.Errorf("after %s: %w", objKeyReadyBudget, last)
		}
		drainSleep(objKeyReadyInterval)
	}
}

var (
	objKeyReadyBudget   = 90 * time.Second
	objKeyReadyInterval = 5 * time.Second
	drainNow            = time.Now
	drainSleep          = time.Sleep
)

// drainOneBucket deletes every object, paging until the bucket reports empty.
// Returns how many it removed.
func drainOneBucket(ak, sk, endpoint, bucket string) (int, error) {
	total, stalled := 0, 0
	for page := 0; page < drainMaxPages; page++ {
		// RETRY THE LIST rather than trusting one answer. A freshly minted key does
		// not reach every object-storage gateway at once, so an authorized key still
		// draws intermittent 403s for a while — observed mid-drain, AFTER a
		// successful readiness probe and after this same key had already deleted
		// objects from the same bucket. Treating the first 403 as final left buckets
		// full and reported "the next cluster will inherit unreadable objects" when
		// the credential was fine.
		refs, err := listWithRetry(ak, sk, endpoint, bucket)
		if err != nil {
			return total, err
		}
		if len(refs) == 0 {
			return total, nil
		}
		keys := make([]string, 0, len(refs))
		for _, r := range refs {
			keys = append(keys, r.Key)
		}
		survived, err := s3DeleteObjects(ak, sk, endpoint, bucket, keys)
		if err != nil {
			return total, err
		}
		deleted := len(keys) - survived
		total += deleted
		if deleted == 0 {
			// Nothing went this round. One stalled round is a transient worth
			// retrying; several in a row means the objects are not going to go, and
			// spinning the full page budget on them only delays the report.
			stalled++
			if stalled >= drainMaxStalledRounds {
				return total, fmt.Errorf("%d object(s) would not delete across %d consecutive attempts",
					survived, stalled)
			}
			drainSleep(objKeyReadyInterval)
			continue
		}
		stalled = 0
	}
	return total, fmt.Errorf("still not empty after %d batches of %d", drainMaxPages, drainBatch)
}

// listWithRetry lists a page, retrying transient authorization failures within a
// bounded budget. A key that is genuinely mis-scoped exhausts it and fails.
func listWithRetry(ak, sk, endpoint, bucket string) ([]objstore.ObjectRef, error) {
	deadline := drainNow().Add(objKeyReadyBudget)
	var last error
	for {
		refs, err := objstore.SampleObjectKeys(ak, sk, endpoint, bucket, drainBatch)
		if err == nil {
			return refs, nil
		}
		last = err
		if !drainNow().Before(deadline) {
			return nil, last
		}
		drainSleep(objKeyReadyInterval)
	}
}

const (
	// drainBatch is S3's DeleteObjects maximum.
	drainBatch = 1000
	// drainMaxStalledRounds is how many consecutive zero-progress passes to tolerate
	// before giving up. Ceph's per-key InternalError clears on a retry; an object that
	// survives several is not transient.
	drainMaxStalledRounds = 5
	// drainMaxPages bounds the loop so a bucket that never reports empty (a delete
	// silently failing, say) fails loudly instead of spinning forever.
	drainMaxPages = 200
)

// s3PostWithBody signs and sends a POST carrying a body, with the Content-MD5 the
// DeleteObjects API requires. Built here rather than folded into s3SignedRequest,
// which is a GET/HEAD probe helper used by the encryption gates — widening it would
// put a body path through code whose whole value is that it is simple enough to
// trust.
var s3PostWithBody = func(ak, sk, endpoint, path, query string, body []byte) (int, string, bool, error) {
	host := s3sig.Host(endpoint)
	region := s3sig.Region(endpoint)
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := s3sig.SHA256Hex(string(body))
	sum := md5.Sum(body) //nolint:gosec // Content-MD5 is required by the DeleteObjects API, not a security choice
	contentMD5 := base64.StdEncoding.EncodeToString(sum[:])

	canonicalHeaders := "content-md5:" + contentMD5 + "\n" +
		"host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "content-md5;host;x-amz-content-sha256;x-amz-date"
	escapedPath := objstore.S3EscapePath(path)
	canonicalRequest := strings.Join([]string{
		"POST", escapedPath, objstore.S3CanonicalQuery(query), canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")
	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, s3sig.SHA256Hex(canonicalRequest),
	}, "\n")
	sig := hex.EncodeToString(s3sig.HMACSHA256(s3sig.SigningKey(sk, dateStamp, region, "s3"), stringToSign))

	req, err := http.NewRequest(http.MethodPost, "https://"+host, bytes.NewReader(body))
	if err != nil {
		return 0, "", false, err
	}
	req.URL.Path = path
	req.URL.RawPath = escapedPath
	req.URL.RawQuery = query
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-MD5", contentMD5)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		ak, scope, signedHeaders, sig))

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, "", false, err
	}
	defer resp.Body.Close()
	// READ LIMIT + TRUNCATION SIGNAL, because one caller COUNTS in this body.
	//
	// The cap was 16 KiB and the response was treated as complete. s3DeleteObjects
	// derives its survivor count from `strings.Count(body, "<Error>")`, and a
	// DeleteObjects response reporting all 1000 keys failed runs to roughly 200 KB —
	// so a batch in which NOTHING was deleted was read as about 80 failures, i.e.
	// ~920 deleted. `deleted == 0` never tripped, the stall detector never fired,
	// the running total was inflated by objects that are still there, and the drain
	// ground through all 200 pages before failing with "still not empty", which
	// names the wrong problem.
	//
	// 1 MiB comfortably holds a full 1000-key failure report; reading one byte past
	// it is how the caller learns the body did NOT fit, so it can decline to count
	// rather than count wrong.
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, s3ResponseReadLimit+1))
	truncated := len(rb) > s3ResponseReadLimit
	if truncated {
		rb = rb[:s3ResponseReadLimit]
	}
	return resp.StatusCode, string(rb), truncated, nil
}

// s3ResponseReadLimit bounds how much of an S3 response body is read. See
// s3PostWithBody for why the number and the truncation flag both matter.
const s3ResponseReadLimit = 1 << 20

// s3DeleteObjects issues a bulk DeleteObjects. Content-MD5 is REQUIRED by the API
// for this call — omit it and S3 answers 400 rather than deleting anything.
var s3DeleteObjects = func(ak, sk, endpoint, bucket string, keys []string) (int, error) {
	type object struct {
		Key string `xml:"Key"`
	}
	payload := struct {
		XMLName xml.Name `xml:"Delete"`
		Quiet   bool     `xml:"Quiet"`
		Objects []object `xml:"Object"`
	}{Quiet: true}
	for _, k := range keys {
		payload.Objects = append(payload.Objects, object{Key: k})
	}
	// Marshal rather than concatenate: a key containing & or < would otherwise
	// produce XML that deletes the wrong thing, or nothing.
	body, err := xml.Marshal(payload)
	if err != nil {
		return 0, err
	}
	code, respBody, truncated, err := s3PostWithBody(ak, sk, endpoint, "/"+bucket, "delete=", body)
	if err != nil {
		return 0, err
	}
	if code < 200 || code >= 300 {
		return 0, fmt.Errorf("DeleteObjects returned HTTP %d: %s", code, harborauth.TruncateForError([]byte(respBody)))
	}
	// A BODY WE DID NOT FULLY READ CANNOT BE COUNTED, and counting it anyway is how
	// a batch that deleted nothing reported ~920 deletions. Report every key as a
	// survivor: the caller's next LIST sees whatever is genuinely still there, and
	// `deleted == 0` trips the stall detector on the round it actually stalled
	// instead of after the full page budget.
	if truncated {
		return len(keys), nil
	}
	// PER-KEY FAILURES ARE NOT A DEAD END. Quiet mode reports only failures, and Ceph
	// returns a transient InternalError for individual keys under load — observed
	// mid-drain on a bucket whose sibling emptied cleanly with the same credential.
	// Treating any <Error> as fatal aborted the whole drain on one unlucky object and
	// reported that the next cluster would inherit unreadable data, when a second
	// attempt would have removed it.
	//
	// The survivors are reported to the caller instead, whose LIST/delete loop sees
	// them again on the next pass and retries them. Only a bucket that stops making
	// progress fails.
	// PARSED, NOT COUNTED. `strings.Count(body, "<Error>")` also counts the string
	// wherever it appears — including inside a <Message> that quotes it — and gives
	// no way to tell a body it could not understand from one reporting no failures.
	// Unmarshalling separates those: a body that does not parse is not evidence
	// that every key was deleted.
	var result struct {
		XMLName xml.Name `xml:"DeleteResult"`
		Errors  []struct {
			Key  string `xml:"Key"`
			Code string `xml:"Code"`
		} `xml:"Error"`
	}
	if err := xml.Unmarshal([]byte(respBody), &result); err != nil {
		return len(keys), nil
	}
	return len(result.Errors), nil
}
