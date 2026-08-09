package gameday

// cobra_gameday.go — the `llz ci wedge-gameday` flag set.
//
// The drill is tools/internal/gameday, which declares the extension.
//
// NO Deps STRUCT — the fourth extraction to need none, and the cheapest reason yet:
// it already took internal/cigate.Deps as a parameter before the move. That package
// came out with `converge` and now has thirteen callers; a capability seam that is
// already shared is one the extraction does not have to invent.

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
)

func WedgeGamedayCmd() *cobra.Command {
	var (
		externalSecret string
		targetApp      string
		namespace      string
		timeout        int
		interval       int
	)
	cmd := &cobra.Command{
		Use:   "wedge-gameday",
		Short: "prove blast-radius containment: break one ExternalSecret, assert only its own carved App degrades",
		Long: "Fault-injection game-day for the blast-radius decomposition. Repoints one\n" +
			"platform ExternalSecret's secretStoreRef at a non-existent store (forcing it\n" +
			"not-Ready), then asserts the carved Application that owns it goes non-Healthy\n" +
			"while platform-bootstrap and every sibling carved App stay Healthy — the\n" +
			"concrete proof the #163 wedge is contained. Always restores the ExternalSecret.\n" +
			"Uses kubectl with the ambient KUBECONFIG. Run on a warm cluster (the\n" +
			"converge-only fast-path reuses one).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return Run(cigate.NewDeps().GrantedBy(Extension().Bindings[0]), Opts{
				ESRef:     externalSecret,
				TargetApp: targetApp,
				Namespace: namespace,
				Timeout:   time.Duration(timeout) * time.Second,
				Interval:  time.Duration(interval) * time.Second,
			})
		},
	}
	cmd.Flags().StringVar(&externalSecret, "externalsecret", "monitoring/loki-object-store", "the ExternalSecret to break, as <namespace>/<name>")
	cmd.Flags().StringVar(&targetApp, "target-app", "llz-observability", "the carved Application expected to go non-Healthy (owns --externalsecret)")
	cmd.Flags().StringVar(&namespace, "namespace", "argocd", "namespace of the Argo Application resources")
	cmd.Flags().IntVar(&timeout, "timeout", 300, "seconds to watch for the fault to surface")
	cmd.Flags().IntVar(&interval, "interval", 5, "seconds between health snapshots")
	return cmd
}
