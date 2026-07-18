package main

// ci_assert_wave_health_vap.go implements `llz ci assert-wave-health-vap` — the
// runtime check that the llz-wave-health-guard ValidatingAdmissionPolicy is
// actually BOUND AND ENFORCING on this cluster.
//
// It replaces the old `wave-health-audit` verb, which enumerated every live
// negative-wave resource and re-applied the VAP's own decision to each. That was
// tautological on a converged cluster: the binding is validationActions: [Deny]
// with failurePolicy: Fail over apiGroups/resources ["*"], so a resource the
// audit would flag could never have been ADMITTED in the first place. The audit's
// flagged set is empty by construction — unless the policy isn't enforcing, which
// is the one signal worth keeping and which this asserts directly, in one
// round-trip instead of a full-cluster enumeration (~45s in the e2e assert lanes).
//
// The check is a negative canary: server-dry-run a kind the guard must reject —
// an apps/Deployment (NOT in allowedKinds; the #163 wedge class) at sync-wave -5 —
// and require the API server to deny it. Dry-run runs the full admission chain
// without persisting anything.
//
// It requires the denial to name wave-health-guard. A bare "denied" is not enough:
// PSS, Kyverno, or a quota could reject the canary for an unrelated reason and a
// laxer check would read that as proof of enforcement it never saw. The canary pod
// spec is therefore restricted-PSS-compliant, so the wave-health guard is the only
// policy with a reason to object.

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// waveHealthCanaryNS is a namespace guaranteed to exist on every cluster. Nothing
// is ever created in it — the canary is dry-run only.
const waveHealthCanaryNS = "default"

// waveHealthCanaryManifest is the resource the guard MUST reject: a health-checked
// kind (apps/Deployment — absent from the VAP's allowedKinds) at a negative
// sync-wave. replicas: 0 and a restricted-PSS-compliant pod spec keep every OTHER
// admission policy indifferent to it, so a denial can only come from the guard.
const waveHealthCanaryManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: llz-wave-health-canary
  namespace: ` + waveHealthCanaryNS + `
  annotations:
    argocd.argoproj.io/sync-wave: "-5"
spec:
  replicas: 0
  selector:
    matchLabels: {app: llz-wave-health-canary}
  template:
    metadata:
      labels: {app: llz-wave-health-canary}
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: canary
          image: registry.k8s.io/pause:3.9
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: ["ALL"]}
`

// waveHealthGuardMarker is the substring the VAP's messageExpression emits. The
// denial must carry it, or we did not observe the guard enforcing.
const waveHealthGuardMarker = "wave-health-guard (admission)"

// dryRunCanaryFn server-dry-runs the canary manifest, returning the combined
// output and the error. Seamed for tests.
var dryRunCanaryFn = func(manifest string) (string, error) {
	cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// classifyWaveHealthCanary turns a dry-run result into a verdict. Pure, so the
// three outcomes are unit-testable without a cluster.
//
// Denied naming the guard  → enforcing (pass).
// Admitted                 → the policy is not enforcing (fail): unbound, absent,
//                            or its binding is Audit/Warn rather than Deny.
// Denied by something else → inconclusive (fail): we cannot claim to have seen the
//                            guard work, and a silently-unbound guard is exactly
//                            the regression this exists to catch.
func classifyWaveHealthCanary(out string, err error) (ok bool, msg string) {
	switch {
	case err == nil:
		return false, "the API server ADMITTED a Deployment at sync-wave -5 — llz-wave-health-guard is not enforcing. " +
			"Check that the ValidatingAdmissionPolicy AND its Binding (validationActions: [Deny]) both exist and that the policy's status reports no type-checking error."
	case strings.Contains(out, waveHealthGuardMarker):
		return true, "llz-wave-health-guard denied the negative-wave canary — the policy is bound and enforcing."
	default:
		return false, fmt.Sprintf("the canary was rejected, but NOT by llz-wave-health-guard, so the guard was never exercised: %s",
			strings.TrimSpace(collapseWS(out)))
	}
}

// collapseWS squeezes a multi-line kubectl error into one log line.
func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }

func ciAssertWaveHealthVAPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "assert-wave-health-vap",
		Short: "assert the llz-wave-health-guard admission policy is bound and enforcing",
		Long: "Server-dry-runs a Deployment at sync-wave -5 — a kind the wave-health guard\n" +
			"must reject — and requires the API server to deny it WITH the guard's own\n" +
			"message. Proves the ValidatingAdmissionPolicy and its Deny binding are live,\n" +
			"which is what makes the static `wave-health-guard` check's PR-time verdict\n" +
			"hold at runtime. Nothing is created: dry-run runs admission without\n" +
			"persisting.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			out, err := dryRunCanaryFn(waveHealthCanaryManifest)
			ok, msg := classifyWaveHealthCanary(out, err)
			if !ok {
				return fmt.Errorf("assert-wave-health-vap: %s", msg)
			}
			fmt.Println("assert-wave-health-vap: " + msg)
			return nil
		},
	}
}
