package main

import (
	"errors"
	"strings"
	"testing"
)

func TestPolicyImageReference(t *testing.T) {
	raw := []byte(`{"spec":{"rules":[{"verifyImages":[{"imageReferences":[
	  "ghcr.io/akamai-consulting/llz:*","ghcr.io/akamai-consulting/llz@*"]}]}]}}`)
	got, err := policyImageReference(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "ghcr.io/akamai-consulting/llz:") {
		t.Errorf("the canary must use a ref the policy matches, got %q", got)
	}
	if strings.Contains(got, "*") {
		t.Errorf("the glob must be resolved to a concrete tag, got %q", got)
	}

	// A policy verifying nothing is a failure, not a pass: there is no canary that
	// could prove enforcement of a rule that matches no images.
	if _, err := policyImageReference([]byte(`{"spec":{"rules":[]}}`)); err == nil {
		t.Error("a policy with no imageReferences must be an error")
	}
	if _, err := policyImageReference([]byte(`nope`)); err == nil {
		t.Error("an unparseable policy must be an error")
	}
}

func TestClassifySignatureCanary(t *testing.T) {
	// Admitted = not enforcing. This is the regression the gate exists for.
	v := classifySignatureCanary("pod/llz-signature-canary created (server dry run)", nil)
	if v.FailWhy == "" {
		t.Fatal("an ADMITTED unsigned image must fail the gate")
	}
	if !strings.Contains(v.FailWhy, "failurePolicy: Ignore") {
		t.Errorf("the failure should mention that a downed Kyverno admits silently, got %q", v.FailWhy)
	}

	// Denied BY the policy = enforcing.
	denied := `Error from server: admission webhook "mutate.kyverno.svc-fail" denied the request: ` +
		`policy Pod/default/llz-signature-canary for resource violation: verify-llz-image-signature: ...`
	if v := classifySignatureCanary(denied, errors.New("exit 1")); v.FailWhy != "" {
		t.Errorf("a denial naming the policy must pass: %s", v.FailWhy)
	}

	// Denied by SOMETHING ELSE proves nothing about this policy — and a laxer
	// check that accepted any denial would report enforcement it never observed.
	other := `Error from server (Forbidden): pods "llz-signature-canary" is forbidden: ` +
		`violates PodSecurity "restricted:latest"`
	v2 := classifySignatureCanary(other, errors.New("exit 1"))
	if v2.FailWhy == "" {
		t.Fatal("a denial from an unrelated admission control must NOT count as proof")
	}
	if !strings.Contains(v2.FailWhy, "NOT by") {
		t.Errorf("the failure should say the denial came from elsewhere, got %q", v2.FailWhy)
	}
}

func TestMutatedStorageClass(t *testing.T) {
	got, err := mutatedStorageClass([]byte(`{"spec":{"storageClassName":"block-storage-retain"}}`))
	if err != nil || got != "block-storage-retain" {
		t.Errorf("unexpected (%q,%v)", got, err)
	}
	if _, err := mutatedStorageClass([]byte(`nope`)); err == nil {
		t.Error("an unparseable dry-run object must be an error")
	}
}

func TestClassifyPVCCanary(t *testing.T) {
	// Mutated = the policy is live.
	ok := classifyPVCCanary(`{"spec":{"storageClassName":"block-storage-retain"}}`, nil)
	if ok.FailWhy != "" {
		t.Errorf("a rewritten class must pass: %s", ok.FailWhy)
	}

	// NOT mutated: every PVC naming the stock class provisions unencrypted, and a
	// StorageClass-name proxy check would still call this healthy. That exact
	// proxy was in place while 13 of 16 PVCs came up unencrypted.
	bad := classifyPVCCanary(`{"spec":{"storageClassName":"linode-block-storage"}}`, nil)
	if bad.FailWhy == "" {
		t.Fatal("an unmutated storageClassName must fail — the encryption mutation is not live")
	}
	if !strings.Contains(bad.FailWhy, "UNENCRYPTED") {
		t.Errorf("the failure should name the consequence, got %q", bad.FailWhy)
	}

	// A dry-run that could not run is a CHECK failure, not evidence about the
	// policy — the same "could not ask is not an answer" split the other gates make.
	cantRun := classifyPVCCanary("connection refused", errors.New("exit 1"))
	if cantRun.FailWhy == "" || !strings.Contains(cantRun.FailWhy, "not evidence") {
		t.Errorf("an un-runnable dry-run must fail as a check failure, got %q", cantRun.FailWhy)
	}

	if v := classifyPVCCanary(`{"spec":{}}`, nil); v.FailWhy == "" {
		t.Error("a response with no storageClassName must fail rather than be assumed good")
	}
}

// seamEnforcement points both cluster interactions at canned results.
func seamEnforcement(t *testing.T, policy []byte, policyErr error, out string, runErr error) {
	oP, oD := readClusterPolicy, dryRunManifest
	t.Cleanup(func() { readClusterPolicy, dryRunManifest = oP, oD })
	readClusterPolicy = func(string) ([]byte, error) { return policy, policyErr }
	dryRunManifest = func(string, ...string) (string, error) { return out, runErr }
}

// An absent policy is a failure: the supply-chain gate is simply not there.
func TestProbeSignatureEnforcementMissingPolicyFails(t *testing.T) {
	seamEnforcement(t, nil, errors.New("NotFound"), "", nil)
	if v := probeSignatureEnforcement(); v.FailWhy == "" {
		t.Error("a missing ClusterPolicy must fail the gate")
	}
}

func TestRunAssertAdmissionEnforcementRejectsUnknownAndEmptyChecks(t *testing.T) {
	if err := runCIAssertAdmissionEnforcement(nil); err == nil {
		t.Error("no checks must fail rather than pass having verified nothing")
	}
	// A typo in the lane wiring must be loud, not a silent no-op.
	if err := runCIAssertAdmissionEnforcement([]string{"signatur"}); err == nil {
		t.Error("an unknown check must fail rather than silently check nothing")
	}
}

func TestRunAssertAdmissionEnforcementHappyPath(t *testing.T) {
	seamEnforcement(t,
		[]byte(`{"spec":{"rules":[{"verifyImages":[{"imageReferences":["ghcr.io/akamai-consulting/llz:*"]}]}]}}`),
		nil,
		"denied the request: verify-llz-image-signature: image is not signed",
		errors.New("exit 1"))
	if err := runCIAssertAdmissionEnforcement([]string{"signature"}); err != nil {
		t.Errorf("a policy-named denial must pass, got %v", err)
	}
}

func TestClassifyCloneCanary(t *testing.T) {
	// Admitted = not enforcing. Every Volume let through this way is UNREAPABLE:
	// the Linode CSI clone API cannot apply the lke<id> ownership tag, so it
	// outlives its cluster permanently — the same cost leak this series closes
	// from the reaper end.
	v := classifyCloneCanary("persistentvolumeclaim/llz-clone-canary created (server dry run)", nil)
	if v.FailWhy == "" {
		t.Fatal("an ADMITTED clone-sourced PVC must fail the gate")
	}
	if !strings.Contains(v.FailWhy, "UNREAPABLE") {
		t.Errorf("the failure must name the consequence, got %q", v.FailWhy)
	}

	denied := `Error from server: admission webhook "validate.kyverno.svc-fail" denied the request: ` +
		`policy PersistentVolumeClaim/istio-system/llz-clone-canary for resource violation: pvc-deny-untaggable-clone: ...`
	if v := classifyCloneCanary(denied, errors.New("exit 1")); v.FailWhy != "" {
		t.Errorf("a denial naming the policy must pass: %s", v.FailWhy)
	}

	// A rejection from anything else proves nothing about THIS policy — a PVC
	// whose dataSource does not exist could be refused by the API server or the
	// CSI for entirely unrelated reasons.
	other := `Error from server (NotFound): persistentvolumeclaims "llz-clone-canary-source" not found`
	v2 := classifyCloneCanary(other, errors.New("exit 1"))
	if v2.FailWhy == "" || !strings.Contains(v2.FailWhy, "NOT by") {
		t.Errorf("an unrelated rejection must not count as proof, got %q", v2.FailWhy)
	}
}
