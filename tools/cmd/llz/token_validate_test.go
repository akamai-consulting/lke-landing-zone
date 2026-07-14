package main

import (
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
)

func TestClassifyAuthStatus(t *testing.T) {
	cases := []struct {
		code int
		want validityStatus
	}{
		{0, vUnreachable},
		{200, vValid},
		{204, vValid},
		{401, vInvalid},
		{403, vInvalid},
		{500, vUnreachable},
		{404, vUnreachable},
	}
	for _, tc := range cases {
		if got, _ := classifyAuthStatus(tc.code); got != tc.want {
			t.Errorf("classifyAuthStatus(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestGithubPATValidity(t *testing.T) {
	cases := []struct {
		state health.PATCheckState
		want  validityStatus
	}{
		{health.PATOK, vValid},
		{health.PATWarn, vWarn},
		{health.PATOverPolicy, vWarn},
		{health.PATNoExpiry, vWarn},
		{health.PATUnparseable, vWarn},
		{health.PATInvalid, vInvalid},
		{health.PATExpired, vInvalid},
		{health.PATUnreachable, vUnreachable},
	}
	for _, tc := range cases {
		got := githubPATValidity("T", tc.state, 30, 200)
		if got.status != tc.want {
			t.Errorf("githubPATValidity(%v) = %v, want %v", tc.state, got.status, tc.want)
		}
	}
}

func TestKindFor(t *testing.T) {
	cases := map[string]tokenKind{
		"LINODE_API_TOKEN":            kindLinode,
		"LINODE_DNS_TOKEN":            kindLinode,
		"OPENBAO_SECRETS_WRITE_TOKEN": kindGitHub,
		"APL_VALUES_REPO_TOKEN":       kindGitHub,
		"E2E_DISPATCH_TOKEN":          kindGitHub,
		"GHCR_READ_TOKEN":             kindGHCR,
		"TF_STATE_ACCESS_KEY":         kindS3,
		"TF_STATE_SECRET_KEY":         kindS3,
		"TF_STATE_BUCKET":             kindNone,
		"HARBOR_URL":                  kindNone,
	}
	for name, want := range cases {
		if got := kindFor(name); got != want {
			t.Errorf("kindFor(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestProbeToken_DispatchesByKind(t *testing.T) {
	// Stub the network seams so probeToken exercises pure dispatch + classification.
	origLinode, origGHCR, origGH := linodeProbe, ghcrTokenProbe, ghPATProbe
	t.Cleanup(func() { linodeProbe, ghcrTokenProbe, ghPATProbe = origLinode, origGHCR, origGH })

	linodeProbe = func(string) (int, error) { return 200, nil }
	ghcrTokenProbe = func(_, _ string) (int, error) { return 403, nil }
	ghPATProbe = func(_, _ string) (int, string, error) {
		// A valid, well-inside-policy expiry so ClassifyPATResponse → PATOK.
		return 200, time.Now().Add(60 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05 -0700"), nil
	}
	now := time.Now()

	if tv := probeToken("LINODE_API_TOKEN", "x", "", now); tv.status != vValid {
		t.Errorf("linode valid: got %v (%s)", tv.status, tv.detail)
	}
	if tv := probeToken("GHCR_READ_TOKEN", "x", "user", now); tv.status != vInvalid {
		t.Errorf("ghcr 403: got %v, want vInvalid", tv.status)
	}
	if tv := probeToken("APL_VALUES_REPO_TOKEN", "x", "", now); tv.status != vValid {
		t.Errorf("github valid: got %v (%s)", tv.status, tv.detail)
	}
	if tv := probeToken("TF_STATE_ACCESS_KEY", "x", "", now); tv.status != vSkipped {
		t.Errorf("s3 not-probed: got %v, want vSkipped", tv.status)
	}
	if tv := probeToken("HARBOR_URL", "x", "", now); tv.status != vSkipped {
		t.Errorf("non-token: got %v, want vSkipped", tv.status)
	}
}

func TestProbeTokenValidities_CountsInvalidAndProbesLocalOnly(t *testing.T) {
	origLinode, origGH := linodeProbe, ghPATProbe
	t.Cleanup(func() { linodeProbe, ghPATProbe = origLinode, origGH })
	linodeProbe = func(string) (int, error) { return 401, nil } // invalid
	ghPATProbe = func(_, _ string) (int, string, error) { return 200, "", nil }

	reqs := []requirement{
		{Name: "LINODE_API_TOKEN", Secret: true, Required: true},
		{Name: "APL_VALUES_REPO_TOKEN", Secret: true, Required: true}, // no local value → skipped/GH-only
		{Name: "TF_STATE_BUCKET", Secret: false},                      // not a probeable kind
	}
	secrets := map[string]string{"LINODE_API_TOKEN": "dead-token"}
	vars := map[string]string{}
	// APL_VALUES_REPO_TOKEN is set on GitHub but has no local value.
	inst := liveState{repoSecrets: map[string]bool{"APL_VALUES_REPO_TOKEN": true}}

	validity, invalid := probeTokenValidities(reqs, secrets, vars, inst, "")
	if invalid != 1 {
		t.Errorf("invalid count = %d, want 1 (the dead Linode token)", invalid)
	}
	if validity["LINODE_API_TOKEN"].status != vInvalid {
		t.Errorf("LINODE_API_TOKEN verdict = %v, want vInvalid", validity["LINODE_API_TOKEN"].status)
	}
	// APL_VALUES_REPO_TOKEN is set on GitHub but has no local value → CI-only skip.
	if validity["APL_VALUES_REPO_TOKEN"].status != vSkipped {
		t.Errorf("APL_VALUES_REPO_TOKEN verdict = %v, want vSkipped (no local value)", validity["APL_VALUES_REPO_TOKEN"].status)
	}
	// TF_STATE_BUCKET isn't a probeable credential → no entry.
	if _, ok := validity["TF_STATE_BUCKET"]; ok {
		t.Errorf("TF_STATE_BUCKET should not be probed")
	}
}
