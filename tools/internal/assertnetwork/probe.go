package assertnetwork

// ci_net_probe.go implements `llz ci net-probe` — a TCP dial, and nothing else.
//
// It exists so assert-network-enforcement can run its probes from a pod using
// THE LLZ IMAGE, which is already in the cluster, already signed, and already
// gated by the image-signature policy. The alternative — pulling busybox or
// netshoot to get a shell with `nc` — needs egress to a public registry from a
// namespace whose whole purpose is to have egress denied, which is a circular
// dependency, and it would drag an unsigned third-party image through an
// admission policy that exists to keep those out.
//
// Distinguishing a REFUSED connection from a TIMED-OUT one matters and is why
// this is not `curl`. A NetworkPolicy drop and an Istio mTLS rejection do not
// look the same on the wire: a policy drop usually blackholes (timeout), while a
// sidecar refusing plaintext resets the connection (refused). Both are "blocked"
// for the gate's purposes, but reporting which one happened is the difference
// between an operator checking Cilium and checking PeerAuthentication.
//
// Exit codes are the interface, because the caller reads them from a pod's
// container status:
//
//	0  connected
//	1  blocked (refused, reset, or timed out) — the reason is on stdout
//	2  the probe could not run at all (bad address)

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// netProbeResult is the outcome of one dial.
type netProbeResult struct {
	Target    string
	Connected bool
	Reason    string // "connected" | "refused" | "timeout" | "dns" | "error: …"
}

// classifyDialError turns a dial error into the reason vocabulary above. Pure.
//
// The distinction is diagnostic, never a verdict: the gate treats every non-nil
// error as blocked. Collapsing them into "failed" would be correct and useless —
// "timeout" points at a NetworkPolicy drop, "refused" at a sidecar reset or a
// closed port, "dns" at neither.
func classifyDialError(err error) string {
	if err == nil {
		return "connected"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return "refused"
	case strings.Contains(msg, "connection reset"):
		return "refused"
	case strings.Contains(msg, "i/o timeout"):
		return "timeout"
	}
	return "error: " + msg
}

// dialTCP is the probe itself. Seamed for tests.
var dialTCP = func(target string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", target, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// probeTCP dials once and classifies the outcome.
func probeTCP(target string, timeout time.Duration) netProbeResult {
	err := dialTCP(target, timeout)
	return netProbeResult{Target: target, Connected: err == nil, Reason: classifyDialError(err)}
}

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
