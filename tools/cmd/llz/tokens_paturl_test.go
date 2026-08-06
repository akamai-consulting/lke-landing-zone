package main

// tokens_paturl_test.go — one test that travelled with the wrong file.
//
// It lived in token_capability_test.go and moved with it to internal/tokeninv,
// but its subject is ghFineGrainedSecretsWriteURL: the WIZARD's pre-filled PAT
// link, in tokens.go, which is credential PROVISIONING and deliberately stayed in
// package main. Third instance of the same lesson this branch keeps re-learning —
// a test's filename says where someone put it, not what it is about.

import (
	"strings"
	"testing"
)

// TestSecretsWritePATURLRequestsEnvironments pins the wizard's pre-filled PAT
// link to the permission that actually governs environment secrets. Every
// credential in catalog() is destined for an infra-<env> ENVIRONMENT secret, so
// a link that pre-fills `secrets=write` mints a token that cannot do the job.
func TestSecretsWritePATURLRequestsEnvironments(t *testing.T) {
	u := ghFineGrainedSecretsWriteURL("llz-openbao-secrets-write", "acme")
	if !strings.Contains(u, "environments=write") {
		t.Errorf("pre-fill must request environments=write; got %q", u)
	}
	if strings.Contains(u, "secrets=write") {
		t.Errorf("pre-fill must NOT request secrets=write (repo-level only, not environment secrets); got %q", u)
	}
	if !strings.Contains(u, "actions=write") {
		t.Errorf("pre-fill should keep actions=write (workflow dispatch); got %q", u)
	}
}
