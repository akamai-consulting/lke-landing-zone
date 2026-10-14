package cli

// mint_bootstrap_objkeys_test.go — did NOT follow its command, same debt as
// credentials_cobra_test.go beside it.
//
// It depends on package main's bao/objkey stubs, and internal/credrotate's
// same-named stubs behave differently — moving it made an "already seeded paths
// are skipped" assertion fail, because the two fixtures disagree about what is
// already seeded. Reconciling them is a real change to what the existing
// credrotate tests assert, not a mechanical one.

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
)

// The mint-bootstrap-objkeys test, returned to package main.
//
// It travelled inside ci_seed_special_test.go and its subject is
// MintBootstrapObjkeys, which has nothing to do with resolve-harbor-url
// or the PVC storage-class audit. Filename-as-subject, eleventh occurrence — and
// this one FAILED on arrival rather than passing quietly, because the fixtures it
// needs are wired in package main.

// mintObjkeysFixture is TestRunCIMintBootstrapObjkeys's setup, extracted so the
// grant-verification cases below start from the same instance, stub and clock
// rather than a second fixture that could disagree with it — which is precisely
// the debt this file's own header records.
//
// The bao stub reports every path SEEDED, because that is the state all of those
// cases are about; puts are captured so "did it overwrite the path" is checkable.
func mintObjkeysFixture(t *testing.T) (*stubLinode, *[][]string) {
	t.Helper()
	dir := chdirTempDir(t)
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("LINODE_API_TOKEN", "linode-tok")
	withGHASummaryFile(t)

	prevNow := credrotate.Now
	credrotate.Now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	t.Cleanup(func() { credrotate.Now = prevNow })

	stub := &stubLinode{}
	prevClient := credrotate.MintObjkeysLinodeClient
	credrotate.MintObjkeysLinodeClient = func(string) credrotate.LinodeAPI { return stub }
	t.Cleanup(func() { credrotate.MintObjkeysLinodeClient = prevClient })

	mustWrite(t, filepath.Join(dir, "landingzone.yaml"),
		"apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: acme\nspec:\n  instance:\n    repo: o/acme\n")
	writeTFVars(t, dir, "object-storage", "primary", `obj_cluster = "us-ord-1"`)

	puts := &[][]string{}
	withBaoExec(t, func(_, _, _ string, args ...string) (string, string, error) {
		if strings.HasPrefix(strings.Join(args, " "), "kv get") {
			return "present\n", "", nil
		}
		*puts = append(*puts, args)
		return "", "", nil
	})
	return stub, puts
}

func TestRunCIMintBootstrapObjkeys(t *testing.T) {
	dir := chdirTempDir(t)
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("LINODE_API_TOKEN", "linode-tok")
	withGHASummaryFile(t)

	fixedNow := time.Unix(1_700_000_000, 0)
	prevNow := credrotate.Now
	credrotate.Now = func() time.Time { return fixedNow }
	t.Cleanup(func() { credrotate.Now = prevNow })

	stub := &stubLinode{}
	prevClient := credrotate.MintObjkeysLinodeClient
	credrotate.MintObjkeysLinodeClient = func(string) credrotate.LinodeAPI { return stub }
	t.Cleanup(func() { credrotate.MintObjkeysLinodeClient = prevClient })

	// mint runs in CI inside the instance checkout, so the label prefix comes from
	// the committed spec (the in-cluster rotator gets OBJ_LABEL_PREFIX instead).
	mustWrite(t, filepath.Join(dir, "landingzone.yaml"),
		"apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: acme\nspec:\n  instance:\n    repo: o/acme\n")

	// obj_cluster unresolvable → hard error, no mint.
	if err := credrotate.RunMintBootstrapObjkeys("primary", false); err == nil {
		t.Error("missing obj_cluster must hard-fail")
	}
	writeTFVars(t, dir, "object-storage", "primary", `obj_cluster = "us-ord-1"`)

	// Fresh bootstrap: the objkey path absent → ONE mint and one seed carrying the
	// complete field set + rotated_at; the DNS PAT entry is never minted here.
	//
	// ONE, NOT THREE. This used to assert loki + harbor + platform-obj. The two
	// per-app keys were read_write credentials written to OpenBao paths whose
	// ExternalSecrets 52465691 deleted — so every bootstrap minted two keys no
	// cluster ever read, and this test pinned that as correct. The consolidated
	// obj-platform key is what apl-core actually consumes, via obj-secrets.
	puts := stubBaoSeedKV(t, "", "") // every `kv get` reports absent
	if err := credrotate.RunMintBootstrapObjkeys("primary", false); err != nil {
		t.Fatal(err)
	}
	if stub.objCreates != 1 {
		t.Fatalf("objkey mints = %d, want 1 (platform-obj only; never the DNS PAT, never the "+
			"retired per-app loki/harbor keys)", stub.objCreates)
	}
	if stub.patCreates != 0 {
		t.Errorf("PAT mints = %d, want 0", stub.patCreates)
	}
	if len(*puts) != 1 {
		t.Fatalf("want one kv put, got %d: %v", len(*puts), *puts)
	}
	rotatedAt := strconv.FormatInt(fixedNow.Unix(), 10)
	wantPuts := []string{
		"kv put secret/obj/platform AWS_ACCESS_KEY_ID=AK AWS_SECRET_ACCESS_KEY=SK rotated_at=" + rotatedAt,
	}
	for i, want := range wantPuts {
		if got := strings.Join((*puts)[i], " "); got != want {
			t.Errorf("kv put %d:\n got %q\nwant %q", i, got, want)
		}
	}

	// Idempotency: already-seeded paths (presentField has a value) → no mint,
	// no put — a rotator-minted key is never clobbered. The seeded key must also
	// still GRANT the buckets, so the account has to contain it: an unlimited
	// key (nil bucket_access) is the older module-minted shape and writes
	// anywhere, which keeps this case about idempotency rather than about grants.
	stub.objkeys = []map[string]any{{"id": jn(300), "access_key": "present", "label": "seeded"}}
	stub.objCreates = 0
	var putsAfterSkip [][]string
	withBaoExec(t, func(_, _, _ string, args ...string) (string, string, error) {
		if strings.HasPrefix(strings.Join(args, " "), "kv get") {
			return "present\n", "", nil // every probe finds a value
		}
		putsAfterSkip = append(putsAfterSkip, args)
		return "", "", nil
	})
	if err := credrotate.RunMintBootstrapObjkeys("primary", false); err != nil {
		t.Fatal(err)
	}
	if stub.objCreates != 0 || len(putsAfterSkip) != 0 {
		t.Errorf("seeded paths must skip: mints=%d puts=%v", stub.objCreates, putsAfterSkip)
	}

	if err := credrotate.RunMintBootstrapObjkeys("", false); err == nil {
		t.Error("missing --region must error")
	}
	t.Setenv("LINODE_API_TOKEN", "")
	if err := credrotate.RunMintBootstrapObjkeys("primary", false); err == nil || !strings.Contains(err.Error(), "LINODE_API_TOKEN") {
		t.Errorf("err = %v, want missing-token refusal", err)
	}
}

// ── resolve-harbor-url ────────────────────────────────────────────────────────

// THE STATE THAT MADE A 42-DAY OUTAGE PERMANENT. The OpenBao path is seeded, so
// the old presence-only check skipped — forever, on every subsequent bootstrap —
// while the key it named could not write a single one of this deployment's
// buckets. Nothing in the repair path could reach it, because the seed is what
// caused the skip.
func TestASeededKeyThatCannotWriteTheBucketsIsNotSilentlySkipped(t *testing.T) {
	stub, _ := mintObjkeysFixture(t)
	// Seeded, present on the account, and scoped to somebody else's buckets —
	// exactly what a key minted under a different objLabelPrefix looks like.
	stub.objkeys = []map[string]any{{
		"id": jn(300), "access_key": "present", "label": "stale",
		"bucket_access": []any{
			map[string]any{"bucket_name": "platform-loki-chunks-primary", "permissions": "read_write"},
		},
	}}
	err := credrotate.RunMintBootstrapObjkeys("primary", false)
	if err == nil {
		t.Fatal("a seeded key that cannot write the deployment's buckets was skipped silently — " +
			"this is the shape that ran 42 days with both consumers 403ing on every write")
	}
	for _, want := range []string{"--reseed", "assert-obj-roundtrip", "platform-loki-chunks-primary"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q — it must name the remedy and what the key "+
				"IS scoped to, or the reader cannot see the prefix mismatch:\n%v", want, err)
		}
	}
	if stub.objCreates != 0 {
		t.Errorf("minted %d key(s) without --reseed — overwriting a live credential is opt-in",
			stub.objCreates)
	}
}

// --reseed is the repair the failure names. Without it the only way out was
// hand-deleting OpenBao paths with the root token.
func TestReseedReplacesAKeyThatCannotWriteTheBuckets(t *testing.T) {
	stub, puts := mintObjkeysFixture(t)
	stub.objkeys = []map[string]any{{
		"id": jn(300), "access_key": "present", "label": "stale",
		"bucket_access": []any{
			map[string]any{"bucket_name": "somebody-elses-bucket", "permissions": "read_write"},
		},
	}}
	if err := credrotate.RunMintBootstrapObjkeys("primary", true); err != nil {
		t.Fatalf("--reseed must repair the path, got %v", err)
	}
	if stub.objCreates != 1 {
		t.Errorf("objkey mints = %d, want 1", stub.objCreates)
	}
	if len(*puts) != 1 {
		t.Errorf("want the OpenBao path overwritten, got %d put(s)", len(*puts))
	}
}

// READ-ONLY IS NOT ENOUGH, and a grant check that only asked "is the bucket
// listed" would pass this. Every consumer here writes; a read_only grant 403s on
// the first PutObject exactly like no grant at all.
func TestAReadOnlyGrantIsTreatedAsNoGrant(t *testing.T) {
	stub, _ := mintObjkeysFixture(t)
	stub.objkeys = []map[string]any{{
		"id": jn(300), "access_key": "present", "label": "readonly",
		"bucket_access": []any{
			map[string]any{"bucket_name": "platform-loki-chunks-primary", "permissions": "read_only"},
		},
	}}
	if err := credrotate.RunMintBootstrapObjkeys("primary", false); err == nil {
		t.Error("a read_only grant was accepted as write access")
	}
}

// FAIL-OPEN ONLY HERE. If Linode cannot be asked, preserve the historical skip:
// failing a bootstrap on an API blip is a worse trade than missing a mismatch
// this run, and only positive evidence should act.
func TestAnUnreachableLinodeAPIStillSkipsASeededPath(t *testing.T) {
	stub, puts := mintObjkeysFixture(t)
	stub.listErr = errors.New("connection reset")
	if err := credrotate.RunMintBootstrapObjkeys("primary", false); err != nil {
		t.Fatalf("an unverifiable grant must skip as before, not fail: %v", err)
	}
	if stub.objCreates != 0 || len(*puts) != 0 {
		t.Errorf("mints=%d puts=%d — an unverified path must not be reseeded",
			stub.objCreates, len(*puts))
	}
}
