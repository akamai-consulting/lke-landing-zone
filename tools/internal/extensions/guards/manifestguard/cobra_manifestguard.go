package manifestguard

// cobra_manifestguard.go — the `guard-manifests` flag sets.
//
// The guards are tools/internal/extensions/guards/manifestguard, which declares the extension.
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
