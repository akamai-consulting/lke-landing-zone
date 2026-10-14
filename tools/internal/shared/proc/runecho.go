package proc

// runecho.go — announce a command, then run it (or not).
//
// Twenty-one call sites in package main did exactly this: print `→ <quoted argv>`
// to stderr, return early under --dry-run, otherwise Run. It lived in commands.go
// as `run`, a name so generic that the closure scanner could not tell it from a
// method and neither could a reader.
//
// The DRY-RUN CHECK IS THE POINT and it is why this is not just sugar. Every one
// of those call sites is a mutating action, and the announcement and the guard
// have to stay together — a caller that prints without checking, or checks without
// printing, is a bug you only find by running the thing you were trying not to run.

import (
	"fmt"
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghcli"
)

// run executes argv, streaming stdio. In dry-run it prints and returns.
func RunEcho(dryRun bool, argv ...string) error {
	fmt.Fprintln(os.Stderr, "→ "+ghcli.Quote(argv))
	if dryRun {
		return nil
	}
	return Run(argv, "")
}
