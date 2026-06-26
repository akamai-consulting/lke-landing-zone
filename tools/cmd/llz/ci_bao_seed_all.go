package main

// ci_bao_seed_all.go implements `llz ci bao-seed-all` — the data-driven driver
// that runs the bootstrap's generic OpenBao KV seeds from one declarative table
// instead of one hand-written workflow step per secret. It REPLACES the six
// scattered `llz ci bao-seed …` steps in llz-bootstrap-openbao.yml (harbor
// admin, github-dispatch-token, cert-automation token, grafana admin, otel
// bearer, loki object-store) with a single step; the per-seed flag wiring those
// steps carried now lives in bootstrapSeeds() below. The harbor-specific seeds
// that derive their own material (robot accounts → ci_harbor_steps.go,
// docker-config → ci_seed_special.go, registry-S3 → ci_bao_seed registry-s3)
// keep their dedicated commands — only the plain `bao-seed` invocations fold in.
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
		// Harbor admin password — read from Harbor's Helm-generated Secret so ESO
		// can sync harbor-admin-credentials. Skips cleanly before Harbor is up.
		{
			path:       "secret/harbor/admin",
			fieldSpecs: []string{"password=k8s:harbor/harbor-admin-password/HARBOR_ADMIN_PASSWORD"},
			onMissing:  "skip",
			missingNotes: []string{
				"harbor-admin-password Secret not found — Harbor not yet deployed.",
				"Re-run this workflow after Harbor is up to seed secret/harbor/admin.",
			},
		},
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
		// Grafana admin — generated once, then idempotent (never rotate a live cred).
		{
			path:          "secret/grafana/admin",
			skipIfPresent: "password",
			fieldSpecs:    []string{"username=literal:admin", "password=gen:base64:24"},
			onMissing:     "error",
			seededMessage: "secret/grafana/admin seeded (new password generated).",
			summaryOnSeed: []string{
				"",
				"**Grafana admin credentials seeded.** Retrieve the password after deploy:",
				"```",
				`kubectl -n llz-openbao exec platform-openbao-0 -- \`,
				`  env VAULT_ADDR=https://127.0.0.1:8200 VAULT_SKIP_VERIFY=true \`,
				"  bao kv get -field=password secret/grafana/admin",
				"```",
			},
		},
		// OTel Collector ingress bearer — generated once, idempotent so reruns
		// don't invalidate in-flight client config bound to the current bearer.
		{
			path:          "secret/otel/ingress",
			skipIfPresent: "token",
			fieldSpecs:    []string{"token=gen:hex:32"},
			onMissing:     "error",
			seededMessage: "secret/otel/ingress seeded (new token generated).",
		},
		// Loki object-store S3 credentials. Hard-defers if absent — Loki would
		// CrashLoopBackOff with no chunk store.
		{
			path:       "secret/loki/object-store",
			fieldSpecs: []string{"AWS_ACCESS_KEY_ID=env:LOKI_S3_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY=env:LOKI_S3_SECRET_KEY"},
			onMissing:  "error",
			missingAnnotations: []string{
				"LOKI_S3_ACCESS_KEY / LOKI_S3_SECRET_KEY not set — secret/loki/object-store not seeded; Loki will CrashLoopBackOff",
			},
			missingNotes: []string{
				"LOKI_S3_ACCESS_KEY / LOKI_S3_SECRET_KEY not set — skipping secret/loki/object-store.",
				fmt.Sprintf("Add them as infra-%s environment secrets and re-run.", region),
				fmt.Sprintf("Create the keys via: linode-cli object-storage keys-create --label platform-loki-%s", region),
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
			"(harbor admin, github-dispatch-token, cert-automation token, grafana admin,\n" +
			"otel bearer, loki object-store) from the bootstrapSeeds() table — replacing\n" +
			"six near-identical inline steps in llz-bootstrap-openbao.yml with one. Each\n" +
			"entry is the same flag set `bao-seed` parses, so behavior is unchanged:\n" +
			"per-seed idempotency guards, on-missing modes, and summary notes. A missing\n" +
			"env:/k8s: source follows that seed's on-missing mode (exit 0, deferring via\n" +
			"BOOTSTRAP_ERRORS where the inline step did); a genuine kv-put failure aborts\n" +
			"before the remaining seeds, exactly as a failed inline step did. Reads\n" +
			"OPENBAO_ROOT_TOKEN, OPENBAO_SECRETS_WRITE_TOKEN, LOKI_S3_*, and HA_ROLE;\n" +
			"harbor/admin reads the in-cluster harbor-admin-password Secret.",
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
