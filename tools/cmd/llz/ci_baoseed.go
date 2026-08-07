package main

// ci_baoseed.go — the three OpenBao seeder flag sets, and their Deps wiring.
//
// The seeders are tools/internal/baoseed. What stays here is the cobra wiring and
// the three capabilities package main owns: applying a manifest, rendering the
// generic Secret shape half a dozen verbs share, and writing a GitHub secret.

import (
	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoseed"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kube"
)

func init() {
	// Delegating closures, never direct assignment: each target is itself a test
	// seam, and capturing its value at init would freeze whatever it pointed at.
	baoseed.Install(
		func(manifest string) error { return kube.Apply(manifest) },
		func(name, env, value string) error { return ghSetSecretFn(name, env, value) },
	)
}

func ciBaoSeedSealKeyCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "bao-seed-seal-key",
		Short: "create the per-cluster static auto-unseal key Secret (openbao-unseal-key)",
		Long: "Creates the `openbao-unseal-key` Secret holding this cluster's 32-byte static\n" +
			"auto-unseal key, which the chart's `seal \"static\"` stanza mounts at\n" +
			"/openbao/seal/unseal.key so every OpenBao pod unseals itself at boot (no managed\n" +
			"KMS on Linode). Run it before the OpenBao pods start; a missing Secret leaves a\n" +
			"pod in ContainerCreating, not crash-looping, so it need only complete. Idempotent\n" +
			"and never-rotating: an existing Secret is left untouched (a changed key bricks\n" +
			"unseal). On a namespace rebuild it restores the key from the infra-<region>\n" +
			"OPENBAO_SEAL_KEY secret; a first-ever bootstrap generates a new key, persists it\n" +
			"to infra-<region> for DR (requires GH_TOKEN/GH_REPO), and prints an offline-backup\n" +
			"banner — losing this key loses the data.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return baoseed.RunSeedSealKey(gopts.dryRun, region) },
	}
	c.Flags().StringVar(&region, "region", "", "region whose infra-<region> environment backs up the key for DR (required)")
	return c
}

// baoseed.RunSeedSealKey mints and escrows the static seal key.
//
// dryRun is a plain BOOL, not package main's globalOpts. A flag is not a
// capability — the three-clause rule's second question — and taking the whole
// struct would drag main's flag model across the boundary to read one bit.
func ciBaoSeedCmd() *cobra.Command {
	var o baoseed.Opts
	c := &cobra.Command{
		Use:   "bao-seed",
		Short: "seed one OpenBao KV path from env/random/K8s-Secret sources (generic seed step)",
		Long: "Generic native port of the \"Seed … in OpenBao\" inline steps of\n" +
			"llz-bootstrap-openbao.yml. Resolves each --field <key>=<source> (env:VAR,\n" +
			"gen:hex:N, gen:base64:N, k8s:NS/SECRET/KEY, literal:VALUE), ::add-mask::es\n" +
			"every resolved secret (literals excepted — they're already visible in the\n" +
			"workflow), and writes them in ONE `kv put` through the in-pod bao CLI.\n" +
			"--skip-if-present makes re-runs idempotent (don't rotate a live credential).\n" +
			"A missing env:/k8s: source follows --on-missing: skip (summary notes only),\n" +
			"warn (::warning:: + notes), or error (::error:: + BOOTSTRAP_ERRORS=true for\n" +
			"the job's final gate) — all exit 0 so the remaining seed steps still run.\n" +
			"--on-missing-standby/--missing-note-standby override the mode/notes when\n" +
			"HA_ROLE=standby. Reads OPENBAO_ROOT_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return baoseed.RunSeed(o) },
	}
	f := c.Flags()
	f.StringVar(&o.Path, "path", "", "KV path to seed, e.g. secret/grafana/admin (required)")
	f.StringArrayVar(&o.FieldSpecs, "field", nil, "<key>=<source> field spec (repeatable, required)")
	f.StringVar(&o.SkipIfPresent, "skip-if-present", "", "skip (exit 0) when this field of --path already has a value")
	f.StringVar(&o.OnMissing, "on-missing", "error", "behavior when an env:/k8s: source is empty: skip|warn|error")
	f.StringVar(&o.OnMissingStandby, "on-missing-standby", "", "on-missing override applied when HA_ROLE=standby")
	f.StringArrayVar(&o.MissingNotes, "missing-note", nil, "$GITHUB_STEP_SUMMARY line emitted on missing sources (repeatable)")
	f.StringArrayVar(&o.MissingNotesStandby, "missing-note-standby", nil, "summary lines replacing --missing-note when the standby override applies (repeatable)")
	f.StringArrayVar(&o.MissingAnnotations, "missing-annotation", nil, "::warning::/::error:: line(s) on missing sources (repeatable; default derived from the missing names)")
	f.StringArrayVar(&o.SummaryOnSeed, "summary-on-seed", nil, "$GITHUB_STEP_SUMMARY line appended only when a fresh seed happened (repeatable)")
	f.StringVar(&o.SeededMessage, "seeded-message", "", "stdout line after a successful seed (default '<path> seeded.')")
	return c
}

func ciBaoSeedAllCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "bao-seed-all",
		Short: "seed every generic OpenBao KV bootstrap path from one declarative table",
		Long: "Data-driven driver that runs the bootstrap's generic `bao-seed` paths\n" +
			"(github-dispatch-token, cert-automation token, linode api-token)\n" +
			"from the bootstrapSeeds() table — replacing\n" +
			"near-identical inline steps in llz-bootstrap-openbao.yml with one. (harbor\n" +
			"admin, grafana admin + otel bearer are written in-cluster via ESO\n" +
			"PushSecrets; loki object-store + harbor registry-s3 are minted + seeded by\n" +
			"`llz ci mint-bootstrap-objkeys`.) Each entry is the same flag set\n" +
			"`bao-seed` parses, so behavior is unchanged: per-seed idempotency guards,\n" +
			"on-missing modes, and summary notes. A missing env:/k8s: source follows\n" +
			"that seed's on-missing mode (exit 0, deferring via BOOTSTRAP_ERRORS where\n" +
			"the inline step did); a genuine kv-put failure aborts before the remaining\n" +
			"seeds. Reads OPENBAO_ROOT_TOKEN, OPENBAO_SECRETS_WRITE_TOKEN,\n" +
			"LINODE_API_TOKEN, and HA_ROLE.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return baoseed.RunSeedAll(region) },
	}
	c.Flags().StringVar(&region, "region", "", "region whose infra-<region> references fill the seed notes (required)")
	return c
}
