package tokenprobe

// TWO OF THE FOUR tests in tokeninv's token_validate_test.go, and only two. The
// other two drive the inventory's validation lane and correctly stayed. These
// drive classifyAuthStatus and githubPATValidity, which are unexported here -- so
// once the probe layer moved they could not have stayed behind even if the file
// had been left alone. Checking each `func Test` rather than moving the file is
// what separated them.

import (
	"strings"
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
	t.Cleanup(func() {
		LinodeProbe, GHCRTokenProbe, GHPATProbe = origLinode, origGHCR, origGH
	})

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

// ValidityCell is what an operator actually reads. It is pure rendering over the
// four statuses, and it arrived here uncovered -- which matters because a wrong
// glyph does not fail anything, it just quietly tells someone their credential is
// fine.
func TestValidityCell(t *testing.T) {
	for _, tc := range []struct {
		name string
		tv   TokenValidity
		want string
	}{
		{"valid", TokenValidity{Name: "T", Status: VValid, Detail: "ok"}, "ok"},
		{"invalid", TokenValidity{Name: "T", Status: VInvalid, Detail: "401"}, "401"},
		{"warn", TokenValidity{Name: "T", Status: VWarn, Detail: "expires in 3d"}, "expires in 3d"},
		{"skipped", TokenValidity{Name: "T", Status: VSkipped}, "not probed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidityCell(tc.tv)
			if !strings.Contains(stripANSI(got), tc.want) {
				t.Errorf("ValidityCell(%v) = %q, want it to contain %q", tc.tv, got, tc.want)
			}
		})
	}
}

// The cells are colourised, so the assertions above compare against plain text.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
