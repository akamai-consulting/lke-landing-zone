package main

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
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/spf13/cobra"
)

func credentialsObjKeyCmd(o *rotatorOpts) *cobra.Command {
	c := &cobra.Command{
		Use:   "obj-key",
		Short: "rotate the TF-state Object Storage key pair (120-day SLA): create + revoke-old",
	}
	c.AddCommand(credentialsObjKeyCreateCmd(o), credentialsObjKeyRevokeOldCmd(o))
	return c
}

func credentialsObjKeyCreateCmd(o *rotatorOpts) *cobra.Command {
	var label, cluster, bucket, permissions, ghaAccessName, ghaSecretName, ghaDeployments string
	c := &cobra.Command{
		Use:   "create",
		Short: "mint a new bucket-scoped OBJ key pair (JSON record on stdout)",
		Long: "Issues a new bucket-scoped Linode Object Storage key, printing the new id +\n" +
			"access_key + secret_key as one JSON record on stdout. With --gha-*-secret-name it\n" +
			"ALSO writes both halves into those GitHub secrets for every infra-<deployment>\n" +
			"environment (--gha-deployments) — the env-scoped copies the workflows read. The\n" +
			"secret half is shown exactly once by the Linode API. Dry-run unless --apply.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			token, apply, err := o.resolve()
			if err != nil {
				return err
			}
			return runCredentialsObjKeyCreate(context.Background(), newObjKeyRotatorClient(token), apply, label, cluster, bucket, permissions, ghaAccessName, ghaSecretName, strings.Fields(ghaDeployments))
		},
	}
	defaultPermissions := os.Getenv("OBJ_BUCKET_PERMISSIONS")
	if defaultPermissions == "" {
		defaultPermissions = "read_write"
	}
	f := c.Flags()
	f.StringVar(&label, "label", os.Getenv("OBJ_LABEL"), "label for the new OBJ key — also the revoke-old drain target (env OBJ_LABEL)")
	f.StringVar(&cluster, "bucket-cluster", os.Getenv("OBJ_BUCKET_CLUSTER"), "Linode object-storage cluster id, e.g. us-ord-10 (env OBJ_BUCKET_CLUSTER)")
	f.StringVar(&bucket, "bucket-name", os.Getenv("OBJ_BUCKET_NAME"), "bucket name to scope the key to (env OBJ_BUCKET_NAME)")
	f.StringVar(&permissions, "bucket-permissions", defaultPermissions, "read_only, read_write, or none (env OBJ_BUCKET_PERMISSIONS)")
	f.StringVar(&ghaAccessName, "gha-access-key-secret-name", os.Getenv("GHA_ACCESS_KEY_SECRET_NAME"), "GitHub secret for the access-key half, written per infra-<deployment> env (env GHA_ACCESS_KEY_SECRET_NAME)")
	f.StringVar(&ghaSecretName, "gha-secret-key-secret-name", os.Getenv("GHA_SECRET_KEY_SECRET_NAME"), "GitHub secret for the secret-key half, written per infra-<deployment> env (env GHA_SECRET_KEY_SECRET_NAME)")
	f.StringVar(&ghaDeployments, "gha-deployments", os.Getenv("GHA_SECRET_DEPLOYMENTS"), "space-separated deployment names whose infra-<name> env gets the new secrets (env GHA_SECRET_DEPLOYMENTS; empty = repo-level)")
	return c
}

func credentialsObjKeyRevokeOldCmd(o *rotatorOpts) *cobra.Command {
	var label string
	var keepNewest int64
	c := &cobra.Command{
		Use:   "revoke-old",
		Short: "daily reaper: keep the N newest same-labeled OBJ keys by id, revoke the rest",
		Long: "Lists every OBJ key matching the label, keeps the N most recent by id (Linode\n" +
			"IDs are monotonically increasing per account, so highest id == newest), and\n" +
			"revokes the rest. keep-newest=2 gives ~30-day overlap with monthly rotation.\n" +
			"Dry-run unless --apply.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			token, apply, err := o.resolve()
			if err != nil {
				return err
			}
			return runCredentialsObjKeyRevokeOld(context.Background(), newObjKeyRotatorClient(token), apply, label, keepNewest)
		},
	}
	f := c.Flags()
	f.StringVar(&label, "label", os.Getenv("OBJ_LABEL"), "label to drain — same label `obj-key create` uses (env OBJ_LABEL)")
	f.Int64Var(&keepNewest, "keep-newest", cli.EnvInt("OBJ_KEEP_NEWEST", 2), "how many most-recent same-labeled keys to keep (env OBJ_KEEP_NEWEST)")
	return c
}

func runCredentialsObjKeyCreate(ctx context.Context, client objKeyAPI, apply bool, label, cluster, bucket, permissions, ghaAccessName, ghaSecretName string, ghaDeployments []string) error {
	slog.Info("creating OBJ key", "label", label, "cluster", cluster, "bucket", bucket, "permissions", permissions)

	if !apply {
		slog.Warn("DRY-RUN: would POST /v4/object-storage/keys")
		return cli.PrintRecord(map[string]any{
			"event":              "linode-obj-key-rotator.create",
			"timestamp_unix":     time.Now().Unix(),
			"dry_run":            true,
			"label":              label,
			"bucket_cluster":     cluster,
			"bucket_name":        bucket,
			"bucket_permissions": permissions,
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
		if err := writeRotatedSecret(ghaSecretName, secretKey, ghaDeployments); err != nil {
			return err
		}
	}
	if ghaAccessName != "" {
		if err := writeRotatedSecret(ghaAccessName, accessKey, ghaDeployments); err != nil {
			return err
		}
	}
	if ghaSecretName != "" || ghaAccessName != "" {
		slog.Info("updated GHA OBJ-key secrets", "deployments", ghaDeployments)
	}

	slog.Info("created new OBJ key", "new_obj_key_id", newID)
	return cli.PrintRecord(map[string]any{
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

func runCredentialsObjKeyRevokeOld(ctx context.Context, client objKeyAPI, apply bool, label string, keepNewest int64) error {
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
	for _, k := range keys {
		if s, _ := k["label"].(string); s != label {
			continue
		}
		id, ok := cli.AsUint64(k["id"])
		if !ok {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })

	now := time.Now().Unix()
	if len(ids) == 0 {
		slog.Warn("no OBJ keys match label — nothing to revoke", "label", label)
		return cli.PrintRecord(map[string]any{
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

	return cli.PrintRecord(map[string]any{
		"event":          "linode-obj-key-rotator.revoke-old",
		"timestamp_unix": now,
		"dry_run":        !apply,
		"label":          label,
		"keep_newest":    keepNewest,
		"kept_ids":       keptIDs,
		"revoked_ids":    revoked,
	})
}
