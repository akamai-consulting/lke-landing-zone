package tfbin

import (
	"os"
	"path/filepath"
	"testing"
)

// withStubPATH puts a PATH containing only the named fake executables in front
// of the real one, so resolution is decided by the test rather than by whatever
// the developer happens to have installed. That independence is the point: the
// bug this resolver exists to prevent was precisely a local machine resolving a
// different binary than CI.
func withStubPATH(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestResolveTFBinPrefersTofu(t *testing.T) {
	t.Setenv(tfBinEnv, "")
	withStubPATH(t, "tofu", "terraform")
	if got := resolveTFBin(); got != "tofu" {
		t.Errorf("with both installed, want tofu (the landing zone runs OpenTofu), got %q", got)
	}
}

// The migration fallback: an adopter's runner that still has only Terraform
// keeps working rather than hard-failing.
func TestResolveTFBinFallsBackToTerraform(t *testing.T) {
	t.Setenv(tfBinEnv, "")
	withStubPATH(t, "terraform")
	if got := resolveTFBin(); got != "terraform" {
		t.Errorf("with only terraform installed, want terraform, got %q", got)
	}
}

// With neither present, name the binary that SHOULD be there. "exec: tofu: not
// found" points at the actual remedy; "terraform: not found" would send someone
// to install the wrong thing.
func TestResolveTFBinNamesTofuWhenNeitherPresent(t *testing.T) {
	t.Setenv(tfBinEnv, "")
	withStubPATH(t)
	if got := resolveTFBin(); got != "tofu" {
		t.Errorf("with neither installed, the error should name tofu, got %q", got)
	}
}

// $TF wins outright — same escape hatch as detect_tf's $TF, used to pin a
// specific build without editing anything.
func TestResolveTFBinHonoursOverride(t *testing.T) {
	withStubPATH(t, "tofu", "terraform")
	t.Setenv(tfBinEnv, "/opt/pinned/tofu-1.12.5")
	if got := resolveTFBin(); got != "/opt/pinned/tofu-1.12.5" {
		t.Errorf("$TF must win, got %q", got)
	}
}

// Bin must NOT cache: tests stub a fake binary on PATH, and a cached answer
// would leak whichever binary the first test in the package happened to resolve.
func TestTFBinIsNotCached(t *testing.T) {
	t.Setenv(tfBinEnv, "")
	withStubPATH(t, "terraform")
	if got := Bin(); got != "terraform" {
		t.Fatalf("first resolution = %q, want terraform", got)
	}
	withStubPATH(t, "tofu", "terraform")
	if got := Bin(); got != "tofu" {
		t.Errorf("after PATH changed, Bin = %q — a cached answer would have kept terraform", got)
	}
}

func TestTFCommandUsesResolvedBinary(t *testing.T) {
	prev := resolveTFBin
	t.Cleanup(func() { resolveTFBin = prev })
	resolveTFBin = func() string { return "tofu" }
	cmd := Command("output", "-json")
	if filepath.Base(cmd.Path) != "tofu" && cmd.Args[0] != "tofu" {
		t.Errorf("Command built %v, want it to exec tofu", cmd.Args)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "output" || cmd.Args[2] != "-json" {
		t.Errorf("args = %v, want [tofu output -json]", cmd.Args)
	}
}
