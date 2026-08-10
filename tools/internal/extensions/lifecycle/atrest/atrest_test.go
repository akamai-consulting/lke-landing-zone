package atrest

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// writeTFRoot lays out a Terraform tree under the path atRestScanDirs resolves.
func writeTFRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, "tools", "internal", "shared", "tfroots", "roots", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The rule that has teeth for the next root someone adds. `llz env add` scaffolds
// Terraform roots, so a fifth root is routine — and one without encryption.tf
// writes kubeconfig_raw and every provider-computed password to the bucket in
// plaintext while passing tf-validate, tflint and checkov, none of which have an
// opinion about a block that is simply absent.
func TestAtRestGuardFailsOnRootWithNoEncryptionBlock(t *testing.T) {
	root := writeTFRoot(t, map[string]string{
		"newroot/backend.tf": "terraform {\n  backend \"s3\" {}\n}\n",
	})
	err := Run(io.Discard, root)
	if err == nil {
		t.Fatal("a backend with no encryption block must fail")
	}
	if !strings.Contains(err.Error(), "unencrypted resource") {
		t.Errorf("unexpected error: %v", err)
	}
}

// The same root WITH the block passes — and its unencrypted fallback is the
// registrable half, so a root that has one and is not in atRestAllowed still
// fails, just with the other remedy.
func TestAtRestGuardAcceptsAnEncryptedRoot(t *testing.T) {
	root := writeTFRoot(t, map[string]string{
		"newroot/backend.tf":    "terraform {\n  backend \"s3\" {}\n}\n",
		"newroot/encryption.tf": "terraform {\n  encryption {\n    state {\n      method = method.aes_gcm.llz\n    }\n  }\n}\n",
	})
	// Every real registry entry is now stale against this synthetic tree, which is
	// its own (correct) failure — assert on the findings instead.
	findings, examined, err := collectAtRestFindings(atRestRepo(root), ScanDirs(atRestRepo(root)))
	if err != nil {
		t.Fatal(err)
	}
	if examined != 2 {
		t.Fatalf("examined %d files, want 2", examined)
	}
	if len(findings) != 0 {
		t.Errorf("an encrypted root with no unencrypted fallback has nothing to report, got %+v", findings)
	}
}

// The node-pool lever, which is what stands between the platform and every image
// layer, emptyDir and kubelet-projected Secret sitting unencrypted on a node disk.
func TestAtRestGuardFailsOnNodePoolWithoutDiskEncryption(t *testing.T) {
	findings := scanResourceLevers(
		"resource \"linode_lke_node_pool\" \"this\" {\n  type = \"g6\"\n}\n", "roots/cluster/main.tf")
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	if findings[0].registrable {
		t.Error("a missing disk_encryption must NOT be registrable — there is no reason for it, only an unmade decision")
	}
	if !strings.Contains(findings[0].what, "disk_encryption") {
		t.Errorf("the finding must name the argument: %q", findings[0].what)
	}
}

func TestAtRestGuardAcceptsAnEncryptedNodePool(t *testing.T) {
	if f := scanResourceLevers(
		"resource \"linode_lke_node_pool\" \"this\" {\n  disk_encryption = \"enabled\"\n}\n",
		"roots/cluster/main.tf"); len(f) != 0 {
		t.Errorf("an encrypted node pool has nothing to report, got %+v", f)
	}
}

// Brace-counted rather than whole-file matched: a later resource's argument must
// not vouch for an earlier one. A whole-file regex passes this input, which is
// exactly the false green that would leave a pool unencrypted in a multi-resource
// root.
func TestAtRestGuardDoesNotLetOneResourceVouchForAnother(t *testing.T) {
	body := "resource \"linode_lke_node_pool\" \"unencrypted\" {\n  type = \"g6\"\n}\n\n" +
		"resource \"linode_lke_node_pool\" \"encrypted\" {\n  disk_encryption = \"enabled\"\n}\n"
	findings := scanResourceLevers(body, "roots/cluster/main.tf")
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want exactly the unencrypted one: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].key, "unencrypted") {
		t.Errorf("the wrong resource was reported: %s", findings[0].key)
	}
}

// A directly-declared Volume spells the same decision with a different argument.
// The CSI-provisioned ones are covered at runtime by assert-volume-encryption
// against the Linode API; nothing covered one declared in HCL.
func TestAtRestGuardCoversDeclaredVolumes(t *testing.T) {
	if f := scanResourceLevers("resource \"linode_volume\" \"data\" {\n  size = 20\n}\n",
		"modules/x/main.tf"); len(f) != 1 || !strings.Contains(f[0].what, "encryption") {
		t.Errorf("a linode_volume with no encryption must be reported, got %+v", f)
	}
}

// A guard that walked nothing prints the same green as one that walked everything.
func TestAtRestGuardFailsOnEmptyCorpus(t *testing.T) {
	err := Run(io.Discard, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "examined 0") {
		t.Fatalf("an empty corpus must fail closed, got %v", err)
	}
}

// Every registered residue must name an exit condition. An accepted residue with
// no test for retiring it is a permanent one wearing a temporary label — which is
// precisely what ADR 0007 (state encryption)'s phase 2 became while it lived as a comment repeated in
// four files.
func TestAtRestAllowedEntriesCarryAnExitCondition(t *testing.T) {
	for k, r := range atRestAllowed {
		if len(strings.TrimSpace(r.exit)) < 30 {
			t.Errorf("%s: exit condition is too short to be a test: %q", k, r.exit)
		}
		if len(strings.TrimSpace(r.reason)) < 40 {
			t.Errorf("%s: reason is too short to be a reason: %q", k, r.reason)
		}
	}
}

// The live tree must be green, and green because it was read.
func TestAtRestGuardPassesOnThisRepo(t *testing.T) {
	if err := Run(io.Discard, "../../../../.."); err != nil {
		t.Fatalf("at-rest-guard must be green on this repo: %v", err)
	}
}

// A comment containing an unbalanced brace must not run the depth counter past
// the end of its resource. When it did, every LATER resource in the file was
// swallowed and then skipped by the outer loop — a silent FALSE NEGATIVE on a
// security gate, which reports green having never looked.
//
// The trigger is not exotic: Terraform in this repo is heavily commented with
// backticked code fragments, and `dynamic {` in prose is enough.
func TestAtRestGuardIsNotBlindedByBracesInComments(t *testing.T) {
	body := "resource \"linode_lke_node_pool\" \"pool\" {\n" +
		"  # the API rejects a bare `{` here — see the note about `dynamic {`\n" +
		"  disk_encryption = \"enabled\"\n" +
		"}\n\n" +
		"resource \"linode_volume\" \"data\" {\n" +
		"  size = 20\n" +
		"}\n"
	f := scanResourceLevers(body, "x.tf")
	if len(f) != 1 {
		t.Fatalf("got %d findings, want 1 (the unencrypted volume AFTER the comment): %+v", len(f), f)
	}
	if !strings.Contains(f[0].key, "linode_volume.data") {
		t.Errorf("the resource after the commented brace was not scanned: %s", f[0].key)
	}
}

// The same failure spelled with a block comment, and with a brace inside a
// STRING — `label = "a{b"` is not an opening block.
func TestStripHCLNoiseRemovesCommentsAndStrings(t *testing.T) {
	lines := []string{
		`resource "x" "y" {`,
		`  label = "a{b#c"`,
		`  /* block { comment`,
		`     still inside { */ size = 1`,
		`  // trailing { comment`,
		`}`,
	}
	noise := stripHCLNoise(lines)
	depth := 0
	for _, l := range noise {
		depth += braceDelta(l)
	}
	if depth != 0 {
		t.Errorf("depth = %d, want 0 — braces in comments and strings must not count:\n%v", depth, noise)
	}
}

// A block comment must be tracked ACROSS lines, so braces in its continuation
// lines contribute nothing. Asserted as "the same content with the comment
// removed counts identically" rather than as a fixed depth: an unclosed `/*`
// genuinely does comment out everything after it, including a closing brace, so
// pinning depth==0 there would assert something untrue about HCL.
func TestStripHCLNoiseTracksBlockCommentsAcrossLines(t *testing.T) {
	withComment := []string{
		`resource "x" "y" {`,
		`  /* a multi-line note {{{`,
		`     that keeps going }}} and }}} */`,
		`  size = 1`,
		`}`,
	}
	without := []string{`resource "x" "y" {`, `  size = 1`, `}`}
	sum := func(lines []string) int {
		d := 0
		for _, l := range stripHCLNoise(lines) {
			d += braceDelta(l)
		}
		return d
	}
	if got, want := sum(withComment), sum(without); got != want {
		t.Errorf("depth with comment = %d, without = %d — a block comment must contribute nothing", got, want)
	}
	if sum(without) != 0 {
		t.Fatalf("control case is wrong: a balanced resource must net 0, got %d", sum(without))
	}
}

// A bucket holds data at rest and has NO argument to encrypt it, so it must be
// REPORTED (silence read as approval — four buckets carrying every image layer
// and every log line were simply never looked at) and it must be REGISTRABLE
// (there is nothing to set, so a non-registrable finding would be a gate nobody
// could ever pass).
func TestAtRestGuardReportsObjectStorageBucketAsRegistrable(t *testing.T) {
	findings := scanResourceLevers(
		"resource \"linode_object_storage_bucket\" \"loki_chunks\" {\n  region = \"us-ord\"\n}\n",
		"terraform-modules/llz-object-storage/main.tf")
	if len(findings) != 1 {
		t.Fatalf("a bucket must produce exactly one finding, got %+v", findings)
	}
	if !findings[0].registrable {
		t.Error("a bucket finding must be registrable — there is no lever to set, so a " +
			"non-registrable finding would be unsatisfiable")
	}
	want := "terraform-modules/llz-object-storage/main.tf:linode_object_storage_bucket.loki_chunks"
	if findings[0].key != want {
		t.Errorf("key = %q, want %q", findings[0].key, want)
	}
}

// The no-lever branch must not swallow the levered ones: an unencrypted volume
// declared after a bucket in the same file still has to fail. The first revision
// skipped the rest of the resource body after a no-lever match, which would have
// hidden exactly this.
func TestAtRestGuardStillSeesLeveredResourcesAfterABucket(t *testing.T) {
	body := "resource \"linode_object_storage_bucket\" \"b\" {\n  region = \"us-ord\"\n}\n\n" +
		"resource \"linode_volume\" \"v\" {\n  size = 20\n}\n"
	findings := scanResourceLevers(body, "terraform-modules/llz-object-storage/main.tf")
	if len(findings) != 2 {
		t.Fatalf("want a finding for each resource, got %+v", findings)
	}
	if findings[1].registrable {
		t.Error("an unencrypted linode_volume must stay NON-registrable — it has a lever")
	}
}

// Every bucket in the real tree must be registered. This is the assertion that
// makes the probe result durable: if someone adds a fifth bucket, the guard fails
// until they say what lands in it and what would retire the entry.
func TestEveryObjectStorageBucketIsRegistered(t *testing.T) {
	root := "../../../../.." // tools/cmd/llz -> repo root, as TestAtRestGuardPassesOnThisRepo
	findings, _, err := collectAtRestFindings(atRestRepo(root), ScanDirs(atRestRepo(root)))
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range findings {
		if !strings.Contains(f.key, "linode_object_storage_bucket.") {
			continue
		}
		seen++
		rule, ok := atRestAllowed[f.key]
		if !ok {
			t.Errorf("bucket %s is not registered in atRestAllowed", f.key)
			continue
		}
		if rule.exit == "" {
			t.Errorf("bucket %s is registered with no exit condition", f.key)
		}
	}
	if seen == 0 {
		t.Fatal("scanned no buckets at all — the guard would report green over them")
	}
}

// atRestRepo is the reader a real scan gets, fenced to a fixture tree, built from
// the extension's own invariant binding.
func atRestRepo(root string) capability.Repo {
	return capability.RepoAt(atRestBinding(), root)
}
