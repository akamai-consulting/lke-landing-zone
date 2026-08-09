package credrotate

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// TempObjkeyLinodeClient is a seam for tests.
var TempObjkeyLinodeClient = func(token string) LinodeAPI {
	return linode.NewClient(token, 30*time.Second)
}

func RunTempObjkeyCreate(region, endpoint, bucketsCSV string) error {
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

	m, err := TempObjkeyLinodeClient(token).CreateObjectStorageKeyBuckets(
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

func RunTempObjkeyDelete() error {
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
	if err := TempObjkeyLinodeClient(token).DeleteObjectStorageKey(context.Background(), id); err != nil {
		return fmt.Errorf("delete temp drain key id=%d: %w", id, err)
	}
	fmt.Printf("temp drain key id=%d deleted.\n", id)
	return nil
}
