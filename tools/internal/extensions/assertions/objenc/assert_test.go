package objenc

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/harborauth"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"
)

// testWindow is the --since the sample tests run with, and testNow the clock they
// pin. Every object withSSESample stubs is dated INSIDE the window, so those cases
// exercise the classification rather than the freshness filter, which has its own
// tests below.
const testWindow = 24 * time.Hour

var testNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// windowStart is the cutover the gate computes from testNow and testWindow.
var windowStart = testNow.Add(-testWindow)

// withSSESample stubs the sample listing and a per-key verdict function, so the
// mixed-bucket case (the one a single-object sample got wrong) is testable.
func withSSESample(t *testing.T, keys []string, listErr error, verdict func(string) sseVerdict) {
	t.Helper()
	refs := make([]objstore.ObjectRef, 0, len(keys))
	for i, k := range keys {
		refs = append(refs, objstore.ObjectRef{Key: k, LastModified: windowStart.Add(time.Duration(i+1) * time.Minute)})
	}
	withSSESampleAt(t, refs, listErr, verdict)
}

func withSSESampleAt(t *testing.T, refs []objstore.ObjectRef, listErr error, verdict func(string) sseVerdict) {
	t.Helper()
	pk, pl, pn, prevCreds := ObjectSSEProbe, objstore.SampleObjectKeys, objEncNow, ObjEncConsumerCreds
	objstore.SampleObjectKeys = func(_, _, _, b string, _ int) ([]objstore.ObjectRef, error) {
		out := make([]objstore.ObjectRef, len(refs))
		copy(out, refs)
		for i := range out {
			out[i].Bucket = b
		}
		return out, listErr
	}
	ObjectSSEProbe = func(_, _, _, _, key string) (sseVerdict, string) { return verdict(key), "stub" }
	objEncNow = func() time.Time { return testNow }
	ObjEncConsumerCreds = func(_ Deps, _, _, _ string) (string, string, error) { return "ak", "sk", nil }
	t.Cleanup(func() {
		ObjectSSEProbe, objstore.SampleObjectKeys, objEncNow, ObjEncConsumerCreds = pk, pl, pn, prevCreds
	})
}

func allEncrypted(string) sseVerdict { return sseEncrypted }

func TestCheckObjectsEncryptedPassesWhenTheWholeSampleIsEncrypted(t *testing.T) {
	d := testDeps(t)
	withSSESample(t, []string{"a", "b", "c"}, nil, allEncrypted)
	if f := checkObjectsAreEncrypted(d, "us-ord-10.linodeobjects.com", []string{"b"}, 50, testWindow); len(f) != 0 {
		t.Errorf("a clean sample must pass, got %+v", f)
	}
}

// THE regression this rewrite exists for. A single-object sample reported color.Green on
// a bucket that is 1-in-N plaintext; the whole sample must be probed.
func TestCheckObjectsEncryptedCatchesOnePlaintextAmongMany(t *testing.T) {
	d := testDeps(t)
	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "rotten"}
	withSSESample(t, keys, nil, func(k string) sseVerdict {
		if k == "rotten" {
			return ssePlaintext
		}
		return sseEncrypted
	})
	f := checkObjectsAreEncrypted(d, "us-ord-10.linodeobjects.com", []string{"b"}, 50, testWindow)
	if len(f) != 1 || !strings.Contains(f[0].Problem, "PLAINTEXT") {
		t.Fatalf("one plaintext object among ten must fail, got %+v", f)
	}
	if !strings.Contains(f[0].Problem, "1 of 10") {
		t.Errorf("the finding must report the COUNT — that is the auditable number: %q", f[0].Problem)
	}
	if !strings.Contains(f[0].Fix, "E2/E3") {
		t.Error("these are fresh writes, so the fix must point at the endpoint generation")
	}
}

// A key deleted between LIST and HEAD (compaction) is not evidence of plaintext.
func TestCheckObjectsEncryptedToleratesObjectsThatVanish(t *testing.T) {
	d := testDeps(t)
	withSSESample(t, []string{"a", "gone"}, nil, func(k string) sseVerdict {
		if k == "gone" {
			return sseAbsent
		}
		return sseEncrypted
	})
	if f := checkObjectsAreEncrypted(d, "us-ord-10.linodeobjects.com", []string{"b"}, 50, testWindow); len(f) != 0 {
		t.Errorf("a raced deletion must not fail the gate, got %+v", f)
	}
}

// An empty bucket and a correctly-encrypted bucket are indistinguishable to every
// other check, so this one must not shrug at zero objects.
func TestCheckObjectsEncryptedFailsOnAnEmptyBucket(t *testing.T) {
	d := testDeps(t)
	withSSESample(t, nil, nil, allEncrypted)
	f := checkObjectsAreEncrypted(d, "us-ord-10.linodeobjects.com", []string{"b"}, 50, testWindow)
	if len(f) != 1 || !strings.Contains(f[0].Problem, "EMPTY") {
		t.Errorf("an empty bucket proves nothing and must not pass, got %+v", f)
	}
}

func TestCheckObjectsEncryptedFailsWhenUnclassifiable(t *testing.T) {
	d := testDeps(t)
	withSSESample(t, []string{"k"}, nil, func(string) sseVerdict { return sseUnknown })
	if f := checkObjectsAreEncrypted(d, "us-ord-10.linodeobjects.com", []string{"b"}, 50, testWindow); len(f) != 1 {
		t.Error("an unclassifiable response is not a pass")
	}
}

// The gate must read the CONSUMER's credentials, not the ambient AWS_* env. In the
// assert-suite those are the Terraform STATE bucket's key, which cannot list the
// data buckets — the first revision reported that 403 as an encryption finding.
func TestCheckObjectsEncryptedFailsWhenConsumerCredsUnreadable(t *testing.T) {
	d := testDeps(t)
	prev := ObjEncConsumerCreds
	ObjEncConsumerCreds = func(_ Deps, _, _, _ string) (string, string, error) {
		return "", "", errors.New("secrets \"loki-s3-linode-credentials\" not found")
	}
	t.Cleanup(func() { ObjEncConsumerCreds = prev })
	f := checkObjectsAreEncrypted(d, "us-ord-10.linodeobjects.com", []string{"b"}, 50, testWindow)
	if len(f) != 1 {
		t.Fatalf("unreadable consumer creds must be one finding, got %+v", f)
	}
	if !strings.Contains(f[0].Problem, LokiObjSecretRef) {
		t.Errorf("the finding must name the Secret it could not read: %q", f[0].Problem)
	}
	if !strings.Contains(f[0].Fix, "assert-obj-roundtrip") {
		t.Errorf("the fix must point at the lane that owns this Secret: %q", f[0].Fix)
	}
}

// No Harbor on the cluster means there is nothing whose writes must be encrypted —
// a skip, not a finding.
func TestCheckHarborPathSkipsWhenHarborIsAbsent(t *testing.T) {
	d := testDeps(t)
	prev := harborauth.NamespaceExists
	var asked []string
	harborauth.NamespaceExists = func(ns string) (bool, error) { asked = append(asked, ns); return false, nil }
	t.Cleanup(func() { harborauth.NamespaceExists = prev })
	if f := checkHarborPath(d, "h", "bucket"); len(f) != 0 {
		t.Errorf("no Harbor on the cluster must not fail the gate, got %+v", f)
	}
	if len(asked) != 1 || asked[0] != HarborNS {
		t.Errorf("guarded on %v, want a single check of %q — the SUBJECT is Harbor, not whichever "+
			"namespace happens to hold a credential", asked, HarborNS)
	}
}

// THE REGRESSION THIS EXISTS FOR. Guarding on llz-cert-automation skipped the
// harbor-push check on 100% of managed clusters, because cert-automation there is
// apl-core's and that namespace is never created — so the one check that guarantees
// a fresh object never ran on the cluster class it protects, and printed a
// reassuring explanation each time. A skip that is structural rather than occasional is not a
// skip, it is a check that does not exist.
func TestCheckHarborPathRunsOnManagedWhereTheRobotNamespaceCannotExist(t *testing.T) {
	d := testDeps(t)
	prevNS, prevSecret := harborauth.NamespaceExists, harborauth.ReadRobotSecret
	t.Cleanup(func() {
		harborauth.NamespaceExists, harborauth.ReadRobotSecret = prevNS, prevSecret
	})

	harborauth.NamespaceExists = func(ns string) (bool, error) { return ns == HarborNS, nil } // managed: no llz-cert-automation
	harborauth.ReadRobotSecret = func(ns, name string) ([]byte, error) {
		if ns == HarborNS && name == harborAdminSecretName {
			return []byte(`{"data":{"HARBOR_ADMIN_PASSWORD":"` +
				base64.StdEncoding.EncodeToString([]byte("s3cret")) + `"}}`), nil
		}
		return nil, errors.New("not found")
	}
	d.KubectlOut = func(...string) (string, error) {
		return `{"items":[{"spec":{"hostnames":["harbor.lke1.akamai-apl.net"]}}]}`, nil
	}

	creds, findings := harborProbeCreds(d)
	if len(findings) > 0 {
		t.Fatalf("managed must fall back to Harbor's own admin credential, got findings %+v", findings)
	}
	if creds.Username != harborAdminUser || creds.Password != "s3cret" {
		t.Errorf("credential = %q/%q, want the harbor admin", creds.Username, creds.Password)
	}
	if creds.RegistryHost != "harbor.lke1.akamai-apl.net" {
		t.Errorf("registry host = %q, want the host read off Harbor's own route — there is no spec "+
			"domainSuffix on managed to render it from", creds.RegistryHost)
	}
}

// Where LLZ's cert-automation IS deployed the robot stays preferred, and a broken
// robot Secret stays a finding. Falling back to admin there would paper over a real
// break in a credential path assert-harbor-roundtrip owns — and would quietly push
// with a much heavier credential than the gate needs.
func TestCheckHarborPathPrefersTheRobotAndDoesNotMaskItsFailure(t *testing.T) {
	d := testDeps(t)
	prevNS, prevSecret := harborauth.NamespaceExists, harborauth.ReadRobotSecret
	t.Cleanup(func() { harborauth.NamespaceExists, harborauth.ReadRobotSecret = prevNS, prevSecret })

	harborauth.NamespaceExists = func(string) (bool, error) { return true, nil } // self-installed: both present
	harborauth.ReadRobotSecret = func(ns, name string) ([]byte, error) {
		if ns == harborauth.RobotSecretNS {
			return nil, errors.New("the robot Secret is unreadable")
		}
		t.Errorf("fell back to %s/%s while %s exists — that masks the robot failure", ns, name, harborauth.RobotSecretNS)
		return nil, errors.New("unreachable")
	}

	_, findings := harborProbeCreds(d)
	if len(findings) != 1 || !strings.Contains(findings[0].Problem, harborauth.RobotSecretNS) {
		t.Fatalf("an unreadable robot Secret must stay one finding naming %s, got %+v", harborauth.RobotSecretNS, findings)
	}
}

// A host that fails harborauth.UsableRegistryHost is the "harbor." truncation class (PR #342):
// non-empty, so it satisfies every `== ""` guard on its way to 401ing every push.
func TestHarborPublicHostRejectsAnUnusableHostname(t *testing.T) {
	d := testDeps(t)
	d.KubectlOut = func(...string) (string, error) {
		return `{"items":[{"spec":{"hostnames":["harbor."]}},{"spec":{"rules":[{"host":"harbor.real.example"}]}}]}`, nil
	}
	got, err := harborPublicHost(d)
	if err != nil {
		t.Fatalf("an Ingress rule should have supplied the host after the bad HTTPRoute: %v", err)
	}
	if got != "harbor.real.example" {
		t.Errorf("host = %q, want the usable one — %q is the truncation class that passes every empty-string guard", got, "harbor.")
	}
}

// NO REGION MUST SKIP, NOT FAIL. The first revision fell through to the flag checks
// and hard-failed on an empty --endpoint, which redded the whole battery for a
// missing argument and reported it as an encryption failure. A cluster that cannot
// say which deployment it is cannot say which buckets it writes.
func TestAssertObjEncryptionSkipsWithNoRegion(t *testing.T) {
	d := testDeps(t)
	if err := RunAssertEncryption(d, "", "", "", "", 50, testWindow); err != nil {
		t.Fatalf("no --region must SKIP, not fail: %v", err)
	}
}

func TestSSEVerdictStrings(t *testing.T) {
	for v, want := range map[sseVerdict]string{
		sseEncrypted: "encrypted", ssePlaintext: "PLAINTEXT", sseCustomerKey: "customer-key",
		sseAbsent: "absent", sseUnknown: "unknown",
	} {
		if got := fmt.Sprint(v); got != want {
			t.Errorf("verdict %d = %q, want %q", v, got, want)
		}
	}
}

// THE VERDICT TABLE. The header is the whole signal on a 200: S3 reports at-rest
// encryption there and nowhere else, so a 200 without it is plaintext — not
// "probably fine". A 400 is what an SSE-C leftover from the retired gateway answers.
func TestClassifySSE(t *testing.T) {
	enc := http.Header{}
	enc.Set("X-Amz-Server-Side-Encryption", "AES256")
	cases := []struct {
		code int
		hdr  http.Header
		body string
		want sseVerdict
	}{
		{200, enc, "", sseEncrypted},
		{200, http.Header{}, "", ssePlaintext},
		{200, nil, "", ssePlaintext},
		{400, nil, "<Error><Code>InvalidRequest</Code></Error>", sseCustomerKey},
		{404, nil, "", sseAbsent},
		{403, nil, "<Error><Code>AccessDenied</Code></Error>", sseUnknown},
		{503, nil, "", sseUnknown},
	}
	for _, tc := range cases {
		got, detail := classifySSE(tc.code, tc.hdr, tc.body)
		if got != tc.want {
			t.Errorf("classifySSE(%d, %v) = %s (%s), want %s", tc.code, tc.hdr, got, detail, tc.want)
		}
	}
	if _, detail := classifySSE(200, enc, ""); !strings.Contains(detail, "AES256") {
		t.Errorf("the detail must carry the algorithm the server reported: %q", detail)
	}
}

// ObjectSSEProbe must read the header off the REAL HEAD response — a probe that
// classified on status alone would call every object plaintext, or every object
// encrypted, depending on which way it defaulted.
func TestObjectSSEProbeReadsTheResponseHeader(t *testing.T) {
	prev := objstore.S3HeadObject
	t.Cleanup(func() { objstore.S3HeadObject = prev })
	var asked string
	objstore.S3HeadObject = func(_, _, _, bucket, key string) (int, http.Header, string, error) {
		asked = bucket + "/" + key
		h := http.Header{}
		h.Set("x-amz-server-side-encryption", "AES256")
		return 200, h, "", nil
	}
	if v, _ := ObjectSSEProbe("ak", "sk", "ep", "bkt", "k/1"); v != sseEncrypted {
		t.Errorf("verdict = %s, want encrypted", v)
	}
	if asked != "bkt/k/1" {
		t.Errorf("probed %q, want bkt/k/1", asked)
	}
	objstore.S3HeadObject = func(_, _, _, _, _ string) (int, http.Header, string, error) {
		return 0, nil, "", errors.New("dial tcp: i/o timeout")
	}
	if v, _ := ObjectSSEProbe("ak", "sk", "ep", "bkt", "k"); v != sseUnknown {
		t.Errorf("a transport error must be unknown, never a verdict about the object: got %s", v)
	}
}

// Harbor absent is NOT a finding — managed renders a minimal app set. Harbor
// present with no --harbor-bucket IS, because that is the fresh-object check being
// skipped on a cluster that has one to give.
func TestCheckHarborPathSkipsWhenNamespaceAbsent(t *testing.T) {
	d := testDeps(t)
	prev := harborauth.NamespaceExists
	harborauth.NamespaceExists = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { harborauth.NamespaceExists = prev })
	if f := checkHarborPath(d, "h", ""); len(f) != 0 {
		t.Errorf("an absent harbor namespace must not fail the gate, got %+v", f)
	}
}

func TestCheckHarborPathFailsWhenBucketOmittedButHarborPresent(t *testing.T) {
	d := testDeps(t)
	prev := harborauth.NamespaceExists
	harborauth.NamespaceExists = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { harborauth.NamespaceExists = prev })
	f := checkHarborPath(d, "h", "")
	if len(f) != 1 || !strings.Contains(f[0].Problem, "push check did not run") {
		t.Fatalf("omitting --harbor-bucket on a Harbor cluster must fail, got %+v", f)
	}
	if !strings.Contains(f[0].Fix, "Loki may not have flushed") {
		t.Error("the fix must explain WHY the other checks do not cover this")
	}
}

// The blob key must match distribution's content-addressable layout, or the check
// HEADs a path that never exists and reports a missing object instead of an
// unencrypted one.
func TestHarborBlobStorageKey(t *testing.T) {
	got := harborBlobStorageKey("sha256:abcdef0123456789")
	want := "docker/registry/v2/blobs/sha256/ab/abcdef0123456789/data"
	if got != want {
		t.Errorf("harborBlobStorageKey = %q, want %q", got, want)
	}
	if harborBlobStorageKey("sha256:") != "" {
		t.Error("a digest with no hex must yield no key rather than a malformed one")
	}
}

// A gate's fix text is its deliverable. Every stage before the storage write
// failed before a byte reached object storage, so none of them may be reported as
// an object-storage problem.
func TestHarborPushFixAttributesTheFailureToItsStage(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantSub string
	}{
		{"registry down", &harborPushError{stage: pushStageReach, err: errors.New("x")}, "REGISTRY is unreachable"},
		{"bad credential", &harborPushError{stage: pushStageAuth, err: errors.New("x")}, "AUTHORIZATION failure"},
		{"no push scope", &harborPushError{stage: pushStageSession, err: errors.New("x")}, "never reached object storage"},
		{"storage", &harborPushError{stage: pushStageComplete, err: errors.New("x")}, "could not PERSIST"},
	}
	for _, tc := range cases {
		if got := harborPushFix(tc.err); !strings.Contains(got, tc.wantSub) {
			t.Errorf("%s: fix = %q, want it to mention %q", tc.name, got, tc.wantSub)
		}
	}
}

// An error that is not a staged push error must not be attributed at all.
func TestHarborPushFixDoesNotGuessOnUnknownErrors(t *testing.T) {
	if got := harborPushFix(errors.New("something else")); !strings.Contains(got, "unclassified") {
		t.Errorf("an unstaged error must not be attributed to a stage: %q", got)
	}
}

// OBJECTS OLDER THAN THE WINDOW ARE NOT A FINDING. Linode enabling SSE-S3 on an
// endpoint leaves everything already in its buckets plaintext until rewritten.
// Counting those would make this check unpassable on every e2e bucket (they are
// reused across clusters) — but they are still probed and REPORTED as residue.
func TestCheckObjectsEncryptedIgnoresObjectsOlderThanTheWindow(t *testing.T) {
	d := testDeps(t)
	refs := []objstore.ObjectRef{
		{Key: "written-after", LastModified: windowStart.Add(time.Hour)},
		{Key: "predates-1", LastModified: windowStart.Add(-240 * time.Hour)},
		{Key: "predates-2", LastModified: windowStart.Add(-99 * time.Hour)},
	}
	withSSESampleAt(t, refs, nil, func(k string) sseVerdict {
		if k == "written-after" {
			return sseEncrypted
		}
		return ssePlaintext // the pre-enablement residue
	})
	var f []Finding
	out := captureStdout(t, func() {
		f = checkObjectsAreEncrypted(d, "us-ord-10.linodeobjects.com", []string{"b"}, 50, testWindow)
	})
	if len(f) != 0 {
		t.Errorf("plaintext objects older than the window are residue, not a breach: %+v", f)
	}
	if !strings.Contains(out, "2 of 2 older sampled objects are still PLAINTEXT") {
		t.Errorf("residue must be REPORTED, not silently skipped; stdout:\n%s", out)
	}
}

// The mirror image, and the property that keeps the filter honest: an object
// written inside the window has no excuse for being plaintext.
func TestCheckObjectsEncryptedStillCatchesPlaintextWrittenInsideTheWindow(t *testing.T) {
	d := testDeps(t)
	refs := []objstore.ObjectRef{
		{Key: "predates", LastModified: windowStart.Add(-time.Hour)},
		{Key: "leaked", LastModified: windowStart.Add(time.Minute)},
	}
	withSSESampleAt(t, refs, nil, func(k string) sseVerdict {
		if k == "leaked" {
			return ssePlaintext
		}
		return sseEncrypted
	})
	f := checkObjectsAreEncrypted(d, "us-ord-10.linodeobjects.com", []string{"b"}, 50, testWindow)
	if len(f) != 1 || !strings.Contains(f[0].Problem, "PLAINTEXT") {
		t.Fatalf("a fresh plaintext write must fail, got %+v", f)
	}
	if !strings.Contains(f[0].Problem, "1 of 1") {
		t.Errorf("the count must be out of the objects actually JUDGED, not the whole sample: %q", f[0].Problem)
	}
}

// SSE-C leftovers from the retired gateway are encrypted, and must not be reported
// as either plaintext or unclassifiable.
func TestCheckObjectsEncryptedAcceptsCustomerKeyLeftovers(t *testing.T) {
	d := testDeps(t)
	withSSESample(t, []string{"new", "ssec-era"}, nil, func(k string) sseVerdict {
		if k == "ssec-era" {
			return sseCustomerKey
		}
		return sseEncrypted
	})
	if f := checkObjectsAreEncrypted(d, "us-ord-10.linodeobjects.com", []string{"b"}, 50, testWindow); len(f) != 0 {
		t.Errorf("an SSE-C object is encrypted; got %+v", f)
	}
}

// A bucket whose every object predates the window proves nothing either way, and a
// check that examined no relevant object must not report green. This is the state a
// consumer that CANNOT WRITE produces.
func TestCheckObjectsEncryptedReportsWhenNothingWasWrittenInsideTheWindow(t *testing.T) {
	d := testDeps(t)
	refs := []objstore.ObjectRef{{Key: "old", LastModified: windowStart.Add(-time.Hour)}}
	withSSESampleAt(t, refs, nil, allEncrypted)
	f := checkObjectsAreEncrypted(d, "us-ord-10.linodeobjects.com", []string{"b"}, 50, testWindow)
	if len(f) != 1 {
		t.Fatalf("nothing written inside the window must be reported, not passed, got %+v", f)
	}
	if !strings.Contains(f[0].Problem, "proves nothing") {
		t.Errorf("the finding must say it proved nothing: %q", f[0].Problem)
	}
	// The Harbor push runs BEFORE this check and writes a blob, so it is the first
	// thing to suspect when the sample has nothing fresh in it.
	if !strings.Contains(f[0].Fix, "Harbor push") {
		t.Errorf("the fix must point at the guaranteed writer first: %q", f[0].Fix)
	}
}

// Both buckets are sampled, and a finding names the one it came from.
//
// Pinning this to Loki's bucket alone is what made the check unsatisfiable on the
// e2e timeline: Loki holds its first chunk for up to chunk_idle_period, so nine
// minutes into a cluster's life that bucket can contain only a previous cluster's
// objects, and the check would report "nothing written" on a fleet that was
// encrypting correctly.
func TestObjEncryptionSamplesEveryBucketAndAttributesFindings(t *testing.T) {
	d := testDeps(t)
	cutover := windowStart
	prevList, prevProbe, prevNow, prevCreds := objstore.SampleObjectKeys, ObjectSSEProbe, objEncNow, ObjEncConsumerCreds
	t.Cleanup(func() {
		objstore.SampleObjectKeys, ObjectSSEProbe, objEncNow, ObjEncConsumerCreds = prevList, prevProbe, prevNow, prevCreds
	})
	objEncNow = func() time.Time { return testNow }
	ObjEncConsumerCreds = func(Deps, string, string, string) (string, string, error) { return "ak", "sk", nil }

	var sampled []string
	objstore.SampleObjectKeys = func(_, _, _, b string, _ int) ([]objstore.ObjectRef, error) {
		sampled = append(sampled, b)
		switch b {
		case "harbor-bucket": // the gate's own probe blob — always fresh
			return []objstore.ObjectRef{{Key: "blobs/data", LastModified: cutover.Add(time.Minute), Bucket: b}}, nil
		default: // Loki has not flushed yet: only a previous cluster's objects
			return []objstore.ObjectRef{{Key: "old/chunk", LastModified: cutover.Add(-240 * time.Hour), Bucket: b}}, nil
		}
	}
	ObjectSSEProbe = func(_, _, _, bucket, _ string) (sseVerdict, string) {
		if bucket == "harbor-bucket" {
			return ssePlaintext, "stub" // so the finding has to name its bucket
		}
		return sseEncrypted, "stub"
	}

	f := checkObjectsAreEncrypted(d, "ep", []string{"harbor-bucket", "loki-bucket"}, 50, testWindow)

	if len(sampled) != 2 {
		t.Fatalf("sampled %v — both buckets must be listed, or a dead consumer makes the check unsatisfiable", sampled)
	}
	if len(f) != 1 || !strings.Contains(f[0].Problem, "PLAINTEXT") {
		t.Fatalf("the fresh harbor object was plaintext and must fail, got %+v", f)
	}
	if !strings.Contains(f[0].Problem, "harbor-bucket/blobs/data") {
		t.Errorf("the finding must name the BUCKET and key it found: %q", f[0].Problem)
	}
}

// The Harbor push must run BEFORE the object sample. It writes the one fresh
// object this gate can guarantee, so sampling first samples a bucket nothing has
// written to yet — the ordering bug that has failed an e2e.
func TestHarborPushRunsBeforeTheObjectSample(t *testing.T) {
	src, err := os.ReadFile("assert.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	harbor := strings.Index(body, "findings = append(findings, checkHarborPath(d, ")
	object := strings.Index(body, "findings = append(findings, checkObjectsAreEncrypted(d, ")
	if harbor < 0 || object < 0 {
		t.Fatal("could not find both call sites — this test's premise no longer holds, revisit it")
	}
	if harbor > object {
		t.Error("checkObjectsAreEncrypted runs before checkHarborPath: the sample then predates the probe blob " +
			"that guarantees it has fresh material, and the check fails on a healthy cluster")
	}
	if !strings.Contains(body, "checkObjectsAreEncrypted(d, endpoint, []string{harborBucket, bucket}, sample, since)") {
		t.Error("the object sample must cover BOTH buckets — Loki's alone is empty on a young cluster")
	}
}

// captureStdout runs fn and returns what it printed — the residue notice is
// stdout-only, and a notice nobody can assert on is one that can silently vanish.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	os.Stdout = prev
	w.Close()
	return <-done
}
