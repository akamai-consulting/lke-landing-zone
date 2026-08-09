package main

// ci_openbao_lifecycle_flags_test.go — the --region requirement for the three
// OpenBao lifecycle verbs.
//
// It used to sit in the bao-status test, which moved to internal/baoread with
// its command. These three did not move — their flag sets are still main's — so
// the assertion came back rather than dragging them across a package boundary to
// keep one table together.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/baolifecycle"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/identityconfig"
	"github.com/spf13/cobra"
)

func TestOpenbaoLifecycleCmdsRequireRegion(t *testing.T) {
	for _, mk := range []func() *cobra.Command{baolifecycle.BaoInitCmd, baolifecycle.BaoRegenRootCmd, identityconfig.BaoConfigureCmd} {
		cmd := mk()
		cmd.SetArgs(nil)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--region") {
			t.Errorf("%s without --region: err = %v, want required-flag error", cmd.Use, err)
		}
	}
}
