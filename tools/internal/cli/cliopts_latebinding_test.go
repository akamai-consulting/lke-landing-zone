package cli

// cliopts_latebinding_test.go — the global flags must still be readable by a
// command that lives in another package.
//
// THIS PINS A BUG THAT WOULD OTHERWISE BE SILENT. --dry-run/--yes/--open are
// PERSISTENT flags on the root command, so cobra does not parse them until a
// command executes — long after every constructor has run. Moving commands into
// extension packages made it tempting to thread `dryRun bool` in as a constructor
// argument, which would capture the pre-parse zero value. Nothing would fail:
// --dry-run would simply stop working, on cloud-mutating commands, which is the
// worst possible place for a silently-ignored flag.
//
// So assert the binding moment directly: parse, then read.

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
)

func TestGlobalFlagsAreParsedBeforeRunE(t *testing.T) {
	orig := cliopts.Global
	t.Cleanup(func() { cliopts.Global = orig })

	for _, tc := range []struct {
		args []string
		want cliopts.Opts
	}{
		{[]string{"--dry-run"}, cliopts.Opts{DryRun: true}},
		{[]string{"--yes"}, cliopts.Opts{Yes: true}},
		{[]string{"-y"}, cliopts.Opts{Yes: true}},
		{[]string{"--open"}, cliopts.Opts{Open: true}},
		{[]string{"--dry-run", "--yes"}, cliopts.Opts{DryRun: true, Yes: true}},
	} {
		cliopts.Global = cliopts.Opts{}
		root := newRootCmd()
		var seen cliopts.Opts
		root.RunE = func(_ *cobra.Command, _ []string) error { seen = cliopts.Global; return nil }
		root.SetArgs(tc.args)
		root.SilenceUsage, root.SilenceErrors = true, true
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if seen != tc.want {
			t.Errorf("%v: RunE saw %+v, want %+v — the globals were read before parsing", tc.args, seen, tc.want)
		}
	}
}
