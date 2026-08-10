package assertnetwork

// cobra_probe.go — the CLI surface for probe.
//
// Split from probe.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func NetProbeCmd() *cobra.Command {
	var timeout int
	c := &cobra.Command{
		Use:   "net-probe <host:port>",
		Short: "dial a TCP address and report connected / refused / timeout (exit 0 = connected, 1 = blocked)",
		Long: "Dials one TCP address and exits 0 if it connected, 1 if it was blocked, 2 if\n" +
			"the probe could not run. Reports WHY it was blocked — a NetworkPolicy drop\n" +
			"usually blackholes (timeout) while an Istio sidecar refusing plaintext resets\n" +
			"(refused), which is the difference between checking Cilium and checking\n" +
			"PeerAuthentication.\n\n" +
			"Exists so assert-network-enforcement can probe from a pod running the llz\n" +
			"image, which is already in the cluster and already signature-gated. Pulling a\n" +
			"shell image instead would need registry egress from a namespace whose point is\n" +
			"to have egress denied, and would drag an unsigned image through the very\n" +
			"admission policy that exists to keep those out.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			target := args[0]
			if _, _, err := net.SplitHostPort(target); err != nil {
				fmt.Printf("probe %s: NOT A VALID host:port (%v)\n", target, err)
				os.Exit(2)
			}
			r := probeTCP(target, time.Duration(timeout)*time.Second)
			fmt.Printf("probe %s: %s\n", r.Target, r.Reason)
			if !r.Connected {
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().IntVar(&timeout, "timeout", 5, "seconds to wait for the connection")
	return c
}
