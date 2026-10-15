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
		Short: "harvest compliance evidence and the instance's answered checklist into an auditor pack",
		Long: "Builds the auditor evidence pack for docs/infosec/control-mapping.md. Harvests\n" +
			"the trivy-operator ClusterComplianceReports apl-core's chart ships (--report;\n" +
			"default k8s-cis-1.23, k8s-nsa-1.0, k8s-pss-baseline-0.1, k8s-pss-restricted-0.1)\n" +
			"and whether the operator's cluster-compliance scanner is on, NetworkPolicy\n" +
			"coverage, restricted-PSS namespaces and the default StorageClass encryption\n" +
			"parameter; records the outcomes of the gates that ran beside it\n" +
			"(--gate <verb>=<PASS|FAIL|SKIPPED>, repeatable; GitHub step outcomes accepted);\n" +
			"and carries the instance's answered checklist from --questionnaire (a data\n" +
			"file in the instance repo; schema in docs/infosec/control-mapping.md).\n\n" +
			"Writes <out-dir>/cis-evidence-<cluster>-<ts>.{json,md,json.sha256} and the step\n" +
			"summary. Fails closed: a report that is absent, empty (0 pass / 0 fail), never\n" +
			"reconciled or older than --max-report-age is NOT_COLLECTED, and so is the whole\n" +
			"pack while the compliance scanner is off. Exits non-zero on a failed report or a\n" +
			"FAIL gate, on an invalid questionnaire, and (with --strict) on a NOT_COLLECTED\n" +
			"pack. --check-questionnaire validates the file and exits without the cluster.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
			// The questionnaire's gates must be real `llz ci` verbs: the siblings of
			// this command in the tree that is running it.
			if p := cmd.Parent(); p != nil {
				o.knownVerbs = map[string]bool{}
				for _, s := range p.Commands() {
					o.knownVerbs[s.Name()] = true
				}
			}
			return runCICISEvidence(o)
		},
	}
	f := c.Flags()
	f.StringVar(&o.cluster, "cluster", os.Getenv("REGION"), "cluster/deployment name for the pack heading (defaults to $REGION)")
	f.StringVar(&o.outDir, "out-dir", cigate.EnvOr("EVIDENCE_OUT_DIR", "."), "directory to write the pack into")
	f.StringArrayVar(&o.gates, "gate", nil, "a gate outcome to record, <verb>=<PASS|FAIL|SKIPPED> (repeatable)")
	f.BoolVar(&o.strict, "strict", cli.EnvBool("EVIDENCE_STRICT", false), "fail when the pack is NOT_COLLECTED (nothing scored, or the compliance scanner is off)")
	f.StringArrayVar(&o.reports, "report", nil, "a ClusterComplianceReport to harvest (repeatable; default: the specs apl-core's trivy-operator chart ships)")
	f.DurationVar(&o.maxReportAge, "max-report-age", DefaultMaxReportAge, "a report reconciled longer ago than this is NOT_COLLECTED")
	f.StringVar(&o.questionnaire, "questionnaire", cigate.EnvOr("EVIDENCE_QUESTIONNAIRE", DefaultQuestionnairePath), "the answered checklist, relative to the working tree (absent = the pack carries none)")
	f.BoolVar(&o.checkQuestionnaire, "check-questionnaire", false, "validate --questionnaire and exit, without reading the cluster")
	return c
}
