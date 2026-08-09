package openbao

// The two `bao` output parsers came here from internal/extensions/baolifecycle and
// their tests came with them. TestParseTokenAndPolicies was TWO tests wearing one
// name -- parseTokenField stayed behind with the regen-root lifecycle, and only
// the policy check moved -- so it was split rather than moved or kept.

import (
	"testing"
)

func TestParseBaoStatus(t *testing.T) {
	sealed, th := ParseStatus(`{"sealed":false,"t":3,"n":5}`)
	if sealed || th != 3 {
		t.Errorf("got sealed=%v t=%d, want false 3", sealed, th)
	}
	if s, _ := ParseStatus(`{"sealed":true,"t":2}`); !s {
		t.Error("want sealed=true")
	}
}

func TestPoliciesIncludeRoot(t *testing.T) {
	if !PoliciesIncludeRoot(`{"data":{"policies":["default","root"]}}`) {
		t.Error("should include root")
	}
	if PoliciesIncludeRoot(`{"data":{"policies":["default"]}}`) {
		t.Error("should not include root")
	}
}
