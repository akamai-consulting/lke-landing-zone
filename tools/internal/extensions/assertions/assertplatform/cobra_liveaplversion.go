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
			"Not that Deployment's chart labels — apl-core relabels them with its operator\n" +
			"chart's packaging version.\n\n" +
			"A tag that is not a release version (apl-core allows a branch name in\n" +
			"otomi.version) is reported as UNKNOWN rather than graded as drift.\n\n" +
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
