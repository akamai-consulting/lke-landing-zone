package main

import (
	"reflect"
	"testing"
)

func TestBaoExecArgv(t *testing.T) {
	got := baoExecArgv("platform-openbao-0", "s.tok", []string{"policy", "list"})
	want := []string{
		"-n", "llz-openbao", "exec", "-i", "-c", "openbao", "platform-openbao-0", "--",
		"env", "VAULT_ADDR=https://127.0.0.1:8200", "VAULT_SKIP_VERIFY=true", "VAULT_TOKEN=s.tok", "bao",
		"policy", "list",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("baoExecArgv\n got: %v\nwant: %v", got, want)
	}
	// bao's own flags must survive untouched as trailing args.
	got2 := baoExecArgv("platform-openbao-0", "t", []string{"write", "-f", "auth/approle/role/x/secret-id", "-format=json"})
	tail := got2[len(got2)-4:]
	if !reflect.DeepEqual(tail, []string{"write", "-f", "auth/approle/role/x/secret-id", "-format=json"}) {
		t.Errorf("bao flags not passed through: %v", tail)
	}
}

// `exec` sets SetInterspersed(false) so bao's own flags pass through cobra
// WITHOUT a `--` separator (the workflow callers omit it). Lock that in, and
// confirm the explicit `--` form still works.
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
			cmd, rest, err := openbaoCmd().Find(tc.args)
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
