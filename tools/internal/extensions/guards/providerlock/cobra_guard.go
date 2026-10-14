package providerlock

// cobra_guard.go — the CLI surface for Run.
//
// Split from providerlock.go so the directory shows its commands at a glance:
// every file named cobra_*.go here is flag wiring and help text, and nothing else.
//
// IT USED TO CARRY --instance, and that flag is gone with the split that made it
// necessary. It asked this same question inside an adopter's repo, because the
// lockfile shipped there as `owned` while the constraint shipped in the ci image.
// Now both halves live in the embedded roots and `llz render` lays them down
// together, so an instance has no lock of its own to be wrong about — see
// providerlock.go.

import "github.com/spf13/cobra"

// Cmd is `llz ci provider-lock-guard`.
func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "provider-lock-guard",
		Short: "fail when a shipped provider lockfile cannot satisfy the constraint declared beside it",
		Long: "Compares the provider pins in tools/internal/shared/tfroots/roots/*/\n" +
			".terraform.lock.hcl against the required_providers constraints declared by the\n" +
			"root beside them AND by every terraform-module that root composes.\n\n" +
			"WHAT IT PROTECTS. A constraint bump has two halves — edit versions.tf, then\n" +
			"regenerate the lock next to it — and nothing else forces the second. Both files\n" +
			"are embedded in this binary and written into every instance's roots together by\n" +
			"`llz render`, so shipping them in disagreement produces a rendered root whose own\n" +
			"`tofu init` refuses it, on every instance, at the first step of every terraform op.\n\n" +
			"IT ALSO CHECKS AGREEMENT FIRST, and that check short-circuits: when the roots and\n" +
			"the modules constrain one provider differently, the intersection they form may be\n" +
			"empty — and then NO lock can satisfy it, so blaming the lock points the fix at the\n" +
			"wrong file. Dependabot produces exactly that shape: it scans terraform-modules/*\n" +
			"and not the generated roots, so it moves one half of the constraint on its own.\n\n" +
			"FATAL: a locked version that violates a declared constraint.\n" +
			"REPORTED ONLY: a constraint with no pin (tofu records it on first init) and a\n" +
			"pin nothing constrains (dead weight tofu ignores). Neither breaks anyone.\n\n" +
			"Offline; reads this repo only. Roots that ship no lockfile (vpc, databases) are\n" +
			"skipped — their providers resolve fresh on every init.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Run(root, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	return c
}
