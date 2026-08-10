package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/openbao"
)

// ci_openbao_lifecycle_test.go — the flag-set tests that came back with the
// cobra wiring. Filename-as-subject, ninth occurrence: these lived beside the
// lane tests and travelled with a file whose subject was not theirs.

func TestRunCIBaoEnsureReadyDryRunAndWiring(t *testing.T) {
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		t.Error("dry-run must not exec")
		return "", "", nil
	})
	if err := openbao.RunEnsureReady(true, "primary", time.Second, time.Second); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if err := openbao.RunEnsureReady(false, "", time.Second, time.Second); err == nil || !strings.Contains(err.Error(), "--region") {
		t.Errorf("missing region = %v, want --region error", err)
	}
	if c := openbao.BaoEnsureReadyCmd(); c.Use != "bao-ensure-ready" {
		t.Errorf("Use = %q, want bao-ensure-ready", c.Use)
	}
}

// TestRunCIBaoEnsureReadyRegeneratesFromQuorumWithoutARootToken covers the state
// the tooling itself creates and used to wedge on.
//
// Bootstrap tells the operator to delete OPENBAO_ROOT_TOKEN once the run is done
// (and `llz status` nags until they do), so every RE-RUN of bootstrap-openbao
// arrives with no token. The regen gate used to require a NON-EMPTY token, which
// made openbao.RunRegenRootCI's own "No OPENBAO_ROOT_TOKEN set — regenerating via
// quorum" branch unreachable: the run reported available=false, silently skipped
// configure and every seed, and failed ~20 minutes later at the converge gate
// blaming unconverged apps. With the recovery quorum present, regenerate.
