package main

// ci_mint_objkeys.go implements `llz ci mint-bootstrap-objkeys` — the
// bootstrap-time twin of the in-cluster rotator (`llz ci rotate-linode-creds`).
// It mints the FIRST Loki / Harbor-registry object-storage keys via the Linode
// API and seeds them into OpenBao, replacing the Terraform-minted keys and the
// whole CI relay that existed around them (`stash-env-secret` → LOKI_S3_* /
// HARBOR_REGISTRY_S3_* GitHub env secrets → `bao-seed` / seed-harbor-registry-s3).
//
// Why not Terraform: the rotator drains SAME-LABELED keys, so a TF-tracked key
// is drained on the rotator's second rotation and TF recreates it on the next
// object-storage apply — a permanent tug-of-war (see
// docs/designs/linode-credential-rotator.md). With this command the
// llz-object-storage module is buckets-only, key lifecycle has ONE owner
// (mint here at bootstrap, rotate in-cluster), and the credentials never
// transit GitHub.
//
// Runs in llz-bootstrap-openbao.yml's bootstrap job (root token live, so the
// OpenBao writes go through the same in-pod bao CLI passthrough as the generic
// seeds). Idempotent: an already-seeded path (its presentField has a value) is
// skipped, so re-bootstraps never clobber a rotator-minted key with a fresh
// bootstrap one. Each seed carries rotated_at so the in-cluster rotator adopts
// the key on its own cadence instead of immediately re-minting.
//
// Env: LINODE_API_TOKEN (mint), OPENBAO_ROOT_TOKEN (seed), GITHUB_STEP_SUMMARY.
// obj_cluster comes from terraform-iac-bootstrap/object-storage/<region>.tfvars
// — the source of truth for which Linode OBJ cluster TF provisioned the
// buckets into (same rationale as the retired seed-harbor-registry-s3).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// mintObjkeysLinodeClient is a seam for tests.
var mintObjkeysLinodeClient = func(token string) rotatorLinodeAPI {
	return linode.NewClient(token, 30*time.Second)
}

func ciMintBootstrapObjkeysCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "mint-bootstrap-objkeys",
		Short: "mint the first Loki/Harbor object-storage keys and seed them into OpenBao",
		Long: "Bootstrap-time twin of the in-cluster rotator: mints the region's scoped\n" +
			"object-storage keys (Loki chunks/ruler/admin, Harbor registry) via the Linode\n" +
			"API and seeds secret/loki/object-store + secret/harbor/registry-s3 in one\n" +
			"step — no Terraform-minted keys, no LOKI_S3_*/HARBOR_REGISTRY_S3_* GitHub\n" +
			"secrets, no stash/reseed relay. Idempotent: already-seeded paths are\n" +
			"skipped (a rotator-minted key is never clobbered). Seeds carry rotated_at\n" +
			"so the rotator adopts them on its own cadence. Reads LINODE_API_TOKEN,\n" +
			"OPENBAO_ROOT_TOKEN; obj_cluster from the object-storage tfvars.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIMintBootstrapObjkeys(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment whose keys to mint (required)")
	return c
}

func runCIMintBootstrapObjkeys(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	minting := os.Getenv("LINODE_API_TOKEN")
	if minting == "" {
		return fmt.Errorf("LINODE_API_TOKEN must be set (mints the object-storage keys)")
	}

	tfv := filepath.Join("terraform-iac-bootstrap", "object-storage", region+".tfvars")
	content, _ := os.ReadFile(tfv)
	objCluster := tfvarsValue(string(content), "obj_cluster")
	if objCluster == "" {
		fmt.Fprintf(os.Stderr, "::error::obj_cluster not found in %s — cannot mint the object-storage keys.\n", tfv)
		return fmt.Errorf("obj_cluster not found in %s", tfv)
	}

	lc := mintObjkeysLinodeClient(minting)
	ctx := context.Background()
	now := rotatorNow()

	for _, e := range buildRotationTable(region, objCluster) {
		if e.kind != credKindObjKey {
			continue // the DNS PAT is seeded from LINODE_DNS_TOKEN / minted by the rotator
		}
		// Idempotency: a seeded path means an earlier bootstrap (or the rotator)
		// owns a live key — minting again would orphan it until the next drain.
		if baoKVGetField(e.baoPath, e.presentField) != "" {
			fmt.Printf("%s: %s already seeded — skipping mint.\n", e.name, e.baoPath)
			continue
		}
		m, err := lc.CreateObjectStorageKeyBuckets(ctx, e.label, e.objCluster, e.buckets, e.permissions)
		if err != nil {
			return fmt.Errorf("mint %s: %w", e.name, err)
		}
		access, secret := cli.AsString(m["access_key"]), cli.AsString(m["secret_key"])
		if access == "" || secret == "" {
			return fmt.Errorf("mint %s returned no access_key/secret_key", e.name)
		}
		maskGHA(secret)
		fields := e.fields(access, secret)
		// rotated_at: the rotator's due-clock — a fresh bootstrap key is not due
		// until rotate-after-days from now, so the rotator adopts rather than
		// immediately re-mints.
		fields["rotated_at"] = strconv.FormatInt(now.Unix(), 10)
		if err := baoKVPutFn(e.baoPath, fields); err != nil {
			return fmt.Errorf("seed %s: %w", e.baoPath, err)
		}
		fmt.Printf("%s: minted %s and seeded %s.\n", e.name, e.label, e.baoPath)
		if err := appendGHAFile("GITHUB_STEP_SUMMARY",
			fmt.Sprintf("Minted object-storage key `%s` and seeded `%s`.", e.label, e.baoPath)); err != nil {
			return err
		}
	}
	return nil
}
