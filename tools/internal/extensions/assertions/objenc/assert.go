package objenc

// assert.go implements `llz ci assert-obj-encryption` — the RUNTIME gate that
// objects landing in Object Storage are actually encrypted at rest.
//
// WHY A RUNTIME GATE AND NOT A CONFIG CHECK. This is the lesson PVC encryption
// taught expensively: `assert-volume-encryption` exists because the config said
// "encrypted" while the Linode API said otherwise, and only the API was telling the
// truth. Here there is not even a config to read: Linode applies SSE-S3 by default
// on E2/E3 endpoints, and nothing Loki or Harbor sends turns it on or off. The only
// place the property exists is on the objects, so that is where it is checked.
//
// WHAT CAN STILL LEAVE AN OBJECT IN THE CLEAR, and why each needs this gate:
//
//   - the deployment's endpoint is E0/E1, which default SSE-S3 does not cover —
//     the bucket name and the endpoint host carry no generation, so nothing in the
//     spec reveals it
//   - the object predates Linode enabling SSE-S3 on the endpoint (it stays
//     plaintext until rewritten)
//   - a provider-side regression, which would otherwise be invisible
//
// THE TWO CHECKS, and why both:
//
//  1. HARBOR PUSH — Harbor writes a fresh blob and it must report SSE on HEAD. The
//     one object this gate can GUARANTEE was written now, by a real writer, so it
//     decides the endpoint's current behaviour without waiting on a consumer.
//  2. OBJECT SAMPLE — real objects in both buckets, judged when written within
//     --since. Catches a writer whose objects differ from the probe's, and reports
//     older plaintext as residue so an operator knows what to rewrite.

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/harborauth"
)

// HarborNS is where apl-core installs Harbor.
const HarborNS = "harbor"

// LokiObjSecretRef is the Secret apl-core's Loki release mounts for object storage —
// the same one assert-obj-roundtrip reads, kept in one place so the two cannot drift.
const LokiObjSecretRef = "monitoring/loki-s3-linode-credentials"

// ObjEncConsumerCreds reads an access/secret pair out of a namespaced Secret.
var ObjEncConsumerCreds = func(d Deps, ref, akField, skField string) (string, string, error) {
	ns, name, ok := strings.Cut(ref, "/")
	if !ok {
		return "", "", fmt.Errorf("secret ref %q is not namespace/name", ref)
	}
	raw, err := d.KubectlOut("-n", ns, "get", "secret", name, "-o", "json")
	if err != nil {
		return "", "", err
	}
	ak, err := d.SecretField([]byte(raw), akField)
	if err != nil {
		return "", "", err
	}
	sk, err := d.SecretField([]byte(raw), skField)
	if err != nil {
		return "", "", err
	}
	return ak, sk, nil
}

// Finding is one failed check.
type Finding struct {
	Check   string
	Problem string
	Fix     string
}

// e1Fix is the remedy when a FRESH object is plaintext — which, outside a
// provider regression, means the endpoint is one default SSE-S3 does not cover.
const e1Fix = "Linode applies SSE-S3 by default only on E2/E3 endpoints. Check this deployment's " +
	"cluster.objectStorage.cluster with `linode-cli object-storage endpoints-list`: on an E0/E1 " +
	"endpoint the buckets are not encrypted, and the fix is to move them to an E2/E3 endpoint " +
	"(buckets are pinned to the endpoint they were created on, so this is a migration, not a flag)"

func RunAssertEncryption(d Deps, endpoint, bucket, harborBucket, region string, sample int, since time.Duration) error {
	// NO REGION MEANS SKIP, NOT FAIL. A cluster that cannot tell us which deployment
	// it is cannot tell us which buckets it writes, and redding the battery for that
	// reports an encryption failure where there is only a missing argument.
	if region == "" {
		fmt.Println("::notice::assert-obj-encryption: no --region, so this gate cannot tell which " +
			"buckets this deployment writes. Checked NOTHING.")
		return nil
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return err
	}

	// Derive from the SPEC rather than from flags/env. The endpoint and both bucket
	// names come from the same functions the renderer and the overlay use, so this
	// gate cannot be pointed at a bucket the deployment does not write to. Explicit
	// flags still win, for probing a specific bucket by hand.
	if e, ok := lz.Env(region); ok {
		if endpoint == "" {
			endpoint = clusterspec.ObjEndpointHost(e.Cluster.ObjectStorage.Cluster)
		}
		if bucket == "" {
			bucket = clusterspec.ObjLokiChunksBucket(lz.ObjLabelPrefix(), region)
		}
		if harborBucket == "" {
			harborBucket = clusterspec.ObjHarborRegistryBucket(lz.ObjLabelPrefix(), region)
		}
	}
	if endpoint == "" {
		return fmt.Errorf("could not determine the object-storage endpoint for %q: the env declares no "+
			"cluster.objectStorage.cluster", region)
	}
	if bucket == "" {
		return fmt.Errorf("could not determine the Loki chunks bucket for %q — without it the object "+
			"sample cannot run", region)
	}

	var findings []Finding
	// HARBOR BEFORE THE SAMPLE, and the ordering is load-bearing. checkHarborPath
	// pushes a blob through Harbor's registry, which is the one fresh object this
	// gate can GUARANTEE exists. Sampling first meant sampling a bucket nothing had
	// written to yet.
	findings = append(findings, checkHarborPath(d, endpoint, harborBucket)...)
	findings = append(findings, checkObjectsAreEncrypted(d, endpoint, []string{harborBucket, bucket}, sample, since)...)

	if len(findings) == 0 {
		fmt.Printf("assert-obj-encryption: objects written to %s are encrypted at rest.\n", endpoint)
		return nil
	}
	for _, f := range findings {
		fmt.Printf("::error::[%s] %s\n  Fix:     %s\n", f.Check, f.Problem, f.Fix)
	}
	return fmt.Errorf("assert-obj-encryption: %d check(s) failed — objects may be landing in PLAINTEXT", len(findings))
}

// checkHarborPath runs the push check when Harbor is on this cluster.
//
// An ABSENT harbor namespace is not a finding: managed App Platform renders a
// minimal app set and simply may not include it, and failing there would red a
// cluster for a component it never ran. An absent --harbor-bucket while the
// namespace IS present is a finding, because that is the one check that guarantees
// a fresh object being skipped on a cluster that has one to give.
func checkHarborPath(d Deps, endpoint, harborBucket string) []Finding {
	// Guard on HARBOR, not on the namespace that happens to hold a credential.
	// llz-cert-automation is LLZ's own cert component, and on a managed cluster
	// cert-automation is apl-core's — so that namespace is NEVER present on the
	// cluster class this gate exists to protect, and guarding on it skipped the
	// check 100% of the time there.
	present, err := harborauth.NamespaceExists(HarborNS)
	if err != nil {
		return []Finding{{
			Check: "harbor-push", Problem: "could not determine whether the harbor namespace exists: " + err.Error(),
			Fix: "this gate cannot decide whether to run the push check without it",
		}}
	}
	if !present {
		fmt.Printf("  harbor-push: namespace %s is absent — no Harbor on this cluster, so there is no "+
			"push path to write a fresh object with\n", HarborNS)
		return nil
	}
	if harborBucket == "" {
		return []Finding{{
			Check:   "harbor-push",
			Problem: "the harbor namespace exists but --harbor-bucket was not given, so the push check did not run",
			Fix: "pass --harbor-bucket. Without it the object sample judges only what consumers happened to " +
				"write within --since, and a young cluster's Loki may not have flushed anything yet",
		}}
	}
	creds, findings := harborProbeCreds(d)
	if len(findings) > 0 {
		return findings
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return []Finding{{
			Check: "harbor-push", Problem: "could not generate probe bytes: " + err.Error(), Fix: "retry",
		}}
	}
	return checkHarborBlobIsEncrypted(d, endpoint, harborBucket, creds, nonce)
}

// harborProbeCreds returns a credential that can push one blob into Harbor.
//
// PREFERS THE LLZ ROBOT where LLZ's cert-automation is deployed, and keeps the old
// strictness there: if that namespace exists, a missing or malformed robot Secret is
// still a finding rather than a quiet downgrade, because on those clusters it is a
// real break in a path assert-harbor-roundtrip owns.
//
// FALLS BACK TO HARBOR'S ADMIN only when that namespace is absent — which on managed
// is always, since cert-automation there is apl-core's. Admin is a heavier credential
// than a scoped robot and would be the wrong default; it is used here because it is
// the one credential that exists wherever Harbor does, and a gate that cannot
// authenticate is a gate that proves nothing. It is the same credential the
// in-cluster harbor provisioner already runs on, and this reads it with the cluster
// access the gate already holds — so it grants no reach it did not have.
func harborProbeCreds(d Deps) (harborauth.RobotCreds, []Finding) {
	robotNS, err := harborauth.NamespaceExists(harborauth.RobotSecretNS)
	if err != nil {
		return harborauth.RobotCreds{}, []Finding{{
			Check: "harbor-push", Problem: "could not determine whether " + harborauth.RobotSecretNS + " exists: " + err.Error(),
			Fix: "this gate cannot choose a credential without it",
		}}
	}
	if !robotNS {
		return harborAdminProbeCreds(d)
	}
	raw, err := harborauth.ReadRobotSecret(harborauth.RobotSecretNS, harborauth.RobotSecretName)
	if err != nil || len(raw) == 0 {
		return harborauth.RobotCreds{}, []Finding{{
			Check:   "harbor-push",
			Problem: fmt.Sprintf("could not read the Harbor robot credential %s/%s", harborauth.RobotSecretNS, harborauth.RobotSecretName),
			Fix:     "assert-harbor-roundtrip covers this credential path; fix it there first",
		}}
	}
	creds, err := harborauth.DecodeRobotSecret(raw)
	if err != nil {
		return harborauth.RobotCreds{}, []Finding{{
			Check: "harbor-push", Problem: "the Harbor robot credential is unusable: " + err.Error(),
			Fix: "assert-harbor-roundtrip covers this credential path; fix it there first",
		}}
	}
	fmt.Printf("  harbor-push: pushing as robot %q (from %s/%s)\n", creds.Username, harborauth.RobotSecretNS, harborauth.RobotSecretName)
	return creds, nil
}

// harborAdminSecret is Harbor's own admin credential, created by its Helm release in
// the harbor namespace — present on managed and self-installed alike.
const (
	harborAdminSecretName = "harbor-admin-password"
	harborAdminSecretKey  = "HARBOR_ADMIN_PASSWORD"
	harborAdminUser       = "admin"
)

func harborAdminProbeCreds(d Deps) (harborauth.RobotCreds, []Finding) {
	fail := func(problem, fix string) (harborauth.RobotCreds, []Finding) {
		return harborauth.RobotCreds{}, []Finding{{Check: "harbor-push", Problem: problem, Fix: fix}}
	}
	raw, err := harborauth.ReadRobotSecret(HarborNS, harborAdminSecretName)
	if err != nil || len(raw) == 0 {
		return fail(
			fmt.Sprintf("%s is absent, and Harbor's own %s/%s could not be read either", harborauth.RobotSecretNS, HarborNS, harborAdminSecretName),
			"Harbor's Helm release creates this Secret; if it is missing, Harbor is not installed yet — "+
				"re-run once it is, rather than accepting an unproven push path")
	}
	pass, err := d.SecretField(raw, harborAdminSecretKey)
	if err != nil || pass == "" {
		return fail(
			fmt.Sprintf("%s/%s has no usable %s", HarborNS, harborAdminSecretName, harborAdminSecretKey),
			"the push check cannot run without a credential that can push")
	}
	host, ferr := harborPublicHost(d)
	if ferr != nil {
		return fail("could not discover Harbor's public registry host: "+ferr.Error(),
			"the push must go through the registry's own S3 client to prove anything, so it needs the host "+
				"`docker push` targets. On managed there is no spec domainSuffix to render it from, so it is read "+
				"from Harbor's route")
	}
	fmt.Printf("  harbor-push: %s is absent (cert-automation is apl-core's here) — pushing as %s to %s\n",
		harborauth.RobotSecretNS, harborAdminUser, host)
	return harborauth.RobotCreds{Username: harborAdminUser, Password: pass, RegistryHost: host}, nil
}

// harborPublicHost reads the host `docker push` targets off Harbor's own route.
//
// Not from the spec: on managed there is no domainSuffix, which is the whole reason
// HarborHost("") once rendered the string "harbor." — non-empty, so it satisfied
// every `== ""` guard on the way to 401ing every push (see PR #342). Reading the
// cluster avoids re-deriving a value the cluster already states.
func harborPublicHost(d Deps) (string, error) {
	out, err := d.KubectlOut("-n", HarborNS, "get", "httproute,ingress", "-o", "json")
	if err != nil {
		return "", err
	}
	var view struct {
		Items []struct {
			Spec struct {
				Hostnames []string `json:"hostnames"` // HTTPRoute
				Rules     []struct {
					Host string `json:"host"` // Ingress
				} `json:"rules"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return "", fmt.Errorf("decoding Harbor's routes: %w", err)
	}
	for _, it := range view.Items {
		for _, h := range it.Spec.Hostnames {
			if harborauth.UsableRegistryHost(h) {
				return h, nil
			}
		}
		for _, r := range it.Spec.Rules {
			if harborauth.UsableRegistryHost(r.Host) {
				return r.Host, nil
			}
		}
	}
	return "", fmt.Errorf("no HTTPRoute or Ingress in the %s namespace declares a usable hostname", HarborNS)
}

// bucketCounts renders "harbor=12, loki=30" for the sample line, so a reader can see
// WHICH bucket supplied the evidence rather than a bare total.
func bucketCounts(d Deps, per map[string]int) string {
	names := make([]string, 0, len(per))
	for b := range per {
		names = append(names, b)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, b := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", b, per[b]))
	}
	return strings.Join(parts, ", ")
}

// checkObjectsAreEncrypted samples REAL objects and fails on any plaintext among
// the ones written within the window.
//
// An EMPTY bucket is a failure, not a pass. "Nothing has been written yet" and
// "everything written is encrypted" produce the same green from a checker that
// shrugs at zero objects.
//
// SAMPLED, and it says so. Any plaintext in the sample is proof of failure; a clean
// sample is evidence of success, not proof of it. The success line prints the
// sample size for exactly that reason.
//
// EVERY BUCKET, not just Loki's. Loki does not flush its first chunk until
// chunk_idle_period, so on a young cluster its bucket holds nothing recent; Harbor's
// always does, because checkHarborPath pushes a blob into it moments before this
// runs.
func checkObjectsAreEncrypted(d Deps, endpoint string, buckets []string, sample int, since time.Duration) []Finding {
	// The CONSUMER's own credentials, read from the Secret it mounts — not the
	// ambient AWS_* env. In the assert-suite those env vars are the TERRAFORM STATE
	// bucket's key, which has no access to the data buckets: reading them reported
	// "403 AccessDenied" as an encryption finding.
	ak, sk, cerr := ObjEncConsumerCreds(d, LokiObjSecretRef, "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY")
	if cerr != nil {
		return []Finding{{
			Check:   "object",
			Problem: "could not read the Loki object-store credentials from " + LokiObjSecretRef + ": " + cerr.Error(),
			Fix: "this is the same Secret assert-obj-roundtrip reads; if that lane is also failing, fix it " +
				"there first. The probe needs only LIST and HEAD on the bucket",
		}}
	}
	var keys []objstore.ObjectRef
	perBucket := map[string]int{}
	for _, b := range buckets {
		if b == "" {
			continue
		}
		got, err := objstore.SampleObjectKeys(ak, sk, endpoint, b, sample)
		if err != nil {
			return []Finding{{
				Check:   "object",
				Problem: fmt.Sprintf("could not list %s: %v", b, err),
				Fix:     "check the key is scoped to this bucket and the endpoint host is the one the bucket was created against",
			}}
		}
		for _, r := range got {
			r.Bucket = b
			keys = append(keys, r)
		}
		perBucket[b] = len(got)
	}
	if len(keys) == 0 {
		return []Finding{{
			Check:   "object",
			Problem: strings.Join(buckets, " and ") + " are EMPTY, so nothing proves writes are encrypted",
			Fix: "write something through the real path (push an image / let Loki flush a chunk) and re-run. " +
				"An empty bucket and a correctly-encrypted bucket look identical to every check except this one",
		}}
	}

	// JUDGE ONLY WHAT WAS WRITTEN WITHIN THE WINDOW. Objects written before Linode
	// enabled SSE-S3 on the endpoint stay plaintext until rewritten — a documented
	// property of the rollout, not a regression. Counting them would make this check
	// unpassable on any bucket with history, which for e2e is every bucket. They are
	// still PROBED and reported, so the residue is visible rather than ignored.
	cutover := objEncNow().Add(-since)
	var fresh, old []objstore.ObjectRef
	for _, k := range keys {
		if k.LastModified.After(cutover) {
			fresh = append(fresh, k)
		} else {
			old = append(old, k)
		}
	}
	if len(fresh) == 0 {
		return []Finding{{
			Check: "object",
			Problem: fmt.Sprintf("nothing has been written to %s since %s (newest of %d sampled: %s), so "+
				"this proves nothing about encryption",
				strings.Join(buckets, " or "), cutover.Format(time.RFC3339), len(keys), keys[0].LastModified.Format(time.RFC3339)),
			Fix: "a check that examined no relevant object must not report green. The Harbor push above " +
				"normally guarantees at least one fresh object, so if THAT skipped or failed this is " +
				"downstream of it. Otherwise no consumer has written within --since — check their logs " +
				"for S3 errors",
		}}
	}

	var plaintext, unknown []string
	for _, k := range fresh {
		switch verdict, _ := ObjectSSEProbe(ak, sk, endpoint, k.Bucket, k.Key); verdict {
		case ssePlaintext:
			plaintext = append(plaintext, k.Bucket+"/"+k.Key)
		case sseEncrypted, sseCustomerKey, sseAbsent:
			// absent: raced a compaction/delete between LIST and HEAD. Not evidence
			// of plaintext, and not worth failing a gate over.
		default:
			unknown = append(unknown, k.Bucket+"/"+k.Key)
		}
	}
	fmt.Printf("  object: %d of %d sampled objects (%s) were written since %s; classifying those\n",
		len(fresh), len(keys), bucketCounts(d, perBucket), cutover.Format(time.RFC3339))
	reportResidue(ak, sk, endpoint, old)

	var out []Finding
	if len(plaintext) > 0 {
		out = append(out, Finding{
			Check: "object",
			Problem: fmt.Sprintf("%d of %d sampled objects in %s are stored in PLAINTEXT (e.g. %s)",
				len(plaintext), len(fresh), strings.Join(buckets, "+"), plaintext[0]),
			Fix: "these were written within --since, so they are not pre-enablement residue. " + e1Fix,
		})
	}
	if len(unknown) > 0 {
		out = append(out, Finding{
			Check:   "object",
			Problem: fmt.Sprintf("%d of %d sampled objects could not be classified (e.g. %s)", len(unknown), len(fresh), unknown[0]),
			Fix:     "an unclassifiable response is not a pass; check the key's permissions on this bucket",
		})
	}
	if len(out) == 0 {
		// len(fresh), not len(keys): the number that means anything is how many
		// objects were actually JUDGED.
		fmt.Printf("  object: 0 of %d judged objects in %s were plaintext (a clean sample is evidence, not proof of full coverage)\n",
			len(fresh), strings.Join(buckets, "+"))
	}
	return out
}

// reportResidue probes objects older than the window and says how many are still
// plaintext. Never a finding — see checkObjectsAreEncrypted — but never silent
// either: an operator who believes "the provider encrypts now" needs to know what
// it did not reach.
func reportResidue(ak, sk, endpoint string, old []objstore.ObjectRef) {
	var plaintext []string
	for _, k := range old {
		if v, _ := ObjectSSEProbe(ak, sk, endpoint, k.Bucket, k.Key); v == ssePlaintext {
			plaintext = append(plaintext, k.Bucket+"/"+k.Key)
		}
	}
	if len(plaintext) > 0 {
		fmt.Printf("::notice::assert-obj-encryption: %d of %d older sampled objects are still PLAINTEXT "+
			"(e.g. %s). Objects written before Linode enabled SSE-S3 stay unencrypted until rewritten "+
			"in place with copy-object.\n", len(plaintext), len(old), plaintext[0])
	}
}
