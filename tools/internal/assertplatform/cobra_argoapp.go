package assertplatform

// cobra_argoapp.go — the CLI surface for argoapp.
//
// Split from argoapp.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/spf13/cobra"
)

func ArgoAppCmd() *cobra.Command {
	var (
		app       string
		parent    string
		namespace string
		within    int
	)
	cmd := &cobra.Command{
		Use:   "assert-argo-app",
		Short: "fail fast (with the parent sync's operationState) when an Argo Application never appears",
		Long: "Polls for the named Argo CD Application to exist. On a healthy bootstrap the\n" +
			"platform-bootstrap sync creates it within a couple of minutes; when the sync is\n" +
			"wedged the pod wait behind this gate would burn its full budget blind. Exits\n" +
			"non-zero immediately if the parent app's operation is terminally Failed, or at\n" +
			"the deadline — either way printing the parent operationState message (the root\n" +
			"cause). Uses kubectl with the ambient KUBECONFIG.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return assertArgoApp(cigate.NewDeps(), namespace, app, parent, time.Duration(within)*time.Second)
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "Application that must appear (required)")
	cmd.Flags().StringVar(&parent, "parent", "platform-bootstrap", "app-of-apps whose operationState explains a missing --app")
	cmd.Flags().StringVar(&namespace, "namespace", "argocd", "namespace of the Application resources")
	cmd.Flags().IntVar(&within, "within", 240, "seconds to wait for --app to exist")
	_ = cmd.MarkFlagRequired("app")
	return cmd
}
