package manifestguard

// cobra_manifestguard.go — the three `guard-manifests` flag sets.
//
// The guards are tools/internal/manifestguard, which declares the extension.
// The wiring travels with the capability rather than sitting in main.

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func ArgoCDRenderedAppsCmd() *cobra.Command {
	var root, renderDir string
	c := &cobra.Command{
		Use:   "argocd-rendered-apps",
		Short: "reject rendered ArgoCD Applications with duplicate Helm parameters",
		Long: "Native port of validate-argocd-rendered-apps.py (the Makefile's\n" +
			"argocd-rendered-apps-check, run after render-charts). Parses every rendered\n" +
			"manifest under the render dir and fails if any ArgoCD Application names the\n" +
			"same Helm parameter twice — a duplicate silently shadows the earlier value at\n" +
			"sync time, which schema validation does not catch.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if renderDir == "" {
				renderDir = "rendered"
			}
			return RunArgoCDRenderedApps(filepath.Join(root, renderDir), os.Stdout)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root")
	c.Flags().StringVar(&renderDir, "render-dir", "rendered", "rendered-manifests directory (relative to --root)")
	return c
}

func AplSchemaValidateCmd() *cobra.Command {
	var valuesPath, chartVersion string
	var skipSchema bool
	cmd := &cobra.Command{
		Use:   "validate-apl-values",
		Short: "check a rendered apl-values file's runtime-placeholder contract + apl-core schema (no cluster)",
		Long: "Two offline checks on a rendered apl-values values.yaml, shifted left from\n" +
			"Release-E2E: (1) every unescaped ${...} still in the file is one of the\n" +
			"secrets-only runtime placeholders `llz ci bootstrap-cluster` fills (else the\n" +
			"bootstrap can't fill it — the ${apl_values_repo_url} class); (2) the values\n" +
			"pass apl-core's chart schema via `helm template apl/apl`, pinned to\n" +
			"--chart-version — or, when that is omitted, to the baseline an unpinned env\n" +
			"actually deploys. The schema check self-skips only on --skip-schema or no helm\n" +
			"on PATH; the var-contract check always runs.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunValidateAplValues(valuesPath, chartVersion, skipSchema)
		},
	}
	cmd.Flags().StringVar(&valuesPath, "values", "", "path to the rendered apl-values values.yaml (required)")
	cmd.Flags().StringVar(&chartVersion, "chart-version", "", "apl-core chart version to pin the schema check (from spec.cluster.bootstrap.aplChartVersion)")
	cmd.Flags().BoolVar(&skipSchema, "skip-schema", false, "skip the helm schema check (var-contract only)")
	_ = cmd.MarkFlagRequired("values")
	return cmd
}

func PlaceholderGuardCmd() *cobra.Command {
	var root, renderDir string
	cmd := &cobra.Command{
		Use:   "placeholder-guard",
		Short: "fail when a rendered manifest still carries placeholder.example.com",
		Long: "Rejects unsubstituted `placeholder.example.com` hostnames in the rendered\n" +
			"manifests. Anything Argo CD reconciles into a cluster must carry real addresses,\n" +
			"never the template's example placeholders. Fails closed on an empty corpus: a\n" +
			"guard that scanned nothing must not report the same color.Green as one that scanned\n" +
			"everything and found none.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// --render-dir is documented as relative to --root, but an ABSOLUTE
			// path must survive: filepath.Join(".", "/tmp/x") cleans to "tmp/x",
			// silently retargeting the scan at a relative path that does not exist
			// — which then trips the empty-corpus failure and reads like a broken
			// render rather than a mangled flag.
			dir := renderDir
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(root, dir)
			}
			return RunPlaceholderGuard(dir)
		},
	}
	cmd.Flags().StringVar(&root, "root", ".", "repository root")
	cmd.Flags().StringVar(&renderDir, "render-dir", "rendered", "rendered-manifests directory (relative to --root)")
	return cmd
}
