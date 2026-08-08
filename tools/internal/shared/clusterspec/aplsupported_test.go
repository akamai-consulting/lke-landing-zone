package clusterspec

// AplVersionSupported and ResolveAplChartVersion arrived here from
// internal/extensions/assertplatform at 0%, which is worth noting given what the
// predicate is FOR: the v6 migration made the template apl-core-6.x-only in ways
// that do not fail until the cluster is already up, and then only as cryptic pod
// errors. An unsupported version reaching a cluster is exactly the class of bug
// this check exists to catch before anything is built.

import (
	"strings"
	"testing"
)

func TestAplVersionSupported(t *testing.T) {
	for _, tc := range []struct {
		name, v, wantErr string
	}{
		{name: "at the floor", v: MinSupportedAplChartVersion},
		{name: "above the floor", v: "6.5.0"},
		{name: "v-prefixed", v: "v6.2.0"},
		{name: "below the floor", v: "5.9.9", wantErr: "NOT supported"},
		// A non-semver is refused rather than treated as "probably fine" — a
		// branch name or a placeholder here would otherwise sail past the floor.
		{name: "not a semver", v: "main", wantErr: "is not a semver"},
		{name: "empty", v: "", wantErr: "is not a semver"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := AplVersionSupported(tc.v, "lab")
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("AplVersionSupported(%q) = %v, want nil", tc.v, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("AplVersionSupported(%q) = nil, want an error", tc.v)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
			// The deployment name is in every message: an operator with four
			// deployments needs to know which one is pinned wrong.
			if !strings.Contains(err.Error(), "lab") {
				t.Errorf("error %q does not name the deployment", err)
			}
		})
	}
}

// ResolveAplChartVersion falls back to the baseline when no spec is present, and
// that is deliberate: a missing spec means "the default applies", not an error.
func TestResolveAplChartVersionFallsBackToBaseline(t *testing.T) {
	t.Chdir(t.TempDir())
	got, err := ResolveAplChartVersion("lab")
	if err != nil {
		t.Fatalf("ResolveAplChartVersion: %v", err)
	}
	if got != BaselineAplChartVersion {
		t.Errorf("no spec on disk = %q, want the baseline %q", got, BaselineAplChartVersion)
	}
	// And whatever it resolves to must itself pass the floor, or a clean install
	// would fail its own preflight.
	if err := AplVersionSupported(got, "lab"); err != nil {
		t.Errorf("the baseline does not satisfy the supported-version floor: %v", err)
	}
}
