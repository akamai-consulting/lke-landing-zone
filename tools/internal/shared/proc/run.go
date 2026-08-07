// Package proc runs a command ATTACHED TO THE OPERATOR'S TERMINAL.
//
// It is the streaming counterpart to internal/kubectlprobe, and the two are
// separate on purpose. kubectlprobe CAPTURES output so a check can classify it;
// this one hands stdout, stderr and stdin straight through, because its callers
// are running copier, gh, tofu and git for a human who needs to see the output as
// it happens — and, for the ones that prompt, to answer them.
//
// Eight files in package main called it and it lived in commands.go, a
// 1,100-line file about the `llz` subcommands. A process runner is not a
// subcommand.
package proc

import (
	"os"
	"os/exec"
	"strings"
)

// execArgv runs argv with an optional stdin string (used to pipe secret values
// into `gh secret set` without putting them in the process arguments).
func Run(argv []string, stdin string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	} else {
		cmd.Stdin = os.Stdin
	}
	return cmd.Run()
}
