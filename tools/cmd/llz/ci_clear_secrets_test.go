package main

import (
	"errors"
	"strings"
	"testing"
)

// stubGHDeleteSecret records env-scoped gh secret deletions, failing those named
// in failFor (e.g. a 404 for an absent secret).
func stubGHDeleteSecret(t *testing.T, failFor map[string]bool) *[]string {
	t.Helper()
	var calls []string
	prev := ghDeleteSecretFn
	ghDeleteSecretFn = func(name, ghEnv string) error {
		calls = append(calls, name+"@"+ghEnv)
		if failFor[name] {
			return errors.New("HTTP 404")
		}
		return nil
	}
	t.Cleanup(func() { ghDeleteSecretFn = prev })
	return &calls
}

func TestClearClusterSecretsDeletesTheClusterScopedSet(t *testing.T) {
	t.Setenv("GH_TOKEN", "pat")
	// One secret 404s — the rest must still be attempted (best-effort).
	calls := stubGHDeleteSecret(t, map[string]bool{"OPENBAO_APPROLE_SECRET_ID_STANDBY": true})

	if err := runCIClearClusterSecrets("infra-primary"); err != nil {
		t.Fatalf("clear-cluster-secrets: %v", err)
	}
	if len(*calls) != len(clusterScopedSecrets) {
		t.Fatalf("attempted %d deletes, want all %d (best-effort continues past a 404)", len(*calls), len(clusterScopedSecrets))
	}
	// Both AppRole suffixes are deleted so no ha_role lookup is needed.
	var sawActive, sawStandby bool
	for _, c := range *calls {
		sawActive = sawActive || c == "OPENBAO_APPROLE_SECRET_ID@infra-primary"
		sawStandby = sawStandby || c == "OPENBAO_APPROLE_SECRET_ID_STANDBY@infra-primary"
	}
	if !sawActive || !sawStandby {
		t.Errorf("both AppRole secret-id names must be deleted (active=%v standby=%v)", sawActive, sawStandby)
	}
}

// No GH_TOKEN → logged no-op, no deletions, no error.
func TestClearClusterSecretsNoTokenIsNoOp(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	calls := stubGHDeleteSecret(t, nil)
	if err := runCIClearClusterSecrets("infra-primary"); err != nil {
		t.Fatalf("no-token path should be a clean no-op, got %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("no deletions expected without GH_TOKEN, got %v", *calls)
	}
}

func TestClearClusterSecretsRequiresEnv(t *testing.T) {
	if err := runCIClearClusterSecrets(""); err == nil || !strings.Contains(err.Error(), "--env") {
		t.Errorf("err = %v, want an --env requirement", err)
	}
}
