package credrotate

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
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

const (
	CredKindPAT    = "pat"
	CredKindObjKey = "objkey"
	// PAT validity must exceed the rotation cadence so the live token never
	// expires between rotations.
	patValidityDays = 90
)

// LinodeAPI is the Linode surface the rotator needs (seamed for tests).
type LinodeAPI interface {
	ListProfileTokens(ctx context.Context) ([]map[string]any, error)
	CreateProfileToken(ctx context.Context, label, scopes, expiry string) (map[string]any, error)
	DeleteProfileToken(ctx context.Context, id uint64) error
	ListObjectStorageKeys(ctx context.Context) ([]map[string]any, error)
	CreateObjectStorageKeyBuckets(ctx context.Context, label, cluster string, buckets []string, permissions string) (map[string]any, error)
	DeleteObjectStorageKey(ctx context.Context, id uint64) error
	Verify(ctx context.Context) error
}

var (
	NewLinodeClient = func(token string) LinodeAPI {
		return capability.CloudFor(objKeyCloudBinding()).Client(token, 30*time.Second)
	}
	NewBaoStore = openLinodeRotatorBaoStore
	Now         = func() time.Time { return time.Now() }
)

// CredEntry is one rotated credential. fields maps the minted material to the
// COMPLETE OpenBao field set (KV v2 writes replace the whole secret), so the
// builder re-derives any static fields (bucket/endpoint/region) too.
type CredEntry struct {
	Name        string   // log label
	Kind        string   // CredKindPAT | CredKindObjKey
	Label       string   // Linode resource label (mint + drain target)
	Scopes      string   // PAT only
	ObjCluster  string   // objkey only
	Buckets     []string // objkey only — every bucket the key grants (one bucket_access each)
	Permissions string   // objkey only
	BaoPath     string
	// presentField is the KV field whose presence means "already seeded" — the
	// bootstrap mint's idempotency probe (mint-bootstrap-objkeys skips a path
	// the rotator or an earlier bootstrap already owns).
	PresentField string
	Fields       func(a, b string) map[string]string // (token,"") for PAT; (access,secret) for objkey
}

// BuildRotationTable is the Phase-1 set of in-cluster-only Linode credentials.
// region/objCluster come from the CronJob env (rendered per-env, like the
// volume-labeler's REGION). Labels + bucket grants MIRROR the llz-object-storage
// module's bootstrap-minted keys ("<label_prefix>-<name>-<region_suffix>",
// label_prefix is the instance's spec.instance.objLabelPrefix — it used to be the
// shared constant "platform", which gave every instance identical labels): the Loki key spans the chunks/ruler/admin buckets —
// the actual bucket names, NOT the key label (an earlier revision minted against
// the nonexistent "platform-loki-<region>" bucket). Pure — unit-tested.
func BuildRotationTable(prefix, region, objCluster string) []CredEntry {
	return []CredEntry{
		{
			Name: "loki-object-store", Kind: CredKindObjKey, Label: prefix + "-loki-" + region,
			ObjCluster: objCluster,
			Buckets: []string{
				prefix + "-loki-chunks-" + region,
				prefix + "-loki-ruler-" + region,
				prefix + "-loki-admin-" + region,
			},
			Permissions: "read_write",
			BaoPath:     "secret/loki/object-store", PresentField: "AWS_ACCESS_KEY_ID",
			Fields: func(access, secret string) map[string]string { return lokiObjectStoreFields(access, secret) },
		},
		{
			Name: "harbor-registry-s3", Kind: CredKindObjKey, Label: prefix + "-harbor-registry-" + region,
			ObjCluster:  objCluster,
			Buckets:     []string{prefix + "-harbor-registry-" + region},
			Permissions: "read_write",
			BaoPath:     "secret/harbor/registry-s3", PresentField: "access_key_id",
			Fields: func(access, secret string) map[string]string {
				return HarborRegistryS3Fields(prefix, region, objCluster, access, secret)
			},
		},
		{
			// The BROAD platform object-storage key for MANAGED App Platform. The
			// platform obj settings (env/settings/obj.yaml `AplObjectStorage`,
			// provider.type=linode) use ONE key for ALL app buckets, unlike the two
			// per-app keys above. Scope it to every provisioned bucket so apl-core's
			// obj-consuming apps (loki, harbor, …) all authenticate with it. Seeded at
			// secret/obj/platform; the obj-secrets ExternalSecret projects the secret
			// half into apl-secrets/obj-secrets (property provider_linode_secretAccessKey),
			// and the reconciler fills obj.yaml.accessKeyId from AWS_ACCESS_KEY_ID.
			Name: "obj-platform", Kind: CredKindObjKey, Label: prefix + "-obj-" + region,
			ObjCluster: objCluster,
			Buckets: []string{
				prefix + "-loki-chunks-" + region,
				prefix + "-loki-ruler-" + region,
				prefix + "-loki-admin-" + region,
				prefix + "-harbor-registry-" + region,
			},
			Permissions: "read_write",
			BaoPath:     "secret/obj/platform", PresentField: "AWS_ACCESS_KEY_ID",
			Fields: func(access, secret string) map[string]string { return objPlatformFields(access, secret) },
		},
	}
}

// objPlatformFields is the secret/obj/platform field set — the broad managed
// object-storage key. AWS_ACCESS_KEY_ID is the (non-sensitive) key id the reconciler
// writes into env/settings/obj.yaml plaintext; AWS_SECRET_ACCESS_KEY is projected into
// apl-secrets/obj-secrets by the obj-secrets ExternalSecret (never committed to git).
func objPlatformFields(access, secret string) map[string]string {
	return map[string]string{"AWS_ACCESS_KEY_ID": access, "AWS_SECRET_ACCESS_KEY": secret}
}

// lokiObjectStoreFields is the secret/loki/object-store field set (the env-var
// names the Loki singleBinary pod reads, matching the loki ExternalSecret).
func lokiObjectStoreFields(access, secret string) map[string]string {
	return map[string]string{"AWS_ACCESS_KEY_ID": access, "AWS_SECRET_ACCESS_KEY": secret}
}

// HarborRegistryS3Fields derives the five secret/harbor/registry-s3 fields.
// The bucket name encodes the deployment region (matches the bucket resource
// label); endpoint/region come from the obj_cluster the object-storage tfvars
// actually provisioned into — NOT guessed from the env name. Lives here (not
// ci_seed_special.go) because the rotation table owns this path now: both the
// bootstrap mint and the rotator write the same complete field set.
func HarborRegistryS3Fields(prefix, region, objCluster, accessKey, secretKey string) map[string]string {
	return map[string]string{
		"access_key_id":     accessKey,
		"secret_access_key": secretKey,
		"bucket_name":       prefix + "-harbor-registry-" + region,
		"endpoint":          "https://" + objCluster + ".linodeobjects.com",
		"region":            objCluster,
	}
}

// IsDue reports whether a credential whose OpenBao secret carries rotatedAt
// (epoch seconds; "" or unparseable on a fresh bootstrap seed) is due at now.
// An unrecognized/absent stamp is treated as due so the rotator adopts a
// bootstrap-seeded secret on its first run. Pure — unit-tested.
func IsDue(rotatedAt string, now time.Time, rotateAfterDays int) bool {
	ts, err := strconv.ParseInt(strings.TrimSpace(rotatedAt), 10, 64)
	if err != nil {
		return true
	}
	return now.Unix()-ts >= int64(rotateAfterDays)*linode.DaySecs
}

// IDsToDrain returns the resource ids to revoke — every id except the keepNewest
// highest (Linode ids increase monotonically, so highest == newest). keepNewest
// is floored at 1 so the live credential is never drained. Pure — unit-tested.
func IDsToDrain(ids []uint64, keepNewest int) []uint64 {
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

func RunRotateLinodeCreds(ctx context.Context, apply bool) error {
	region := os.Getenv("REGION")
	if region == "" {
		return fmt.Errorf("REGION must be set")
	}
	objCluster := os.Getenv("OBJ_CLUSTER")
	minting := linode.InClusterLinodeToken()
	if minting == "" {
		return fmt.Errorf("LINODE_TOKEN must be set (the in-cluster Linode token used to mint replacements)")
	}
	rotateAfter := int(cli.EnvInt("ROTATE_AFTER_DAYS", 80))
	keepNewest := int(cli.EnvInt("KEEP_NEWEST", 2))

	// In-cluster: there is no instance repo here, so the prefix is rendered into
	// the reconciler Deployment by `llz render` (RenderReconcilerEnvPatch). No
	// default — matching keys under the wrong prefix rotates ANOTHER instance's
	// credentials, which is silent and worse than stopping.
	prefix := os.Getenv("OBJ_LABEL_PREFIX")
	if prefix == "" {
		return fmt.Errorf("OBJ_LABEL_PREFIX must be set (the instance's Object Storage label prefix). " +
			"`llz render` writes it into the llz-reconciler Deployment; re-render and re-apply the reconciler if it is missing")
	}
	table := BuildRotationTable(prefix, region, objCluster)
	for _, e := range table {
		if e.Kind == CredKindObjKey && objCluster == "" {
			return fmt.Errorf("OBJ_CLUSTER must be set to rotate object-storage keys (e.g. %s)", e.Name)
		}
	}

	lc := NewLinodeClient(minting)
	now := Now()

	// OpenBao login is deferred until at least one credential is due, so a no-op
	// run (nothing due) does not require OpenBao to be reachable.
	var bao openbao.BaoStore
	ensureBao := func() error {
		if bao != nil {
			return nil
		}
		b, err := NewBaoStore(ctx)
		bao = b
		return err
	}

	var rotated, skipped []string
	for _, e := range table {
		if err := ensureBao(); err != nil {
			return err
		}
		rotatedAt, _, err := bao.Get(ctx, e.BaoPath, "rotated_at")
		if err != nil {
			return fmt.Errorf("read %s rotated_at: %w", e.BaoPath, err)
		}
		if !IsDue(rotatedAt, now, rotateAfter) {
			skipped = append(skipped, e.Name)
			fmt.Printf("%s: not due (rotated_at=%s, threshold %dd)\n", e.Name, rotatedAt, rotateAfter)
			continue
		}
		if !apply {
			fmt.Printf("%s: DUE — would rotate (dry-run)\n", e.Name)
			rotated = append(rotated, e.Name+" (dry-run)")
			continue
		}
		if err := rotateOne(ctx, lc, bao, e, now, keepNewest); err != nil {
			return fmt.Errorf("rotate %s: %w", e.Name, err)
		}
		fmt.Printf("%s: rotated → %s\n", e.Name, e.BaoPath)
		rotated = append(rotated, e.Name)
	}

	fmt.Printf("rotate-linode-creds: rotated=%v skipped=%v\n", rotated, skipped)
	return nil
}

// rotateOne mints, verifies, writes, and drains a single credential. The order
// is load-bearing: nothing old is revoked until the new credential is verified
// (PAT) and written to OpenBao.
func rotateOne(ctx context.Context, lc LinodeAPI, bao openbao.BaoStore, e CredEntry, now time.Time, keepNewest int) error {
	var fields map[string]string
	switch e.Kind {
	case CredKindPAT:
		expiry := linode.FmtLinodeTS(now.Unix() + patValidityDays*linode.DaySecs)
		m, err := lc.CreateProfileToken(ctx, e.Label, e.Scopes, expiry)
		if err != nil {
			return err
		}
		token := cli.AsString(m["token"])
		if token == "" {
			return fmt.Errorf("mint returned no token")
		}
		// Verify the new token works BEFORE the old one is drained.
		if err := NewLinodeClient(token).Verify(ctx); err != nil {
			return fmt.Errorf("new token failed verification — not draining the old one: %w", err)
		}
		fields = e.Fields(token, "")
	case CredKindObjKey:
		m, err := lc.CreateObjectStorageKeyBuckets(ctx, e.Label, e.ObjCluster, e.Buckets, e.Permissions)
		if err != nil {
			return err
		}
		access, secret := cli.AsString(m["access_key"]), cli.AsString(m["secret_key"])
		if access == "" || secret == "" {
			return fmt.Errorf("mint returned no access_key/secret_key")
		}
		fields = e.Fields(access, secret)
	default:
		return fmt.Errorf("unknown credential kind %q", e.Kind)
	}

	fields["rotated_at"] = strconv.FormatInt(now.Unix(), 10)
	if err := bao.Write(ctx, e.BaoPath, fields); err != nil {
		return fmt.Errorf("write %s: %w", e.BaoPath, err)
	}
	DrainOld(ctx, lc, e, keepNewest)
	return nil
}

// DrainOld revokes older same-labeled resources, keeping the newest keepNewest.
// Best-effort: the new credential is already live + written, so a failed revoke
// is logged but does not fail the run (keep-newest-N converges next run).
func DrainOld(ctx context.Context, lc LinodeAPI, e CredEntry, keepNewest int) {
	var (
		items []map[string]any
		del   func(context.Context, uint64) error
		err   error
	)
	switch e.Kind {
	case CredKindPAT:
		items, err = lc.ListProfileTokens(ctx)
		del = lc.DeleteProfileToken
	case CredKindObjKey:
		items, err = lc.ListObjectStorageKeys(ctx)
		del = lc.DeleteObjectStorageKey
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::%s: list for drain failed (new credential is live; will converge next run): %v\n", e.Name, err)
		return
	}
	for _, id := range IDsToDrain(idsByLabel(items, e.Label), keepNewest) {
		if err := del(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "::warning::%s: revoke id=%d failed (will retry next run): %v\n", e.Name, id, err)
		}
	}
}

// openLinodeRotatorBaoStore logs in to OpenBao via Kubernetes auth (the
// linode-rotator role) using the pod's ServiceAccount token, trusting the
// mounted openbao CA, and returns a write-capable client.
func openLinodeRotatorBaoStore(ctx context.Context) (openbao.BaoStore, error) {
	return OpenBaoStore(ctx, "linode-rotator")
}

// OpenBaoStore logs in to OpenBao via Kubernetes auth for the named role and
// returns a write-capable client.
//
// IT IS A PLAIN VAR OVER shared/openbao NOW, not an installed seam. It used to be
// a func-valued var defaulting to an error, filled in at startup by an init() in
// internal/extensions/openbao that called credrotate.InstallBaoStore -- an
// extension reaching sideways into another extension to wire itself up, and the
// last of this package's four inbound edges.
//
// The indirection existed because the in-cluster login path (the ServiceAccount
// token, the mounted CA, the address and mount defaults) lived in an extension
// that this one must not import. It lives in internal/shared/openbao now, so the
// seam has nothing left to bridge: what this package owns is WHICH ROLE a rotation
// logs in as, and that was always just the argument.
//
// Tests that need to substitute the store still can -- it is still a var.
var OpenBaoStore = openbao.OpenInClusterStore
