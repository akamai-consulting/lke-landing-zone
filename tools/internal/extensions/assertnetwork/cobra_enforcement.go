package assertnetwork

// cobra_enforcement.go — the CLI surface for enforcement.
//
// Split from enforcement.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/spf13/cobra"
)

func NetworkEnforcementCmd() *cobra.Command {
	var checks, image, namespace, allowed, denied, mtlsTarget string
	var timeout int
	var keep bool
	c := &cobra.Command{
		Use:   "assert-network-enforcement",
		Short: "fail unless NetworkPolicy and mTLS are actually enforced in the data plane",
		Long: "Opens real connections from a real pod, because these are the two enforcement\n" +
			"properties that cannot be server-dry-run: admission answers from the API server,\n" +
			"but a dropped packet is only knowable by sending one.\n\n" +
			"EVERY negative carries a POSITIVE CONTROL. \"The connection failed\" is equally\n" +
			"consistent with the policy working, the image not pulling, DNS being down, or\n" +
			"the pod never being scheduled — so each check dials an ALLOWED address that\n" +
			"must connect and a DENIED one that must not, from the same pod in the same run.\n" +
			"If the allowed dial fails the run is INCONCLUSIVE and fails as such, rather\n" +
			"than being reported as enforcement it never observed.\n\n" +
			"  netpol — a scratch namespace with default-deny egress (DNS + apiserver only).\n" +
			"           Proves the CNI enforces NetworkPolicy at all; a cluster where it\n" +
			"           silently does not is one where every default-deny here is decorative.\n" +
			"  mtls   — the same pod, deliberately unmeshed, dialing a STRICT-mesh port in\n" +
			"           plaintext. Istio must refuse it. SKIPS unless the target namespace\n" +
			"           actually declares STRICT: a PERMISSIVE mesh accepts plaintext by\n" +
			"           design, and harbor ships PERMISSIVE on purpose (ADR 0010 step 3).\n\n" +
			"MUTATING: creates and deletes a scratch namespace, a NetworkPolicy and a Pod.\n" +
			"Cleanup runs on failure too. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertNetworkEnforcement(netEnforceOpts{
				checks:     cigate.SplitCSVList(checks),
				image:      image,
				namespace:  namespace,
				allowed:    allowed,
				denied:     denied,
				mtlsTarget: mtlsTarget,
				timeout:    time.Duration(timeout) * time.Second,
				keep:       keep,
			})
		},
	}
	c.Flags().StringVar(&checks, "checks", "netpol,mtls", "comma-separated checks to run (netpol, mtls)")
	c.Flags().StringVar(&image, "image", "", "llz image for the probe pod (default: the image this binary's reconciler Deployment runs)")
	c.Flags().StringVar(&namespace, "namespace", netEnforceNamespace, "scratch namespace to create and delete")
	c.Flags().StringVar(&allowed, "allowed-target", netEnforceAllowedTarget, "host:port the probe MUST reach (the positive control)")
	c.Flags().StringVar(&denied, "denied-target", netEnforceDeniedTarget, "host:port the NetworkPolicy must block")
	c.Flags().StringVar(&mtlsTarget, "mtls-target", netEnforceMTLSTarget, "host:port on a STRICT-mesh workload that must refuse plaintext")
	c.Flags().IntVar(&timeout, "timeout", 8, "seconds each dial waits")
	c.Flags().BoolVar(&keep, "keep", false, "keep the scratch namespace for debugging (default: always delete)")
	return c
}
