package tofudriver

// cobra_tofu.go — the CLI surface for the hand-run passthrough.
//
// Split from tofu.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"golang.org/x/term"
)

func TofuCmd() *cobra.Command {
	var o TofuOpts
	c := &cobra.Command{
		Use:   "tofu [--region <env>] -- <tofu args>",
		Short: "run OpenTofu by hand with this instance's state-encryption and backend environment",
		Long: "Runs OpenTofu with TF_ENCRYPTION, the object-storage backend credentials and\n" +
			"the Linode token resolved from .llz/secrets.env at your instance root — the\n" +
			"same values CI reads from repository secrets. Arguments after `--` are passed\n" +
			"through untouched.\n" +
			"\n" +
			"WHY YOU NEED IT: every Terraform root carries encryption.tf, which holds only\n" +
			"the enforcement posture — the key material arrives from $TF_ENCRYPTION, so an\n" +
			"apply without it FAILS rather than silently writing plaintext state. A hand-run\n" +
			"`tofu` therefore dies on \"Invalid expression … A single static variable\n" +
			"reference is required\", which names neither the passphrase nor the variable.\n" +
			"\n" +
			"Anything already exported in your environment wins and is left alone, so this\n" +
			"is a no-op in CI and never overrides a value you set deliberately.\n" +
			"\n" +
			"Verbs that change infrastructure need --yes, and --dry-run prints the command\n" +
			"without running it. NOTE that the `llz ci tf-*` verbs (tf-apply, tf-destroy,\n" +
			"tf-import) do NOT gate: they are CI plumbing whose confirmation lives in the\n" +
			"calling workflow, so reaching for one by hand bypasses this gate.\n" +
			"\n" +
			"  llz tofu --region primary -- init -upgrade   # backend config filled in too\n" +
			"  llz tofu -- plan -var-file=primary.tfvars\n" +
			"  eval \"$(llz tofu --export)\"                  # same environment, your shell\n" +
			"  eval \"$(llz tofu --shell-init)\"              # rc snippet: bare `tofu` works",
		// Stop flag parsing at the first positional so `llz tofu init -upgrade`
		// hands `-upgrade` to OpenTofu instead of failing on an unknown llz flag.
		// A `--` separator works too and is what the help shows, because it is the
		// form that keeps working when OpenTofu grows a flag llz also has.
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			// Resolved here rather than inside RunTofu so the secret-printing
			// guard is a plain field a test can set either way.
			o.StdoutIsTerminal = term.IsTerminal(int(os.Stdout.Fd()))
			// The global flags are read HERE, the way teardown reads them: RunTofu
			// stays a plain function a test can drive with either value.
			o.DryRun, o.Yes = cliopts.Global.DryRun, cliopts.Global.Yes
			return RunTofu(os.Stdout, os.Stderr, o, args)
		},
	}
	c.Flags().SetInterspersed(false)
	c.Flags().StringVar(&o.Region, "region", "", "deployment whose state init should configure its backend for (derives key <root>/<region>/terraform.tfstate)")
	c.Flags().StringVar(&o.StateKey, "state-key", "", "explicit backend state key, overriding the one --region derives (the shared-VPC root keys by network, not by deployment)")
	c.Flags().BoolVar(&o.Export, "export", false, "print the environment as shell exports instead of running OpenTofu: eval \"$(llz tofu --export)\"")
	c.Flags().BoolVar(&o.ShellInit, "shell-init", false, "print an rc snippet defining a tofu shell function that routes through this command")
	return c
}
