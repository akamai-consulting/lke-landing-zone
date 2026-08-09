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
	if err := credrotate.RunMintBootstrapObjkeys("primary"); err == nil {
		t.Error("missing obj_cluster must hard-fail")
	}
	writeTFVars(t, dir, "object-storage", "primary", `obj_cluster = "us-ord-1"`)

	// Fresh bootstrap: all objkey paths absent → three mints + three seeds carrying
	// the complete field sets + rotated_at (loki + harbor + the consolidated
	// obj-platform key); the DNS PAT entry is never minted here.
	puts := stubBaoSeedKV(t, "", "") // every `kv get` reports absent
	if err := credrotate.RunMintBootstrapObjkeys("primary"); err != nil {
		t.Fatal(err)
	}
	if stub.objCreates != 3 {
		t.Fatalf("objkey mints = %d, want 3 (loki + harbor + platform-obj; never the DNS PAT)", stub.objCreates)
	}
	if stub.patCreates != 0 {
		t.Errorf("PAT mints = %d, want 0", stub.patCreates)
	}
	if len(*puts) != 3 {
		t.Fatalf("want three kv puts, got %d: %v", len(*puts), *puts)
	}
	rotatedAt := strconv.FormatInt(fixedNow.Unix(), 10)
	wantPuts := []string{
		"kv put secret/loki/object-store AWS_ACCESS_KEY_ID=AK AWS_SECRET_ACCESS_KEY=SK rotated_at=" + rotatedAt,
		"kv put secret/harbor/registry-s3 access_key_id=AK bucket_name=acme-harbor-registry-primary " +
			"endpoint=https://us-ord-1.linodeobjects.com region=us-ord-1 rotated_at=" + rotatedAt + " secret_access_key=SK",
		"kv put secret/obj/platform AWS_ACCESS_KEY_ID=AK AWS_SECRET_ACCESS_KEY=SK rotated_at=" + rotatedAt,
	}
	for i, want := range wantPuts {
		if got := strings.Join((*puts)[i], " "); got != want {
			t.Errorf("kv put %d:\n got %q\nwant %q", i, got, want)
		}
	}

	// Idempotency: already-seeded paths (presentField has a value) → no mint,
	// no put — a rotator-minted key is never clobbered.
	stub.objCreates = 0
	var putsAfterSkip [][]string
	withBaoExec(t, func(_, _, _ string, args ...string) (string, string, error) {
		if strings.HasPrefix(strings.Join(args, " "), "kv get") {
			return "present\n", "", nil // every probe finds a value
		}
		putsAfterSkip = append(putsAfterSkip, args)
		return "", "", nil
	})
	if err := credrotate.RunMintBootstrapObjkeys("primary"); err != nil {
		t.Fatal(err)
	}
	if stub.objCreates != 0 || len(putsAfterSkip) != 0 {
		t.Errorf("seeded paths must skip: mints=%d puts=%v", stub.objCreates, putsAfterSkip)
	}

	if err := credrotate.RunMintBootstrapObjkeys(""); err == nil {
		t.Error("missing --region must error")
	}
	t.Setenv("LINODE_API_TOKEN", "")
	if err := credrotate.RunMintBootstrapObjkeys("primary"); err == nil || !strings.Contains(err.Error(), "LINODE_API_TOKEN") {
		t.Errorf("err = %v, want missing-token refusal", err)
	}
}

// ── resolve-harbor-url ────────────────────────────────────────────────────────
