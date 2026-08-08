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
			"container fallbacks and env blocks, and in Go constants that derive\n" +
			"TF_IMAGE/KUBE_IMAGE. This asserts every restatement agrees with the ARG.\n\n" +
			"It has drifted before: the Go constants sat on Terraform 1.9.8 after the\n" +
			"Dockerfile and build matrix moved to OpenTofu 1.12.5, which would have\n" +
			"scaffolded new instances onto the wrong image.\n\n" +
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
