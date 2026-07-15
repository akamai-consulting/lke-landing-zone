package main

// ci_prune_remote.go implements `llz ci prune-remote-apl-values` — a copier _task that
// removes the apl-values trees a scaffolded instance fetches from the template repo at
// render time instead of vendoring: the token-free platform components (all but the ones
// pinned local, e.g. clusterHealthWorkflow) and the whole _shared/manifest base. `llz
// render` emits each env's overlay with pinned remote refs (github.com/.../…?ref=<ver>)
// into those trees, so an instance carries only the thin per-env overlays + the local
// bits (values.yaml, _shared/custom, the pinned-local components). This is the footprint
// half of the remote-refs refactor — the components live ONCE upstream. Mirrors the
// deliver-docs prune: copy-everything-then-slim, driven by the clusterspec registry so
// the list can't drift. Idempotent (missing dirs are a no-op); never touches _shared/
// values.yaml, _shared/custom, or the pinned-local components.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/spf13/cobra"
)

func ciPruneRemoteAplValuesCmd() *cobra.Command {
	var aplDir string
	c := &cobra.Command{
		Use:   "prune-remote-apl-values",
		Short: "remove the apl-values trees fetched remotely from the template repo (components + _shared/manifest)",
		Long: "Deletes the platform-component dirs and the _shared/manifest base that a\n" +
			"scaffolded instance references remotely (the overlays `llz render` emits point\n" +
			"at github.com/<org>/lke-landing-zone//…?ref=<version>), so an instance vendors\n" +
			"only its thin per-env overlays + local bits. Registry-driven + idempotent; run\n" +
			"as a copier _task after render. Pinned-local components (clusterHealthWorkflow)\n" +
			"and _shared/values.yaml / _shared/custom are never touched.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runPruneRemoteAplValues(aplDir, os.Stdout)
		},
	}
	c.Flags().StringVar(&aplDir, "apl-values", "apl-values", "path to the instance's apl-values directory")
	return c
}

func runPruneRemoteAplValues(aplDir string, out io.Writer) error {
	// The shared manifest base + every remotely-fetched component dir. _shared/manifest
	// is pruned wholesale (the overlay fetches it remotely; the local instance-custom
	// App + per-env pieces live in the overlays, not here).
	targets := []string{filepath.Join(aplDir, "_shared", "manifest")}
	for _, name := range clusterspec.RemotelyFetchedComponents() {
		targets = append(targets, filepath.Join(aplDir, "components", name))
	}

	pruned := 0
	for _, t := range targets {
		if _, err := os.Stat(t); os.IsNotExist(err) {
			continue // already absent (idempotent / non-copier layout)
		}
		if err := os.RemoveAll(t); err != nil {
			return fmt.Errorf("prune %s: %w", t, err)
		}
		fmt.Fprintf(out, "  pruned (fetched remotely): %s\n", t)
		pruned++
	}
	// If the components dir is now empty of remote dirs but still holds the pinned-local
	// ones, that's expected — leave it. Report the net effect.
	if pruned == 0 {
		fmt.Fprintln(out, "  nothing to prune (already slim, or not a scaffolded instance)")
		return nil
	}
	fmt.Fprintf(out, "%d remotely-fetched apl-values tree(s) pruned — they are referenced from the template repo at the pinned version.\n", pruned)
	return nil
}
