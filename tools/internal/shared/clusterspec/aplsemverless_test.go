package clusterspec

import "testing"

// AplSemverLess was added for internal/assertplatform's chart-floor check and
// shipped without a direct test — it was exercised only through that lane, which
// left this package a fraction under its coverage floor and, more to the point,
// left the UNPARSEABLE ordering unasserted. That ordering is the load-bearing part:
// an unset or malformed pin must sort BELOW the floor so the gate refuses it,
// rather than sorting above and passing silently.
func TestAplSemverLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
		why  string
	}{
		{"6.0.0", "6.1.0", true, "minor"},
		{"6.1.0", "6.0.0", false, "minor, reversed"},
		{"5.9.9", "6.0.0", true, "major dominates"},
		{"6.1.0", "6.1.1", true, "patch"},
		{"6.1.0", "6.1.0", false, "equal is not less"},
		{"v6.0.0", "6.1.0", true, "a leading v is tolerated"},
		{"6.1.0-rc.1", "6.1.0", false, "a pre-release suffix is ignored, so these compare equal"},
		{"", "6.0.0", true, "an UNSET pin must sort below the floor, or the gate passes it"},
		{"not-a-version", "6.0.0", true, "a MALFORMED pin must sort below the floor too"},
		{"6.0.0", "", false, "and nothing sorts below unparseable"},
		{"garbage", "rubbish", false, "two unparseable versions are not ordered"},
	} {
		if got := AplSemverLess(c.a, c.b); got != c.want {
			t.Errorf("AplSemverLess(%q, %q) = %v, want %v — %s", c.a, c.b, got, c.want, c.why)
		}
	}
}
