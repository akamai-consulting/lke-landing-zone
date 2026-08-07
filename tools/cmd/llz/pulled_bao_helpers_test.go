package main

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghsecret"
)

// pulled_helpers_test.go — helpers the moved tests use, copied across the new
// package boundary. Copied, not shared: each takes a *testing.T.

// withGHSetSecret swaps the gh-secret seam, recording "name@env" calls.
func withGHSetSecret(t *testing.T, fail func(name string) error) *[]string {
	t.Helper()
	orig := ghsecret.SetFn
	calls := new([]string)
	ghsecret.SetFn = func(name, ghEnv, value string) error {
		*calls = append(*calls, name+"@"+ghEnv+"="+value)
		if fail != nil {
			return fail(name)
		}
		return nil
	}
	t.Cleanup(func() { ghsecret.SetFn = orig })
	return calls
}
