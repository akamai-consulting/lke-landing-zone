package credrotate

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/objenc"
)

// MintObjkeysLinodeClient is a seam for tests.
var MintObjkeysLinodeClient = func(token string) LinodeAPI {
	return linode.NewClient(token, 30*time.Second)
}

func RunMintBootstrapObjkeys(region string) error {
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

	lc := MintObjkeysLinodeClient(minting)
	ctx := context.Background()
	now := Now()

	// CI, inside the instance checkout — read the prefix from the spec (the
	// in-cluster rotator gets the same value via OBJ_LABEL_PREFIX instead).
	prefix, err := objenc.LabelPrefixFor("mint-bootstrap-objkeys")
	if err != nil {
		return err
	}
	for _, e := range BuildRotationTable(prefix, region, objCluster) {
		if e.Kind != CredKindObjKey {
			continue // the DNS PAT is seeded from LINODE_DNS_TOKEN / minted by the rotator
		}
		// Idempotency: a seeded path means an earlier bootstrap (or the rotator)
		// owns a live key — minting again would orphan it until the next drain.
		// An unreadable path is not an unseeded one: reading "" off a sealed pod
		// mints a REAL object-storage key at Linode and overwrites the live one,
		// breaking Loki/Harbor S3 auth until the next drain. Fail closed.
		seeded, verdict := baoread.KVGetFieldOK(e.BaoPath, e.PresentField)
		if verdict == baoread.Unknown {
			return baoread.ErrReadUnknown(e.BaoPath, e.PresentField, "mint a replacement key for "+e.Name)
		}
		if seeded != "" {
			fmt.Printf("%s: %s already seeded — skipping mint.\n", e.Name, e.BaoPath)
			continue
		}
		m, err := lc.CreateObjectStorageKeyBuckets(ctx, e.Label, e.ObjCluster, e.Buckets, e.Permissions)
		if err != nil {
			return fmt.Errorf("mint %s: %w", e.Name, err)
		}
		access, secret := cli.AsString(m["access_key"]), cli.AsString(m["secret_key"])
		if access == "" || secret == "" {
			return fmt.Errorf("mint %s returned no access_key/secret_key", e.Name)
		}
		maskGHA(secret)
		fields := e.Fields(access, secret)
		// rotated_at: the rotator's due-clock — a fresh bootstrap key is not due
		// until rotate-after-days from now, so the rotator adopts rather than
		// immediately re-mints.
		fields["rotated_at"] = strconv.FormatInt(now.Unix(), 10)
		if err := baoread.KVPut(e.BaoPath, fields); err != nil {
			return fmt.Errorf("seed %s: %w", e.BaoPath, err)
		}
		fmt.Printf("%s: minted %s and seeded %s.\n", e.Name, e.Label, e.BaoPath)
		if err := appendGHAFile("GITHUB_STEP_SUMMARY",
			fmt.Sprintf("Minted object-storage key `%s` and seeded `%s`.", e.Label, e.BaoPath)); err != nil {
			return err
		}
	}
	return nil
}
