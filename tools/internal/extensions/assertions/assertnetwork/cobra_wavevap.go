package assertnetwork

// cobra_wavevap.go — the CLI surface for wavevap.
//
// Split from wavevap.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"fmt"

	"github.com/spf13/cobra"
)

func WaveHealthVAPCmd() *cobra.Command {
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
			out, err := dryRunCanaryFn(WaveHealthCanaryManifest)
			ok, msg := classifyWaveHealthCanary(out, err)
			if !ok {
				return fmt.Errorf("assert-wave-health-vap: %s", msg)
			}
			fmt.Println("assert-wave-health-vap: " + msg)
			return nil
		},
	}
}
