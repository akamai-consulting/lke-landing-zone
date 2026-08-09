package main

// credentials.go implements `llz credentials` — the mutating half of the two
// policy-tracked Linode credential lifecycles, folded in from the former
// standalone linode-pat-rotator and linode-obj-key-rotator binaries:
//
//   - `pat create` / `pat revoke-old` — the shared LINODE_API_TOKEN PAT
//     (the PAT-rotation policy). create mints a new PAT with the configured label / scopes /
//     validity; revoke-old is the daily reaper that keeps the newest
//     same-labeled PAT and revokes any older sibling past the grace window.
//     Stateless: the label IS the record of which PAT is current.
//   - `obj-key create` / `obj-key revoke-old` — the 120-day Object Storage key
//     SLA (TF-state bucket pair). Same create / revoke-old shape, but two OBJ
//     API differences make it diverge: the create response returns BOTH
//     access_key and secret_key (the secret half is shown exactly once), and
//     the keys API exposes no `created` timestamp, so revoke-old drains by
//     keep-newest-N (Linode ids increase monotonically per account).
//
// Output contract (consumed by the linode-credentials composite action): each
// subcommand prints exactly one JSON record on stdout (the action parses
// .new_pat_id / .new_token / .new_obj_key_id / .new_access_key /
// .new_secret_key / .revoked_ids / .kept_*); all logging goes to stderr, and
// freshly minted secret values are ::add-mask::ed on stderr before the record
// is printed so a step that pipes our stdout through `tee` is still scrubbed.
// Dry-run is the default; --apply (env ROTATION_APPLY=true) arms it. The
// `.event` strings keep the former binary names — they are part of the audit
// record format, not a live binary reference.

import (
	"log/slog"
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/credrotate"
	"github.com/spf13/cobra"
)

func credentialsCmd() *cobra.Command {
	o := &credrotate.Opts{}
	c := &cobra.Command{
		Use:   "credentials",
		Short: "rotate the shared Linode credentials (PAT + TF-state OBJ key); dry-run unless --apply",
		Long: "Mutating half of the policy-tracked Linode credential lifecycles, invoked by the\n" +
			"linode-credentials composite action (llz-secret-rotation.yml). `pat` owns the\n" +
			"shared LINODE_API_TOKEN (90-day policy): create mints a new PAT, revoke-old drains\n" +
			"older same-labeled siblings past a grace window. `obj-key` owns the 120-day\n" +
			"TF-state Object Storage key SLA: create mints a bucket-scoped pair, revoke-old\n" +
			"drains by keep-newest-N (the OBJ keys API exposes no created time). Every\n" +
			"subcommand prints one JSON record on stdout, logs to stderr, ::add-mask::es\n" +
			"fresh secrets, and is a dry-run unless --apply (env ROTATION_APPLY=true).",
		PersistentPreRun: func(*cobra.Command, []string) {
			// The standalone rotator binaries logged structured JSON to stderr;
			// keep that format for the rotation audit trail in the action logs.
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
		},
	}
	pf := c.PersistentFlags()
	pf.StringVar(&o.Token, "linode-token", "", "Linode PAT with account:read_write (default: env LINODE_TOKEN)")
	pf.BoolVar(&o.Apply, "apply", false, "arm the Linode API writes (default: env ROTATION_APPLY; dry-run otherwise)")
	c.AddCommand(credentialsPATCmd(o), credentialsObjKeyCmd(o), credentialsLKEAdminCmd(o))
	return c
}
