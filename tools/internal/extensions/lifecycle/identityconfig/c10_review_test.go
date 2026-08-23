package identityconfig

// c10_review_test.go — the identityconfig gates for the C10 findings of the
// 2026-08-13 review. Both are a writer that quietly wrote something other than
// what it said.

import (
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/forge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// ── the hostAliases patch type ───────────────────────────────────────────────

// TestKeycloakPinReplacesTheHostAliasList.
//
// PodSpec.HostAliases is declared `patchStrategy:"merge" patchMergeKey:"ip"`, so a
// STRATEGIC patch merges the list BY IP — a new gateway ClusterIP is a new key, so
// the entry is APPENDED and the old one is kept. The pod then gets two /etc/hosts
// lines for the same hostname and resolves the FIRST: the stale, dead IP. The JWKS
// fetch breaks, and the "gateway Service was recreated" branch this patch exists
// to handle can never converge — every subsequent run reads the stale first entry,
// sees it differ, and appends another.
//
// Asserting on the FLAG rather than on an outcome, deliberately: the outcome is
// produced by the apiserver's merge semantics, which a unit test cannot host. The
// flag is the whole decision, and it is one word.
func TestKeycloakPinReplacesTheHostAliasList(t *testing.T) {
	var got []string
	prev := kubectlprobe.Exec
	t.Cleanup(func() { kubectlprobe.Exec = prev })
	kubectlprobe.Exec = func(_ string, args ...string) ([]byte, error) {
		got = args
		return nil, nil
	}
	if err := patchWithWebhookRetry(`{"spec":{}}`); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "--type=strategic") {
		t.Error("a STRATEGIC patch merges hostAliases by `ip`, so a changed gateway ClusterIP APPENDS a " +
			"second alias for the same hostname instead of replacing the first. /etc/hosts then resolves " +
			"the stale dead IP, and the recreated-Service branch this patch exists for never converges.")
	}
	if !strings.Contains(joined, "--type=merge") {
		t.Errorf("expected a JSON merge patch, which replaces the array wholesale; got: %s", joined)
	}
}

// ── a forge failure must not take human team-login with it ───────────────────

// TestForgeFailureStillConfiguresKeycloakTeamAuth.
//
// The forge-resolution failure branch used to `return steps`, which also skipped
// the keycloakTeamSteps append at the end of the function. So `LLZ_FORGE=ghes`
// without LLZ_FORGE_HOST silently omitted the entire `keycloak` auth mount and
// every <name>-writer policy and role — permanently breaking
// `llz openbao login --team` for every operator — under a warning that mentions
// only GitHub-OIDC. The two are unrelated: one is CI identity, the other is human
// identity, and failing to resolve the forge says nothing about the second.
func TestForgeFailureStillConfiguresKeycloakTeamAuth(t *testing.T) {
	prev := forgeFromEnv
	t.Cleanup(func() { forgeFromEnv = prev })
	forgeFromEnv = func() (forge.Forge, error) {
		return nil, errors.New("LLZ_FORGE=ghes but LLZ_FORGE_HOST is unset")
	}

	teams := []clusterspec.Team{{Name: "payments"}}
	steps := baoConfigureSteps("acme/instance", "https://keycloak.example.com/realms/otomi", teams)

	var sawKeycloakMount, sawTeamPolicy, sawJWTRole bool
	for _, s := range steps {
		joined := strings.Join(s.args, " ")
		switch {
		case strings.Contains(joined, "auth/keycloak") || strings.Contains(joined, "auth enable -path=keycloak"):
			sawKeycloakMount = true
		case strings.Contains(joined, "payments-writer"):
			sawTeamPolicy = true
		case strings.Contains(joined, "auth/jwt/role/"):
			sawJWTRole = true
		}
	}
	if !sawKeycloakMount || !sawTeamPolicy {
		t.Errorf("a forge failure dropped the keycloak team auth (mount=%v policy=%v). That breaks "+
			"`llz openbao login --team` for every operator, and has nothing to do with the CI OIDC "+
			"config that actually failed to resolve.", sawKeycloakMount, sawTeamPolicy)
	}
	if sawJWTRole {
		t.Error("the GitHub-OIDC roles must still be skipped — that IS what could not be resolved")
	}
}

// TestForgeSuccessStillConfiguresBoth pins the exclusion: the ordinary path must
// keep producing both halves.
func TestForgeSuccessStillConfiguresBoth(t *testing.T) {
	teams := []clusterspec.Team{{Name: "payments"}}
	steps := baoConfigureSteps("acme/instance", "https://keycloak.example.com/realms/otomi", teams)
	var sawJWTRole, sawTeamPolicy bool
	for _, s := range steps {
		joined := strings.Join(s.args, " ")
		if strings.Contains(joined, "auth/jwt/role/") {
			sawJWTRole = true
		}
		if strings.Contains(joined, "payments-writer") {
			sawTeamPolicy = true
		}
	}
	if !sawJWTRole || !sawTeamPolicy {
		t.Errorf("the ordinary path must configure both (jwt=%v keycloak=%v)", sawJWTRole, sawTeamPolicy)
	}
}
