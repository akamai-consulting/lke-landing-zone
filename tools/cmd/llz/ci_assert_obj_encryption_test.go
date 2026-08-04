package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// pod builds a harbor-registry pod list. certDirs maps container name -> the
// SSL_CERT_DIR value it carries ("" = absent); mounted lists containers with the CA
// volumeMount; hasVolume controls the pod-level volume.
func podJSON(name string, certDirs map[string]string, mounted map[string]bool, hasVolume bool) string {
	var cs []string
	for _, c := range []string{"registry", "registryctl", "istio-proxy"} {
		env := ""
		if v, ok := certDirs[c]; ok && v != "" {
			env = `{"name":"SSL_CERT_DIR","value":"` + v + `"}`
		}
		mnt := ""
		if mounted[c] {
			mnt = `{"name":"llz-obj-proxy-ca"}`
		}
		cs = append(cs, `{"name":"`+c+`","env":[`+env+`],"volumeMounts":[`+mnt+`]}`)
	}
	vols := ""
	if hasVolume {
		vols = `{"name":"llz-obj-proxy-ca"}`
	}
	return `{"items":[{"metadata":{"name":"` + name + `"},"spec":{"containers":[` +
		strings.Join(cs, ",") + `],"volumes":[` + vols + `]}}]}`
}

func fullyMutatedPods() string {
	ca := "/etc/ssl/certs:/etc/pki/tls/certs:/etc/llz/ca"
	return podJSON("harbor-registry-abc",
		map[string]string{"registry": ca, "registryctl": ca},
		map[string]bool{"registry": true, "registryctl": true}, true)
}

func withObjEncKubectl(t *testing.T, out string, err error) {
	t.Helper()
	prev := objEncKubectl
	objEncKubectl = func(...string) (string, error) { return out, err }
	t.Cleanup(func() { objEncKubectl = prev })
}

// A pod admitted while Kyverno was down carries neither the env nor the volume, and
// nothing else in the system notices. This is the check that exists for the
// failurePolicy: Ignore race.
func TestAssertObjEncryptionCatchesPodMissingTheCA(t *testing.T) {
	withObjEncKubectl(t, podJSON("harbor-registry-abc", nil, nil, false), nil)
	f := checkRegistryPodsCarryCA()
	if len(f) < 2 {
		t.Fatalf("want findings for the missing volume AND both containers, got %+v", f)
	}
	joined := ""
	for _, x := range f {
		joined += x.problem
	}
	if !strings.Contains(joined, "llz-obj-proxy-ca") || !strings.Contains(joined, "registry") {
		t.Errorf("findings must name the gaps: %+v", f)
	}
}

// THE regression this rewrite exists for. The jsonpath revision joined SSL_CERT_DIR
// across ALL containers and did a strings.Contains, so a pod where `registry` was
// mutated and `registryctl` was NOT passed cleanly — the one value that came back
// contained the mount path. registryctl runs GC against the same S3 backend, so that
// pod has a lane that silently cannot reach object storage.
func TestAssertObjEncryptionCatchesPartialMutation(t *testing.T) {
	ca := "/etc/ssl/certs:/etc/pki/tls/certs:/etc/llz/ca"
	withObjEncKubectl(t, podJSON("harbor-registry-abc",
		map[string]string{"registry": ca},             // registryctl MISSING
		map[string]bool{"registry": true}, true), nil) // and unmounted
	f := checkRegistryPodsCarryCA()
	if len(f) != 1 {
		t.Fatalf("a half-mutated pod must produce exactly one finding, got %+v", f)
	}
	if !strings.Contains(f[0].problem, "registryctl") {
		t.Errorf("the finding must name the container that is missing it: %q", f[0].problem)
	}
	if !strings.Contains(f[0].fix, "GC") {
		t.Error("the fix must say why registryctl matters, or it reads as pedantry")
	}
}

// A container that has no business reaching S3 must not be required to trust the CA.
func TestAssertObjEncryptionIgnoresUnrelatedContainers(t *testing.T) {
	withObjEncKubectl(t, fullyMutatedPods(), nil)
	if f := checkRegistryPodsCarryCA(); len(f) != 0 {
		t.Errorf("istio-proxy lacking the CA must not be a finding, got %+v", f)
	}
}

func TestAssertObjEncryptionPassesAFullyMutatedPod(t *testing.T) {
	withObjEncKubectl(t, fullyMutatedPods(), nil)
	if f := checkRegistryPodsCarryCA(); len(f) != 0 {
		t.Errorf("a correctly mutated pod must produce no findings, got %+v", f)
	}
}

// A gate that examined nothing must not report green — the same rule the sibling
// guards' requireCorpus enforces.
func TestAssertObjEncryptionFailsWhenNoRegistryPodsExist(t *testing.T) {
	withObjEncKubectl(t, `{"items":[{"metadata":{"name":"other"},"spec":{"containers":[],"volumes":[]}}]}`, nil)
	f := checkRegistryPodsCarryCA()
	if len(f) != 1 || !strings.Contains(f[0].problem, "no harbor-registry pods") {
		t.Errorf("zero pods examined must fail, got %+v", f)
	}
}

// Undecodable output is not a pass.
func TestAssertObjEncryptionFailsOnUndecodablePodList(t *testing.T) {
	withObjEncKubectl(t, "not json", nil)
	if f := checkRegistryPodsCarryCA(); len(f) != 1 || !strings.Contains(f[0].problem, "decode") {
		t.Errorf("a gate that cannot read the pods must fail, got %+v", f)
	}
}

// The failure that looks exactly like success: everything Healthy, nothing routed
// through the proxy, every byte plaintext.
func TestAssertObjEncryptionCatchesMissingDNSRewrite(t *testing.T) {
	withObjEncKubectl(t, "{}", nil)
	f := checkEndpointResolvesToProxy("us-ord-10.linodeobjects.com")
	if len(f) != 1 || !strings.Contains(f[0].problem, "going DIRECT") {
		t.Fatalf("a missing rewrite must fail loudly, got %+v", f)
	}
	if !strings.Contains(f[0].fix, "LAST") {
		t.Error("the fix must preserve the ordering constraint — flipping DNS before trust breaks the registry")
	}
}

func TestAssertObjEncryptionAcceptsAPresentRewrite(t *testing.T) {
	withObjEncKubectl(t,
		`{"objproxy.include":"rewrite name us-ord-10.linodeobjects.com obj-proxy.obj-proxy.svc.cluster.local\n"}`, nil)
	if f := checkEndpointResolvesToProxy("us-ord-10.linodeobjects.com"); len(f) != 0 {
		t.Errorf("a live rewrite must pass, got %+v", f)
	}
}

// A rewrite that names the endpoint but sends it somewhere else is not a pass.
func TestAssertObjEncryptionCatchesRewriteToTheWrongTarget(t *testing.T) {
	withObjEncKubectl(t, `{"objproxy.include":"rewrite name us-ord-10.linodeobjects.com somewhere-else.svc\n"}`, nil)
	if f := checkEndpointResolvesToProxy("us-ord-10.linodeobjects.com"); len(f) != 1 {
		t.Error("a rewrite pointing away from the proxy must fail")
	}
}

// ── check 3, the outcome check ──────────────────────────────────────────────

// withSSECSample stubs the sample listing and a per-key verdict function, so the
// mixed-bucket case (the one a single-object sample got wrong) is testable.
func withSSECSample(t *testing.T, keys []string, listErr error, verdict func(string) ssecVerdict) {
	t.Helper()
	pk, pl := s3ObjectSSECProbe, s3SampleObjectKeys
	s3SampleObjectKeys = func(_, _, _, _ string, _ int) ([]string, error) { return keys, listErr }
	s3ObjectSSECProbe = func(_, _, _, _, key string) (ssecVerdict, string) { return verdict(key), "stub" }
	prevCreds := objEncConsumerCreds
	objEncConsumerCreds = func(_, _, _ string) (string, string, error) { return "ak", "sk", nil }
	t.Cleanup(func() {
		s3ObjectSSECProbe, s3SampleObjectKeys, objEncConsumerCreds = pk, pl, prevCreds
	})
}

func allEncrypted(string) ssecVerdict { return ssecEncrypted }

func TestCheckObjectsEncryptedPassesWhenTheWholeSampleIsEncrypted(t *testing.T) {
	withSSECSample(t, []string{"a", "b", "c"}, nil, allEncrypted)
	if f := checkObjectsAreEncrypted("us-ord-10.linodeobjects.com", "b", 50); len(f) != 0 {
		t.Errorf("a clean sample must pass, got %+v", f)
	}
}

// THE regression this rewrite exists for. A single-object sample reported green on
// a bucket that is 1-in-N plaintext; the whole sample must be probed.
func TestCheckObjectsEncryptedCatchesOnePlaintextAmongMany(t *testing.T) {
	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "rotten"}
	withSSECSample(t, keys, nil, func(k string) ssecVerdict {
		if k == "rotten" {
			return ssecPlaintext
		}
		return ssecEncrypted
	})
	f := checkObjectsAreEncrypted("us-ord-10.linodeobjects.com", "b", 50)
	if len(f) != 1 || !strings.Contains(f[0].problem, "PLAINTEXT") {
		t.Fatalf("one plaintext object among ten must fail, got %+v", f)
	}
	if !strings.Contains(f[0].problem, "1 of 10") {
		t.Errorf("the finding must report the COUNT — that is the auditable number: %q", f[0].problem)
	}
	if !strings.Contains(f[0].fix, "retention") {
		t.Error("the fix must distinguish a bypassing proxy from objects that simply predate it")
	}
}

// A key deleted between LIST and HEAD (compaction) is not evidence of plaintext.
func TestCheckObjectsEncryptedToleratesObjectsThatVanish(t *testing.T) {
	withSSECSample(t, []string{"a", "gone"}, nil, func(k string) ssecVerdict {
		if k == "gone" {
			return ssecAbsent
		}
		return ssecEncrypted
	})
	if f := checkObjectsAreEncrypted("us-ord-10.linodeobjects.com", "b", 50); len(f) != 0 {
		t.Errorf("a raced deletion must not fail the gate, got %+v", f)
	}
}

// An empty bucket and a correctly-encrypted bucket are indistinguishable to every
// other check, so this one must not shrug at zero objects.
func TestCheckObjectsEncryptedFailsOnAnEmptyBucket(t *testing.T) {
	withSSECSample(t, nil, nil, allEncrypted)
	f := checkObjectsAreEncrypted("us-ord-10.linodeobjects.com", "b", 50)
	if len(f) != 1 || !strings.Contains(f[0].problem, "EMPTY") {
		t.Errorf("an empty bucket proves nothing and must not pass, got %+v", f)
	}
}

func TestCheckObjectsEncryptedFailsWhenUnclassifiable(t *testing.T) {
	withSSECSample(t, []string{"k"}, nil, func(string) ssecVerdict { return ssecUnknown })
	if f := checkObjectsAreEncrypted("us-ord-10.linodeobjects.com", "b", 50); len(f) != 1 {
		t.Error("an unclassifiable response is not a pass")
	}
}

// The gate must read the CONSUMER's credentials, not the ambient AWS_* env. In the
// assert-suite those are the Terraform STATE bucket's key, which cannot list the
// data buckets — the first revision reported that 403 as an encryption finding.
func TestCheckObjectsEncryptedFailsWhenConsumerCredsUnreadable(t *testing.T) {
	prev := objEncConsumerCreds
	objEncConsumerCreds = func(_, _, _ string) (string, string, error) {
		return "", "", errors.New("secrets \"loki-s3-linode-credentials\" not found")
	}
	t.Cleanup(func() { objEncConsumerCreds = prev })
	f := checkObjectsAreEncrypted("us-ord-10.linodeobjects.com", "b", 50)
	if len(f) != 1 {
		t.Fatalf("unreadable consumer creds must be one finding, got %+v", f)
	}
	if !strings.Contains(f[0].problem, lokiObjSecretRef) {
		t.Errorf("the finding must name the Secret it could not read: %q", f[0].problem)
	}
	if !strings.Contains(f[0].fix, "NOT the SSE-C key") {
		t.Errorf("the fix must say the probe needs bucket creds but NOT the encryption key: %q", f[0].fix)
	}
}

// An absent llz-cert-automation namespace means the credential component was never
// deployed (managed renders a minimal app set) — not an encryption failure.
func TestCheckHarborPathSkipsWhenCredentialNamespaceAbsent(t *testing.T) {
	prev := namespaceExists
	var asked string
	namespaceExists = func(ns string) (bool, error) { asked = ns; return false, nil }
	t.Cleanup(func() { namespaceExists = prev })
	if f := checkHarborPath("h", "bucket"); len(f) != 0 {
		t.Errorf("an absent credential namespace must not fail the gate, got %+v", f)
	}
	if asked != harborRobotSecretNS {
		t.Errorf("guarded on %q, but the robot Secret lives in %q — checking the wrong namespace is "+
			"how this reported a missing credential as an encryption finding", asked, harborRobotSecretNS)
	}
}

// NO REGION MUST SKIP, NOT FAIL. The first revision fell through to the flag checks
// and hard-failed on an empty --endpoint, which redded the whole battery for a
// missing argument and reported it as an encryption failure. A cluster that cannot
// say which deployment it is cannot say whether the gateway is enabled on it.
func TestAssertObjEncryptionSkipsWithNoRegion(t *testing.T) {
	if err := runAssertObjEncryption("", "", "", "", 50); err != nil {
		t.Fatalf("no --region must SKIP, not fail: %v", err)
	}
}

func TestSSECVerdictStrings(t *testing.T) {
	for v, want := range map[ssecVerdict]string{
		ssecEncrypted: "encrypted", ssecPlaintext: "PLAINTEXT",
		ssecAbsent: "absent", ssecUnknown: "unknown",
	} {
		if got := fmt.Sprint(v); got != want {
			t.Errorf("verdict %d = %q, want %q", v, got, want)
		}
	}
}

// The lane must be in the battery, GATING, and it must carry the harbor-bucket —
// without which the CA-chain check silently does not run. The lane list is the one
// place a new gate can be declared and never actually run.
func TestObjEncryptionLaneIsRegisteredAndGating(t *testing.T) {
	lanes := assertSuiteLanes("e2e")
	var found *suiteLane
	for i := range lanes {
		if lanes[i].Name == "obj-encryption" {
			found = &lanes[i]
		}
	}
	if found == nil {
		t.Fatal("obj-encryption is not in assertSuiteLanes — nothing would ever run the gate")
	}
	if !found.Gating {
		t.Error("the lane must GATE: a report-only encryption check is a check nobody acts on")
	}
	flat := strings.Join(found.Steps[0], " ")
	if !strings.Contains(flat, "assert-obj-encryption") || !strings.Contains(flat, "--region") {
		t.Errorf("lane step must invoke the gate with a region: %q", flat)
	}
	// It must carry NOTHING else. Endpoint and bucket names are derived from the
	// spec; the earlier revision passed them from three env vars that no workflow
	// exported, so the lane would have failed on a missing flag rather than on
	// encryption — a gate misconfigured into always-red teaches people to ignore it.
	for _, invented := range []string{"OBJ_ENDPOINT_HOST", "LOKI_CHUNKS_BUCKET", "HARBOR_REGISTRY_BUCKET"} {
		if strings.Contains(flat, invented) {
			t.Errorf("lane still reads %s, which nothing sets: %q", invented, flat)
		}
	}
}

// The gate self-skips when the component is off, and the skip must be LOUD — a
// silent pass is indistinguishable from a real one, and would mean disabling the
// component silences the gate that guards it.
func TestAssertObjEncryptionSkipsLoudlyWhenComponentDisabled(t *testing.T) {
	// No spec on disk in the test working dir, so LoadInstance fails — which proves
	// the region path is reached at all. The behavioural contract (skip vs run) is
	// exercised through ssecSeedEnabled's own tests.
	if err := runAssertObjEncryption("h", "b", "", "no-such-env", 5); err == nil {
		t.Skip("no instance spec in the test dir; component gating covered by the seeder tests")
	}
}

// Harbor absent is NOT a finding — managed renders a minimal app set. Harbor
// present with no --harbor-bucket IS, because that is the CA-chain check being
// skipped on a cluster that needs it.
func TestCheckHarborPathSkipsWhenNamespaceAbsent(t *testing.T) {
	prev := namespaceExists
	namespaceExists = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { namespaceExists = prev })
	if f := checkHarborPath("h", ""); len(f) != 0 {
		t.Errorf("an absent harbor namespace must not fail the gate, got %+v", f)
	}
}

func TestCheckHarborPathFailsWhenBucketOmittedButHarborPresent(t *testing.T) {
	prev := namespaceExists
	namespaceExists = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { namespaceExists = prev })
	f := checkHarborPath("h", "")
	if len(f) != 1 || !strings.Contains(f[0].problem, "CA-chain check did not run") {
		t.Fatalf("omitting --harbor-bucket on a Harbor cluster must fail, got %+v", f)
	}
	if !strings.Contains(f[0].fix, "samples Loki") {
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

// A gate's fix text is its deliverable. The first revision printed "this is the CA
// chain" for EVERY push failure, including an unreachable registry and a rejected
// credential — sending the reader to a component that was working fine while the
// real one was down.
func TestHarborPushFixAttributesTheFailureToItsStage(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantSub string
		mustNot string
	}{
		{"registry down", &harborPushError{stage: pushStageReach, err: errors.New("x")},
			"REGISTRY is unreachable", "CA"},
		{"bad credential", &harborPushError{stage: pushStageAuth, err: errors.New("x")},
			"AUTHORIZATION failure", "CONFIRMED CA"},
		{"no push scope", &harborPushError{stage: pushStageSession, err: errors.New("x")},
			"not evidence about the CA", "CONFIRMED CA"},
		{"storage, cert named", &harborPushError{stage: pushStageComplete, err: errors.New("x"),
			body: `{"errors":[{"detail":"x509: certificate signed by unknown authority"}]}`},
			"CONFIRMED CA CHAIN", ""},
		{"storage, cause unclear", &harborPushError{stage: pushStageComplete, err: errors.New("x"),
			body: `{"errors":[{"detail":"NoSuchBucket"}]}`},
			"before assuming it", "CONFIRMED CA"},
	}
	for _, tc := range cases {
		got := harborPushFix(tc.err)
		if !strings.Contains(got, tc.wantSub) {
			t.Errorf("%s: fix = %q, want it to mention %q", tc.name, got, tc.wantSub)
		}
		if tc.mustNot != "" && strings.Contains(got, tc.mustNot) {
			t.Errorf("%s: fix must NOT blame %q: %q", tc.name, tc.mustNot, got)
		}
	}
}

// An error that is not a staged push error must not be attributed at all.
func TestHarborPushFixDoesNotGuessOnUnknownErrors(t *testing.T) {
	if got := harborPushFix(errors.New("something else")); !strings.Contains(got, "unclassified") {
		t.Errorf("an unstaged error must not be attributed to a stage: %q", got)
	}
}

func TestLooksLikeTLSFailure(t *testing.T) {
	for _, s := range []string{
		"x509: certificate signed by unknown authority",
		"tls: failed to verify certificate",
		"certificate is valid for foo, not bar",
	} {
		if !looksLikeTLSFailure(s) {
			t.Errorf("should read as a TLS failure: %q", s)
		}
	}
	for _, s := range []string{"NoSuchBucket", "AccessDenied", ""} {
		if looksLikeTLSFailure(s) {
			t.Errorf("must NOT read as a TLS failure: %q", s)
		}
	}
}
