package main

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

func lzWith(region string, toggles map[string]clusterspec.ComponentToggle) *clusterspec.LandingZone {
	return &clusterspec.LandingZone{
		Spec: clusterspec.Spec{
			Environments: map[string]clusterspec.Environment{
				region: {Components: toggles},
			},
		},
	}
}

func boolp(b bool) *bool { return &b }

func TestBroadPATSeedEnabled(t *testing.T) {
	tests := []struct {
		name   string
		lz     *clusterspec.LandingZone
		region string
		want   bool
	}{
		{
			name:   "enabled for the region",
			lz:     lzWith("e2e", map[string]clusterspec.ComponentToggle{"broadPatRotator": {Enabled: boolp(true)}}),
			region: "e2e",
			want:   true,
		},
		{
			name:   "explicitly disabled",
			lz:     lzWith("e2e", map[string]clusterspec.ComponentToggle{"broadPatRotator": {Enabled: boolp(false)}}),
			region: "e2e",
			want:   false,
		},
		{
			// broadPatRotator is DefaultDisabled, so an env that never mentions it is off.
			name:   "absent toggle → default-disabled",
			lz:     lzWith("e2e", map[string]clusterspec.ComponentToggle{}),
			region: "e2e",
			want:   false,
		},
		{
			name:   "unknown region",
			lz:     lzWith("e2e", map[string]clusterspec.ComponentToggle{"broadPatRotator": {Enabled: boolp(true)}}),
			region: "prod",
			want:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := broadPATSeedEnabled(tc.lz, tc.region); got != tc.want {
				t.Errorf("broadPATSeedEnabled(%q) = %v, want %v", tc.region, got, tc.want)
			}
		})
	}
}
