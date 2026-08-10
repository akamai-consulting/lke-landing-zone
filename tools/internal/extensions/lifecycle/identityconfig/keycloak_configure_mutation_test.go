package identityconfig

// keycloak_configure_mutation_test.go — the guards in keycloak-configure that
// must FAIL a half-wired realm rather than report color.Green. Mutation testing found
// each of these inert-but-passing: a swallowed mapper error, a status set that
// accepts what it should reject (and rejects what it should accept), the
// created-client id read from the wrong place, and the poll/sleep budgets of the
// two retry loops. Every case is httptest/seam-driven — no network, no kubectl.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/keycloak"
)

// withScopeBudget shrinks BOTH poll knobs, so a wait test runs instantly and the
// "~<duration>" in the give-up message is exactly predictable.
func withScopeBudget(attempts int, interval time.Duration) func() {
	oldA, oldI := keycloak.ScopeAttempts, keycloak.ScopeInterval
	keycloak.ScopeAttempts, keycloak.ScopeInterval = attempts, interval
	return func() { keycloak.ScopeAttempts, keycloak.ScopeInterval = oldA, oldI }
}

// TestKcDo_SetsContentTypeOnlyWithBody: Keycloak rejects a JSON write that isn't
// declared application/json, and a bodyless GET/PUT must not claim one. The
// header therefore tracks the body, not the other way around.
func TestKeycloakConnect_AttemptAndSleepBudget(t *testing.T) {
	var attempts int
	orig := PortForwardFn
	PortForwardFn = func() (string, func(), error) {
		attempts++
		return "", func() {}, fmt.Errorf("pod not found")
	}
	defer func() { PortForwardFn = orig }()
	defer withScopeBudget(4, 100*time.Millisecond)()

	var sleeps []time.Duration
	_, _, cleanup, err := Connect(&http.Client{}, "u", "p", func(d time.Duration) { sleeps = append(sleeps, d) })
	cleanup()
	if err == nil {
		t.Fatal("a persistently-unreachable Keycloak must time out")
	}
	if attempts != 4 {
		t.Errorf("attempted %d times, want exactly keycloak.ScopeAttempts (4)", attempts)
	}
	if len(sleeps) != 3 {
		t.Errorf("slept %d times, want attempts-1 (3) — no sleep after the final attempt", len(sleeps))
	}
	for i, d := range sleeps {
		if d != 100*time.Millisecond {
			t.Errorf("sleep %d = %s, want keycloak.ScopeInterval", i, d)
		}
	}
}

// TestRunCIKeycloakConfigure_TeamsGate: spec.teams is the intent gate. No teams
// must short-circuit BEFORE any Keycloak work; teams declared must proceed to it.
// Both directions are checked from the printed output, in dry-run so nothing
// touches a cluster.
func TestRunCIKeycloakConfigure_TeamsGate(t *testing.T) {
	t.Run("no teams short-circuits", func(t *testing.T) {
		t.Chdir(t.TempDir()) // no landingzone.yaml → SpecTeams() is empty
		var err error
		var stdout string
		stderr := captureStderr(t, func() {
			stdout = captureStdout(t, func() { err = RunKeycloakConfigure(true, "primary") })
		})
		if err != nil {
			t.Fatalf("no-teams run must be a clean no-op, got %v", err)
		}
		if !strings.Contains(stdout, "No spec.teams declared") {
			t.Errorf("stdout = %q, want the no-teams no-op message", stdout)
		}
		if strings.Contains(stderr, "dry-run") {
			t.Errorf("no teams must not reach the client work, stderr = %q", stderr)
		}
	})

	t.Run("teams declared proceeds to the client work", func(t *testing.T) {
		dir := t.TempDir()
		spec := `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: t }
spec:
  instance: { upstreamOrg: akamai-consulting, repo: o/t, forge: github, templateVersion: v0.4.0 }
  teams:
    - { name: platform, openbaoSubtree: secret/platform }
  defaults:
    cluster:
      k8sVersion: v1.33.6+lke7
      nodePool: { type: g8-dedicated-8-4, count: 3 }
`
		if err := os.WriteFile(filepath.Join(dir, "landingzone.yaml"), []byte(spec), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if teams := SpecTeams(); len(teams) != 1 {
			t.Fatalf("fixture spec must declare one team, got %v", teams)
		}
		var err error
		var stdout string
		stderr := captureStderr(t, func() {
			stdout = captureStdout(t, func() { err = RunKeycloakConfigure(true, "primary") })
		})
		if err != nil {
			t.Fatalf("dry-run with teams: %v", err)
		}
		if !strings.Contains(stderr, "would ensure the public device-flow client") {
			t.Errorf("declared teams must reach the client work, stderr = %q", stderr)
		}
		if strings.Contains(stdout, "No spec.teams declared") {
			t.Errorf("declared teams must not print the no-teams no-op, stdout = %q", stdout)
		}
	})
}
