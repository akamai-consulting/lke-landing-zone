package versionpins

// cobra_pins.go — the CLI surface for pins.
//
// Split from pins.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"os"

	"github.com/spf13/cobra"
)

func Cmd() *cobra.Command {
	var root string
	var verbose bool
	c := &cobra.Command{
		Use:   "version-pins",
		Short: "fail when a tool-version pin disagrees with the Dockerfile ARG block",
		Long: "dockerfiles/Dockerfile's ARG block is the single source of truth for tool\n" +
			"versions, but the same numbers are restated in the build matrix, in workflow\n" +
			"env blocks, in the Makefile, and in Go constants that derive\n" +
			"TF_IMAGE/KUBE_IMAGE. This asserts every restatement agrees with the ARG.\n\n" +
			"It has drifted before: the Go constants sat on Terraform 1.9.8 after the\n" +
			"Dockerfile and build matrix moved to OpenTofu 1.12.5, which would have\n" +
			"scaffolded new instances onto the wrong image.\n\n" +
			"One class is gated the OTHER way: a job's container image must name :latest,\n" +
			"never a version. Pinning one points that job at a tag build-images.yml has not\n" +
			"published yet, so every version bump cost one 'manifest unknown' red and a\n" +
			"manual re-run. Container images are found by YAML position, not by matching the\n" +
			"`vars.<X>_IMAGE ||` fallback expression, so the rule holds however they are\n" +
			"spelled; in instance-template/ it inverts again and hardcoding one is refused.\n\n" +
			"The closed classes also have to match something: a declared fallback that is\n" +
			"gone, a tag constant renamed out of the scanned tree, or a build-matrix row the\n" +
			"pattern can no longer see is an error rather than a class that quietly checks\n" +
			"nothing. (The open-ended classes — a version-tagged image reference, a bare ARG\n" +
			"restatement — are catch-alls for sites nobody declared, so they may match none.)\n\n" +
			"Runs offline — no registry, no network.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return Run(root, verbose, os.Stdout, os.Stderr)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root to scan")
	c.Flags().BoolVar(&verbose, "verbose", false, "list every restatement, not just the drifted ones")
	return c
}
