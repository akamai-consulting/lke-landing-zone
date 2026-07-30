package main

import (
	"strings"
	"testing"
)

// TestClearClusterSecretsReportsPerSecretOutcome pins which of the two log lines
// each secret gets. Teardown is best-effort and never returns an error, so this
// log is the ONLY signal an operator has about whether a cluster-scoped secret
// was actually removed — reporting a successful delete as "could not delete"
// (or, worse, a failed one as deleted) leaves a revoked OPENBAO_ROOT_TOKEN in
// place while the log says it is gone.
func TestClearClusterSecretsReportsPerSecretOutcome(t *testing.T) {
	t.Setenv("GH_TOKEN", "pat")
	stubGHDeleteSecret(t, map[string]bool{"OPENBAO_SEAL_KEY": true})

	var err error
	out := captureStdout(t, func() { err = runCIClearClusterSecrets("infra-primary") })
	if err != nil {
		t.Fatalf("clear-cluster-secrets: %v", err)
	}

	// The secret that deleted cleanly is reported as deleted, not as a warning.
	if !strings.Contains(out, "Deleted GH env secret infra-primary / OPENBAO_ROOT_TOKEN") {
		t.Errorf("a successful delete must be logged as deleted:\n%s", out)
	}
	// The one that 404'd is the warning — and it must be the ONLY warning.
	if !strings.Contains(out, "::warning::Could not delete infra-primary / OPENBAO_SEAL_KEY") {
		t.Errorf("the failed delete must be logged as a warning:\n%s", out)
	}
	if n := strings.Count(out, "::warning::Could not delete"); n != 1 {
		t.Errorf("got %d failure warnings, want exactly 1 (only OPENBAO_SEAL_KEY failed):\n%s", n, out)
	}
	if n := strings.Count(out, "Deleted GH env secret"); n != len(clusterScopedSecrets)-1 {
		t.Errorf("got %d success lines, want %d:\n%s", n, len(clusterScopedSecrets)-1, out)
	}
}
