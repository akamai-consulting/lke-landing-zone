package mutabletags

// cobra_guard.go — the CLI surface for Run.
//
// Split from guard.go so the directory shows its commands at a glance: every file
// named cobra_*.go here is flag wiring and help text, and nothing else.

import (
	"github.com/spf13/cobra"
)

// Cmd is `llz ci mutable-tag-guard`.
func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "mutable-tag-guard",
		Short: "fail when build-images.yml can publish :latest or :<version> from a non-default ref",
		Long: "Holds the publish policy of .github/workflows/build-images.yml: a MUTABLE tag\n" +
			"(`:latest`, `:<version>`) may be pushed only when PUBLISH_MUTABLE is set from\n" +
			"github.ref, while `:sha-<commit>` is pushed from every ref.\n\n" +
			"That workflow's dispatch is deliberately not gated on the ref — release-e2e and\n" +
			"e2e-instantiate drive it on feature branches — so the gate lives on the publish.\n" +
			"Without it a branch build repoints the tag lint.yml's container fallback resolves,\n" +
			"the one `llz ci assert-image-fresh` reads a baked sha out of, and whatever an\n" +
			"instance that never pinned TF_IMAGE runs. Every `--tag` is individually\n" +
			"well-formed, and the moved tag looks the same afterwards, so only the relation\n" +
			"between the publish and the ref is checkable.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Run(root, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	return c
}
