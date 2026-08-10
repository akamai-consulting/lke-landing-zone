package assertobs

// cobra_promrulescheck.go — the CLI surface for promrulescheck.
//
// Split from promrulescheck.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"os"

	"github.com/spf13/cobra"
)

func CheckPromRulesCmd() *cobra.Command {
	var rulesDirs []string
	c := &cobra.Command{
		Use:   "check-prom-rules [file ...]",
		Short: "promtool check rules over PrometheusRule CRDs (extracts spec.groups first)",
		Long: "Native port of the former template-scripts/linting-and-validation/\n" +
			"check-prometheus-rule-crds.py (the Makefile's prom-rules-check). For each\n" +
			"PrometheusRule CRD it extracts spec.groups into the bare-groups document\n" +
			"`promtool check rules` understands, then runs promtool against it. With no\n" +
			"file args it validates every *.yaml under --rules-dir (tolerating both the\n" +
			"instance layout and the template's instance-template/ nesting), skipping\n" +
			"cleanly when that directory is absent.",
		RunE: func(_ *cobra.Command, args []string) error {
			return runCICheckPromRules(rulesDirs, args, os.Stdout)
		},
	}
	c.Flags().StringSliceVar(&rulesDirs, "rules-dir", defaultPromRulesDirs,
		"directories walked for PrometheusRule CRDs when no file args are given (repeatable)")
	return c
}
