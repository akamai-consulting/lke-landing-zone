package main

// ci_plaintext.go — the `llz ci plaintext-guard` flag set.
//
// The guard is tools/internal/plaintext, which declares the extension. What stays
// here is the cobra wiring and the help text.
//
// ZERO injected capabilities, because the guard reads files and nothing else —
// which is the same thing its `gate` binding claims, and is checked rather than
// asserted (TestPackageStaysFilesOnly).

import (
	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/plaintext"
)

func ciPlaintextGuardCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "plaintext-guard",
		Short: "fail when an unencrypted in-cluster hop is not registered as an accepted residual",
		Long: "Static gate on cleartext in-cluster communication (docs/adr/0010-in-cluster-mtls.md).\n" +
			"Scans platform-apl/ and kubernetes-charts/ for `scheme: http` scrapes,\n" +
			"`insecureSkipVerify: true`, http:// URLs to in-cluster Services (fully\n" +
			"qualified OR the short svc.namespace / svc forms), and Istio mesh policy that\n" +
			"accepts cleartext (PeerAuthentication mode: PERMISSIVE, DestinationRule\n" +
			"tls.mode: DISABLE), plus tools/ for InsecureSkipVerify. Every hit must be\n" +
			"registered in plaintextAllowed with a reason and an owner; unregistered hits\n" +
			"fail, and so do registry entries whose hop no longer exists.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return plaintext.Run(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return cmd
}
