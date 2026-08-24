package summarysecret

// cobra_guard.go — the CLI surface for guard.
//
// Split from guard.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "summary-secret-guard",
		Short: "fail when a function that masks secrets writes a computed value to the job summary",
		Long: "Parses the Go tree and, for every function that calls `ghsecret.Mask`, requires\n" +
			"that each argument to a $GITHUB_STEP_SUMMARY append be a string literal —\n" +
			"unless the call site is registered with a reason.\n" +
			"\n" +
			"`ghsecret.Mask` redacts the LOG stream. A job summary is a Markdown file\n" +
			"GitHub renders exactly as written, and Actions READ — a far wider grant than\n" +
			"environment-secret write — is enough to open it. `llz ci bao-init` masked the\n" +
			"OpenBao root token and all five recovery shares and then wrote the raw init\n" +
			"payload into a fenced block; the mask three lines above is what made the\n" +
			"append look safe. The Mask call is the author's own marker that the function\n" +
			"holds material that must not be printed, so this scopes the strict rule to\n" +
			"exactly those functions.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return Run(root)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root")
	return c
}
