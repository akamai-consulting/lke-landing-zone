package templatemanifest

// cobra_manifest.go — the CLI surface for manifest.
//
// Split from manifest.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
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
		RunE: func(cmd *cobra.Command, _ []string) error {
			// FENCED AT THE TREE CONTAINING THE SCAFFOLD, not at the scaffold.
			// checkCopierFencing reads copier.yml from the scaffold root's PARENT,
			// so a reader rooted at instance-template/ would refuse it — and the
			// check has a "no copier config here" no-op branch, so the refusal
			// would not fail the gate. It would silently stop checking, which is
			// the vacuous-green shape this whole family of guards exists to refuse.
			//
			// RepoContaining puts the fence one level up and re-expresses the
			// scaffold under it, so `--root ../instance-template` becomes a reader
			// at `..` scanning `instance-template` — with copier.yml in reach.
			// AN EMPTY --root MEANS AUTO-DETECT and must stay empty. Clean("")
			// is ".", so handing it to RepoContaining would turn "look for
			// instance-template/ then ." into "look in .", silently scanning
			// tools/ when the gate is run with no flags.
			repo, rel := capability.RepoContaining(gateBinding(), root)
			if root == "" {
				repo, rel = capability.RepoAt(gateBinding(), "."), ""
			}
			// The COMMAND's writers, not os.Stdout/os.Stderr. The gate driver sets
			// both (c.SetOut/c.SetErr) so it can capture a gate's findings, and
			// this one wrote past them. OutOrStdout falls back to os.Stdout when
			// unset, so nothing changes for a plain CLI invocation.
			return manifest.RunFrom(repo, rel, classifyPath, listClass, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	c.Flags().StringVar(&root, "root", "", "scaffold root containing .template-manifest (default: auto-detect instance-template/ or .)")
	c.Flags().StringVar(&classifyPath, "classify", "", "print the update class for a scaffold-relative path")
	c.Flags().StringVar(&listClass, "list", "", "list scaffold files in the given class ("+manifest.ClassNames()+")")
	return c
}
