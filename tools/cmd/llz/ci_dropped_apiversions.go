package main

// ci_dropped_apiversions.go — `llz ci dropped-apiversions`, the CI face of the
// dropped-apiVersion guard that `llz lint` also runs at pre-commit.
//
// It needs its own CI entry point because NO CI job invokes `llz lint` / `llz
// precommit` — the Go-native lint sequence is a developer pre-commit gate, and a
// developer's installed llz can lag the tree. The other manifest guards
// (wave-health, wave-dependency, mesh-egress, monitoring-label) each expose a
// `llz ci <guard>` subcommand wired into $(LINT_K8S), which lint.yml runs via
// `make lint-k8s`; this follows that convention so the template's own
// platform-apl/ tree is actually gated on every PR.
//
// Unlike the pre-commit step this REFUSES an empty corpus: it runs in the template
// repo, where platform-apl/ and kubernetes-charts/ always hold YAML, so examining
// nothing means the trees moved and the guard has been silently inert.
//
// The scan walks the filesystem rather than `git ls-files`: this job runs inside the
// ci-kubernetes container, where git against the mounted checkout fails, which took
// the whole gate down the first time round. See manifestYAMLFiles.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/guardkit"
)

func ciDroppedAPIVersionsCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "dropped-apiversions",
		Short: "no manifest may declare an apiVersion apl-core's bundled operators no longer serve",
		Long: "Fails if any YAML under the scanned manifest trees declares an\n" +
			"apiVersion a converged apl-core dependency no longer serves (see droppedAPIs\n" +
			"in checks.go). Such a manifest cannot apply — it surfaces only as an opaque\n" +
			"Argo SyncFailed (\"no matches for kind … in version …\") at deploy time.\n\n" +
			"Scans the operator-owned escape hatches (kubernetes-custom/,\n" +
			"kubernetes-charts/), which `llz upgrade` never rewrites, AND the shared\n" +
			"platform-apl/ tree, which no CRD-aware gate covers: k8s-lint, k8s-validate\n" +
			"and the kind server-side dry-run all read $RENDER_DIR, built from\n" +
			"kubernetes-charts/*/ only. Every instance fetches platform-apl/ remotely, so\n" +
			"a stale apiVersion there breaks every instance at once.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIDroppedAPIVersions(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repository root to scan")
	return cmd
}

func runCIDroppedAPIVersions(root string) error {
	hits, examined, err := scanDroppedAPIVersions(root)
	if err != nil {
		return err
	}
	if err := guardkit.RequireCorpus("dropped-apiversions", examined, scannedManifestTrees); err != nil {
		return err
	}
	if err := reportDroppedAPIVersions(hits); err != nil {
		return err
	}
	fmt.Printf("dropped-apiversions: %d manifest(s) scanned, none declare a dropped apiVersion.\n", examined)
	return nil
}
