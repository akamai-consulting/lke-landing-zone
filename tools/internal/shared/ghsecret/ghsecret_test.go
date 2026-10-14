package ghsecret_test

// Set and Delete shell out to `gh` and are covered by the untestable-loc budget,
// not here. Mask is the half that has a decision in it, and the decision is the
// empty-value guard: emitting `::add-mask::` for "" masks every empty string in
// the log, which is every line break GitHub renders.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghsecret"
)

func TestMaskOnlyUnderActionsAndOnlyForValues(t *testing.T) {
	for _, tc := range []struct {
		name, actions, value string
		want                 bool
	}{
		{"masks under actions", "true", "s3cret", true},
		{"silent off actions", "", "s3cret", false},
		{"silent for empty value", "true", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", tc.actions)
			out := captureStdout(t, func() { ghsecret.Mask(tc.value) })
			if got := out != ""; got != tc.want {
				t.Errorf("emitted=%v want %v (output %q)", got, tc.want, out)
			}
		})
	}
}
