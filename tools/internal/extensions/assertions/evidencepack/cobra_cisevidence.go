package evidencepack

// cobra_cisevidence.go — the CLI surface for harvest.go. Flag wiring and help
// text only.

import (
	"log/slog"
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/spf13/cobra"
)

// CISEvidenceCmd is `llz ci cis-evidence`.
func CISEvidenceCmd() *cobra.Command {
	var o cisEvidenceOpts
	c := &cobra.Command{
		Use:   "cis-evidence",
		Short: "harvest CIS + org-control compliance evidence and the answered org checklist into an auditor pack",
		Long: "Builds the auditor evidence pack for docs/infosec/control-mapping.md. Harvests\n" +
			"the trivy-operator ClusterComplianceReports (cis + llz-org-controls),\n" +
			"NetworkPolicy coverage, restricted-PSS namespaces and the default StorageClass\n" +
			"encryption parameter, records the outcomes of the gates that ran beside it\n" +
			"(--gate <verb>=<PASS|FAIL|SKIPPED>, repeatable; GitHub step outcomes accepted),\n" +
			"and carries the pre-filled org checklist answers (LLZ provides / instance\n" +
			"configures / adopter process, with gaps stated).\n\n" +
			"Writes <out-dir>/cis-evidence-<cluster>-<ts>.{json,md,json.sha256} and the step\n" +
			"summary. Every harvest is best-effort — a missing CRD/unreachable cluster is\n" +
			"recorded as 'not collected', never as passing; a pack that collected nothing\n" +
			"reports NOT_COLLECTED. Exits non-zero on a failed compliance report or a FAIL\n" +
			"gate, and (with --strict) on a NOT_COLLECTED pack.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
			return runCICISEvidence(o)
		},
	}
	f := c.Flags()
	f.StringVar(&o.cluster, "cluster", os.Getenv("REGION"), "cluster/deployment name for the pack heading (defaults to $REGION)")
	f.StringVar(&o.outDir, "out-dir", cigate.EnvOr("EVIDENCE_OUT_DIR", "."), "directory to write the pack into")
	f.StringArrayVar(&o.gates, "gate", nil, "a gate outcome to record, <verb>=<PASS|FAIL|SKIPPED> (repeatable)")
	f.BoolVar(&o.strict, "strict", cli.EnvBool("EVIDENCE_STRICT", false), "fail when the pack collected nothing at all")
	return c
}
