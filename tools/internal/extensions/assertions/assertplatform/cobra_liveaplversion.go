package assertplatform

// cobra_liveaplversion.go — the CLI surface for liveaplversion.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/spf13/cobra"
)

func AplDeployedVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "assert-apl-deployed-version",
		Short: "compare the apl-core version RUNNING on this cluster against the one this llz release targets",
		Long: "Reads the apl-core version from the image tag of the apl-operator container,\n" +
			"which apl-core sets from otomi.version — the platform version itself — and compares\n" +
			"it against " + clusterspec.BaselineAplChartVersion + ".\n\n" +
			"NOT the chart labels: helm.sh/chart and app.kubernetes.io/version are written by the\n" +
			"apl-operator SUB-chart from its own Chart.yaml (0.2.0 / 1.16.0), so they never equal\n" +
			"the platform version. Reading them reported a healthy v6.2.1 cluster as 0.2.0.\n\n" +
			"This is the only check that observes the DEPLOYED version. `assert-apl-version` reads\n" +
			"the spec, and on Linode's managed App Platform the spec cannot know: apl_enabled is a\n" +
			"create-time boolean and the Linode API exposes no version field, so Linode owns the\n" +
			"rollout entirely.\n\n" +
			"A major apart fails — llz is untested against it. A minor or patch apart warns: that\n" +
			"is the routine state while a rollout is in flight. Being unable to read the version\n" +
			"at all is a FAILURE, not a pass.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return assertAplDeployedVersion() },
	}
}
