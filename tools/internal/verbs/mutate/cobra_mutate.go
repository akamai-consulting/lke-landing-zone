package mutate

// cobra_mutate.go — the `llz ci mutate` flag set.
//
// The run is tools/internal/verbs/mutate, which declares the extension. Ninety-seven
// lines of RunE moved with it: the closure held the control run, the gremlins
// invocation, harness validation and the survivor diff, which is the whole verb.
// A flag set that keeps its verb in a callback has not been extracted, it has been
// relabelled.

import (
	"github.com/spf13/cobra"
)

func MutateCmd() *cobra.Command {
	var pkg, baselinePath, outPath string
	var coefficient int
	var integration bool
	c := &cobra.Command{
		Use:   "mutate",
		Short: "mutation-test a package, validating the harness before reporting a score",
		Long: "Runs gremlins over a package and refuses to report a score until three checks\n" +
			"pass: the suite is color.Green on unmutated source (CONTROL), a provably-equivalent\n" +
			"mutant comes back LIVED (CANARY), and no mutant timed out. Every gremlins\n" +
			"failure mode found so far surfaced as a flattering 100%, never as an error, so\n" +
			"the score is the one signal that cannot detect a broken run.\n\n" +
			"Survivors are diffed against --baseline; the exit code reflects NEW survivors,\n" +
			"not the absolute score (equivalent mutants cap that below 100% by construction).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Run(Opts{
				Pkg: pkg, BaselinePath: baselinePath, OutPath: outPath,
				Coefficient: coefficient, Integration: integration,
				Out: cmd.OutOrStdout(),
			})
		},
	}
	c.Flags().StringVar(&pkg, "package", "", "package to mutate, e.g. ./internal/linode")
	c.Flags().StringVar(&baselinePath, "baseline", "", "JSON baseline of accepted survivors")
	c.Flags().StringVar(&outPath, "out", "", "write the machine-readable run to this path")
	c.Flags().IntVar(&coefficient, "timeout-coefficient", 100, "gremlins timeout multiplier; low values manufacture timeouts")
	c.Flags().BoolVar(&integration, "integration", false, "run the whole module suite per mutant (REQUIRED for package main)")
	return c
}
