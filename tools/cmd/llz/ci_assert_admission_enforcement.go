package main

// ci_assert_admission_enforcement.go implements `llz ci assert-admission-enforcement`
// — the runtime proof that the admission policies this repo ships are actually
// BOUND AND ENFORCING, not merely present.
//
// It is the same argument assert-wave-health-vap makes for the wave-health VAP,
// applied to the two Kyverno policies that carry a security property:
//
//   verify-llz-image-signature      an unsigned first-party image must be REJECTED
//   pvc-force-encrypted-storage-class  a PVC asking for an unencrypted class must
//                                      be REWRITTEN onto the encrypting one
//
// Why a static check cannot do this. `kubectl get clusterpolicy` proves the YAML
// is in the cluster. It says nothing about whether Kyverno's webhook is
// registered, whether the policy compiled, or whether `validationFailureAction`
// is still Enforce — and a policy that exists but does not enforce looks
// identical in git, in Argo, and in converge. Both of these are one edit
// (Enforce → Audit) away from being decorative, and nothing would go red.
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

	"github.com/spf13/cobra"
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
)

func ciAssertAdmissionEnforcementCmd() *cobra.Command {
	var checks string
	c := &cobra.Command{
		Use:   "assert-admission-enforcement",
		Short: "fail unless the shipped Kyverno policies are bound and actually enforcing",
		Long: "Server-dry-runs a canary against each security-carrying Kyverno policy and\n" +
			"requires that policy's OWN response:\n\n" +
			"  signature — a Pod referencing an UNSIGNED first-party llz image must be\n" +
			"              rejected by verify-llz-image-signature.\n" +
			"  pvc       — a PVC asking for an unencrypted StorageClass must come back\n" +
			"              rewritten to " + encryptedStorageClass + " by\n" +
			"              pvc-force-encrypted-storage-class.\n\n" +
			"`kubectl get clusterpolicy` proves the YAML is present; it says nothing about\n" +
			"whether the webhook is registered, the policy compiled, or\n" +
			"validationFailureAction is still Enforce. A decorative policy looks identical\n" +
			"in git, in Argo and in converge. Both policies also ship failurePolicy: Ignore\n" +
			"so a webhook outage cannot wedge admission — which means a Kyverno that is\n" +
			"down ADMITS everything silently. This is what notices.\n\n" +
			"A denial from anything OTHER than the policy under test is inconclusive and\n" +
			"fails: we cannot claim to have observed enforcement we did not see. Dry-run\n" +
			"only — nothing is persisted. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertAdmissionEnforcement(splitCSVList(checks))
		},
	}
	c.Flags().StringVar(&checks, "checks", "signature,pvc",
		"comma-separated checks to run (signature, pvc)")
	return c
}

// enforcementVerdict is one policy's runtime outcome.
type enforcementVerdict struct {
	Check   string
	Policy  string
	Detail  string
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
	return execOutput("kubectl", "get", "clusterpolicy", name, "-o", "json")
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
			"unrelated policy proves nothing about the one under test. Output: " + truncateForError([]byte(out))
	}
	return v
}

// ── PVC encryption policy ────────────────────────────────────────────────────

// pvcCanaryManifest asks for the unencrypted LKE stock class, which the policy
// must rewrite. Small and ReadWriteOnce so it is an ordinary request in every
// respect except the class it names.
const pvcCanaryManifest = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: llz-pvc-encryption-canary
  namespace: ` + pvcCanaryNamespace + `
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: linode-block-storage
  resources:
    requests:
      storage: 1Gi
`

// mutatedStorageClass extracts spec.storageClassName from a dry-run's returned
// object. Pure.
func mutatedStorageClass(raw []byte) (string, error) {
	var obj struct {
		Spec struct {
			StorageClassName string `json:"storageClassName"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("decoding the dry-run PVC: %w", err)
	}
	return obj.Spec.StorageClassName, nil
}

// classifyPVCCanary judges the dry-run's returned object. Pure.
//
// This policy MUTATES rather than denies, so the assertion is on what came back,
// not on whether it was rejected. An unmutated class means every PVC that asks
// for the stock class provisions UNENCRYPTED — the exact state in which 13 of 16
// PVCs on lke637888 came up unencrypted while a StorageClass-name proxy check
// reported everything fine.
func classifyPVCCanary(out string, err error) enforcementVerdict {
	v := enforcementVerdict{Check: "pvc", Policy: pvcEncryptionPolicyName}
	if err != nil {
		v.FailWhy = "the PVC canary could not be dry-run (" + truncateForError([]byte(out)) + ") — " +
			"this is a check failure, not evidence about the policy"
		return v
	}
	got, perr := mutatedStorageClass([]byte(out))
	if perr != nil {
		v.FailWhy = perr.Error()
		return v
	}
	switch got {
	case encryptedStorageClass:
		v.Detail = fmt.Sprintf("%s rewrote linode-block-storage → %s — the mutation is live", pvcEncryptionPolicyName, got)
	case "":
		v.FailWhy = "the dry-run PVC came back with no storageClassName — the response is not the shape this check can judge"
	default:
		v.FailWhy = fmt.Sprintf("a PVC asking for linode-block-storage came back as %q, not %q — "+
			"%s is not mutating. Every PVC that names the stock class will provision UNENCRYPTED, and a "+
			"StorageClass-name check would still report it healthy",
			got, encryptedStorageClass, pvcEncryptionPolicyName)
	}
	return v
}

// ── orchestration ────────────────────────────────────────────────────────────

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

func probePVCEnforcement() enforcementVerdict {
	// -o json so the MUTATED object comes back for inspection.
	out, err := dryRunManifest(pvcCanaryManifest, "-o", "json")
	return classifyPVCCanary(out, err)
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
		default:
			// An unknown check is a FAILURE, not a skip: a typo in the lane wiring
			// would otherwise silently reduce this gate to checking nothing.
			vs = append(vs, enforcementVerdict{Check: c,
				FailWhy: "unknown check — the lane is asking for something this verb does not implement"})
		}
	}

	var bad []string
	for _, v := range vs {
		if v.FailWhy != "" {
			fmt.Printf("FAIL: %s — %s\n", v.Check, v.FailWhy)
			bad = append(bad, v.Check)
		} else {
			fmt.Printf("OK: %s — %s\n", v.Check, v.Detail)
		}
	}
	sort.Strings(bad)

	if len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "::error::admission policies not enforcing: %s\n", strings.Join(bad, ", "))
		return fmt.Errorf("admission policies not enforcing: %s", strings.Join(bad, ", "))
	}
	fmt.Printf("All %d admission policy check(s) observed enforcing.\n", len(vs))
	return nil
}
