package main

// Followed hasMainBranchRule here — it parses GitHub's Deployment-branch-policy
// response, which is this package's business.

import (
	"errors"
	"testing"
)

func TestHasMainBranchRule(t *testing.T) {
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "gh" || len(args) == 0 || args[0] != "api" {
			t.Errorf("hasMainBranchRule shelled out to %q %v, want gh api ...", name, args)
		}
		return []byte(`{"branch_policies":[{"name":"main"},{"name":"release/*"}]}`), nil
	})
	if !hasMainBranchRule("o/r", "infra-dev", "main") {
		t.Error("hasMainBranchRule(main present) = false, want true")
	}
	if hasMainBranchRule("o/r", "infra-dev", "develop") {
		t.Error("hasMainBranchRule(absent) = true, want false")
	}

	// A gh failure is reported as "no rule" (false), never a panic.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("gh down") })
	if hasMainBranchRule("o/r", "infra-dev", "main") {
		t.Error("hasMainBranchRule(gh error) = true, want false")
	}
}
