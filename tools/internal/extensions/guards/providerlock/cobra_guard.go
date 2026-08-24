package providerlock

// cobra_guard.go — the CLI surface for Run.
//
// Split from providerlock.go so the directory shows its commands at a glance:
// every file named cobra_*.go here is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

// Cmd is `llz ci provider-lock-guard`.
func Cmd() *cobra.Command {
	var root string
	var instance bool
	c := &cobra.Command{
		Use:   "provider-lock-guard",
		Short: "fail when a delivered provider lockfile cannot satisfy a constraint the template ships",
		Long: "Compares the provider pins in instance-template/terraform-iac-bootstrap/*/\n" +
			".terraform.lock.hcl against the required_providers constraints declared by the\n" +
			"embedded TF root AND by every terraform-module that root composes.\n\n" +
			"THE ASYMMETRY IT GUARDS. The constraint ships in the ci image — the roots are\n" +
			"generated at every terraform op by the llz inside vars.TF_IMAGE, and the *.tf\n" +
			"are gitignored. The pin ships in the adopter's repo, where .template-manifest\n" +
			"classes the lockfile `owned`: seeded once and never re-touched by an upgrade.\n" +
			"Raise a constraint past the shipped pin and a NEW adopter is fine while EVERY\n" +
			"EXISTING one is hard-blocked at `tofu init`, which the terraform-init composite\n" +
			"runs without -upgrade. No e2e lane can see it: release-e2e force-pushes a fresh\n" +
			"instantiation every run, so the greenfield path is the only one it exercises.\n\n" +
			"FATAL: a locked version that violates a declared constraint.\n" +
			"REPORTED ONLY: a constraint with no pin (tofu records it on first init) and a\n" +
			"pin nothing constrains (dead weight tofu ignores). Neither breaks an adopter.\n\n" +
			"Offline; reads this repo only. Roots that ship no lockfile (vpc, databases) are\n" +
			"skipped — their providers resolve fresh on every init.\n\n" +
			"--instance asks the same question INSIDE AN ADOPTER'S REPO, which is where the\n" +
			"asymmetry above actually bites. It reads the locks from terraform-iac-bootstrap/\n" +
			"at the repo root and the constraints from the roots COMPILED INTO THIS BINARY —\n" +
			"an instance has no tools/ tree to read them from, and the llz running the check\n" +
			"is the one whose `llz render` writes the roots. Run it on the upgrade PR, where\n" +
			"a newly-raised constraint and an untouched `owned` lockfile first disagree; with\n" +
			"no lock committed at all it passes, because nothing can be stale.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if instance {
				return RunInstance(root, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			return Run(root, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	c.Flags().BoolVar(&instance, "instance", false,
		"scan an ADOPTER'S instance repo: locks from terraform-iac-bootstrap/, constraints from the roots embedded in this binary")
	return c
}
