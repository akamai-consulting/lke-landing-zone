package main

import (
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// An unknown subcommand under any command GROUP must fail loud (non-nil error,
// "unknown command"), not silently print help and exit 0. This is the regression
// guard for the stale-image skew that no-op'd the cluster-bootstrap
// apl_pipeline_ready gate: the baked llz lacked `ci wait-apl-pipeline`, so
// `llz ci wait-apl-pipeline` printed help, exited 0, and the AppProject apply
// raced the Argo CD CRDs. Cobra only auto-rejects unknown subcommands at the
// root; hardenUnknownSubcommands extends that to every group.
func TestUnknownSubcommandFailsLoud(t *testing.T) {
	groups := [][]string{
		{"ci", "wait-apl-pipeline-typo"}, // the command group that actually bit us
		{"ci", "definitely-not-a-subcommand"},
		{"env", "not-a-real-subcommand"},
		{"credentials", "not-a-real-subcommand"},
		{"check", "not-a-real-subcommand"},  // hidden group (lint/validate dispatch here)
	}
	for _, args := range groups {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := newRootCmd()
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs(args)
			err := root.Execute()
			if err == nil {
				t.Fatalf("`llz %s` returned nil error; an unknown subcommand must fail loud", strings.Join(args, " "))
			}
			if !strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("`llz %s` error = %q, want it to mention \"unknown command\"", strings.Join(args, " "), err)
			}
		})
	}
}

// A bare group invocation (`llz ci` with no subcommand) must stay benign: print
// help and exit 0. Hardening unknown subcommands must not regress this.
func TestBareGroupPrintsHelpExitsZero(t *testing.T) {
	for _, group := range []string{"ci", "env", "credentials", "check"} {
		t.Run(group, func(t *testing.T) {
			root := newRootCmd()
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs([]string{group})
			if err := root.Execute(); err != nil {
				t.Fatalf("`llz %s` returned %v; a bare group should print help and exit 0", group, err)
			}
		})
	}
}

// hardenUnknownSubcommands targets only non-runnable groups: it must not clobber
// a leaf command's own Args validator, nor stamp NoArgs on a runnable command.
func TestHardenUnknownSubcommandsLeavesLeavesAndRunnablesAlone(t *testing.T) {
	leaf := &cobra.Command{Use: "leaf", Args: cobra.ExactArgs(2), Run: func(*cobra.Command, []string) {}}
	runnableParent := &cobra.Command{Use: "runnable", Run: func(*cobra.Command, []string) {}}
	runnableParent.AddCommand(&cobra.Command{Use: "child", Run: func(*cobra.Command, []string) {}})
	group := &cobra.Command{Use: "group"}
	group.AddCommand(&cobra.Command{Use: "child", Run: func(*cobra.Command, []string) {}})
	root := &cobra.Command{Use: "root"}
	root.AddCommand(leaf, runnableParent, group)

	hardenUnknownSubcommands(root)

	if leaf.Args == nil {
		t.Error("leaf Args validator was cleared")
	}
	if runnableParent.Args != nil {
		t.Error("runnable parent should not have been hardened")
	}
	if group.Args == nil {
		t.Error("non-runnable group should have been hardened with NoArgs")
	}
}
