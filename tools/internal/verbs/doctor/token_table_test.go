package doctor

// token_table_test.go — the half of token_validate_test.go that stayed,
// because ProbeTokenValidities did: it is keyed by the wizard's `envreq.Requirement`
// and renders `llz doctor`'s table, not a CI verdict.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

func TestProbeTokenValidities_CountsInvalidAndProbesLocalOnly(t *testing.T) {
	origLinode, origGH := tokenprobe.LinodeProbe, tokenprobe.GHPATProbe
	t.Cleanup(func() { tokenprobe.LinodeProbe, tokenprobe.GHPATProbe = origLinode, origGH })
	tokenprobe.LinodeProbe = func(string) (int, error) { return 401, nil } // invalid
	tokenprobe.GHPATProbe = func(_, _ string) (int, string, error) { return 200, "", nil }

	reqs := []envreq.Requirement{
		{Name: "LINODE_API_TOKEN", Secret: true, Required: true},
		{Name: "APL_VALUES_REPO_TOKEN", Secret: true, Required: true}, // no local value → skipped/GH-only
		{Name: "TF_STATE_BUCKET", Secret: false},                      // not a probeable kind
	}
	secrets := map[string]string{"LINODE_API_TOKEN": "dead-token"}
	vars := map[string]string{}
	// APL_VALUES_REPO_TOKEN is set on GitHub but has no local value.
	inst := envreq.NewLiveState(nil, map[string]bool{"APL_VALUES_REPO_TOKEN": true}, nil, nil)

	validity, invalid := ProbeTokenValidities(reqs, secrets, vars, inst, "")
	if invalid != 1 {
		t.Errorf("invalid count = %d, want 1 (the dead Linode token)", invalid)
	}
	if validity["LINODE_API_TOKEN"].Status != tokenprobe.VInvalid {
		t.Errorf("LINODE_API_TOKEN verdict = %v, want tokenprobe.VInvalid", validity["LINODE_API_TOKEN"].Status)
	}
	// APL_VALUES_REPO_TOKEN is set on GitHub but has no local value → CI-only skip.
	if validity["APL_VALUES_REPO_TOKEN"].Status != tokenprobe.VSkipped {
		t.Errorf("APL_VALUES_REPO_TOKEN verdict = %v, want tokenprobe.VSkipped (no local value)", validity["APL_VALUES_REPO_TOKEN"].Status)
	}
	// TF_STATE_BUCKET isn't a probeable credential → no entry.
	if _, ok := validity["TF_STATE_BUCKET"]; ok {
		t.Errorf("TF_STATE_BUCKET should not be probed")
	}
}
