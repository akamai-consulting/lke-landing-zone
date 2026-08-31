package doctor

// token_capabilities_test.go — `llz doctor` must ASK the authorization question,
// not just the validity one.
//
// The regression these guard is not a wrong answer, it is an unasked question:
// the table printed "✓ set / ⚠ warn" for a PAT that could not write the
// repo-level secrets the cluster needs, because scope was probed only in CI.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

func capReqs() []envreq.Requirement {
	return []envreq.Requirement{
		{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Secret: true, Required: true},
		{Name: "APL_VALUES_REPO_TOKEN", Secret: true, Required: true},
		{Name: "TF_STATE_BUCKET", Secret: false}, // not a credential — must stay absent
	}
}

// THE LOCAL VALUE IS PROBED. .llz/secrets.env is exactly where the wizard put it
// and where the validity probe already reads from; refusing to use it for scope
// is what made the local report silent.
func TestProbeTokenCapabilities_ProbesLocallyCachedSecrets(t *testing.T) {
	orig := tokenprobe.GHCapabilityProbe
	t.Cleanup(func() { tokenprobe.GHCapabilityProbe = orig })
	// Environments: write granted, repo-level Secrets refused — the live shape of
	// the outage.
	var asked []string
	tokenprobe.GHCapabilityProbe = func(_, _, path string) (int, error) {
		asked = append(asked, path)
		if strings.Contains(path, "/environments/") {
			return 200, nil
		}
		return 403, nil
	}
	origGit := tokenprobe.GitRefsProbe
	t.Cleanup(func() { tokenprobe.GitRefsProbe = origGit })
	tokenprobe.GitRefsProbe = func(_, _, _ string) (int, error) { return 200, nil }

	secrets := map[string]string{
		"OPENBAO_SECRETS_WRITE_TOKEN": "live-but-underscoped",
		"APL_VALUES_REPO_TOKEN":       "fine",
	}
	inst := envreq.NewLiveState(nil, nil, nil, nil)
	cc := tokenprobe.CapContext{Repo: "acme/platform", Region: "prod"}

	caps, denied := ProbeTokenCapabilities(capReqs(), secrets, map[string]string{}, inst, cc)

	if len(asked) == 0 {
		t.Fatal("no capability probe was made — the report would be a claim about a question never asked")
	}
	if denied != 1 {
		t.Errorf("denied = %d, want 1 (repo-level Secrets on a REQUIRED credential)", denied)
	}
	if got := len(caps["OPENBAO_SECRETS_WRITE_TOKEN"]); got != 2 {
		t.Fatalf("OPENBAO_SECRETS_WRITE_TOKEN checks = %d, want 2", got)
	}
	worst, ok := tokenprobe.WorstCapability(caps["OPENBAO_SECRETS_WRITE_TOKEN"])
	if !ok || worst != tokenprobe.CapDenied {
		t.Errorf("column verdict = %v, want CapDenied — one granted check must not mask the refused one", worst)
	}
	if _, ok := caps["TF_STATE_BUCKET"]; ok {
		t.Error("a plain variable has no scope requirement and must not appear")
	}
}

// A CREDENTIAL WITH NO LOCAL VALUE REPORTS SKIPPED, NOT OK. GitHub never hands a
// secret value to a laptop, so a repo-only secret genuinely cannot be probed
// here — and saying nothing at all would read as "no scope requirement", which is
// the false clean bill this whole change is about.
func TestProbeTokenCapabilities_UnreadableSecretIsSkippedNotSilent(t *testing.T) {
	orig := tokenprobe.GHCapabilityProbe
	t.Cleanup(func() { tokenprobe.GHCapabilityProbe = orig })
	called := false
	tokenprobe.GHCapabilityProbe = func(_, _, _ string) (int, error) { called = true; return 200, nil }

	inst := envreq.NewLiveState(nil, map[string]bool{"OPENBAO_SECRETS_WRITE_TOKEN": true}, nil, nil)
	caps, denied := ProbeTokenCapabilities(capReqs(), map[string]string{}, map[string]string{}, inst, tokenprobe.CapContext{Repo: "acme/platform", Region: "prod"})

	if called {
		t.Error("probed with no credential value")
	}
	if denied != 0 {
		t.Errorf("denied = %d, want 0 — an unprobed credential must never be reported as refused", denied)
	}
	rs := caps["OPENBAO_SECRETS_WRITE_TOKEN"]
	// ONE SKIP PER REGISTERED CHECK, each naming its op. A single Op-less row
	// cannot say WHICH grants went unasked, and is the one result a hint can never
	// be looked up for.
	ops := tokenprobe.CapabilityChecksFor("OPENBAO_SECRETS_WRITE_TOKEN")
	if len(rs) != len(ops) {
		t.Fatalf("skip rows = %d, want one per registered check (%d)", len(rs), len(ops))
	}
	for _, cr := range rs {
		if cr.Status != tokenprobe.CapSkipped {
			t.Errorf("%q: status = %v, want CapSkipped", cr.Op, cr.Status)
		}
		if cr.Op == "" {
			t.Error("a skip must name the check it could not run")
		}
		if !strings.Contains(cr.Detail, "validate-tokens") {
			t.Errorf("the skip must point at where the question CAN be asked; got %q", cr.Detail)
		}
	}
}

// THE COUNT IS OF CREDENTIALS, NOT REFUSALS. One PAT refused both of its grants
// is one credential to go and fix; reporting "2 required credential(s)" sends the
// reader looking for a second broken token that does not exist.
func TestProbeTokenCapabilities_CountsCredentialsNotRefusals(t *testing.T) {
	orig := tokenprobe.GHCapabilityProbe
	t.Cleanup(func() { tokenprobe.GHCapabilityProbe = orig })
	tokenprobe.GHCapabilityProbe = func(_, _, _ string) (int, error) { return 403, nil } // BOTH grants refused

	reqs := []envreq.Requirement{{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Secret: true, Required: true}}
	caps, denied := ProbeTokenCapabilities(reqs, map[string]string{"OPENBAO_SECRETS_WRITE_TOKEN": "tok"},
		map[string]string{}, envreq.NewLiveState(nil, nil, nil, nil), tokenprobe.CapContext{Repo: "acme/platform", Region: "prod"})

	if n := len(caps["OPENBAO_SECRETS_WRITE_TOKEN"]); n != 2 {
		t.Fatalf("checks = %d, want 2 refusals — the fixture must actually refuse both, or this proves nothing", n)
	}
	if denied != 1 {
		t.Errorf("denied = %d, want 1 — one credential, however many of its grants were refused", denied)
	}
}

// AN OPTIONAL CREDENTIAL'S DENIAL IS REPORTED AND DOES NOT BLOCK — the same rule
// `llz ci validate-tokens` applies, so doctor and CI agree on what stops a build.
func TestProbeTokenCapabilities_OptionalDenialDoesNotBlock(t *testing.T) {
	orig := tokenprobe.GHCapabilityProbe
	t.Cleanup(func() { tokenprobe.GHCapabilityProbe = orig })
	tokenprobe.GHCapabilityProbe = func(_, _, _ string) (int, error) { return 403, nil }

	reqs := []envreq.Requirement{{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Secret: true, Required: false}}
	secrets := map[string]string{"OPENBAO_SECRETS_WRITE_TOKEN": "tok"}
	caps, denied := ProbeTokenCapabilities(reqs, secrets, map[string]string{}, envreq.NewLiveState(nil, nil, nil, nil), tokenprobe.CapContext{Repo: "acme/platform", Region: "prod"})
	if denied != 0 {
		t.Errorf("denied = %d, want 0 for an optional credential", denied)
	}
	if len(caps["OPENBAO_SECRETS_WRITE_TOKEN"]) == 0 {
		t.Error("an optional denial must still be REPORTED — silent is how it stays unfixed")
	}
}
