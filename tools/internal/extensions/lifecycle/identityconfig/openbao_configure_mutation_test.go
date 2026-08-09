package identityconfig

// Mutation-test gap closure for ci_openbao_configure.go. bao-configure runs ONCE
// per cluster and never re-runs, so a skip it takes silently is permanent: the
// team auth mount, the GitHub-OIDC role, or the audit-device alarm simply never
// exist. Every warn-and-skip predicate here is therefore load-bearing.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfigureSpec lays down a minimal instance spec (optionally with teams)
// and chdirs into it, which is how SpecTeams/keycloakIssuerFor read the world.
func writeConfigureSpec(t *testing.T, teamsBlock string) {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("landingzone.yaml", `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: t }
spec:
  instance: { upstreamOrg: akamai-consulting, repo: o/t, forge: github, templateVersion: v0.4.0 }
`+teamsBlock+`
  defaults:
    cluster:
      k8sVersion: v1.33.6+lke7
      nodePool: { type: g8-dedicated-8-4, count: 3 }
`)
	write("environments/primary.yaml", `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: primary }
spec:
  cluster:
    clusterLabel: c-primary
    region: us-sea
    bootstrap: { name: b-primary, domainSuffix: primary.example.com }
    objectStorage: { cluster: us-sea-1 }
`)
	t.Chdir(dir)
}

// The keycloak-issuer parse must reject a URL with an EMPTY host segment. The
// derived value becomes the OpenBao mount's bound_issuer; a leftover path
// fragment there binds the mount to an issuer no token will ever carry, and the
// team login fails with nothing pointing at the cause.
func TestManagedDomainFromIssuerEmptyHostSegment(t *testing.T) {
	for _, in := range []string{
		"https://keycloak./realms/otomi", // empty domain, path present
		"https://keycloak./",
		"https://keycloak.",
	} {
		if got := ManagedDomainFromIssuer(in); got != "" {
			t.Errorf("ManagedDomainFromIssuer(%q) = %q, want %q — a path fragment is not a domain", in, got, "")
		}
	}
}

// A run with NO teams declared is the ordinary case and must be SILENT about
// teams. A spurious "failed validation" or "no Keycloak issuer" warning trains
// operators to ignore the two warnings that actually mean a team lost its
// OpenBao access, and the second one also nils out a perfectly good team list.
func TestRunCIBaoConfigureNoTeamsWarnsAboutNothing(t *testing.T) {
	t.Chdir(t.TempDir()) // no spec at all → SpecTeams() nil, issuer ""
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	t.Setenv("GITHUB_REPOSITORY", "acme/platform")

	var err error
	errOut := captureStderr(t, func() { err = RunBaoConfigure(true, "primary") })
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if strings.Contains(errOut, "spec.teams failed validation") {
		t.Errorf("an empty team list is valid — it must not be reported as a validation failure:\n%s", errOut)
	}
	if strings.Contains(errOut, "no Keycloak issuer could be derived") {
		t.Errorf("with no teams declared there is nothing to warn about:\n%s", errOut)
	}
	// GITHUB_REPOSITORY is set, so the OIDC skip-warning must not fire either.
	if strings.Contains(errOut, "GITHUB_REPOSITORY unset") {
		t.Errorf("GITHUB_REPOSITORY is set — the GitHub-OIDC skip must not be announced:\n%s", errOut)
	}
}

// …and when it really is unset, the skip MUST be announced: the in-cluster PAT
// rotation stays unavailable until someone re-runs with it set.
func TestRunCIBaoConfigureAnnouncesMissingGitHubRepository(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	t.Setenv("GITHUB_REPOSITORY", "")

	var err error
	errOut := captureStderr(t, func() { err = RunBaoConfigure(true, "primary") })
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(errOut, "GITHUB_REPOSITORY unset") {
		t.Errorf("a skipped GitHub-OIDC setup must be announced:\n%s", errOut)
	}
}

// Valid teams WITH a derivable issuer are the working configuration: no warning,
// and the keycloak steps are actually in the applied sequence. Warning here — or
// nilling the team list because an EMPTY validation-error slice read as failure —
// permanently drops every team's OpenBao role on a command that never re-runs.
func TestRunCIBaoConfigureValidTeamsAreConfiguredSilently(t *testing.T) {
	writeConfigureSpec(t, "  teams:\n    - { name: gsap, openbaoSubtree: secret/gsap }\n")
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	t.Setenv("GITHUB_REPOSITORY", "acme/platform")

	var err error
	errOut := captureStderr(t, func() { err = RunBaoConfigure(true, "primary") })
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if strings.Contains(errOut, "spec.teams failed validation") {
		t.Errorf("a valid team list must not be reported as failing validation:\n%s", errOut)
	}
	if strings.Contains(errOut, "no Keycloak issuer could be derived") {
		t.Errorf("the issuer IS derivable from cluster.bootstrap.domainSuffix — no skip warning expected:\n%s", errOut)
	}
	// The dry-run prints the sequence it would apply; the team steps must be in it.
	if !strings.Contains(errOut, "auth/keycloak/config") || !strings.Contains(errOut, "gsap") {
		t.Errorf("the keycloak team steps were dropped from the sequence:\n%s", errOut)
	}
}

// Teams declared but NO derivable issuer is the misconfiguration the warning
// exists for — it must fire, because the team roles are being skipped.
func TestRunCIBaoConfigureTeamsWithoutIssuerWarns(t *testing.T) {
	writeConfigureSpec(t, "  teams:\n    - { name: gsap, openbaoSubtree: secret/gsap }\n")
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	t.Setenv("GITHUB_REPOSITORY", "acme/platform")

	var err error
	// "nope" is not a declared deployment → keycloakIssuerFor returns "".
	errOut := captureStderr(t, func() { err = RunBaoConfigure(true, "nope") })
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(errOut, "no Keycloak issuer could be derived") {
		t.Errorf("teams declared with no derivable issuer must warn — the roles are being skipped:\n%s", errOut)
	}
}

// An invalid team subtree must be REFUSED (warn + drop), not built into policies.
func TestRunCIBaoConfigureInvalidTeamsAreDropped(t *testing.T) {
	// openbaoSubtree outside the KV mount — ValidateTeams rejects it.
	writeConfigureSpec(t, "  teams:\n    - { name: gsap, openbaoSubtree: sys/ }\n")
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	t.Setenv("GITHUB_REPOSITORY", "acme/platform")

	var err error
	errOut := captureStderr(t, func() { err = RunBaoConfigure(true, "primary") })
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(errOut, "spec.teams failed validation") {
		t.Errorf("an unvalidated team subtree must be refused loudly:\n%s", errOut)
	}
	if strings.Contains(errOut, "auth/keycloak/config") {
		t.Errorf("no keycloak team step may be built from an invalid spec subtree:\n%s", errOut)
	}
}

// A missing audit device is reported by deferring the failure into GITHUB_ENV for
// the job's final gate. If that write fails the run must FAIL: swallowing it
// leaves an OpenBao serving platform credentials with no audit log and a color.Green
// bootstrap.
func TestRunCIBaoConfigureUnwritableGHAEnvIsFatal(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	t.Setenv("GITHUB_REPOSITORY", "acme/platform")
	// A directory can never be opened for append — the BOOTSTRAP_ERRORS write fails.
	t.Setenv("GITHUB_ENV", t.TempDir())

	var calls []string
	withBaoExec(t, configureStub(t, &calls, func(cmd string) (string, string, error, bool) {
		if strings.HasPrefix(cmd, "audit list") {
			return "No audit devices are enabled.\n", "", nil, true
		}
		return "", "", nil, false
	}))

	var err error
	captureStderr(t, func() {
		captureStdout(t, func() { err = RunBaoConfigure(false, "primary") })
	})
	if err == nil {
		t.Fatal("a failed BOOTSTRAP_ERRORS write must fail the run — otherwise a cluster with no audit device reports a clean bootstrap")
	}
}
