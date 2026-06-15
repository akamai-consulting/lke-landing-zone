package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGhTokenURL(t *testing.T) {
	got := ghTokenURL("repo,workflow", "llz-openbao-secrets-write")
	want := "https://github.com/settings/tokens/new?scopes=repo,workflow&description=llz-openbao-secrets-write"
	if got != want {
		t.Errorf("ghTokenURL\n got: %s\nwant: %s", got, want)
	}
}

func TestCatalogExcludesAutoStashed(t *testing.T) {
	// These are written by the build, not the operator — the wizard must not ask.
	banned := []string{"OPENBAO_UNSEAL_KEY_1", "LOKI_S3_ACCESS_KEY", "HARBOR_PASSWORD", "OPENBAO_APPROLE_ROLE_ID"}
	for _, s := range catalog() {
		for _, b := range banned {
			if s.Name == b {
				t.Errorf("catalog must not request auto-stashed secret %q", b)
			}
		}
	}
}

func TestRenderEnvFileSorted(t *testing.T) {
	got := renderEnvFile(map[string]string{"B": "2", "A": "1", "C": "3"})
	want := "A=1\nB=2\nC=3\n"
	if got != want {
		t.Errorf("renderEnvFile\n got: %q\nwant: %q", got, want)
	}
}

func TestWriteEnvFilePermsAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	in := map[string]string{"LINODE_API_TOKEN": "abc=123", "TF_STATE_BUCKET": "tf-state"}
	if err := writeEnvFile(path, in); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
	// Value containing '=' must survive (split on first '=' only).
	out := readEnvFile(path)
	if out["LINODE_API_TOKEN"] != "abc=123" || out["TF_STATE_BUCKET"] != "tf-state" {
		t.Errorf("round trip: %v", out)
	}
}

func TestReadEnvFileIgnoresCommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.env")
	if err := os.WriteFile(path, []byte("# comment\n\nA=1\n  B=2 \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := readEnvFile(path)
	if len(m) != 2 || m["A"] != "1" || m["B"] != "2" {
		t.Errorf("readEnvFile: %v", m)
	}
}

func TestReadEnvFileMissingIsEmpty(t *testing.T) {
	if m := readEnvFile(filepath.Join(t.TempDir(), "nope.env")); len(m) != 0 {
		t.Errorf("missing file should yield empty map, got %v", m)
	}
}
