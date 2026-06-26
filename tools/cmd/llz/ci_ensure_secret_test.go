package main

import (
	"os"
	"strings"
	"testing"
)

// withSeedRand swaps the crypto/rand seam with a deterministic filler so a
// generated value is reproducible under test.
func withSeedRand(t *testing.T, fill byte) {
	t.Helper()
	prev := seedRandRead
	seedRandRead = func(b []byte) error {
		for i := range b {
			b[i] = fill
		}
		return nil
	}
	t.Cleanup(func() { seedRandRead = prev })
}

// readGithubEnv returns the contents of the temp $GITHUB_ENV file.
func readGithubEnv(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

// First run: the secret is unset → generate, persist via gh, export the TF var.
func TestEnsureEnvSecretGeneratesWhenMissing(t *testing.T) {
	withSeedRand(t, 0)
	calls := stubGHSecretSet(t, nil)
	envFile := t.TempDir() + "/gh.env"
	t.Setenv("GITHUB_ENV", envFile)
	t.Setenv("LOKI_ADMIN_PASSWORD", "")

	if err := runCIEnsureEnvSecret("infra-primary", "LOKI_ADMIN_PASSWORD", "loki_admin_password", 24); err != nil {
		t.Fatalf("ensure-env-secret: %v", err)
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "LOKI_ADMIN_PASSWORD@infra-primary=") {
		t.Fatalf("gh secret set calls = %v, want one LOKI_ADMIN_PASSWORD@infra-primary", *calls)
	}
	// The generated value is alphanumeric (base64 of zero bytes → "AAA…", no /+=).
	gen := strings.TrimPrefix((*calls)[0], "LOKI_ADMIN_PASSWORD@infra-primary=")
	if gen == "" || strings.ContainsAny(gen, "/+=") {
		t.Errorf("generated value %q is empty or not alphanumeric", gen)
	}
	if got := readGithubEnv(t, envFile); got != "TF_VAR_loki_admin_password="+gen+"\n" {
		t.Errorf("GITHUB_ENV = %q, want TF_VAR_loki_admin_password=%s", got, gen)
	}
}

// Later runs: the secret is already set → reuse verbatim, no gh write.
func TestEnsureEnvSecretReusesWhenSet(t *testing.T) {
	calls := stubGHSecretSet(t, nil)
	envFile := t.TempDir() + "/gh.env"
	t.Setenv("GITHUB_ENV", envFile)
	t.Setenv("LOKI_ADMIN_PASSWORD", "stored-pw")

	if err := runCIEnsureEnvSecret("infra-primary", "LOKI_ADMIN_PASSWORD", "loki_admin_password", 24); err != nil {
		t.Fatalf("ensure-env-secret: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("gh secret set should not be called when the value exists, got %v", *calls)
	}
	if got := readGithubEnv(t, envFile); got != "TF_VAR_loki_admin_password=stored-pw\n" {
		t.Errorf("GITHUB_ENV = %q, want the stored value re-exported", got)
	}
}

// Generating with no --env to persist into is fatal (the value would churn).
func TestEnsureEnvSecretMissingEnvIsFatal(t *testing.T) {
	t.Setenv("LOKI_ADMIN_PASSWORD", "")
	if err := runCIEnsureEnvSecret("", "LOKI_ADMIN_PASSWORD", "loki_admin_password", 24); err == nil ||
		!strings.Contains(err.Error(), "--env") {
		t.Errorf("err = %v, want an --env requirement", err)
	}
}

func TestEnsureEnvSecretRequiresName(t *testing.T) {
	if err := runCIEnsureEnvSecret("infra-primary", "", "loki_admin_password", 24); err == nil ||
		!strings.Contains(err.Error(), "--name") {
		t.Errorf("err = %v, want a --name requirement", err)
	}
}
