package main

// ci_database.go — the `llz ci` database flag sets, and the Deps wiring.
//
// The rotation, seeding, probing and assertion are tools/internal/database. What
// stays here is the cobra wiring and the ONE capability package main owns: how to
// reach OpenBao. The package owns which ROLE the database credential lives under.

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/database"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/reconcilelanes"
)

func init() {
	// A delegating closure, not a direct assignment: openbaoClientForward is itself
	// reachable through seams a test may swap, and capturing its value at init
	// would freeze whatever it pointed at. That bug has cost this campaign twice.
	database.InstallOpenBaoForward(func(role string) (*openbao.Client, func(), error) {
		return openbaoClientForward(role)
	})
}

func ciRotateDBAdminCmd() *cobra.Command {
	var region string
	var apply, rotateNow bool
	var afterDays int
	c := &cobra.Command{
		Use:   "rotate-db-admin",
		Short: "rotate the admin password on each Managed Postgres cluster (due-based)",
		Long: "Rotates the `akmadmin` password on every Linode Managed PostgreSQL cluster in\n" +
			"the deployment whose secret/infra/db-admin/<name> credential is older than\n" +
			"--rotate-after-days, and writes the replacement back to OpenBao.\n\n" +
			"REPORT-ONLY unless --apply is passed. The Linode API offers only an in-place\n" +
			"credential RESET — there is no second credential to verify before swapping, and\n" +
			"the old password dies immediately — so the mutation is irreversible and is not\n" +
			"armed by default.\n\n" +
			"After a successful rotation the command refreshes Terraform state, because\n" +
			"`llz ci seed-db-admin` reconciles OpenBao toward state and would otherwise push\n" +
			"the pre-rotation password back over the live one.\n\n" +
			"--rotate-now ignores the age check and rotates every seeded cluster. This is\n" +
			"rotate-on-create: bootstrap runs it right after `llz ci seed-db-admin` so the\n" +
			"PROVISIONING credential Terraform handed over — the one sitting in Terraform\n" +
			"state — is replaced within the same run that created it.\n\n" +
			"Run with the databases root as the working directory. Reads LINODE_TOKEN and\n" +
			"OPENBAO_ROOT_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return database.RunRotateDBAdmin(region, apply, rotateNow, afterDays)
		},
	}
	c.Flags().BoolVar(&rotateNow, "rotate-now", false, "rotate every seeded cluster regardless of age (rotate-on-create; still requires --apply)")
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) being rotated — labels the run summary (required)")
	c.Flags().BoolVar(&apply, "apply", false, "arm the rotation; without it the command only reports what is due")
	c.Flags().IntVar(&afterDays, "rotate-after-days", database.RotateAfterDays, "rotate a credential older than this many days")
	return c
}

func ciDBDeclaredCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "db-declared",
		Short: "report whether a deployment declares any database clusters",
		Long: "Writes `declared=true|false` to $GITHUB_OUTPUT so the bootstrap workflow can\n" +
			"skip the databases terraform-init + admin seed on the majority of instances\n" +
			"that declare no databases — initializing that root unconditionally would put\n" +
			"a new failure mode on the critical bootstrap path for no benefit.\n\n" +
			"Reads the RENDERED terraform-iac-bootstrap/databases/<region>.tfvars. Reading\n" +
			"the per-env file is sound because `databases` is deliberately NOT inherited\n" +
			"from spec.defaults.cluster (llz validate rejects it there), so the env's own\n" +
			"tfvars is the whole truth.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return database.RunDBDeclared(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) to check (required)")
	return c
}

func ciDBSummaryCmd() *cobra.Command {
	var region, phase string
	c := &cobra.Command{
		Use:   "db-summary",
		Short: "render the databases step summary (apply | destroy-plan)",
		Long: "Appends the $GITHUB_STEP_SUMMARY block for the databases root.\n\n" +
			"--phase apply: reads the `labels` output and lists what was provisioned, or\n" +
			"says plainly that nothing was — a deployment declaring no clusters applies\n" +
			"cleanly and must not look like a silent failure. Points at the bootstrap\n" +
			"workflow, which owns the admin seed.\n\n" +
			"--phase destroy-plan: the data-loss caution. Unlike a bucket, a destroyed\n" +
			"Managed Postgres takes its data with it and Linode keeps no snapshot.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return database.RunDBSummary(region, phase) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) being reported (required)")
	c.Flags().StringVar(&phase, "phase", "", "apply | destroy-plan (required)")
	return c
}

func ciAssertDatabaseCmd() *cobra.Command {
	var settle, interval, timeout int
	c := &cobra.Command{
		Use:   "assert-database",
		Short: "fail unless every declared Managed Postgres accepts its seeded admin credential",
		Long: "Connects to each Managed Postgres declared under " + reconcilelanes.DBAdminRoot + " and completes\n" +
			"the TLS + authentication handshake with the seeded admin credential. No query is\n" +
			"issued — reaching ReadyForQuery is the whole answer.\n\n" +
			"db-declared and db-summary report what the spec asks for; seed-db-admin writes\n" +
			"the credential. Nothing checked it still WORKS, and rotate-db-admin resets the\n" +
			"password IN PLACE with no overlap window, so the failure mode is a live endpoint\n" +
			"that answers every TCP connect and rejects the credential every consumer holds —\n" +
			"a rotation that half landed, or an OpenBao copy never re-seeded. A reachability\n" +
			"check passes straight through that; only authentication separates them.\n\n" +
			"Self-skips when no cluster is declared. Does NOT skip when one is declared and\n" +
			"its credential is missing — that is the half-seeded state, and it is a finding.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return database.RunAssertDatabase(time.Duration(timeout)*time.Second,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().IntVar(&timeout, "timeout", 20, "seconds for the TLS + auth handshake")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}

// dbVerdict is one cluster's outcome.
// missingFields names the empty required fields. Pure.
//
// A half-populated record is its own failure and must not be probed: dialing
// with an empty password would produce a "rejected" verdict that blames the
// database for a seeding problem.
func ciSeedDBAdminCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "seed-db-admin",
		Short: "seed each Managed Postgres cluster's admin connection into OpenBao",
		Long: "Reads the `databases` root's single `connections` output (a map keyed by\n" +
			"cluster name) and writes one secret/infra/db-admin/<name> per entry —\n" +
			"endpoint, port, username, password, ca, sslmode. Reading ONE output keeps a\n" +
			"cluster's endpoint and password from ever being paired across clusters.\n\n" +
			"A no-op when the map is empty, so it runs unconditionally on a deployment\n" +
			"that declared no databases. Idempotent, and it compares the cluster's\n" +
			"ENDPOINT, not its password: OpenBao is authoritative for the credential once\n" +
			"`llz ci rotate-db-admin` has run, so a path already pointing at this cluster\n" +
			"is left completely alone. A path pointing at a DIFFERENT cluster (a recreate)\n" +
			"is re-seeded. Paths are never deleted — removing a cluster from the spec\n" +
			"leaves its credential for an operator to reap, because the cluster usually\n" +
			"still exists at that point and this is the only way back into it.\n\n" +
			"Run with the databases root as the working directory. Reads\n" +
			"OPENBAO_ROOT_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return database.RunSeedDBAdmin(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) being seeded — labels the run summary (required)")
	return c
}
