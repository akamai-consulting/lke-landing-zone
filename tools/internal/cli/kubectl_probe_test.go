package cli

import (
	"os"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// TestMain zeroes the probe retry delay for the whole package. Every probe now
// retries an unanswerable kubectl call, and the tests stub execOutput with
// errors — without this each stubbed failure would pay two real 3s sleeps.
// It also catches renderRootsFn's `<self> render <env> --tfvars-only` shell-out:
// under `go test` os.Executable() is THIS binary, so without the guard every
// shell-out re-runs the whole suite (recursively) and the run hangs instead of
// failing. See renderReexecChild in fetchkubeconfig_state_deadline_test.go.
func TestMain(m *testing.M) {
	kubectlprobe.Delay = 0
	if renderReexecChild() {
		os.Exit(0)
	}
	os.Exit(m.Run())
}
