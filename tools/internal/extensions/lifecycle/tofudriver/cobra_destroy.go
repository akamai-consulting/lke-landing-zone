package tofudriver

// cobra_destroy.go — the CLI surface for destroy.
//
// Split from destroy.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"os"

	"github.com/spf13/cobra"
)

func DestroyCmd() *cobra.Command {
	var varFile, planOut string
	var refreshOnly bool
	c := &cobra.Command{
		Use:   "tf-destroy --var-file <f>",
		Short: "terraform destroy via an explicit -destroy plan, or --refresh-only",
		Long: "Native port of the inline destroy/refresh terraform steps. Default: a\n" +
			"two-phase destroy — `terraform plan -destroy -out=<plan> -no-color\n" +
			"-var-file=<f>` then `terraform apply <plan>` (the explicit-plan form the\n" +
			"workflow used, so the destroy that runs is exactly the one shown). With\n" +
			"--refresh-only it instead runs `terraform apply -refresh-only\n" +
			"-auto-approve -var-file=<f>` (repopulate state, no resource changes —\n" +
			"the post-rotation kubeconfig refresh). Mutating: keep it behind the\n" +
			"caller's assert-destroy-confirm guard.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCITFDestroy(os.Stdout, varFile, planOut, refreshOnly)
		},
	}
	c.Flags().StringVar(&varFile, "var-file", "", "terraform -var-file (required)")
	c.Flags().StringVar(&planOut, "plan-out", "destroy-plan.bin", "file for the saved -destroy plan")
	c.Flags().BoolVar(&refreshOnly, "refresh-only", false, "apply -refresh-only instead of destroying")
	_ = c.MarkFlagRequired("var-file")
	return c
}
