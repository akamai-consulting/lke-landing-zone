package templatemanifest

// cobra_manifest.go — the CLI surface for manifest.
//
// Split from manifest.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
	"github.com/spf13/cobra"
)

func Cmd() *cobra.Command {
	var root, classifyPath, listClass string
	c := &cobra.Command{
		Use:   "template-manifest",
		Short: "validate or query the scaffold .template-manifest update classes",
		Long: "Validates that every scaffold file is classified by .template-manifest\n" +
			"(" + manifest.ClassNames() + "), or queries the class/list for callers that need\n" +
			"the same last-match-wins rules. Auto-detects instance-template/ in the\n" +
			"template repo, else .template-manifest in the current directory.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return manifest.Run(root, classifyPath, listClass, os.Stdout, os.Stderr)
		},
	}
	c.Flags().StringVar(&root, "root", "", "scaffold root containing .template-manifest (default: auto-detect instance-template/ or .)")
	c.Flags().StringVar(&classifyPath, "classify", "", "print the update class for a scaffold-relative path")
	c.Flags().StringVar(&listClass, "list", "", "list scaffold files in the given class ("+manifest.ClassNames()+")")
	return c
}
