package credrotate

// credentials_objkey.go implements `llz credentials obj-key create|revoke-old`
// — the 120-day SLA for the Terraform-state bucket access pair
// (TF_STATE_ACCESS_KEY + TF_STATE_SECRET_KEY), moved verbatim from the former
// cmd/linode-obj-key-rotator binary.
//
// Sibling to `llz credentials pat` — same create / revoke-old shape, but two
// Object Storage API differences make it diverge:
//
//  1. The create response returns BOTH access_key and secret_key (the secret
//     half is shown exactly once — same constraint as a PAT token).
//  2. The OBJ keys API exposes NO `created` timestamp, so the PAT's
//     grace-by-age drain doesn't apply. revoke-old instead sorts same-labeled
//     keys by `id` (Linode IDs increase monotonically per account) and keeps
//     the N most recent.
//
// See credentials.go for the shared contract (one JSON record on stdout, logs
// + ::add-mask:: on stderr, dry-run unless armed).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

func RunObjKeyCreate(ctx context.Context, client ObjKeyAPI, apply bool, label, cluster, bucket, permissions, ghaAccessName, ghaSecretName string, ghaDeployments []string) error {
	slog.Info("creating OBJ key", "label", label, "cluster", cluster, "bucket", bucket, "permissions", permissions)

	if !apply {
		// A DRY RUN STILL ASKS THE API ONE READ-ONLY QUESTION — and the one worth
		// asking. It used to return here having called nothing, so it reported
		// green with a token that could not reach Object Storage, and green for a
		// bucket the key would be scoped to in the WRONG region: the class
		// rotation_identity.go documents, where the armed path mints a key that
		// cannot read the state it exists to unlock. The probe looks the bucket up
		// in the region the create would put in bucket_access.
		probe, err := probeStateBucket(ctx, client, cluster, bucket)
		if err != nil {
			return err
		}
		slog.Warn("DRY-RUN: would POST /v4/object-storage/keys")
		return cli.PrintRecordAndLog(map[string]any{
			"event":              "linode-obj-key-rotator.create",
			"timestamp_unix":     time.Now().Unix(),
			"dry_run":            true,
			"label":              label,
			"bucket_cluster":     cluster,
			"bucket_name":        bucket,
			"bucket_permissions": permissions,
			"probe":              probe,
		})
	}

	resp, err := client.CreateObjectStorageKey(ctx, label, cluster, bucket, permissions)
	if err != nil {
		return err
	}
	newID, ok := cli.AsUint64(resp["id"])
	if !ok {
		return fmt.Errorf("create response missing .id")
	}
	accessKey, ok := resp["access_key"].(string)
	if !ok || accessKey == "" {
		return fmt.Errorf("create response missing .access_key")
	}
	secretKey, ok := resp["secret_key"].(string)
	if !ok || secretKey == "" {
		return fmt.Errorf("create response missing .secret_key")
	}
	// Linode returns the secret half exactly once. Mask both halves before they
	// could leak into a caller's log buffer.
	fmt.Fprintf(os.Stderr, "::add-mask::%s\n", accessKey)
	fmt.Fprintf(os.Stderr, "::add-mask::%s\n", secretKey)

	// Write the new pair into each infra-<deployment> env. Secret half FIRST,
	// access half SECOND: a workflow that reads mid-rotation then sees (new access,
	// OLD secret) and fails to auth — the safe failure, since the previous key is
	// still live (drained separately, daily) so a retry succeeds.
	if ghaSecretName != "" {
		if err := WriteRotatedSecret(ghaSecretName, secretKey, ghaDeployments); err != nil {
			return err
		}
	}
	if ghaAccessName != "" {
		if err := WriteRotatedSecret(ghaAccessName, accessKey, ghaDeployments); err != nil {
			return err
		}
	}
	if ghaSecretName != "" || ghaAccessName != "" {
		slog.Info("updated GHA OBJ-key secrets", "deployments", ghaDeployments)
	}

	slog.Info("created new OBJ key", "new_obj_key_id", newID)
	return cli.PrintRecordAndLog(map[string]any{
		"event":              "linode-obj-key-rotator.create",
		"timestamp_unix":     time.Now().Unix(),
		"dry_run":            false,
		"label":              label,
		"bucket_cluster":     cluster,
		"bucket_name":        bucket,
		"bucket_permissions": permissions,
		"new_obj_key_id":     newID,
		"new_access_key":     accessKey,
		"new_secret_key":     secretKey,
	})
}

func RunObjKeyRevokeOld(ctx context.Context, client ObjKeyAPI, apply bool, label string, keepNewest int64) error {
	if keepNewest < 1 {
		return fmt.Errorf("keep_newest=%d must be >= 1 — refusing to revoke the live key", keepNewest)
	}

	keys, err := client.ListObjectStorageKeys(ctx)
	if err != nil {
		return err
	}

	// Linode IDs increase monotonically per account — sort same-labeled keys by
	// id descending so the newest is index 0.
	var ids []uint64
	legacyN := 0
	for _, k := range keys {
		s, _ := k["label"].(string)
		if s == legacyRotationLabels[rotationKindTFStateKey] {
			legacyN++
		}
		if s != label {
			continue
		}
		id, ok := cli.AsUint64(k["id"])
		if !ok {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	// Report-only, and only when the drain is NOT already pointed at the legacy
	// label (an operator cleaning up by hand does not need to be told about it).
	if label != legacyRotationLabels[rotationKindTFStateKey] {
		reportLegacyRotationLabels(rotationKindTFStateKey, legacyN)
	}

	now := time.Now().Unix()
	if len(ids) == 0 {
		slog.Warn("no OBJ keys match label — nothing to revoke", "label", label)
		return cli.PrintRecordAndLog(map[string]any{
			"event":          "linode-obj-key-rotator.revoke-old",
			"timestamp_unix": now,
			"dry_run":        !apply,
			"label":          label,
			"keep_newest":    keepNewest,
			"kept_ids":       []uint64{},
			"revoked_ids":    []uint64{},
		})
	}

	keep := int(keepNewest)
	if keep > len(ids) {
		keep = len(ids)
	}
	keptIDs := append([]uint64{}, ids[:keep]...)
	revoked := []uint64{}
	for _, id := range ids[keep:] {
		if !apply {
			slog.Warn("DRY-RUN: would DELETE OBJ key", "id", id)
		} else {
			if err := client.DeleteObjectStorageKey(ctx, id); err != nil {
				return err
			}
			slog.Info("revoked", "id", id)
		}
		revoked = append(revoked, id)
	}

	return cli.PrintRecordAndLog(map[string]any{
		"event":          "linode-obj-key-rotator.revoke-old",
		"timestamp_unix": now,
		"dry_run":        !apply,
		"label":          label,
		"keep_newest":    keepNewest,
		"kept_ids":       keptIDs,
		"revoked_ids":    revoked,
	})
}

// probeStateBucket is the OBJ-key dry run's authenticated read: the bucket the
// key would be scoped to must be visible to this token in the region the create
// sends (linode.ObjRegion — the same derivation CreateObjectStorageKeyBuckets
// uses, so the probe cannot ask a different question from the one the mint asks).
func probeStateBucket(ctx context.Context, client ObjKeyAPI, cluster, bucket string) (map[string]any, error) {
	if bucket == "" {
		return nil, fmt.Errorf("dry-run probe: no bucket name (--bucket-name / OBJ_BUCKET_NAME) — an armed create would send an empty bucket_access entry")
	}
	region := linode.ObjRegion(cluster)
	buckets, err := client.ListObjectStorageBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("dry-run probe: GET /v4/object-storage/buckets failed with this token, so an armed create would fail too: %w", err)
	}
	var elsewhere []string
	for _, b := range buckets {
		if cli.AsString(b["label"]) != bucket {
			continue
		}
		r := cli.AsString(b["region"])
		if r == "" {
			r = linode.ObjRegion(cli.AsString(b["cluster"]))
		}
		if r == region {
			return map[string]any{"call": "GET /v4/object-storage/buckets", "ok": true, "bucket": bucket, "region": region}, nil
		}
		elsewhere = append(elsewhere, r)
	}
	if len(elsewhere) > 0 {
		return nil, fmt.Errorf("dry-run probe: bucket %q exists in region(s) %v, not %q (from cluster %q) — an armed create would mint a key that cannot read it", bucket, elsewhere, region, cluster)
	}
	return nil, fmt.Errorf("dry-run probe: this token sees no bucket %q on the account (looked in region %q, from cluster %q) — an armed create would mint a key for a bucket that is not there", bucket, region, cluster)
}
