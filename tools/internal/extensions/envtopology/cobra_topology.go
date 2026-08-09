package envtopology

// cobra_topology.go — the CLI surface for topology.
//
// Split from topology.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"fmt"

	topo "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envtopology"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/spf13/cobra"
)

func RoleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "role <topo.Deployment>",
		Short: "print a topo.Deployment's OpenBao HA role (active|standby|standalone)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			tfDir, _, _ := instancelayout.Detect()
			deps, err := topo.ReadTopology(tfDir)
			if err != nil {
				return err
			}
			d, ok := topo.FindDeployment(deps, args[0])
			if !ok {
				return fmt.Errorf("no such topo.Deployment %q (run `llz env list`)", args[0])
			}
			fmt.Println(d.HARole)
			return nil
		},
	}
}
func PeerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "peer <topo.Deployment>",
		Short: "print a topo.Deployment's HA peer (the other member of its ha_group); errors for standalone",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			tfDir, _, _ := instancelayout.Detect()
			deps, err := topo.ReadTopology(tfDir)
			if err != nil {
				return err
			}
			if _, ok := topo.FindDeployment(deps, args[0]); !ok {
				return fmt.Errorf("no such topo.Deployment %q (run `llz env list`)", args[0])
			}
			peer, ok, err := topo.PeerOf(deps, args[0])
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("topo.Deployment %q is standalone — it has no HA peer", args[0])
			}
			fmt.Println(peer)
			return nil
		},
	}
}

// ResolveCmd is the combined role+peer probe the bootstrap-openbao "Resolve
// role + peer" step ran as inline bash (two `llz env role`/`env peer` calls plus
// a defensive case-guard against a stale binary printing help into
// $GITHUB_OUTPUT). It emits role=/peer= to $GITHUB_OUTPUT in one call — peer is
// empty for a standalone topo.Deployment (expected, not an error). The stale-binary
// guard is unnecessary by construction: a binary too old to know this subcommand
// fails cleanly with "unknown command" rather than corrupting the output file.
func ResolveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resolve <topo.Deployment>",
		Short: "emit role= and peer= GitHub Actions outputs for a topo.Deployment (HA role + peer)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			tfDir, _, _ := instancelayout.Detect()
			deps, err := topo.ReadTopology(tfDir)
			if err != nil {
				return err
			}
			return writeHAResolution(deps, args[0])
		},
	}
}
