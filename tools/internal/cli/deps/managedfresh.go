package deps

// managedfresh.go — the `llz ci managed-fresh` flag set.
//
// IT LIVES BESIDE ITS ASSEMBLER, WHICH IS WHAT MADE IT DRIVEABLE. This file used
// to open "STAYS IN PACKAGE MAIN: it is handed sustainDeps(), one of the fifteen
// deps assemblers ... a command that needs main to assemble its capability's Deps
// cannot live on the other side of that assembly." That was true, and it was the
// entire reason `template-sustain` sat in registry/gates.go's undrivenGates while
// its gate binding was perfectly ordinary. The assembler moved out of main; the
// command followed; the registry drives it now.
//
// The guard is tools/internal/extensions/assertions/sustain, which already owned
// template-sustain. The manifest MACHINERY is not here: ADR 0014 pins
// .template-manifest as the single ownership authority in shared/manifest, and the
// guard reaches it through one narrow Deps field rather than by taking the model
// across.

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/sustain"
)

// ManagedFreshCmd is `llz ci managed-fresh`, and the registry's gate driver runs it.
func ManagedFreshCmd() *cobra.Command {
	var write bool
	var root string
	c := &cobra.Command{
		Use:   "managed-fresh",
		Short: "fail when a template-owned scaffold file drifts from the template",
		Long: "Verifies every token-free file in a digest-locked class of .template-manifest\n" +
			"(today: `managed` — the vendored llz-*.yml bodies, composite actions and the\n" +
			"template-owned configs) still matches the digest the template shipped in\n" +
			sustain.ManagedLockPath + ". These files are template-owned: `llz upgrade` overwrites them\n" +
			"from a clean render, so a local edit is silently lost on the next bump. Failing\n" +
			"here turns that silent loss into a CI error.\n\n" +
			"Runs offline — no copier, no template checkout, no network.\n\n" +
			"--write regenerates the lock; it is for the TEMPLATE repo (CI asserts the lock\n" +
			"is current), not for instances.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return sustain.RunManagedFresh(Sustain(), root, write, os.Stdout, os.Stderr)
		},
	}
	c.Flags().BoolVar(&write, "write", false, "regenerate the lock from the scaffold (template repo only)")
	c.Flags().StringVar(&root, "root", "", "scaffold root containing .template-manifest (default: auto-detect instance-template/ or .)")
	return c
}
