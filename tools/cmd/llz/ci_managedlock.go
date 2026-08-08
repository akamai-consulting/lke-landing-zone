package main

// STAYS IN PACKAGE MAIN: it is handed sustainDeps(), one of the fifteen deps
// assemblers that make up main's dependency-injection layer. A command that
// needs main to assemble its capability's Deps cannot live on the other side of
// that assembly.
//
// ci_managedlock.go — the `llz ci managed-fresh` flag set.
//
// The guard is tools/internal/sustain, which already owned template-sustain and
// now owns this too. The manifest MACHINERY stays here: ADR 0014 pins
// .template-manifest as the single ownership authority, and the guard reaches it
// through one narrow Deps field rather than by taking the model across.

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
)

// lockableScaffoldFiles answers sustain's LockableScaffoldFiles: the scaffold root
// plus every file under it whose .template-manifest class is digest-locked.
//
// The class table lives here by ADR 0014 and cannot move; what crosses the
// boundary is the ANSWER, not the model.
func lockableScaffoldFiles(root string) (string, []string, error) {
	m, err := manifest.Load(root)
	if err != nil {
		return "", nil, err
	}
	files, err := manifest.ScaffoldFiles(m.Root)
	if err != nil {
		return "", nil, err
	}
	var out []string
	for _, rel := range files {
		if c, ok := manifest.LookupClass(m.Classify(rel)); ok && c.DigestLocked {
			out = append(out, rel)
		}
	}
	return m.Root, out, nil
}

func ciManagedFreshCmd() *cobra.Command {
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
			return sustain.RunManagedFresh(sustainDeps(), root, write, os.Stdout, os.Stderr)
		},
	}
	c.Flags().BoolVar(&write, "write", false, "regenerate the lock from the scaffold (template repo only)")
	c.Flags().StringVar(&root, "root", "", "scaffold root containing .template-manifest (default: auto-detect instance-template/ or .)")
	return c
}
