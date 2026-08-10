package coverageguard

// cobra_guard.go — the CLI surface for guard.
//
// Split from guard.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"os"

	"github.com/spf13/cobra"
)

func Cmd() *cobra.Command {
	var profile, makefile string
	var bank bool
	c := &cobra.Command{
		Use:   "check-coverage <pkg-suffix=min>...",
		Short: "enforce per-package minimum statement coverage from a Go coverprofile",
		Long: "Native port of check-go-coverage.sh (the per-package\n" +
			"floor enforced by `make coverage`). Each <pkg-suffix>=<min> argument matches\n" +
			"the END of a package import path (cmd/llz -> .../tools/cmd/llz) and a minimum\n" +
			"statement-coverage percentage. Fails if any gated package is below its floor\n" +
			"or produced no coverage data. Packages without a threshold are not gated.\n\n" +
			"--bank RAISES each floor to what its package now measures, and writes them\n" +
			"back to COVERAGE_MINS. It never lowers one, and it refuses to run while\n" +
			"anything is below its floor: banking a red run would raise the floors that\n" +
			"passed and leave the failure in place. Run it after adding tests, and commit\n" +
			"the result with them.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if bank {
				return RunBank(profile, makefile, args, os.Stdout)
			}
			return Run(profile, args, os.Stdout)
		},
	}
	c.Flags().StringVar(&profile, "profile", "", "path to the Go coverprofile (required)")
	c.Flags().BoolVar(&bank, "bank", false, "raise each floor to what its package measures (never lowers; refuses while red)")
	c.Flags().StringVar(&makefile, "makefile", "Makefile", "the Makefile holding COVERAGE_MINS, rewritten by --bank")
	return c
}
