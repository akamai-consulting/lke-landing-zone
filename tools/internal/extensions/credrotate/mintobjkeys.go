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
	"sort"
	"strconv"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/objenc"
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
		// Drain same-labelled siblings now that THIS key is the recorded one.
		//
		// The idempotency check above guards the seeded direction only, so the
		// window between the mint and the seed was unprotected: a bootstrap that
		// died there left a live scoped key nobody tracked, and the re-run — seeing
		// an unseeded path — minted another. The secret half is shown exactly once,
		// so an orphan cannot be adopted; the only way to converge is to remove it
		// once its replacement is safely recorded. Same keep-newest shape the
		// in-cluster PAT already uses, and deliberately AFTER the seed: draining
		// first would delete the live key if the seed then failed.
		if newID, ok := cli.AsUint64(m["id"]); ok {
			drainSupersededObjKeys(ctx, lc, e.Label, newID)
		}
		if err := ghaout.Append("GITHUB_STEP_SUMMARY",
			fmt.Sprintf("Minted object-storage key `%s` and seeded `%s`.", e.Label, e.BaoPath)); err != nil {
			return err
		}
	}
	return nil
}

// objKeyKeepNewest is how many same-labelled keys survive a drain, INCLUDING the
// one just minted. Matches `llz credentials obj-key revoke-old --keep-newest`,
// whose default is 2 for the same reason.
const objKeyKeepNewest = 2

// drainSupersededObjKeys bounds the number of same-labelled object-storage keys,
// keeping the objKeyKeepNewest newest by id and deleting the rest.
//
// KEEP-NEWEST, NOT DELETE-ALL-BUT-MINE. The first cut deleted every key carrying
// the label except the one just minted, which is wrong in the one state that is
// not a fresh install: a re-bootstrap against a LIVE cluster whose OpenBao path
// was wiped mints a replacement, seeds it, and would then have revoked the key
// every running consumer still holds until ESO re-syncs. The rotator keeps two
// generations precisely so a credential swap has an overlap window, and a drain
// that collapses it turns a recovery into an outage. Bounding accumulation is
// the actual goal — the bug was that an interrupted bootstrap orphaned a key
// nothing ever reaped — and a cap of two achieves it without removing the
// overlap.
//
// Linode ids increase monotonically per account, so highest id == newest; the
// caller's freshly minted key is therefore always among those kept.
//
// Best-effort by construction: the key it protects is already live and seeded by
// the time this runs, so a listing or delete that fails must not fail the
// bootstrap. It only ever touches this instance's own label (labels are
// namespaced per instance — see objlabels.go), so it cannot reach a sibling
// instance's credentials the way the old shared-label drains could.
func drainSupersededObjKeys(ctx context.Context, lc LinodeAPI, label string, keepID uint64) {
	keys, err := lc.ListObjectStorageKeys(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::could not list object-storage keys to drain superseded %q (%v) — the new key is live and seeded; re-run to retry the drain.\n", label, err)
		return
	}
	var ids []uint64
	for _, k := range keys {
		if cli.AsString(k["label"]) != label {
			continue
		}
		if id, ok := cli.AsUint64(k["id"]); ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	if len(ids) <= objKeyKeepNewest {
		return
	}
	for _, id := range ids[objKeyKeepNewest:] {
		// Never the key we just recorded, whatever the ordering says. ids is sorted
		// newest-first and the mint is the newest, so this cannot trigger — which is
		// exactly why it is cheap insurance against a future reordering.
		if id == keepID {
			continue
		}
		if err := lc.DeleteObjectStorageKey(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "::warning::could not delete superseded object-storage key %d (%s): %v\n", id, label, err)
			continue
		}
		fmt.Printf("  drained superseded key %d (%s) — beyond the %d newest kept for rotation overlap.\n",
			id, label, objKeyKeepNewest)
	}
}
