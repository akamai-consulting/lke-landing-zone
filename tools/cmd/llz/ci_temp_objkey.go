package main

// ci_temp_objkey.go implements `llz ci temp-objkey create|delete` — a
// short-lived scoped object-storage key for the destroy-time bucket drain in
// llz-terraform.yml's destroy-object-storage job. The drain used to read the
// Loki/Harbor key credentials from Terraform outputs; those keys are no longer
// TF-managed (see the llz-object-storage module's "Access keys" note), so the
// drain mints its own temporary key around the s5cmd sweep and deletes it in
// an always() step.
//
// The label (llz-drain-<region>) is DISTINCT from the rotator's labels so the
// in-cluster rotator's keep-newest-N drain never counts or revokes it; the
// paired delete (plus its distinct label making leftovers identifiable) keeps
// a crashed run from leaking a live credential silently.
//
// create: mints read_write on the given buckets, masks the secret, and exports
//   TEMP_OBJKEY_ID / TEMP_OBJKEY_ACCESS / TEMP_OBJKEY_SECRET via $GITHUB_ENV.
// delete: revokes the key id in TEMP_OBJKEY_ID (no-op when unset/empty — the
//   create may have been skipped on an already-drained teardown re-run).
//
// Env: LINODE_API_TOKEN, GITHUB_ENV.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// tempObjkeyLinodeClient is a seam for tests.
var tempObjkeyLinodeClient = func(token string) rotatorLinodeAPI {
	return linode.NewClient(token, 30*time.Second)
}

func ciTempObjkeyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "temp-objkey",
		Short: "mint/delete a short-lived scoped OBJ key for the destroy-time bucket drain",
	}

	var region, endpoint, buckets string
	create := &cobra.Command{
		Use:   "create",
		Short: "mint a temporary read_write key on the given buckets; export TEMP_OBJKEY_* via $GITHUB_ENV",
		Long: "Mints a short-lived scoped key (label llz-drain-<region> — distinct from\n" +
			"the rotator's labels so its keep-newest-N drain never touches it) with\n" +
			"read_write on --buckets, for the destroy job's s5cmd sweep. The OBJ\n" +
			"cluster is derived from --endpoint (https://<cluster>.linodeobjects.com —\n" +
			"the module's s3_endpoint output). Exports TEMP_OBJKEY_ID/ACCESS/SECRET to\n" +
			"$GITHUB_ENV (secret masked). Pair with `temp-objkey delete` in an\n" +
			"always() step. Reads LINODE_API_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCITempObjkeyCreate(region, endpoint, buckets)
		},
	}
	create.Flags().StringVar(&region, "region", "", "deployment name — labels the key llz-drain-<region> (required)")
	create.Flags().StringVar(&endpoint, "endpoint", "", "S3 endpoint URL, e.g. https://us-ord-1.linodeobjects.com (required)")
	create.Flags().StringVar(&buckets, "buckets", "", "comma-separated bucket labels the key may drain (required)")

	del := &cobra.Command{
		Use:   "delete",
		Short: "revoke the temporary key exported by `temp-objkey create` (no-op when unset)",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return runCITempObjkeyDelete() },
	}

	c.AddCommand(create, del)
	return c
}

func runCITempObjkeyCreate(region, endpoint, bucketsCSV string) error {
	if region == "" || endpoint == "" || bucketsCSV == "" {
		return fmt.Errorf("--region, --endpoint and --buckets are required")
	}
	token := os.Getenv("LINODE_API_TOKEN")
	if token == "" {
		return fmt.Errorf("LINODE_API_TOKEN must be set")
	}
	objCluster := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"), ".linodeobjects.com")
	if objCluster == "" || strings.Contains(objCluster, "/") {
		return fmt.Errorf("cannot derive the OBJ cluster from endpoint %q (want https://<cluster>.linodeobjects.com)", endpoint)
	}
	var buckets []string
	for _, b := range strings.Split(bucketsCSV, ",") {
		if b = strings.TrimSpace(b); b != "" {
			buckets = append(buckets, b)
		}
	}
	if len(buckets) == 0 {
		return fmt.Errorf("--buckets resolved to an empty list")
	}

	m, err := tempObjkeyLinodeClient(token).CreateObjectStorageKeyBuckets(
		context.Background(), "llz-drain-"+region, objCluster, buckets, "read_write")
	if err != nil {
		return fmt.Errorf("mint temp drain key: %w", err)
	}
	id, ok := cli.AsUint64(m["id"])
	access, secret := cli.AsString(m["access_key"]), cli.AsString(m["secret_key"])
	if !ok || access == "" || secret == "" {
		return fmt.Errorf("mint temp drain key: response missing id/access_key/secret_key")
	}
	maskGHA(secret)
	fmt.Printf("temp drain key llz-drain-%s minted (id=%d, %d bucket(s)).\n", region, id, len(buckets))
	return appendGHAFile("GITHUB_ENV",
		"TEMP_OBJKEY_ID="+strconv.FormatUint(id, 10),
		"TEMP_OBJKEY_ACCESS="+access,
		"TEMP_OBJKEY_SECRET="+secret)
}

func runCITempObjkeyDelete() error {
	idRaw := strings.TrimSpace(os.Getenv("TEMP_OBJKEY_ID"))
	if idRaw == "" {
		fmt.Println("TEMP_OBJKEY_ID unset — no temp drain key to delete.")
		return nil
	}
	token := os.Getenv("LINODE_API_TOKEN")
	if token == "" {
		return fmt.Errorf("LINODE_API_TOKEN must be set")
	}
	id, err := strconv.ParseUint(idRaw, 10, 64)
	if err != nil {
		return fmt.Errorf("TEMP_OBJKEY_ID %q is not a key id", idRaw)
	}
	if err := tempObjkeyLinodeClient(token).DeleteObjectStorageKey(context.Background(), id); err != nil {
		return fmt.Errorf("delete temp drain key id=%d: %w", id, err)
	}
	fmt.Printf("temp drain key id=%d deleted.\n", id)
	return nil
}
