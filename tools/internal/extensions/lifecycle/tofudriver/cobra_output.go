package tofudriver

// cobra_output.go — the CLI surface for output.
//
// Split from output.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func OutputCmd() *cobra.Command {
	var asJSON, allowMissing bool
	var outKey, outFile string
	c := &cobra.Command{
		Use:   "tf-output <name>",
		Short: "read one terraform output cleanly (-json internally; no warning leak)",
		Long: "Assimilates the inline `terraform output -raw/-json <name>` reads.\n" +
			"Reads the whole output set via `terraform output -json` (once), so a\n" +
			"zero-output state yields a clean absence instead of leaking Terraform's\n" +
			"'No outputs found' warning into the value. Renders the named output's\n" +
			"value: raw (a string value verbatim; a complex value as compact JSON) by\n" +
			"default, or --json to force compact JSON. Destination: --out-key K appends\n" +
			"`K=<value>` to $GITHUB_OUTPUT; --out-file PATH writes the value there\n" +
			"(e.g. kubeconfig_raw → $KUBECONFIG); otherwise it prints to stdout. A\n" +
			"missing output is an error unless --allow-missing (then it is empty).",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runCITFOutput(args[0], asJSON, allowMissing, outKey, outFile)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "render the value as compact JSON (default: raw for strings)")
	c.Flags().BoolVar(&allowMissing, "allow-missing", false, "a missing output yields an empty value instead of an error")
	c.Flags().StringVar(&outKey, "out-key", "", "append `<key>=<value>` to $GITHUB_OUTPUT instead of printing")
	c.Flags().StringVar(&outFile, "out-file", "", "write the value to this file instead of printing")
	return c
}
