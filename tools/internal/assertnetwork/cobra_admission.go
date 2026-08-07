package assertnetwork

// cobra_admission.go — the CLI surface for admission.
//
// Split from admission.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/spf13/cobra"
)

func AdmissionEnforcementCmd() *cobra.Command {
	var checks string
	c := &cobra.Command{
		Use:   "assert-admission-enforcement",
		Short: "fail unless the shipped Kyverno policies are bound and actually enforcing",
		Long: "Server-dry-runs a canary against each security-carrying Kyverno policy and\n" +
			"requires that policy's OWN response:\n\n" +
			"  signature — a Pod referencing an UNSIGNED first-party llz image must be\n" +
			"              rejected by verify-llz-image-signature.\n" +
			"  pvc       — SKIPS. pvc-force-encrypted-storage-class is no longer the\n" +
			"              encryption mechanism and is no longer deployed; #382 recreates\n" +
			"              LKE's stock StorageClasses encrypted at bootstrap instead,\n" +
			"              because a Kyverno mutation cannot win the race against\n" +
			"              apl-core's own PVCs. assert-volume-encryption gates the outcome.\n" +
			"  clone     — a clone/snapshot-sourced PVC must be rejected by\n" +
			"              " + clonePolicyName + ". The Linode CSI clone API cannot\n" +
			"              apply the lke<id> ownership tag, so such a Volume is UNREAPABLE.\n\n" +
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
			return runCIAssertAdmissionEnforcement(cigate.SplitCSVList(checks))
		},
	}
	c.Flags().StringVar(&checks, "checks", "signature,clone",
		"comma-separated checks to run (signature, clone). `pvc` is accepted and SKIPS — see probePVCEnforcement")
	return c
}
