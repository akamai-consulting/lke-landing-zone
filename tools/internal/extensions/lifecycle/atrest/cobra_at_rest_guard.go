package atrest

// cobra_at_rest_guard.go — `llz ci at-rest-guard`: the pre-apply gate on encryption
// at rest (docs/adr/0007-terraform-state-encryption.md).
//
// The scanner, the residual registry, the corpus location and the report all live
// in tools/internal/extensions/lifecycle/atrest. What is left here is the flag set.
//
// That the corpus location could move too is the whole reason guardkit exists: it
// is shared with seven other guards, so while it sat in package main this file
// would have had to keep it, and internal/atrest's tests would have been split
// across a package boundary by nothing but where a helper happened to live.

import (
	"os"

	"github.com/spf13/cobra"
)

func AtRestGuardCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "at-rest-guard",
		Short: "fail when a Terraform-declared resource is not encrypted at rest",
		Long: "Static gate on encryption at rest (docs/adr/0007-terraform-state-encryption.md).\n" +
			"Asserts that every Terraform ROOT declares a `terraform { encryption }` block —\n" +
			"state holds kubeconfig_raw and every Managed Postgres root_password in the\n" +
			"clear — and that every linode_lke_node_pool / linode_instance sets\n" +
			"disk_encryption and every linode_volume sets encryption.\n\n" +
			"Encryption is decided at CREATE and is immutable afterwards, so there is no\n" +
			"remediation for a resource that came up unencrypted — only a rebuild. That is\n" +
			"why this is a pre-apply gate and not a report.\n\n" +
			"The ADR 0007 (state encryption) phase-1 unencrypted fallback is registered in atRestAllowed with\n" +
			"an exit condition; unregistered gaps fail, and so do registry entries whose\n" +
			"gap is gone. A missing disk_encryption cannot be registered at all.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return Run(os.Stdout, root) },
	}
	c.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return c
}
