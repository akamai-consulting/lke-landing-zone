package teardown

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/spf13/cobra"
)

// cobra_root.go — the `llz <verb>` flag set, moved out of package main.
//
// An extension owns its own command; main owns the tree. These are ROOT-level
// verbs rather than `llz ci` ones, which changes nothing about the rule.

func ReapCmd() *cobra.Command {
	var o ReapOpts
	c := &cobra.Command{
		Use:   "reap",
		Short: "sweep orphaned Linode resources from failed cluster cycles (--yes to delete)",
		Long: "Account-wide manual sweep of Linode resources whose backing LKE cluster is\n" +
			"gone — NodeBalancers, VPCs, Volumes (and, with --cluster-label, the orphan\n" +
			"cluster + its node firewall + BYO VPC), in dependency order. Reads the Linode\n" +
			"PAT from LINODE_API_TOKEN (or LINODE_TOKEN). Dry-run by default; deletes only\n" +
			"with --yes. Volumes need a scope (--region or --volume-ids).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunReap(cliopts.Global.DryRun, cliopts.Global.Yes, o)
		},
	}
	f := c.Flags()
	f.StringVar(&o.Region, "region", "", "scope NodeBalancers/VPCs/Volumes to one Linode region (e.g. us-ord)")
	f.StringVar(&o.ClusterLabel, "cluster-label", "", "also reap the orphan cluster + its node firewall + <label>-vpc")
	f.StringVar(&o.Env, "env", "", "also reap the deployment's minted Linode creds (obj-storage keys <objLabelPrefix>-loki-<env>/<objLabelPrefix>-harbor-registry-<env> + in-cluster PAT llz-incluster-<objLabelPrefix>-<env>)")
	f.StringVar(&o.FwLabel, "fw-label", "", "exact firewall label to search (default: platform-nodes-fw + <label>-nodes)")
	f.StringVar(&o.VolumeIDs, "volume-ids", "", "space-separated Volume id allowlist (scopes the Volume sweep)")
	f.StringVar(&o.TagMustInclude, "tag-must-include", "", "only delete Volumes whose tags include this (e.g. block-storage)")
	f.BoolVar(&o.Force, "force", false, "delete the node firewall even if a live cluster still carries --cluster-label")
	return c
}
