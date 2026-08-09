package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeLKELister struct {
	versions []string
	err      error
	tier     string // records what was asked for
}

func (f *fakeLKELister) ListLKEVersions(_ context.Context, tier string) ([]string, error) {
	f.tier = tier
	return f.versions, f.err
}

// withLKELister installs a fake account; nil models "no LINODE_TOKEN".
func withLKELister(t *testing.T, l lkeVersionLister) {
	t.Helper()
	prev := doctorLinodeClient
	t.Cleanup(func() { doctorLinodeClient = prev })
	doctorLinodeClient = func() lkeVersionLister {
		if l == nil {
			return nil
		}
		return l
	}
}

// The load-bearing property: this section is ADVISORY. Its response shape has
// not been verified against an entitled account, so it must never be able to
// fail a build that would have worked. ReportLinodeAccount returns nothing —
// this test exists so a future refactor cannot quietly give it an error return
// and wire it into doctor's errs.
func TestReportLinodeAccountNeverBlocks(t *testing.T) {
	for _, l := range []lkeVersionLister{
		nil,
		&fakeLKELister{err: errors.New("401 Invalid Token")},
		&fakeLKELister{versions: nil},
		&fakeLKELister{versions: []string{"v1.33.6+lke7"}},
	} {
		withLKELister(t, l)
		// Compiles only while the function has no error return, and cannot panic
		// on any of the four shapes.
		captureStdout(t, func() { ReportLinodeAccount([]string{"v1.33.6+lke7"}) })
	}
}

func TestReportLinodeAccountWithoutAToken(t *testing.T) {
	withLKELister(t, nil)
	out := captureStdout(t, func() { ReportLinodeAccount([]string{"v1.33.6+lke7"}) })
	if !strings.Contains(out, "LINODE_TOKEN") {
		t.Errorf("should say how to enable the check, got:\n%s", out)
	}
	if strings.Contains(out, "NOT in the account") {
		t.Errorf("must not claim a verdict it could not reach:\n%s", out)
	}
}

// An auth failure is the interesting case — it's what a missing entitlement or
// an under-scoped PAT looks like — but it is also what a network blip looks
// like, so it reports as uncertainty, not as a verdict.
func TestReportLinodeAccountAuthFailureIsUncertainty(t *testing.T) {
	withLKELister(t, &fakeLKELister{err: errors.New("GET /v4beta/... returned 401 (check the PAT scope): Invalid Token")})
	out := captureStdout(t, func() { ReportLinodeAccount([]string{"v1.33.6+lke7"}) })
	for _, want := range []string{"could not list", "entitled"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestReportLinodeAccountFlagsAVersionTheAccountDoesNotOffer(t *testing.T) {
	withLKELister(t, &fakeLKELister{versions: []string{"v1.32.4+lke2", "v1.31.8+lke5"}})
	out := captureStdout(t, func() { ReportLinodeAccount([]string{"v1.33.6+lke7"}) })
	if !strings.Contains(out, "NOT in the account") {
		t.Errorf("a retired pin should be flagged:\n%s", out)
	}
	if !strings.Contains(out, "v1.32.4+lke2") {
		t.Errorf("should list what IS offered:\n%s", out)
	}
}

func TestReportLinodeAccountAsksTheEnterpriseTier(t *testing.T) {
	f := &fakeLKELister{versions: []string{"v1.33.6+lke7"}}
	withLKELister(t, f)
	captureStdout(t, func() { ReportLinodeAccount(nil) })
	if f.tier != "enterprise" {
		t.Errorf("tier = %q, want enterprise — the standard catalog answers for a product LLZ does not use", f.tier)
	}
}

// The spec pins a full build; a catalog may list either that or a bare
// major.minor, and the enterprise spelling is unverified. Crying wolf on a good
// spec is worse than silence for an advisory check, so agreement on major.minor
// counts as offered.
func TestLKEVersionOfferedMatchingIsGenerous(t *testing.T) {
	offered := []string{"v1.33.6+lke7", "1.32"}
	for _, want := range []string{"v1.33.6+lke7", "1.33.6", "v1.33.9+lke9", "1.33", "v1.32.0+lke1"} {
		if !lkeVersionOffered(want, offered) {
			t.Errorf("lkeVersionOffered(%q) = false, want true", want)
		}
	}
	for _, want := range []string{"v1.30.1+lke1", "1.29", "", "garbage"} {
		if lkeVersionOffered(want, offered) {
			t.Errorf("lkeVersionOffered(%q) = true, want false", want)
		}
	}
}

func TestMajorMinor(t *testing.T) {
	for in, want := range map[string]string{
		"v1.33.6+lke7": "1.33",
		"1.33.6":       "1.33",
		"1.33":         "1.33",
		"v1.33":        "1.33",
		"1":            "",
		"":             "",
		"garbage":      "",
	} {
		if got := majorMinor(in); got != want {
			t.Errorf("majorMinor(%q) = %q, want %q", in, got, want)
		}
	}
}
