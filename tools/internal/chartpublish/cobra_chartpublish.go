package chartpublish

// cobra_chartpublish.go — the `llz ci chart-publish-check` flag set.
//
// The check is tools/internal/chartpublish, which declares the extension. The
// wiring travels with the capability rather than sitting in main.
//
// Opts is EXPORTED and threaded as a parameter rather than installed: the call
// tree is two frames and every seam already sat at the top of it as a package
// var, which is the shape internal/promote documented. Installation stays the
// exception it was for internal/converge.

import (
	"github.com/spf13/cobra"
)

func ChartPublishCheckCmd() *cobra.Command {
	var root, ref, templateRepo string
	var publishIfMissing bool
	var interval, timeout int
	c := &cobra.Command{
		Use:   "chart-publish-check",
		Short: "verify (or publish + wait for) the pinned first-party (llz-*) chart versions in GHCR",
		Long: "Scans the apl-values Argo Application manifests for first-party (llz-*) chart\n" +
			"pins (repoURL + chart + targetRevision/version) and fails if any pinned version\n" +
			"is not present in its OCI registry. A pin the registry never received 404s at\n" +
			"Argo sync time — on a cold bootstrap that silently strands the support-plane app\n" +
			"and times out the OpenBao bootstrap on `namespaces \"llz-openbao\" not found`.\n" +
			"As a preflight, an unpublished chart fails fast, not 15 minutes in. With\n" +
			"--publish-if-missing it instead dispatches publish-charts.yml on --ref and waits\n" +
			"for the pins to land (the chart analog of `pin-instance-images --build-if-missing`).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return Run(Defaults(Opts{
				Root: root, PublishIfMissing: publishIfMissing,
				Ref: ref, TemplateRepo: templateRepo,
			}, interval, timeout))
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root to scan for apl-values chart pins")
	c.Flags().BoolVar(&publishIfMissing, "publish-if-missing", false, "if a pinned chart is unpublished, dispatch publish-charts.yml on --ref and wait — instead of failing")
	c.Flags().StringVar(&ref, "ref", "", "branch/tag to dispatch publish-charts.yml on (required with --publish-if-missing)")
	c.Flags().StringVar(&templateRepo, "template-repo", "", "owner/name of the repo hosting publish-charts.yml (required with --publish-if-missing)")
	c.Flags().IntVar(&interval, "interval", 20, "seconds between registry re-checks while waiting for a publish")
	c.Flags().IntVar(&timeout, "timeout", 600, "max seconds to wait for the dispatched charts to publish")
	return c
}
