package onboard

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorFailsOnMissingRequiredConfig pins the gate's contract.
//
// cmdDoctorE2E used to PRINT "✗ N required item(s) missing" and then return nil,
// so `llz doctor --env <env>` exited 0 on an instance that could not build. Its
// own docs say "Green when every required item is set", and `llz up` runs it as
// the stage between the token wizard and the dispatch — so with --skip-tokens
// (which the quickstart documents) a missing credential reached CI rather than
// the gate built to catch it.
func TestDoctorFailsOnMissingRequiredConfig(t *testing.T) {
	withLookPath(t, func(f string) (string, error) { return "/usr/bin/" + f, nil })
	dir := chdirTempDir(t)
	mustWrite(t, filepath.Join(dir, ".copier-answers.yml"), "instance_repo: acme/inst\n")

	// A repo that exists but has NO variables and NO secrets: every required item
	// is missing. gh answers every lookup with an empty listing.
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "gh" {
			return nil, nil
		}
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "/environments"):
			return []byte(`{"environments":[]}`), nil
		case strings.Contains(joined, "secrets"):
			return []byte(`{"secrets":[]}`), nil
		case strings.Contains(joined, "variables"):
			return []byte(`{"variables":[]}`), nil
		}
		return []byte(`{}`), nil
	})

	var err error
	out := captureStdout(t, func() { err = DoctorE2E("acme/inst", "lab", false) })
	if err == nil {
		t.Fatalf("doctor must FAIL when required config is missing; it printed:\n%s", out)
	}
	if !strings.Contains(err.Error(), "required item(s) not set") {
		t.Errorf("error should name the shortfall, got: %v", err)
	}
	// The fix has to travel with the refusal — that is this codebase's standard.
	if !strings.Contains(err.Error(), "llz tokens") {
		t.Errorf("error should carry the remediation, got: %v", err)
	}
}
