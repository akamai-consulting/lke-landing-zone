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
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
)

// MintObjkeysLinodeClient is a seam for tests.
var MintObjkeysLinodeClient = func(token string) LinodeAPI {
	return capability.CloudFor(objKeyCloudBinding()).Client(token, 30*time.Second)
}

func RunMintBootstrapObjkeys(region string, reseed bool) error {
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
	prefix, err := clusterspec.LabelPrefixFor("mint-bootstrap-objkeys")
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
			grants, why, known := seededKeyGrants(ctx, lc, seeded, e)
			switch {
			case !known:
				// Could not ask Linode. Preserve the historical behaviour exactly —
				// skip — because the alternative is failing a bootstrap on an API
				// blip, and the seeded key is probably fine. Only positive evidence
				// of a mismatch is acted on below.
				fmt.Printf("%s: %s already seeded (grant not verified: %s) — skipping mint.\n",
					e.Name, e.BaoPath, why)
				continue
			case grants:
				fmt.Printf("%s: %s already seeded and its key still grants the buckets — skipping mint.\n",
					e.Name, e.BaoPath)
				continue
			case !reseed:
				return fmt.Errorf("%s: %s is seeded but the key it names cannot write this "+
					"deployment's buckets — %s.\n"+
					"This is NOT a state a re-run repairs: the seeded path is what makes this "+
					"command skip, so every subsequent bootstrap skips it too and the consumer "+
					"stays unable to write.\n"+
					"Confirm with `llz ci assert-obj-roundtrip`, then re-run with --reseed to "+
					"mint a replacement scoped to the current buckets and overwrite %s",
					e.Name, e.BaoPath, why, e.BaoPath)
			default:
				fmt.Printf("::warning::%s: reseeding %s — the seeded key %s.\n", e.Name, e.BaoPath, why)
			}
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

// seededKeyGrants asks Linode whether the ALREADY-SEEDED key can still write the
// buckets this deployment uses.
//
// WHY PRESENCE WAS NEVER THE RIGHT QUESTION. The skip above exists so a
// re-bootstrap does not clobber a rotator-minted key, and that reasoning is
// sound. But it asks only "does the OpenBao path hold a value", and a seeded
// value is not a working one. Linode scopes an object-storage key to named
// buckets AT CREATE TIME and the grant is not implied by the key existing, so a
// key minted under one label prefix grants nothing on buckets created under
// another — and objlabels.go records exactly that migration ("it used to be the
// shared constant platform"). The result is a credential that authenticates and
// then 403s on every write, invisible to this command forever, because the thing
// that makes it skip is the thing that is wrong.
//
// Observed: 42 days of 403 AccessDenied on both consumers, a Loki chunks bucket
// with zero objects, and a Harbor registry that had never stored a layer.
//
// known=false means Linode could not be asked. That is deliberately NOT a
// finding: the caller preserves the old skip, because failing a bootstrap on an
// API blip is a worse trade than missing a mismatch this run. Only positive
// evidence acts.
func seededKeyGrants(ctx context.Context, lc LinodeAPI, accessKey string, e CredEntry) (grants bool, why string, known bool) {
	keys, err := lc.ListObjectStorageKeys(ctx)
	if err != nil {
		return false, fmt.Sprintf("could not list object-storage keys (%v)", err), false
	}
	var found map[string]any
	for _, k := range keys {
		if cli.AsString(k["access_key"]) == accessKey {
			found = k
			break
		}
	}
	if found == nil {
		// The listing SUCCEEDED and the key is not in it: OpenBao names a
		// credential the account no longer has. That is a finding, not an unknown.
		return false, "names access key " + accessKey + ", which no longer exists on this account", true
	}
	// A nil bucket_access is an unlimited key — it can write any bucket, which is
	// what the older module-minted keys looked like. Not the shape this command
	// mints, but not broken either.
	raw, ok := found["bucket_access"]
	if !ok || raw == nil {
		return true, "", true
	}
	access, ok := raw.([]any)
	if !ok {
		return false, "has a bucket_access this check cannot read", false
	}
	granted := map[string]bool{}
	for _, a := range access {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		// read_only is not enough: every consumer here WRITES. A read_only grant
		// passes a "the bucket is listed" check and 403s on the first PutObject.
		if perm := cli.AsString(m["permissions"]); !strings.Contains(perm, "write") {
			continue
		}
		granted[cli.AsString(m["bucket_name"])] = true
	}
	var missing []string
	for _, b := range e.Buckets {
		if !granted[b] {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 {
		return false, fmt.Sprintf("grants no write access on %s (key %s is scoped to %s)",
			strings.Join(missing, ", "), accessKey, grantedList(granted)), true
	}
	return true, "", true
}

// grantedList renders what the key CAN write, sorted, so a prefix mismatch is
// legible at a glance rather than inferred from what is missing.
func grantedList(granted map[string]bool) string {
	if len(granted) == 0 {
		return "nothing"
	}
	var out []string
	for b := range granted {
		out = append(out, b)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
