package main

// ci_bao_seed_all.go implements `llz ci bao-seed-all` — the data-driven driver
// that runs the bootstrap's generic OpenBao KV seeds from one declarative table
// instead of one hand-written workflow step per secret. It REPLACES the
// scattered `llz ci bao-seed …` steps in llz-bootstrap-openbao.yml
// (github-dispatch-token, cert-automation token, linode api-token, certmanager
// dns01) with a single step; the per-seed flag wiring those steps carried
// now lives in bootstrapSeeds() below. (harbor admin, grafana admin + otel bearer
// used to seed here too; they are now written in-cluster via ESO PushSecrets.
// loki object-store + harbor registry-s3 moved to `llz ci
// mint-bootstrap-objkeys`, which mints the keys itself instead of relaying
// GitHub secrets.) The seeds that derive their own material (harbor robots →
// the in-cluster provisioner / seed-standby-harbor-robots) keep their
// dedicated commands — only the plain `bao-seed` invocations fold in.
//
// Each entry IS a baoSeedOpts (the exact flag set ci_bao_seed.go parses), so the
// behavior of every seed — sources, idempotency guard, on-missing mode, summary
// notes — is identical to the inline steps it replaces; runCIBaoSeed does the
// work. The infra-<region> references the inline steps built by shell-
// interpolating ${REGION} are filled from --region here.

import (
	"fmt"

	"github.com/spf13/cobra"
)

// bootstrapSeeds returns the generic KV seeds the OpenBao bootstrap runs, in the
// same order the workflow ran them. region fills the infra-<region> references
// the inline steps produced by shell-interpolating ${REGION}.
//
// onMissing is set on EVERY entry (even the gen:-only ones that can never go
// missing) because runCIBaoSeed validates it up front — an empty mode is
// rejected. "error" matches the bao-seed flag default the inline steps relied on
// when they passed no --on-missing.
func bootstrapSeeds(region string) []baoSeedOpts {
	return []baoSeedOpts{
		// secret/harbor/admin is NO LONGER seeded here — an ESO PushSecret
		// (apl-values/components/harbor/harbor-admin-push.yaml) mirrors Harbor's
		// Helm-generated harbor-admin-password Secret into OpenBao via the
		// write-scoped openbao-push store, replacing the kubectl-exec kv put.
		//
		// GitHub dispatch token for the harbor-ready PostSync hook. Hard-defers on
		// active/standalone (BOOTSTRAP_ERRORS); summary-note skip on a standby,
		// where harbor-ready is the active peer's concern.
		{
			path:             "secret/infra/github-dispatch-token",
			fieldSpecs:       []string{"token=env:OPENBAO_SECRETS_WRITE_TOKEN"},
			onMissing:        "error",
			onMissingStandby: "skip",
			missingAnnotations: []string{
				fmt.Sprintf("OPENBAO_SECRETS_WRITE_TOKEN is not set for the infra-%s environment.", region),
				"Without it, secret/infra/github-dispatch-token cannot be seeded and the harbor-ready",
				"PostSync hook will fail to dispatch build-firewall-controller. Add the secret and re-run.",
			},
			missingNotesStandby: []string{
				"OPENBAO_SECRETS_WRITE_TOKEN not set — skipping secret/infra/github-dispatch-token (standby).",
			},
		},
		// cert-automation GitHub token (same PAT) for the cert-automation-github-token
		// ExternalSecret in llz-cert-automation.
		{
			path:         "secret/cert-automation/github-token",
			fieldSpecs:   []string{"token=env:OPENBAO_SECRETS_WRITE_TOKEN"},
			onMissing:    "skip",
			missingNotes: []string{"OPENBAO_SECRETS_WRITE_TOKEN not set — skipping secret/cert-automation/github-token."},
		},
		// Linode API token for the linode-volume-labeler ExternalSecret (and any
		// other in-cluster Linode API consumer). Seeds the SAME path the daily
		// rotation pipeline (secret-rotation.yml → propagate-linode-pat) keeps
		// fresh, so the consumer reads one canonical, rotating credential via ESO
		// instead of a static TF-injected Secret. on-missing skip: the labeler is
		// non-critical (cosmetic PV labels) and rotation will seed it later anyway.
		{
			path:         "secret/linode/api-token",
			fieldSpecs:   []string{"token=env:LINODE_API_TOKEN"},
			onMissing:    "skip",
			missingNotes: []string{"LINODE_API_TOKEN not set — skipping secret/linode/api-token (rotation will seed it)."},
		},
		// secret/grafana/admin and secret/otel/ingress are NO LONGER seeded here.
		// Both are self-generated, in-cluster-only secrets, so they moved to a
		// kube-native ESO flow: a Password generator mints the value and a
		// PushSecret writes it into OpenBao with updatePolicy: IfNotExists (the
		// declarative equivalent of the old generate-once + skip-if-present). See
		// apl-values/_shared/manifest/generated-secrets/ and the eso-pusher policy/
		// role in ci_openbao_configure.go. This drops two root-token + kubectl-exec
		// seed steps from the bootstrap.
		//
		// secret/loki/object-store is NO LONGER seeded here (nor from LOKI_S3_*
		// GitHub secrets): `llz ci mint-bootstrap-objkeys` mints the scoped key via
		// the Linode API and seeds the path directly (rotated_at-stamped), and the
		// in-cluster rotator owns it after first boot — the credential never
		// transits GitHub.
		//
		// cert-manager DNS-01 token (Linode PAT scoped to DNS zone write). Seeding
		// it here folds the common case of bootstrap-dns.yml into this bootstrap:
		// when LINODE_DNS_TOKEN is provisioned up front, the KV path is ready
		// before the dns tree is ever applied. on-missing skip: DNS is optional at
		// bootstrap (cluster-bootstrap uses a placeholder for apl-core's schema),
		// and bootstrap-dns.yml remains the late-provisioning/recovery path that
		// seeds this same path once the token exists. skip-if-present: on a
		// linodeCredRotator env the in-cluster rotator OWNS this path after first
		// boot (it mints fresh DNS-scoped PATs and drains old ones) — a re-run
		// re-seeding the stale GitHub copy would clobber a live rotator-minted
		// token with a possibly-drained one. Deliberate updates go through
		// bootstrap-dns.yml, whose kv put is unconditional.
		{
			path:          "secret/certmanager/dns01",
			fieldSpecs:    []string{"token=env:LINODE_DNS_TOKEN"},
			skipIfPresent: "token",
			onMissing:     "skip",
			missingNotes: []string{
				"LINODE_DNS_TOKEN not set — skipping secret/certmanager/dns01.",
				fmt.Sprintf("Provision it as an infra-%s environment secret, then run bootstrap-dns.yml (the late-provisioning path).", region),
			},
		},
	}
}

func ciBaoSeedAllCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "bao-seed-all",
		Short: "seed every generic OpenBao KV bootstrap path from one declarative table",
		Long: "Data-driven driver that runs the bootstrap's generic `bao-seed` paths\n" +
			"(github-dispatch-token, cert-automation token, linode api-token,\n" +
			"certmanager dns01) from the bootstrapSeeds() table — replacing\n" +
			"near-identical inline steps in llz-bootstrap-openbao.yml with one. (harbor\n" +
			"admin, grafana admin + otel bearer are written in-cluster via ESO\n" +
			"PushSecrets; loki object-store + harbor registry-s3 are minted + seeded by\n" +
			"`llz ci mint-bootstrap-objkeys`.) Each entry is the same flag set\n" +
			"`bao-seed` parses, so behavior is unchanged: per-seed idempotency guards,\n" +
			"on-missing modes, and summary notes. A missing env:/k8s: source follows\n" +
			"that seed's on-missing mode (exit 0, deferring via BOOTSTRAP_ERRORS where\n" +
			"the inline step did); a genuine kv-put failure aborts before the remaining\n" +
			"seeds. Reads OPENBAO_ROOT_TOKEN, OPENBAO_SECRETS_WRITE_TOKEN,\n" +
			"LINODE_API_TOKEN, LINODE_DNS_TOKEN, and HA_ROLE.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIBaoSeedAll(region) },
	}
	c.Flags().StringVar(&region, "region", "", "region whose infra-<region> references fill the seed notes (required)")
	return c
}

func runCIBaoSeedAll(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	for _, o := range bootstrapSeeds(region) {
		fmt.Printf("=== seeding %s ===\n", o.path)
		if err := runCIBaoSeed(o); err != nil {
			return fmt.Errorf("seed %s: %w", o.path, err)
		}
	}
	return nil
}
