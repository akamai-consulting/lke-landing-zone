package selfupgrade

import (
	"os"
	"reflect"
	"testing"
)

func TestAssetName(t *testing.T) {
	if got := assetName("linux", "arm64"); got != "llz-linux-arm64" {
		t.Errorf("assetName: got %q", got)
	}
	if got := assetName("darwin", "amd64"); got != "llz-darwin-amd64" {
		t.Errorf("assetName: got %q", got)
	}
}

func TestChecksumFor(t *testing.T) {
	sums := "abc123  llz-linux-amd64\n" +
		"def456  llz-darwin-arm64\n" +
		"ghi789 *llz-linux-arm64\n"
	if got, ok := checksumFor(sums, "llz-darwin-arm64"); !ok || got != "def456" {
		t.Errorf("checksumFor darwin-arm64 = %q ok=%v", got, ok)
	}
	// Tolerate the '*' binary-mode marker.
	if got, ok := checksumFor(sums, "llz-linux-arm64"); !ok || got != "ghi789" {
		t.Errorf("checksumFor linux-arm64 = %q ok=%v", got, ok)
	}
	if _, ok := checksumFor(sums, "llz-windows-amd64"); ok {
		t.Error("expected no checksum for absent asset")
	}
}

func TestReleaseDownloadArgv(t *testing.T) {
	got := releaseDownloadArgv("akamai-consulting/lke-landing-zone", "v0.0.39", "llz-linux-amd64", "/tmp/x")
	want := []string{"gh", "release", "download", "v0.0.39",
		"--repo", "akamai-consulting/lke-landing-zone",
		"--pattern", "llz-linux-amd64", "--pattern", "SHA256SUMS",
		"--dir", "/tmp/x", "--clobber"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("releaseDownloadArgv\n got: %v\nwant: %v", got, want)
	}
}

func TestReplaceExecutable(t *testing.T) {
	dir := t.TempDir()
	self := dir + "/llz"
	if err := os.WriteFile(self, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := dir + "/llz-linux-amd64"
	if err := os.WriteFile(newBin, []byte("new-binary-contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := replaceExecutable(self, newBin); err != nil {
		t.Fatalf("replaceExecutable: %v", err)
	}
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-binary-contents" {
		t.Errorf("after update, self = %q", got)
	}
	if fi, err := os.Stat(self); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("self should be 0755 executable, got mode %v err %v", fi.Mode().Perm(), err)
	}
}
