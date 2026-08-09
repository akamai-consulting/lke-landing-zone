package tokeninv

import (
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
)

func TestClassifyAuthStatus(t *testing.T) {
	cases := []struct {
		code int
		want ValidityStatus
	}{
		{0, VUnreachable},
		{200, VValid},
		{204, VValid},
		{401, VInvalid},
		{403, VInvalid},
		{500, VUnreachable},
		{404, VUnreachable},
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
		want  ValidityStatus
	}{
		{health.PATOK, VValid},
		{health.PATWarn, VWarn},
		{health.PATOverPolicy, VWarn},
		{health.PATNoExpiry, VWarn},
		{health.PATUnparseable, VWarn},
		{health.PATInvalid, VInvalid},
		{health.PATExpired, VInvalid},
		{health.PATUnreachable, VUnreachable},
	}
	for _, tc := range cases {
		got := githubPATValidity("T", tc.state, 30, 200)
		if got.Status != tc.want {
			t.Errorf("githubPATValidity(%v) = %v, want %v", tc.state, got.Status, tc.want)
		}
	}
}

func TestKindFor(t *testing.T) {
	cases := map[string]TokenKind{
		"LINODE_API_TOKEN":            kindLinode,
		"LINODE_DNS_TOKEN":            kindLinode,
		"OPENBAO_SECRETS_WRITE_TOKEN": kindGitHub,
		"APL_VALUES_REPO_TOKEN":       kindGitHub,
		"E2E_DISPATCH_TOKEN":          kindGitHub,
		"GHCR_READ_TOKEN":             kindGHCR,
		"TF_STATE_ACCESS_KEY":         KindS3,
		"TF_STATE_SECRET_KEY":         KindS3,
		"TF_STATE_BUCKET":             KindNone,
		"HARBOR_URL":                  KindNone,
	}
	for name, want := range cases {
		if got := KindFor(name); got != want {
			t.Errorf("KindFor(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestProbeToken_DispatchesByKind(t *testing.T) {
	// Stub the network seams so ProbeToken exercises pure dispatch + classification.
	origLinode, origGHCR, origGH := LinodeProbe, GHCRTokenProbe, GHPATProbe
	t.Cleanup(func() { LinodeProbe, GHCRTokenProbe, GHPATProbe = origLinode, origGHCR, origGH })

	LinodeProbe = func(string) (int, error) { return 200, nil }
	GHCRTokenProbe = func(_, _ string) (int, error) { return 403, nil }
	GHPATProbe = func(_, _ string) (int, string, error) {
		// A valid, well-inside-policy expiry so ClassifyPATResponse → PATOK.
		return 200, time.Now().Add(60 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05 -0700"), nil
	}
	now := time.Now()

	if tv := ProbeToken("LINODE_API_TOKEN", "x", "", now); tv.Status != VValid {
		t.Errorf("linode valid: got %v (%s)", tv.Status, tv.Detail)
	}
	if tv := ProbeToken("GHCR_READ_TOKEN", "x", "user", now); tv.Status != VInvalid {
		t.Errorf("ghcr 403: got %v, want VInvalid", tv.Status)
	}
	if tv := ProbeToken("APL_VALUES_REPO_TOKEN", "x", "", now); tv.Status != VValid {
		t.Errorf("github valid: got %v (%s)", tv.Status, tv.Detail)
	}
	if tv := ProbeToken("TF_STATE_ACCESS_KEY", "x", "", now); tv.Status != VSkipped {
		t.Errorf("s3 not-probed: got %v, want VSkipped", tv.Status)
	}
	if tv := ProbeToken("HARBOR_URL", "x", "", now); tv.Status != VSkipped {
		t.Errorf("non-token: got %v, want VSkipped", tv.Status)
	}
}
