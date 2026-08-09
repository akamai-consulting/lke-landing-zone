package main

import (
	"reflect"
	"testing"

	openbaoext "github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/openbao"
)

// The flag-set tests for `llz openbao`, which stay with the command tree.

func TestOpenbaoExecPassthroughFlags(t *testing.T) {
	baoArgs := []string{"write", "-f", "auth/approle/role/x/secret-id", "-format=json"}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no separator", append([]string{"exec"}, baoArgs...)},
		{"explicit --", append([]string{"exec", "--"}, baoArgs...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, rest, err := openbaoext.OpenbaoCmd().Find(tc.args)
			if err != nil {
				t.Fatalf("Find: %v", err)
			}
			if cmd.Name() != "exec" {
				t.Fatalf("resolved to %q, want exec", cmd.Name())
			}
			if err := cmd.ParseFlags(rest); err != nil {
				t.Fatalf("ParseFlags rejected bao flags: %v", err)
			}
			if got := cmd.Flags().Args(); !reflect.DeepEqual(got, baoArgs) {
				t.Errorf("passthrough args = %v, want %v", got, baoArgs)
			}
		})
	}
}
