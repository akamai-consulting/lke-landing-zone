package assertnetwork

// ci_assert_admission_enforcement.go implements `llz ci assert-admission-enforcement`
// — the runtime proof that the admission policies this repo ships are actually
// BOUND AND ENFORCING, not merely present.
//
// It is the same argument assert-wave-health-vap makes for the wave-health VAP,
// applied to the two Kyverno policies that carry a security property:
//
//   verify-llz-image-signature      an unsigned first-party image must be REJECTED
//   pvc-deny-untaggable-clone       a clone-sourced PVC must be REJECTED
//
// It used to carry a third, `pvc`, over pvc-force-encrypted-storage-class. That
// policy is no longer the mechanism and is no longer deployed — see
// probePVCEnforcement for why the check now skips rather than fails.
//
// Why a static check cannot do this. `kubectl get clusterpolicy` proves the YAML
// is in the cluster. It says nothing about whether Kyverno's webhook is
// registered, whether the policy compiled, or whether `validationFailureAction`
// is still Enforce — and a policy that exists but does not enforce looks
// identical in git, in Argo, and in converge. Both of these are one edit
// (Enforce → Audit) away from being decorative, and nothing would go color.Red.
//
// The method is a server-side dry run: submit a resource the policy must act on
// and require the policy's OWN response. Dry-run runs the full admission chain —
// webhooks, mutation, validation — without persisting anything, so this is
// read-only in effect despite exercising the write path.
//
// REQUIRING THE POLICY'S OWN RESPONSE IS LOAD-BEARING. A bare "denied" is not
// proof: PSS, a quota, a namespace that does not exist, or an unrelated policy
// could reject the canary and a laxer check would read that as enforcement it
// never observed. Each check therefore matches on the policy name in the
// response, and a denial from anything else is INCONCLUSIVE — which fails, since
// we cannot claim to have seen the policy work.
//
// A NOTE ON failurePolicy: Ignore. Both policies ship with failurePolicy: Ignore
// so a Sigstore or webhook hiccup cannot wedge cluster-wide admission. That is
// the right production trade-off and it is exactly why this gate matters: with
// Ignore, a Kyverno that is down does not reject anything, it ADMITS everything,
// silently. This gate is the thing that notices.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/harborauth"
)

const (
	// signaturePolicyName is the Kyverno ClusterPolicy that must reject unsigned
	// first-party images.
	signaturePolicyName = "verify-llz-image-signature"
	// pvcEncryptionPolicyName is the ClusterPolicy that must rewrite an
	// unencrypted StorageClass request onto the encrypting one.
	pvcEncryptionPolicyName = "pvc-force-encrypted-storage-class"
	// encryptedStorageClass is the class the PVC policy rewrites to.
	encryptedStorageClass = "block-storage-retain"
	// pvcCanaryNamespace must be one the PVC policy matches on (its rule scopes to
	// [gitea, istio-system]); istio-system exists on every apl-core cluster.
	pvcCanaryNamespace = "istio-system"
	// clonePolicyName denies clone/snapshot-sourced PVCs. The Linode CSI clone API
	// cannot apply the lke<id> ownership tag, so such a Volume is UNREAPABLE — a
	// permanent orphan after teardown, which is the same cost leak the volume
	// selection fixes in this series address from the other end.
	clonePolicyName = "pvc-deny-untaggable-clone"
)

// cloneCanaryManifest is a PVC sourced from a clone, which the policy must deny.
// It names a dataSource that need not exist: the deny fires on the FIELD being
// set, before anything resolves it.
const cloneCanaryManifest = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: llz-clone-canary
  namespace: ` + pvcCanaryNamespace + `
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ` + encryptedStorageClass + `
  dataSource:
    kind: PersistentVolumeClaim
    name: llz-clone-canary-source
  resources:
    requests:
      storage: 1Gi
`

// classifyCloneCanary judges the clone-deny dry run. Pure.
//
// Same rule as the signature canary: the denial must NAME the policy. A PVC
// referencing a dataSource that does not exist could be rejected by the API
// server or the CSI for unrelated reasons, and reading that as proof would
// certify a policy this run never exercised.
func classifyCloneCanary(out string, err error) enforcementVerdict {
	v := enforcementVerdict{Check: "clone", Policy: clonePolicyName}
	switch {
	case err == nil:
		v.FailWhy = "the API server ADMITTED a clone-sourced PVC — " + clonePolicyName + " is not enforcing. " +
			"The Linode CSI clone API cannot apply the lke<id> ownership tag, so every Volume admitted this way is " +
			"UNREAPABLE and outlives its cluster permanently"
	case strings.Contains(out, clonePolicyName):
		v.Detail = clonePolicyName + " rejected the clone-sourced canary — the policy is bound and enforcing"
	default:
		v.FailWhy = "the canary was rejected, but NOT by " + clonePolicyName +
			" — so this run did not observe clone-deny enforcement. Output: " + harborauth.TruncateForError([]byte(out))
	}
	return v
}

// enforcementVerdict is one policy's runtime outcome.
type enforcementVerdict struct {
	Check   string
	Policy  string
	Detail  string
	Skipped bool // the policy this check covers is no longer the mechanism
	FailWhy string
}

// ── shared dry-run seam ──────────────────────────────────────────────────────

// dryRunManifest server-dry-runs a manifest, returning combined output and error.
// Seamed for tests. Distinct from assert-wave-health-vap's dryRunCanaryFn so the
// two gates can be stubbed independently.
var dryRunManifest = func(manifest string, extraArgs ...string) (string, error) {
	args := append([]string{"apply", "--dry-run=server", "-f", "-"}, extraArgs...)
	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// readClusterPolicy fetches a Kyverno ClusterPolicy. Seamed for tests.
var readClusterPolicy = func(name string) ([]byte, error) {
	return deps.Exec("kubectl", "get", "clusterpolicy", name, "-o", "json")
}

// ── signature policy ─────────────────────────────────────────────────────────

// policyImageReference returns the first imageReferences glob a ClusterPolicy
// verifies, so the canary uses a ref the policy ACTUALLY matches rather than one
// this file hardcodes. Pure.
//
// Hardcoding the image would make the canary silently unmatched the day the org
// or image name changes: the policy would ignore it, the dry-run would succeed,
// and the gate would report "not enforcing" against a perfectly healthy policy.
// Reading the glob from the live policy keeps the canary aimed at whatever the
// policy is actually guarding.
func policyImageReference(raw []byte) (string, error) {
	var p struct {
		Spec struct {
			Rules []struct {
				VerifyImages []struct {
					ImageReferences []string `json:"imageReferences"`
				} `json:"verifyImages"`
			} `json:"rules"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", fmt.Errorf("decoding ClusterPolicy %s: %w", signaturePolicyName, err)
	}
	for _, r := range p.Spec.Rules {
		for _, vi := range r.VerifyImages {
			for _, ref := range vi.ImageReferences {
				// Turn the glob into a concrete, certainly-unsigned tag.
				// "ghcr.io/org/llz:*" → "ghcr.io/org/llz:llz-unsigned-canary".
				if base, ok := strings.CutSuffix(ref, ":*"); ok {
					return base + ":llz-admission-canary-unsigned", nil
				}
				if base, ok := strings.CutSuffix(ref, "@*"); ok {
					return base + ":llz-admission-canary-unsigned", nil
				}
			}
		}
	}
	return "", fmt.Errorf("ClusterPolicy %s declares no imageReferences — nothing is being verified", signaturePolicyName)
}

// signatureCanaryManifest is a minimal, restricted-PSS-compliant Pod using the
// unsigned reference. PSS compliance matters: it removes every reason OTHER than
// the signature policy for admission to object.
func signatureCanaryManifest(image string) string {
	return `apiVersion: v1
kind: Pod
metadata:
  name: llz-signature-canary
  namespace: default
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: canary
      image: ` + image + `
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: {drop: ["ALL"]}
`
}

// classifySignatureCanary turns the dry-run result into a verdict. Pure.
func classifySignatureCanary(out string, err error) enforcementVerdict {
	v := enforcementVerdict{Check: "signature", Policy: signaturePolicyName}
	switch {
	case err == nil:
		v.FailWhy = "the API server ADMITTED a Pod running an UNSIGNED first-party image — " +
			signaturePolicyName + " is not enforcing. Either the policy is absent, its " +
			"validationFailureAction is no longer Enforce, Kyverno's webhook is not registered, or " +
			"Kyverno is down (failurePolicy: Ignore means a webhook outage admits everything silently). " +
			"Check: kubectl get clusterpolicy " + signaturePolicyName + " -o yaml, and the kyverno pods"
	case strings.Contains(out, signaturePolicyName):
		v.Detail = signaturePolicyName + " rejected the unsigned-image canary — the policy is bound and enforcing"
	default:
		// Denied, but not by the policy under test. We did not observe enforcement.
		v.FailWhy = "the canary was rejected, but NOT by " + signaturePolicyName +
			" — so this run did not observe signature enforcement. A denial from PSS, a quota, or an " +
			"unrelated policy proves nothing about the one under test. Output: " + harborauth.TruncateForError([]byte(out))
	}
	return v
}

// ── PVC encryption policy ────────────────────────────────────────────────────

func probeSignatureEnforcement() enforcementVerdict {
	raw, err := readClusterPolicy(signaturePolicyName)
	if err != nil {
		return enforcementVerdict{Check: "signature", Policy: signaturePolicyName,
			FailWhy: fmt.Sprintf("ClusterPolicy %s not found (%v) — the supply-chain gate is absent entirely", signaturePolicyName, err)}
	}
	image, err := policyImageReference(raw)
	if err != nil {
		return enforcementVerdict{Check: "signature", Policy: signaturePolicyName, FailWhy: err.Error()}
	}
	out, runErr := dryRunManifest(signatureCanaryManifest(image))
	v := classifySignatureCanary(out, runErr)
	if v.FailWhy == "" {
		v.Detail += " (canary image " + image + ")"
	}
	return v
}

// probePVCEnforcement no longer asserts anything, and that is the correct
// behaviour rather than a gap.
//
// This check required pvc-force-encrypted-storage-class to rewrite a PVC naming
// the stock class onto block-storage-retain. #382 REPLACED that mechanism, and
// its own commentary in ci_bootstrap_cluster.go explains why the policy could
// never have worked: apl-core installs Kyverno AND creates the PVCs, and creates
// most of them first. Measured on lke637888, kyverno-admission-controller went
// Available at 15:59:50 with 11 of 13 PVCs already created. A mutation applied
// the instant Kyverno can admit one still arrives too late for 11 of them.
//
// What ships instead is `llz ci bootstrap-cluster` recreating LKE's stock
// StorageClasses with the CSI encryption parameter, 67 seconds before the first
// PVC exists. On lke638084 both classes came up
// `encrypted="" → encrypted="true"`, and the ClusterPolicy is not installed at
// all — `clusterpolicies.kyverno.io "pvc-force-encrypted-storage-class" not
// found`. The check was therefore asserting a mechanism that had already been
// deliberately retired, on a cluster that was behaving correctly.
//
// It SKIPS rather than being deleted so that `--checks signature,pvc,clone` from
// an older vendored workflow keeps working; an unknown check is a hard failure
// here by design, so silently removing the name would break those callers.
//
// The invariant itself is NOT uncovered: assert-volume-encryption gates the
// outcome — every PV-backed Linode Volume encrypted at rest — against the Linode
// API, and its failure text already points at the stock classes and at LKE
// re-promoting its own unencrypted definitions. Adding a second, differently
// shaped assertion of the same property here is how two gates drift apart.
func probePVCEnforcement() enforcementVerdict {
	return enforcementVerdict{
		Check:   "pvc",
		Policy:  pvcEncryptionPolicyName,
		Skipped: true,
		Detail: "SKIP: " + pvcEncryptionPolicyName + " is no longer the encryption mechanism and is no longer " +
			"deployed — #382 recreates LKE's stock StorageClasses encrypted at bootstrap, because a Kyverno " +
			"mutation cannot win the race against apl-core's own PVCs. The outcome is gated by " +
			"assert-volume-encryption against the Linode API.",
	}
}

func runCIAssertAdmissionEnforcement(checks []string) error {
	fmt.Println("## Admission-enforcement assertion (are the shipped policies actually live?)")
	if len(checks) == 0 {
		fmt.Fprintln(os.Stderr, "::error::no --checks given — refusing to pass having verified nothing")
		return fmt.Errorf("no --checks given — refusing to pass vacuously")
	}

	var vs []enforcementVerdict
	for _, c := range checks {
		switch c {
		case "signature":
			vs = append(vs, probeSignatureEnforcement())
		case "pvc":
			vs = append(vs, probePVCEnforcement())

		case "clone":
			out, runErr := dryRunManifest(cloneCanaryManifest)
			vs = append(vs, classifyCloneCanary(out, runErr))
		default:
			// An unknown check is a FAILURE, not a skip: a typo in the lane wiring
			// would otherwise silently reduce this gate to checking nothing.
			vs = append(vs, enforcementVerdict{Check: c,
				FailWhy: "unknown check — the lane is asking for something this verb does not implement"})
		}
	}

	var bad []string
	observed := 0
	for _, v := range vs {
		switch {
		case v.FailWhy != "":
			fmt.Printf("FAIL: %s — %s\n", v.Check, v.FailWhy)
			bad = append(bad, v.Check)
		case v.Skipped:
			fmt.Printf("%s\n", v.Detail)
		default:
			observed++
			fmt.Printf("OK: %s — %s\n", v.Check, v.Detail)
		}
	}
	sort.Strings(bad)

	if len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "::error::admission policies not enforcing: %s\n", strings.Join(bad, ", "))
		return fmt.Errorf("admission policies not enforcing: %s", strings.Join(bad, ", "))
	}
	// Refuse to report success having only skipped. A suite reduced to skips is the
	// vacuous pass this whole file exists to prevent.
	if observed == 0 {
		fmt.Fprintln(os.Stderr, "::error::every requested check skipped — nothing was observed enforcing")
		return fmt.Errorf("every requested check skipped — refusing to pass vacuously")
	}
	fmt.Printf("All %d admission policy check(s) observed enforcing.\n", observed)
	return nil
}
