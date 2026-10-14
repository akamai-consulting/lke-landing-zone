package teardown

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
)

// TestShouldWaitForDetachMatchesTheClientBinding.
//
// waitVolumesDetached POSTs /detach, and the client it uses is built from
// `cloudBinding(g.Yes && !g.DryRun)` — so on a dry run the READ binding refuses
// every POST, the wait can never converge, and it burns its full budget (600s on
// the destroy job's real flags) emitting a warning per volume per poll before the
// sweep it precedes prints "would delete" anyway.
//
// Both arms matter and neither had a test: `if false` here silently resurrects
// the 16-orphan-volume incident the wait exists for, and `if true` restores the
// ten-minute dry-run stall.
func TestShouldWaitForDetachMatchesTheClientBinding(t *testing.T) {
	for name, tc := range map[string]struct {
		yes, dryRun, want bool
	}{
		"confirmed, not a dry run": {true, false, true},
		"dry run overrides --yes":  {true, true, false},
		"unconfirmed":              {false, false, false},
		"neither":                  {false, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			g := cliopts.Opts{Yes: tc.yes, DryRun: tc.dryRun}
			got := shouldWaitForDetach(g)
			if got != tc.want {
				t.Errorf("shouldWaitForDetach(Yes=%v DryRun=%v) = %v, want %v", tc.yes, tc.dryRun, got, tc.want)
			}
			// The predicate must agree with the binding the client is built from,
			// or the wait runs against a transport that refuses it.
			if mutating := tc.yes && !tc.dryRun; got != mutating {
				t.Errorf("the wait ran=%v while the client binding was mutating=%v — they must be the "+
					"same expression", got, mutating)
			}
		})
	}
}
