package branchpolicy

// Followed HasMainBranchRule here — it parses GitHub's Deployment-branch-policy
// response, which is this package's business.

import (
	"errors"
	"testing"
)

func TestHasMainBranchRule(t *testing.T) {
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "gh" || len(args) == 0 || args[0] != "api" {
			t.Errorf("HasMainBranchRule shelled out to %q %v, want gh api ...", name, args)
		}
		return []byte(`{"branch_policies":[{"name":"main"},{"name":"release/*"}]}`), nil
	})
	if !HasMainBranchRule("o/r", "infra-dev", "main") {
		t.Error("HasMainBranchRule(main present) = false, want true")
	}
	if HasMainBranchRule("o/r", "infra-dev", "develop") {
		t.Error("HasMainBranchRule(absent) = true, want false")
	}

	// A gh failure is reported as "no rule" (false), never a panic.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("gh down") })
	if HasMainBranchRule("o/r", "infra-dev", "main") {
		t.Error("HasMainBranchRule(gh error) = true, want false")
	}
}
