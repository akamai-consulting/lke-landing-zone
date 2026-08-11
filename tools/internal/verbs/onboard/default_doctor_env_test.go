package onboard

// default_doctor_env_test.go — which deployment a bare `llz doctor` reports on.
//
// IT USED TO BE THE CONSTANT "e2e", which is the TEMPLATE's own throwaway lane
// and a deployment no adopter has. Measured against a live instance mid-upgrade,
// that produced one correct finding — a genuinely missing, newly-required
// TF_STATE_ENCRYPTION_PASSPHRASE — wrapped in three wrong instructions: a report
// headed `infra-e2e`, a fix reading `llz tokens --env e2e --yes`, and a closing
// "run `llz env add e2e` first". An advisory nobody trusts is an advisory nobody
// reads, and `llz upgrade`'s post-upgrade readiness check inherits whatever this
// resolves.
//
// THESE LIVE HERE, next to the resolver. They were only ever exercised from
// internal/verbs/upgrade's tests, which prove the coupling but count toward that
// package's coverage rather than this one's — so the function this package
// exports had no test of its own in the package that owns it.

import (
	"os"
	"path/filepath"
	"testing"
)

// writeInstance lays down the minimum spec-driven instance: identity in
// landingzone.yaml, one deployment per environments/<env>.yaml. Deployments are
// never authored inline — LoadSplit rejects that outright — so a helper that put
// them there would resolve nothing and every assertion below would "pass" by
// falling through to the same default it is meant to replace.
func writeInstance(t *testing.T, dir string, envs ...string) {
	t.Helper()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "terraform-iac-bootstrap", "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("landingzone.yaml", "apiVersion: llz.akamai-consulting.io/v1alpha1\n"+
		"kind: LandingZone\nmetadata:\n  name: test\nspec:\n  instance:\n    repo: o/r\n")
	for _, e := range envs {
		write(filepath.Join("environments", e+".yaml"), "apiVersion: llz.akamai-consulting.io/v1alpha1\n"+
			"kind: ClusterDefinition\nmetadata:\n  name: "+e+"\nspec:\n  cluster:\n    region: us-ord\n")
	}
}

func TestDefaultDoctorEnvUsesTheInstancesOwnDeployment(t *testing.T) {
	dir := t.TempDir()
	writeInstance(t, dir, "prod")
	t.Chdir(dir)

	if got := DefaultDoctorEnv(); got != "prod" {
		t.Errorf("DefaultDoctorEnv() = %q, want %q — an adopter's readiness report would name a "+
			"deployment they do not have, and tell them to provision secrets for it", got, "prod")
	}
}

// EnvNames is sorted, so the choice is stable rather than map-iteration order —
// otherwise a two-deployment instance would report on a different one per run.
func TestDefaultDoctorEnvIsStableAcrossDeployments(t *testing.T) {
	dir := t.TempDir()
	writeInstance(t, dir, "staging", "prod", "lab")
	t.Chdir(dir)

	first := DefaultDoctorEnv()
	if first != "lab" {
		t.Errorf("DefaultDoctorEnv() = %q, want the first sorted deployment %q", first, "lab")
	}
	for i := 0; i < 5; i++ {
		if got := DefaultDoctorEnv(); got != first {
			t.Fatalf("DefaultDoctorEnv() returned %q then %q — map iteration order is leaking, so the "+
				"advisory would report on a different deployment each run", first, got)
		}
	}
}

// Outside an instance there is nothing to resolve, and the old constant is still
// the right answer: a bare `llz doctor` in the template repo must not start
// failing.
func TestDefaultDoctorEnvFallsBackOutsideAnInstance(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := DefaultDoctorEnv(); got != FallbackDoctorEnv {
		t.Errorf("DefaultDoctorEnv() = %q outside an instance, want the %q fallback", got, FallbackDoctorEnv)
	}
}

// The flag has to stay reachable, and its default must be the fallback rather
// than a resolved value: resolution happens in RunE, so that building the command
// tree (which every `llz` invocation does, including `--help`) never stats the
// filesystem or parses YAML.
func TestDoctorEnvFlagDefaultsToTheFallback(t *testing.T) {
	f := DoctorCmd().Flags().Lookup("env")
	if f == nil {
		t.Fatal("`llz doctor` lost its --env flag")
	}
	if f.DefValue != FallbackDoctorEnv {
		t.Errorf("--env default is %q, want the cheap %q fallback — resolving at construction time "+
			"would put a spec parse on every `llz` command", f.DefValue, FallbackDoctorEnv)
	}
}
