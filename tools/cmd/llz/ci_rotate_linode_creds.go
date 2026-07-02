package main

// ci_rotate_linode_creds.go implements `llz ci rotate-linode-creds` — the
// in-cluster Linode credential rotator (cred-hardening #4, Phase 1). It runs in
// the cluster (the linodeCredRotator CronJob, on the slim llz image) and rotates
// the in-cluster-ONLY Linode-minted credentials — the object-storage keys (Loki,
// Harbor registry) — writing each straight to OpenBao.
// No CI step, no `propagate-pat`, no GitHub secret. See
// docs/designs/linode-credential-rotator.md.
//
// Per credential, when DUE (the OpenBao secret's `rotated_at` is older than
// --rotate-after-days, or absent on a fresh bootstrap seed): mint via the Linode
// API (authenticating with the in-cluster LINODE_TOKEN), VERIFY the new
// credential before touching the old, write it to OpenBao via a Kubernetes-auth
// role (linode-rotator), then drain older same-labeled resources keeping the N
// newest. Verify-before-write + keep-newest-N means a bad mint or a failed write
// never breaks a consumer.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
	"github.com/spf13/cobra"
)

const (
	credKindPAT    = "pat"
	credKindObjKey = "objkey"
	// PAT validity must exceed the rotation cadence so the live token never
	// expires between rotations.
	patValidityDays = 90
)

// rotatorLinodeAPI is the Linode surface the rotator needs (seamed for tests).
type rotatorLinodeAPI interface {
	ListProfileTokens(ctx context.Context) ([]map[string]any, error)
	CreateProfileToken(ctx context.Context, label, scopes, expiry string) (map[string]any, error)
	DeleteProfileToken(ctx context.Context, id uint64) error
	ListObjectStorageKeys(ctx context.Context) ([]map[string]any, error)
	CreateObjectStorageKeyBuckets(ctx context.Context, label, cluster string, buckets []string, permissions string) (map[string]any, error)
	DeleteObjectStorageKey(ctx context.Context, id uint64) error
	Verify(ctx context.Context) error
}

// baoStore is the OpenBao surface the rotator needs (seamed for tests).
type baoStore interface {
	Get(ctx context.Context, path, key string) (string, bool, error)
	Write(ctx context.Context, path string, data map[string]string) error
}

var (
	linodeRotatorClient = func(token string) rotatorLinodeAPI { return linode.NewClient(token, 30*time.Second) }
	newRotatorBaoStore  = openLinodeRotatorBaoStore
	rotatorNow          = func() time.Time { return time.Now() }
)

// credEntry is one rotated credential. fields maps the minted material to the
// COMPLETE OpenBao field set (KV v2 writes replace the whole secret), so the
// builder re-derives any static fields (bucket/endpoint/region) too.
type credEntry struct {
	name        string   // log label
	kind        string   // credKindPAT | credKindObjKey
	label       string   // Linode resource label (mint + drain target)
	scopes      string   // PAT only
	objCluster  string   // objkey only
	buckets     []string // objkey only — every bucket the key grants (one bucket_access each)
	permissions string   // objkey only
	baoPath     string
	// presentField is the KV field whose presence means "already seeded" — the
	// bootstrap mint's idempotency probe (mint-bootstrap-objkeys skips a path
	// the rotator or an earlier bootstrap already owns).
	presentField string
	fields       func(a, b string) map[string]string // (token,"") for PAT; (access,secret) for objkey
}

// buildRotationTable is the Phase-1 set of in-cluster-only Linode credentials.
// region/objCluster come from the CronJob env (rendered per-env, like the
// volume-labeler's REGION). Labels + bucket grants MIRROR the llz-object-storage
// module's bootstrap-minted keys ("<label_prefix>-<name>-<region_suffix>",
// label_prefix "platform"): the Loki key spans the chunks/ruler/admin buckets —
// the actual bucket names, NOT the key label (an earlier revision minted against
// the nonexistent "platform-loki-<region>" bucket). Pure — unit-tested.
func buildRotationTable(region, objCluster string) []credEntry {
	return []credEntry{
		{
			name: "loki-object-store", kind: credKindObjKey, label: "platform-loki-" + region,
			objCluster: objCluster,
			buckets: []string{
				"platform-loki-chunks-" + region,
				"platform-loki-ruler-" + region,
				"platform-loki-admin-" + region,
			},
			permissions: "read_write",
			baoPath:     "secret/loki/object-store", presentField: "AWS_ACCESS_KEY_ID",
			fields: func(access, secret string) map[string]string { return lokiObjectStoreFields(access, secret) },
		},
		{
			name: "harbor-registry-s3", kind: credKindObjKey, label: "platform-harbor-registry-" + region,
			objCluster:  objCluster,
			buckets:     []string{"platform-harbor-registry-" + region},
			permissions: "read_write",
			baoPath:     "secret/harbor/registry-s3", presentField: "access_key_id",
			fields: func(access, secret string) map[string]string {
				return harborRegistryS3Fields(region, objCluster, access, secret)
			},
		},
	}
}

// lokiObjectStoreFields is the secret/loki/object-store field set (the env-var
// names the Loki singleBinary pod reads, matching the loki ExternalSecret).
func lokiObjectStoreFields(access, secret string) map[string]string {
	return map[string]string{"AWS_ACCESS_KEY_ID": access, "AWS_SECRET_ACCESS_KEY": secret}
}

// harborRegistryS3Fields derives the five secret/harbor/registry-s3 fields.
// The bucket name encodes the deployment region (matches the bucket resource
// label); endpoint/region come from the obj_cluster the object-storage tfvars
// actually provisioned into — NOT guessed from the env name. Lives here (not
// ci_seed_special.go) because the rotation table owns this path now: both the
// bootstrap mint and the rotator write the same complete field set.
func harborRegistryS3Fields(region, objCluster, accessKey, secretKey string) map[string]string {
	return map[string]string{
		"access_key_id":     accessKey,
		"secret_access_key": secretKey,
		"bucket_name":       "platform-harbor-registry-" + region,
		"endpoint":          "https://" + objCluster + ".linodeobjects.com",
		"region":            objCluster,
	}
}

// isDue reports whether a credential whose OpenBao secret carries rotatedAt
// (epoch seconds; "" or unparseable on a fresh bootstrap seed) is due at now.
// An unrecognized/absent stamp is treated as due so the rotator adopts a
// bootstrap-seeded secret on its first run. Pure — unit-tested.
func isDue(rotatedAt string, now time.Time, rotateAfterDays int) bool {
	ts, err := strconv.ParseInt(strings.TrimSpace(rotatedAt), 10, 64)
	if err != nil {
		return true
	}
	return now.Unix()-ts >= int64(rotateAfterDays)*linode.DaySecs
}

// idsToDrain returns the resource ids to revoke — every id except the keepNewest
// highest (Linode ids increase monotonically, so highest == newest). keepNewest
// is floored at 1 so the live credential is never drained. Pure — unit-tested.
func idsToDrain(ids []uint64, keepNewest int) []uint64 {
	if keepNewest < 1 {
		keepNewest = 1
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	if len(ids) <= keepNewest {
		return nil
	}
	return ids[keepNewest:]
}

// idsByLabel extracts the ids of list items whose `label` matches exactly.
func idsByLabel(items []map[string]any, label string) []uint64 {
	var ids []uint64
	for _, it := range items {
		if cli.AsString(it["label"]) != label {
			continue
		}
		if id, ok := cli.AsUint64(it["id"]); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func ciRotateLinodeCredsCmd() *cobra.Command {
	var apply bool
	c := &cobra.Command{
		Use:   "rotate-linode-creds",
		Short: "rotate the in-cluster Linode object-storage keys and write them to OpenBao",
		Long: "In-cluster Linode credential rotator (runs in the linodeCredRotator CronJob).\n" +
			"For each in-cluster-only Linode credential that is DUE (OpenBao `rotated_at`\n" +
			"older than --rotate-after-days, or absent), mints a fresh one via the Linode\n" +
			"API (auth: LINODE_TOKEN), verifies it, writes it to OpenBao via the\n" +
			"linode-rotator Kubernetes-auth role, then drains older same-labeled resources\n" +
			"keeping the newest --keep-newest. Dry-run unless --apply. Env: REGION,\n" +
			"OBJ_CLUSTER, LINODE_TOKEN, OPENBAO_ADDR, OPENBAO_CA_FILE, OPENBAO_KUBERNETES_\n" +
			"{ROLE,MOUNT}, SA_TOKEN_FILE, ROTATE_AFTER_DAYS, KEEP_NEWEST.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runRotateLinodeCreds(context.Background(), apply) },
	}
	c.Flags().BoolVar(&apply, "apply", false, "actually rotate; without it, list what is due and exit")
	return c
}

func runRotateLinodeCreds(ctx context.Context, apply bool) error {
	region := os.Getenv("REGION")
	if region == "" {
		return fmt.Errorf("REGION must be set")
	}
	objCluster := os.Getenv("OBJ_CLUSTER")
	minting := os.Getenv("LINODE_TOKEN")
	if minting == "" {
		return fmt.Errorf("LINODE_TOKEN must be set (the in-cluster Linode token used to mint replacements)")
	}
	rotateAfter := int(cli.EnvInt("ROTATE_AFTER_DAYS", 80))
	keepNewest := int(cli.EnvInt("KEEP_NEWEST", 2))

	table := buildRotationTable(region, objCluster)
	for _, e := range table {
		if e.kind == credKindObjKey && objCluster == "" {
			return fmt.Errorf("OBJ_CLUSTER must be set to rotate object-storage keys (e.g. %s)", e.name)
		}
	}

	lc := linodeRotatorClient(minting)
	now := rotatorNow()

	// OpenBao login is deferred until at least one credential is due, so a no-op
	// run (nothing due) does not require OpenBao to be reachable.
	var bao baoStore
	ensureBao := func() error {
		if bao != nil {
			return nil
		}
		b, err := newRotatorBaoStore(ctx)
		bao = b
		return err
	}

	var rotated, skipped []string
	for _, e := range table {
		if err := ensureBao(); err != nil {
			return err
		}
		rotatedAt, _, err := bao.Get(ctx, e.baoPath, "rotated_at")
		if err != nil {
			return fmt.Errorf("read %s rotated_at: %w", e.baoPath, err)
		}
		if !isDue(rotatedAt, now, rotateAfter) {
			skipped = append(skipped, e.name)
			fmt.Printf("%s: not due (rotated_at=%s, threshold %dd)\n", e.name, rotatedAt, rotateAfter)
			continue
		}
		if !apply {
			fmt.Printf("%s: DUE — would rotate (dry-run)\n", e.name)
			rotated = append(rotated, e.name+" (dry-run)")
			continue
		}
		if err := rotateOne(ctx, lc, bao, e, now, keepNewest); err != nil {
			return fmt.Errorf("rotate %s: %w", e.name, err)
		}
		fmt.Printf("%s: rotated → %s\n", e.name, e.baoPath)
		rotated = append(rotated, e.name)
	}

	fmt.Printf("rotate-linode-creds: rotated=%v skipped=%v\n", rotated, skipped)
	return nil
}

// rotateOne mints, verifies, writes, and drains a single credential. The order
// is load-bearing: nothing old is revoked until the new credential is verified
// (PAT) and written to OpenBao.
func rotateOne(ctx context.Context, lc rotatorLinodeAPI, bao baoStore, e credEntry, now time.Time, keepNewest int) error {
	var fields map[string]string
	switch e.kind {
	case credKindPAT:
		expiry := linode.FmtLinodeTS(now.Unix() + patValidityDays*linode.DaySecs)
		m, err := lc.CreateProfileToken(ctx, e.label, e.scopes, expiry)
		if err != nil {
			return err
		}
		token := cli.AsString(m["token"])
		if token == "" {
			return fmt.Errorf("mint returned no token")
		}
		// Verify the new token works BEFORE the old one is drained.
		if err := linodeRotatorClient(token).Verify(ctx); err != nil {
			return fmt.Errorf("new token failed verification — not draining the old one: %w", err)
		}
		fields = e.fields(token, "")
	case credKindObjKey:
		m, err := lc.CreateObjectStorageKeyBuckets(ctx, e.label, e.objCluster, e.buckets, e.permissions)
		if err != nil {
			return err
		}
		access, secret := cli.AsString(m["access_key"]), cli.AsString(m["secret_key"])
		if access == "" || secret == "" {
			return fmt.Errorf("mint returned no access_key/secret_key")
		}
		fields = e.fields(access, secret)
	default:
		return fmt.Errorf("unknown credential kind %q", e.kind)
	}

	fields["rotated_at"] = strconv.FormatInt(now.Unix(), 10)
	if err := bao.Write(ctx, e.baoPath, fields); err != nil {
		return fmt.Errorf("write %s: %w", e.baoPath, err)
	}
	drainOld(ctx, lc, e, keepNewest)
	return nil
}

// drainOld revokes older same-labeled resources, keeping the newest keepNewest.
// Best-effort: the new credential is already live + written, so a failed revoke
// is logged but does not fail the run (keep-newest-N converges next run).
func drainOld(ctx context.Context, lc rotatorLinodeAPI, e credEntry, keepNewest int) {
	var (
		items []map[string]any
		del   func(context.Context, uint64) error
		err   error
	)
	switch e.kind {
	case credKindPAT:
		items, err = lc.ListProfileTokens(ctx)
		del = lc.DeleteProfileToken
	case credKindObjKey:
		items, err = lc.ListObjectStorageKeys(ctx)
		del = lc.DeleteObjectStorageKey
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::%s: list for drain failed (new credential is live; will converge next run): %v\n", e.name, err)
		return
	}
	for _, id := range idsToDrain(idsByLabel(items, e.label), keepNewest) {
		if err := del(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "::warning::%s: revoke id=%d failed (will retry next run): %v\n", e.name, id, err)
		}
	}
}

// openLinodeRotatorBaoStore logs in to OpenBao via Kubernetes auth (the
// linode-rotator role) using the pod's ServiceAccount token, trusting the
// mounted openbao CA, and returns a write-capable client.
func openLinodeRotatorBaoStore(ctx context.Context) (baoStore, error) {
	addr := envOrDefault(os.Getenv, "OPENBAO_ADDR", "https://platform-openbao.llz-openbao.svc.cluster.local:8200")
	mount := envOrDefault(os.Getenv, "OPENBAO_KUBERNETES_MOUNT", "kubernetes")
	role := envOrDefault(os.Getenv, "OPENBAO_KUBERNETES_ROLE", "linode-rotator")
	saFile := envOrDefault(os.Getenv, "SA_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token")
	// TLS to OpenBao: mount the CA and set OPENBAO_CA_FILE to verify it; otherwise
	// OPENBAO_SKIP_VERIFY=true falls back to the established in-cluster posture
	// (every baoExec uses VAULT_SKIP_VERIFY) for pod→OpenBao traffic.
	var httpClient *http.Client
	if caFile := os.Getenv("OPENBAO_CA_FILE"); caFile != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read OPENBAO_CA_FILE: %w", err)
		}
		if httpClient, err = openbao.HTTPClientWithCA(caPEM, 30*time.Second); err != nil {
			return nil, err
		}
	} else if cli.EnvBool("OPENBAO_SKIP_VERIFY", false) {
		httpClient = openbao.HTTPClientInsecure(30 * time.Second)
	} else {
		return nil, fmt.Errorf("set OPENBAO_CA_FILE (mounted openbao CA) or OPENBAO_SKIP_VERIFY=true")
	}
	jwt, err := os.ReadFile(saFile)
	if err != nil {
		return nil, fmt.Errorf("read ServiceAccount token: %w", err)
	}
	token, err := openbao.KubernetesLogin(ctx, httpClient, addr, mount, role, strings.TrimSpace(string(jwt)))
	if err != nil {
		return nil, err
	}
	return openbao.NewWithClient(addr, token, "", httpClient), nil
}
