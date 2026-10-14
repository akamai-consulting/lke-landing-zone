package credrotate

// cobra_rotatelinode.go — the `llz ci rotate-linode-creds` flag set.
//
// The rotation table and its drivers are tools/internal/extensions/lifecycle/credrotate, which owns the
// whole credential-rotation family: the framework, the three rotators, the table
// that says WHICH credentials exist and where they live, and the in-cluster token
// resolver they share.

import (
	"context"

	"github.com/spf13/cobra"
)

func RotateLinodeCredsCmd() *cobra.Command {
	var apply bool
	c := &cobra.Command{
		Use:   "rotate-linode-creds",
		Short: "rotate the in-cluster Linode object-storage keys and write them to OpenBao",
		Long: "In-cluster Linode credential rotator (runs in the linodeCredRotator CronJob).\n" +
			"For each in-cluster-only Linode credential that is DUE (OpenBao `rotated_at`\n" +
			"older than --rotate-after-days, or absent), mints a fresh one via the Linode\n" +
			"API (auth: LINODE_TOKEN), verifies it, writes it to OpenBao via the\n" +
			"linode-rotator Kubernetes-auth role, then drains older same-labeled resources\n" +
			"keeping the newest --keep-newest. Dry-run unless --apply. Env: REGION,\n" +
			"OBJ_CLUSTER, LINODE_TOKEN, OPENBAO_ADDR, OPENBAO_CA_FILE, OPENBAO_KUBERNETES_\n" +
			"{ROLE,MOUNT}, SA_TOKEN_FILE, ROTATE_AFTER_DAYS, KEEP_NEWEST.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunRotateLinodeCreds(context.Background(), apply)
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "actually rotate; without it, list what is due and exit")
	return c
}
