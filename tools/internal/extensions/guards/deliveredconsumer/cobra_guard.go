package deliveredconsumer

// cobra_guard.go — the CLI surface for Run.
//
// Split from guard.go so the directory shows its commands at a glance: every file
// named cobra_*.go here is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

// Cmd is `llz ci delivered-consumer-guard`.
func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "delivered-consumer-guard",
		Short: "fail when a delivered `managed` file has no consumer, or names one that is gone",
		Long: "Every file .template-manifest classifies `managed` is shipped to every instance\n" +
			"and re-delivered by every `llz upgrade`. This asserts each one names something\n" +
			"that reads it (deliveredconsumer.Consumers), and that consumers which are code\n" +
			"symbols or repo paths still exist.\n\n" +
			"apl-values/values.yaml was a 425-line managed file whose renderer was retired at\n" +
			"the managed App Platform pivot. The file kept shipping, `llz upgrade` kept\n" +
			"overwriting local edits to it, and four docs kept sending operators there. An\n" +
			"instance carried a Loki WAL-replay fix in it, believed it applied, and ran an\n" +
			"OOM-crashlooping ingester for 16 days with log ingestion down. Deleting the\n" +
			"renderer would now fail this gate in the same commit.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return Run(root) },
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	return c
}
