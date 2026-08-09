package assertsecrets

// cobra_rotationhealth.go — the CLI surface for rotationhealth.
//
// Split from rotationhealth.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"time"

	"github.com/spf13/cobra"
)

func RotationHealthCmd() *cobra.Command {
	var prom, namespace string
	var settle, interval int
	var strict, requireInventory bool
	c := &cobra.Command{
		Use:   "assert-rotation-health",
		Short: "fail unless every rotatable credential is being observed and is within its rotation SLA",
		Long: "Gates the credential-rotation lifecycle. For every credential reconcilelanes.CredPaths declares\n" +
			"with an ALERTABLE class (automated / on-demand), asserts a\n" +
			"llz_credential_age_days series exists AND its age is within the class SLA.\n\n" +
			"The missing series is the point. reconcilelanes.CredPaths declares the credential, the\n" +
			"openbao-gauges lane samples it, and LLZCredentialRotationOverdue alerts on the\n" +
			"result — so a credential that is declared but publishes nothing disappears from\n" +
			"the single pane AND can never fire an alert, because a rule over an absent\n" +
			"series never evaluates. That is the native failure of this subsystem (the\n" +
			"`static` class exists because a whole group of paths had silently published no\n" +
			"series at all) and nothing gated it.\n\n" +
			"Non-alertable classes (generate-once / tracks-source / static) are reported,\n" +
			"never gated: nothing will ever lower their age, so failing on it would be a\n" +
			"permanent color.Red. --strict also gates their 365d info threshold.\n\n" +
			"ALSO gates the GitHub write-time lane: the secret-age probe authenticated, every\n" +
			"ghSecretTargets credential expected present IS present, and the one expected\n" +
			"absent (OPENBAO_ROOT_TOKEN) is absent. That feed has no age when it breaks, so\n" +
			"the age assertion above cannot reach it. Skipped with a loud message where the\n" +
			"inventory writer has never run; --require-inventory makes it a failure.\n\n" +
			"Does NOT force a rotation. assert-broad-pat-rotation already exercises one full\n" +
			"cycle and is safe only because its PAT family is throwaway; forcing lke-admin,\n" +
			"obj-key, db-admin or the state passphrase mid-run would break the cluster the\n" +
			"gate is measuring. Read-only. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertRotationHealth(prom, namespace, strict, requireInventory,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().BoolVar(&requireInventory, "require-inventory", false,
		"fail if the token-inventory ConfigMap has never been written (default: report the skip). "+
			"For callers that run `llz ci token-inventory` themselves — there, an absent inventory is a break, not a fresh cluster")
	c.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"the Prometheus Service as <namespace>/<name>:<port> to port-forward to")
	c.Flags().StringVar(&namespace, "namespace", "llz-reconciler", "namespace label the gauges carry")
	c.Flags().BoolVar(&strict, "strict", false,
		"also gate the non-alertable classes against the 365d info threshold")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
