package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTFRoot lays out a Terraform tree under the path atRestScanDirs resolves.
func writeTFRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, "tools", "internal", "tfroots", "roots", name)
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
	err := runCIAtRestGuard(root)
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
	findings, examined, err := collectAtRestFindings(root, atRestScanDirs(root))
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
	err := runCIAtRestGuard(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "examined 0") {
		t.Fatalf("an empty corpus must fail closed, got %v", err)
	}
}

// Every registered residue must name an exit condition. An accepted residue with
// no test for retiring it is a permanent one wearing a temporary label — which is
// precisely what ADR 0007's phase 2 became while it lived as a comment repeated in
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
	if err := runCIAtRestGuard("../../.."); err != nil {
		t.Fatalf("at-rest-guard must be green on this repo: %v", err)
	}
}
