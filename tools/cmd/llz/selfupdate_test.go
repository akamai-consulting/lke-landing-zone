package main

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

func TestNormalizeLLZTag(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"1.2.3", "v1.2.3"},
		{"v1.2.3", "v1.2.3"},
		{"llz/v1.2.3", "v1.2.3"}, // legacy prefixed ref accepted, normalized to bare
		{"  v0.1.0 ", "v0.1.0"},
	} {
		if got := normalizeLLZTag(tc.in); got != tc.want {
			t.Errorf("normalizeLLZTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSemverAndLess(t *testing.T) {
	if _, _, _, ok := semver("dev"); ok {
		t.Error("dev should not parse as semver")
	}
	if _, _, _, ok := semver("dev-abc123"); ok {
		t.Error("dev-<sha> should not parse as semver")
	}
	if m, n, p, ok := semver("llz/v1.2.3"); !ok || m != 1 || n != 2 || p != 3 {
		t.Errorf("semver(llz/v1.2.3) = %d.%d.%d ok=%v", m, n, p, ok)
	}
	if m, _, _, ok := semver("v2.0.0-rc1"); !ok || m != 2 {
		t.Errorf("semver pre-release core: m=%d ok=%v", m, ok)
	}

	if !semverLess("v1.2.3", "v1.2.4") {
		t.Error("1.2.3 < 1.2.4")
	}
	if !semverLess("v1.9.0", "v1.10.0") {
		t.Error("1.9.0 < 1.10.0 (numeric, not lexical)")
	}
	if semverLess("v2.0.0", "v1.9.9") {
		t.Error("2.0.0 is not < 1.9.9")
	}
	// A dev build sorts below any real release, so self-update always proceeds.
	if !semverLess("dev", "v0.1.0") {
		t.Error("dev should sort below v0.1.0")
	}
}

func TestLatestLLZTag(t *testing.T) {
	tags := []string{
		"llz-pool/v0.1.0", // module track (prefixed) — ignored
		"llz/v0.1.0",      // legacy CLI tag (prefixed) — ignored
		"llz/v0.10.0",     // legacy CLI tag (prefixed) — ignored
		"v0.2.0",
		"v0.10.0", // highest bare
		"v0.3.0",
		"vbroken", // unparseable — ignored
	}
	got, ok := latestLLZTag(tags)
	if !ok || got != "v0.10.0" {
		t.Errorf("latestLLZTag = %q ok=%v, want v0.10.0", got, ok)
	}
	if _, ok := latestLLZTag([]string{"llz-pool/v1.0.0", "llz/v9.9.9"}); ok {
		t.Error("expected no bare vX.Y.Z tag")
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

func TestReleaseArgv(t *testing.T) {
	if got := releaseListArgv("akamai-consulting/lke-landing-zone"); !reflect.DeepEqual(got,
		[]string{"gh", "release", "list", "--repo", "akamai-consulting/lke-landing-zone",
			"--limit", "200", "--json", "tagName,isDraft,isPrerelease"}) {
		t.Errorf("releaseListArgv: got %v", got)
	}
	got := releaseDownloadArgv("akamai-consulting/lke-landing-zone", "v0.2.0", "llz-linux-amd64", "/tmp/x")
	want := []string{"gh", "release", "download", "v0.2.0",
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
