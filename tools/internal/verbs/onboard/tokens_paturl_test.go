package onboard

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

// TestSecretsWritePATURLRequestsBothSecretPermissions pins the wizard's
// pre-filled PAT link to BOTH grants this one credential needs.
//
// THIS ASSERTION USED TO SAY THE OPPOSITE about secrets=write, and the reasoning
// it carried was sound for the case it was written about: every credential in
// catalog() lands in an infra-<env> ENVIRONMENT secret, which "Secrets" does not
// govern, so a link offering only that mints a token that 403s on the first
// writeback. What it missed is that the same PAT acquired a second consumer —
// `llz ci bao-seed-all` seeds it into the cluster, where the harbor-robot-
// provisioner publishes REPO-level HARBOR_* secrets with it. That consumer needs
// exactly the permission this test forbade, and forbidding it shipped a wizard
// that mints an under-scoped token by construction: the CronJob 403s every five
// minutes and converge hard-fails the bootstrap on it.
//
// Neither grant implies the other, so the link must request both and this test
// must fail if either goes missing.
func TestSecretsWritePATURLRequestsBothSecretPermissions(t *testing.T) {
	u := ghFineGrainedSecretsWriteURL("llz-openbao-secrets-write", "acme")
	if !strings.Contains(u, "environments=write") {
		t.Errorf("pre-fill must request environments=write (the infra-<env> secrets CI writes back); got %q", u)
	}
	if !strings.Contains(u, "secrets=write") {
		t.Errorf("pre-fill must request secrets=write (the repo-level HARBOR_* secrets the in-cluster provisioner publishes); got %q", u)
	}
	if !strings.Contains(u, "actions=write") {
		t.Errorf("pre-fill should keep actions=write (workflow dispatch); got %q", u)
	}
}
