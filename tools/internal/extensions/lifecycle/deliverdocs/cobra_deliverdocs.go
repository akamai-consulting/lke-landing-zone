package deliverdocs

// cobra_deliverdocs.go — the `llz ci deliver-docs` flag set.
//
// The verb itself is tools/internal/extensions/lifecycle/deliverdocs, which declares the extension.
// What stays here is what package main owns: the cobra wiring and the help text.
// Nothing else — the extension needs no injected capabilities, because everything
// it touches is the instance repo's own files, which is exactly what its grant
// line says.

import (
	"github.com/spf13/cobra"
)

func DeliverDocsCmd() *cobra.Command {
	var dir, org, ref, root, templateRoot string
	c := &cobra.Command{
		Use:   "deliver-docs",
		Short: "prune a copied-in docs/ to the operator set + write a version-pinned pointer to the rest",
		Long: "Slims an instance's docs/ to quickstart.md + runbooks/ + playbooks/ and writes\n" +
			"docs/README.md pointing at the full docs for the pinned template version in the\n" +
			"(public) template repo. Run after copying the template's docs/ in; the same verb\n" +
			"backs both the copier render step and release-e2e, so the keep-set can't drift.\n" +
			"With --template-root, ALSO repoints links in the instance's root-level Markdown\n" +
			"(README.md, AGENTS.md, …) that target template-only paths, which the docs/-scoped\n" +
			"rewrite never sees.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return Run(dir, org, ref, root, templateRoot)
		},
	}
	c.Flags().StringVar(&dir, "docs", "docs", "the already-copied docs directory to prune in place")
	c.Flags().StringVar(&org, "org", "", "template org for the reference URL (e.g. akamai-consulting)")
	c.Flags().StringVar(&ref, "ref", "", "template ref/tag the instance is pinned to (for the version-matched URL)")
	c.Flags().StringVar(&root, "root", ".", "the instance root, whose top-level Markdown is repointed (needs --template-root)")
	c.Flags().StringVar(&templateRoot, "template-root", "", "path to the template checkout; enables the instance-root link repoint (copier passes _copier_conf.src_path)")
	return c
}
