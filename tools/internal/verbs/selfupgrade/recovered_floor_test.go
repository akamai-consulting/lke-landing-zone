package selfupgrade

// THESE TESTS EXIST BECAUSE MOVING CODE OUT DROPPED A FLOOR. The version
// vocabulary (Semver, Less, LatestLLZTag, NormalizeLLZTag) went down to
// internal/shared/llzver, and it was among the best-covered code here — so this
// package's percentage fell without a single line of it getting worse. Lowering
// the floor would have been the wrong read of that number: the floor was
// describing a package that no longer exists, and the honest way to restore it is
// to cover something that was never covered. verifyChecksum and walkUpgradeFiles
// were both at 0% and both are pure filesystem work, testable without a network
// or a release.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "llz-linux-amd64")
	body := []byte("pretend this is a binary")
	if err := os.WriteFile(bin, body, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := hex.EncodeToString(func() []byte { h := sha256.Sum256(body); return h[:] }())
	sums := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sums, []byte(sum+"  llz-linux-amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := verifyChecksum(bin, sums, "llz-linux-amd64"); err != nil {
		t.Errorf("matching checksum should verify: %v", err)
	}

	// A CORRUPTED OR SUBSTITUTED DOWNLOAD IS THE WHOLE POINT of this function —
	// self-update overwrites the running binary with whatever it fetched, so a
	// mismatch that verified would be arbitrary code execution on the operator's
	// machine.
	if err := os.WriteFile(bin, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := verifyChecksum(bin, sums, "llz-linux-amd64")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("tampered binary: got %v, want a checksum mismatch", err)
	}

	if err := verifyChecksum(bin, sums, "llz-darwin-arm64"); err == nil ||
		!strings.Contains(err.Error(), "no checksum for") {
		t.Errorf("asset absent from SHA256SUMS: got %v", err)
	}
	if err := verifyChecksum(bin, filepath.Join(dir, "absent"), "llz-linux-amd64"); err == nil ||
		!strings.Contains(err.Error(), "read SHA256SUMS") {
		t.Errorf("missing SHA256SUMS: got %v", err)
	}
	if err := verifyChecksum(filepath.Join(dir, "absent"), sums, "llz-linux-amd64"); err == nil {
		t.Error("missing binary should not verify")
	}
}

func TestWalkUpgradeFilesSkipsGeneratedTrees(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		"main.tf", "docs/README.md",
		// Each of these three is skipped for its own reason: .git is history,
		// .terraform is provider cache, .llz is llz's own state — none is
		// template-owned, and walking them would make the upgrade diff meaningless.
		".git/config", ".terraform/providers/x", ".llz/state.json",
	} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := walkUpgradeFiles(root)
	if err != nil {
		t.Fatalf("walkUpgradeFiles: %v", err)
	}
	// Slash-separated and sorted, so the result is comparable across platforms and
	// stable enough to diff two snapshots against each other.
	if want := []string{"docs/README.md", "main.tf"}; !reflect.DeepEqual(got, want) {
		t.Errorf("walkUpgradeFiles = %v, want %v", got, want)
	}

	if _, err := walkUpgradeFiles(filepath.Join(root, "absent")); err == nil {
		t.Error("walking a missing root should error, not return an empty list — " +
			"an empty list reads as 'the instance has no files' and would delete everything")
	}
}
