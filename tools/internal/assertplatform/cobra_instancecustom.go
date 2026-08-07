package assertplatform

// cobra_instancecustom.go — the CLI surface for instancecustom.
//
// Split from instancecustom.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/spf13/cobra"
)

func InstanceCustomCmd() *cobra.Command {
	var (
		namespace string
		appSet    string
		within    int
	)
	cmd := &cobra.Command{
		Use:   "assert-instance-custom",
		Short: "fail unless the escape-hatch App instance-custom-<ns> exists and is Synced+Healthy",
		Long: "Proves the operator escape hatch (the instance-custom ApplicationSet syncing\n" +
			"kubernetes-custom/) works end to end. The release-e2e instantiate step seeds a\n" +
			"trivial manifest under kubernetes-custom/namespaces/<ns>/; this polls for the\n" +
			"generated Application instance-custom-<ns> to EXIST (the git directory generator\n" +
			"discovered the dir), then for it to reach Synced + Healthy (namespace created,\n" +
			"manifest applied). A generation fault or a sync failure fails the gate WITH the\n" +
			"ApplicationSet / Application diagnostics that explain it. Uses kubectl with the\n" +
			"ambient KUBECONFIG. Read-only.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return assertInstanceCustom(cigate.NewDeps(), namespace, appSet, time.Duration(within)*time.Second)
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", "llz-e2e-custom",
		"the kubernetes-custom/namespaces/<ns> basename the release-e2e seed uses; the asserted App is instance-custom-<ns>")
	cmd.Flags().StringVar(&appSet, "appset", "instance-custom",
		"the ApplicationSet whose status explains a generated App that never appeared")
	cmd.Flags().IntVar(&within, "within", 300,
		"seconds to wait for the App to appear AND reach Synced+Healthy — the sole wait for the escape hatch: converge reports instance-custom apps but no longer gates on them (CatInstance is non-fatal), so this deadline must cover full generate+sync, not just margin")
	return cmd
}
