package objenc

// harbor_blob.go — the check that proves the CA chain, by making
// HARBOR write a blob rather than writing one ourselves.
//
// WHY THIS EXISTS SEPARATELY FROM THE OTHER THREE CHECKS. CheckRegistryPodsCarryCA
// proves the Kyverno mutation LANDED — the env and the volume are on the pod. It
// does not prove the CA in that volume actually validates the proxy's certificate.
// A wrong CA, a cert with the wrong SAN, an expired bundle: every one of those
// leaves the pod looking correctly mutated and every S3 call failing.
//
// And checkObjectsAreEncrypted samples the LOKI bucket, which proves the proxy
// encrypts — but Loki reaches the proxy without any CA work at all (its config
// carries insecure_skip_verify: true). So a color.Green Loki check says nothing about
// whether Harbor can complete the handshake.
//
// The only thing that proves it is Harbor's own registry successfully writing to
// object storage through the proxy. That is what this does: the OCI distribution
// blob-upload dance, ending in a real PUT, after which the blob must be present in
// the registry bucket AND encrypted.
//
// It is deliberately a REAL push and not an upload session that gets cancelled.
// assert-harbor-roundtrip already opens and cancels one — that proves
// authorization, which is a different question, and it moves no bytes to S3. A
// cancelled session never reaches the storage driver, so it would prove nothing
// here.
//
// WHAT IT LEAVES BEHIND: one blob of a few bytes, in a probe repository, with no
// tag. Harbor's garbage collector reclaims untagged blobs; the alternative — a
// DELETE through the registry API — is not universally enabled and would fail the
// gate for a reason unrelated to encryption.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/harborauth"
)

// harborPushStage is WHERE a probe push failed. It exists because the fix text is
// the deliverable of a gate like this, and the first revision printed the same one
// — "this is the CA chain" — for every failure, including an unreachable registry
// and a rejected credential. A gate that confidently names the wrong cause is worse
// than one that says it does not know: it sends the reader to the CA, which is
// fine, while the registry is simply down.
type harborPushStage int

const (
	pushStageReach    harborPushStage = iota // could not reach /v2/ at all
	pushStageAuth                            // challenge/token/scope
	pushStageSession                         // registry refused an upload session
	pushStageComplete                        // the PUT — the storage driver's turn
)

// harborPushError carries the stage so the caller can attribute the failure.
type harborPushError struct {
	stage harborPushStage
	err   error
	body  string
}

func (e *harborPushError) Error() string { return e.err.Error() }

// tlsFailureMarkers are what a Go storage driver says when it cannot validate the
// proxy's certificate. Their PRESENCE upgrades the diagnosis from "the storage
// backend refused" to "the CA chain is broken"; their absence does not rule it out,
// so the fix text says so rather than guessing.
var tlsFailureMarkers = []string{
	"x509", "certificate signed by unknown authority", "tls:", "certificate is valid for",
	"failed to verify certificate",
}

func looksLikeTLSFailure(s string) bool {
	low := strings.ToLower(s)
	for _, m := range tlsFailureMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// harborEncProbeRepo is the repository the probe blob is pushed to. Distinct from
// harborProbeRepo so a cancelled-session probe and a real-push probe never
// interfere, and so the blobs this leaves are identifiable.
const harborEncProbeRepo = "library/llz-obj-encryption-probe"

// pushProbeBlobThroughHarbor uploads a small blob via the OCI v2 API and returns
// its digest. The bytes are unique per run so a push cannot be satisfied by a blob
// that a previous run already put there — a deduplicated push would return success
// without the storage driver touching S3 at all, which is exactly the false pass
// this check exists to avoid.
func pushProbeBlobThroughHarbor(d Deps, creds harborauth.RobotCreds, repo string, nonce []byte) (string, error) {
	base := "https://" + creds.RegistryHost
	token, err := harborPushToken(d, creds, repo)
	if err != nil {
		stage := pushStageAuth
		if strings.Contains(err.Error(), "reaching the registry") {
			stage = pushStageReach
		}
		return "", &harborPushError{stage: stage, err: err}
	}

	loc, err := harborauth.UploadSession(base, repo, token)
	if err != nil {
		return "", &harborPushError{stage: pushStageSession, err: err}
	}
	if loc == "" {
		return "", &harborPushError{stage: pushStageSession,
			err: fmt.Errorf("the registry accepted the upload session but returned no Location header")}
	}
	if strings.HasPrefix(loc, "/") {
		loc = base + loc
	}

	sum := sha256.Sum256(nonce)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	// Monolithic completion: PUT the bytes with ?digest=. Simpler than PATCH-then-PUT
	// and equally a storage-driver write, which is the only property under test.
	sep := "?"
	if strings.Contains(loc, "?") {
		sep = "&"
	}
	req, err := http.NewRequest(http.MethodPut, loc+sep+"digest="+digest, bytes.NewReader(nonce))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(nonce))

	resp, err := harborauth.HTTP(req)
	if err != nil {
		return "", &harborPushError{stage: pushStageComplete, err: fmt.Errorf("completing the blob upload: %w", err)}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		// The storage driver's stage: the registry accepted the session and then could
		// not persist the bytes. That is where a broken CA surfaces — but so does a
		// misconfigured bucket, so the CAUSE is attributed by the caller from the body
		// rather than asserted here.
		return "", &harborPushError{
			stage: pushStageComplete,
			body:  string(body),
			err: fmt.Errorf("the registry REFUSED to complete a blob upload (HTTP %d): %s",
				resp.StatusCode, harborauth.TruncateForError(body)),
		}
	}
	return digest, nil
}

// harborPushToken runs the bearer handshake and returns a token that actually
// carries push, reusing the same parsing as assert-harbor-roundtrip. A token
// without the access is refused here for the same reason it is there: Harbor
// returns a valid JWT with an EMPTY access list when the robot lacks the scope, so
// "I got a token" is not proof of authorization.
func harborPushToken(d Deps, creds harborauth.RobotCreds, repo string) (string, error) {
	base := "https://" + creds.RegistryHost
	req, err := http.NewRequest(http.MethodGet, base+"/v2/", nil)
	if err != nil {
		return "", err
	}
	resp, err := harborauth.HTTP(req)
	if err != nil {
		return "", fmt.Errorf("reaching the registry at %s: %w", base, err)
	}
	challenge := resp.Header.Get("Www-Authenticate")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return "", fmt.Errorf("GET /v2/ returned HTTP %d, expected 401 with a Bearer challenge", resp.StatusCode)
	}
	ch, err := harborauth.ParseBearerChallenge(challenge)
	if err != nil {
		return "", err
	}
	tok, access, err := requestHarborToken(ch, creds, repo)
	if err != nil {
		return "", err
	}
	if missing := harborauth.MissingActions(harborauth.GrantedActions(access, repo), "push", "pull"); len(missing) > 0 {
		return "", fmt.Errorf("the robot's token does not grant %v on %s — this is an authorization "+
			"problem, not an encryption one; assert-harbor-roundtrip covers it", missing, repo)
	}
	return tok, nil
}

// harborBlobStorageKey is where the registry's S3 driver stores a blob's data.
// distribution lays blobs out under a content-addressable prefix derived from the
// digest, so the object key is derivable rather than something to search for.
func harborBlobStorageKey(digest string) string {
	hexsum := strings.TrimPrefix(digest, "sha256:")
	if len(hexsum) < 2 {
		return ""
	}
	return fmt.Sprintf("docker/registry/v2/blobs/sha256/%s/%s/data", hexsum[:2], hexsum)
}

// checkHarborBlobIsEncrypted is the whole Check:   push through Harbor, then read
// the resulting object back with a keyless HEAD.
//
// The two failures it separates are the reason it exists. If the push FAILS, the CA
// chain is broken. If the push SUCCEEDS but the blob is plaintext, the CA is fine
// and the traffic bypassed the proxy. Those need different fixes and every other
// check in this gate conflates them.
func checkHarborBlobIsEncrypted(d Deps, endpoint, bucket string, creds harborauth.RobotCreds, nonce []byte) []Finding {
	digest, err := pushProbeBlobThroughHarbor(d, creds, harborEncProbeRepo, nonce)
	if err != nil {
		return []Finding{{
			Check:   "harbor-push",
			Problem: err.Error(),
			Fix:     harborPushFix(err),
		}}
	}
	key := harborBlobStorageKey(digest)
	ak, sk, cerr := objEncBucketReadCreds(d)
	if cerr != nil {
		return []Finding{{
			Check:   "harbor-push",
			Problem: "pushed a blob through Harbor but cannot verify it: " + cerr.Error(),
			Fix: "the readback needs only HEAD on " + bucket + ", and NOT the SSE-C key. " +
				"This is the same Secret the [object] check reads",
		}}
	}

	// The registry completes the upload before the object is necessarily visible to
	// a separate LIST/HEAD, so poll briefly rather than racing it.
	deadline := objEncNow().Add(30 * time.Second)
	for {
		verdict, detail := ObjectSSECProbe(ak, sk, endpoint, bucket, key)
		switch verdict {
		case ssecEncrypted:
			fmt.Printf("  harbor-push: blob %s written BY HARBOR is encrypted — the CA chain and the proxy both work\n", digest[:19])
			return nil
		case ssecPlaintext:
			return []Finding{{
				Check:   "harbor-push",
				Problem: fmt.Sprintf("Harbor wrote %s and it landed in PLAINTEXT (%s)", key, detail),
				Fix: "the CA chain is FINE (the push succeeded) — the traffic bypassed obj-proxy. " +
					"Check the CoreDNS rewrite resolves the endpoint to the proxy Service and that " +
					"llz_objproxy_ssec_injected_total is climbing",
			}}
		case ssecAbsent:
			if objEncNow().After(deadline) {
				return []Finding{{
					Check:   "harbor-push",
					Problem: fmt.Sprintf("Harbor reported the blob written but %s never appeared in %s", key, bucket),
					Fix:     "the registry may be writing to a different bucket or endpoint than this check reads; compare its config.yml regionendpoint with --endpoint",
				}}
			}
			objEncSleep(3 * time.Second)
		default:
			return []Finding{{
				Check:   "harbor-push",
				Problem: fmt.Sprintf("could not classify the pushed blob %s: %s", key, detail),
				Fix:     "an unclassifiable response is not a pass; check the credential's permissions on " + bucket,
			}}
		}
	}
}

// objEncBucketReadCreds returns the credential this gate reads objects back with.
//
// THE CLUSTER'S CREDENTIAL FIRST, ambient AWS_* only as a fallback — the opposite of
// what this check did, and the ordering matters. In the assert-suite AWS_* is the
// TERRAFORM STATE bucket's key, which has no access to the data buckets: reading it
// here turned a working encryption path into
//
//	could not classify the pushed blob …: HTTP 403
//
// with a fix line advising the reader to check permissions on a bucket whose
// permissions were fine. The [object] check already carries a comment explaining
// precisely this trap ("a gate that reads whatever credentials happen to be in
// scope is a gate that reports its own misconfiguration as a breach") and this
// function is next to it in the same file — it simply was not applied here.
//
// The platform's own key is used rather than Harbor's because Harbor's registry
// takes its S3 credentials from pod env, not from an AWS_*-shaped Secret anything
// can read; the keys are account-scoped, so the consumer credential reaches both
// buckets. Verified: a HEAD with it against the harbor bucket answers 400
// (SSE-C required), not 403.
func objEncBucketReadCreds(d Deps) (string, string, error) {
	if ak, sk, err := ObjEncConsumerCreds(d, LokiObjSecretRef, "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"); err == nil && ak != "" && sk != "" {
		return ak, sk, nil
	}
	ak, sk := objEncAccessKey(), objEncSecretKey()
	if ak == "" || sk == "" {
		return "", "", fmt.Errorf("no object-storage credentials: %s is unreadable and AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY are unset", LokiObjSecretRef)
	}
	return ak, sk, nil
}

// requestHarborToken performs the token-service exchange. It duplicates the
// sequence inside probeHarborRoundTrip rather than refactoring that function:
// assert-harbor-roundtrip is a shipped, tested gate on the authorization path, and
// carving a helper out of it to serve a new caller risks changing its behaviour for
// a benefit that is only tidiness. The PURE parts (harborauth.ParseBearerChallenge,
// harborauth.ParseTokenResponse, harborauth.GrantedActions, harborauth.MissingActions) are shared, which is where
// the drift risk actually lives.
func requestHarborToken(ch harborauth.BearerChallenge, creds harborauth.RobotCreds, repo string) (string, []harborauth.TokenAccess, error) {
	tokURL, err := url.Parse(ch.Realm)
	if err != nil {
		return "", nil, fmt.Errorf("token realm %q is not a URL: %w", ch.Realm, err)
	}
	q := tokURL.Query()
	q.Set("scope", "repository:"+repo+":pull,push")
	if ch.Service != "" {
		q.Set("service", ch.Service)
	}
	tokURL.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, tokURL.String(), nil)
	if err != nil {
		return "", nil, err
	}
	req.SetBasicAuth(creds.Username, creds.Password)
	resp, err := harborauth.HTTP(req)
	if err != nil {
		return "", nil, fmt.Errorf("token request to %s: %w", ch.Realm, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", nil, fmt.Errorf("the robot credential was REJECTED by the token service (HTTP 401)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("token request returned HTTP %d: %s", resp.StatusCode, harborauth.TruncateForError(body))
	}
	return harborauth.ParseTokenResponse(body)
}

// Seams. The push path is the one part of this gate that cannot be exercised
// without a registry, so everything around it is stubbable.
var (
	objEncAccessKey = func() string { return os.Getenv("AWS_ACCESS_KEY_ID") }
	objEncSecretKey = func() string { return os.Getenv("AWS_SECRET_ACCESS_KEY") }
	objEncNow       = time.Now
	objEncSleep     = time.Sleep
)

// harborPushFix attributes a push failure to its stage. Only the storage stage is
// evidence about the CA at all — everything earlier failed before the registry ever
// tried to write a byte, and blaming the CA there sends the reader to a component
// that is working fine.
func harborPushFix(err error) string {
	var pe *harborPushError
	if !errors.As(err, &pe) {
		return "unclassified push failure; assert-harbor-roundtrip covers the registry's auth path"
	}
	switch pe.stage {
	case pushStageReach:
		return "the REGISTRY is unreachable — this says nothing about encryption. Check harbor-core is " +
			"Running and its ingress resolves; assert-harbor-roundtrip covers this path"
	case pushStageAuth:
		return "an AUTHORIZATION failure, not an encryption one: the robot credential was rejected or " +
			"carries no push scope on this repository. assert-harbor-roundtrip is the gate for that — fix it there first"
	case pushStageSession:
		return "the registry refused to OPEN an upload session, so it never reached object storage. " +
			"Most likely the robot lacks push on " + harborEncProbeRepo + ", or the project does not exist. " +
			"This is not evidence about the CA"
	default:
		if looksLikeTLSFailure(pe.body) {
			return "CONFIRMED CA CHAIN: the registry's own error names a certificate failure, so it could not " +
				"validate the obj-proxy certificate. Check the Kyverno harbor-obj-proxy-ca mutation is on the " +
				"RUNNING pod (`kubectl -n harbor get pod -o yaml | grep SSL_CERT_DIR`) and that the certificate's " +
				"dnsNames match the endpoint host clients dial"
		}
		return "the registry accepted the upload and then could not PERSIST it, so the failure is in the " +
			"storage path. With the DNS rewrite live the likeliest cause is the CA chain — but the registry's " +
			"error does not name a certificate, so check `kubectl -n harbor logs deploy/harbor-registry` " +
			"before assuming it. A wrong bucket or endpoint fails identically here"
	}
}
