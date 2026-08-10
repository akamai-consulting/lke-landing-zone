// Command llz is the adopter-facing front-end for standing up and maintaining an
// LKE landing-zone instance. It does not reimplement the bootstrap — it
// orchestrates the existing tools the quickstart documents (`copier`, `gh`,
// `kubectl`, the repo's `scripts/*.sh`, the Linode API) and adds the token wizard
// that provisions every credential the instance needs.
//
// EVERYTHING IS IN internal/cli, AND THIS FILE IS THE POINT. package main cannot
// be imported, so for as long as the command tree lived here nothing outside it
// could construct one — which is why the guard that every extension verb is
// reachable had to be written inside main, why the registry could not drive a
// command main assembled, and why ADR 0014 had to budget this package by line
// count instead of by boundary.
//
// So main holds the two things that genuinely cannot move: the `main` symbol, and
// the os.Exit that must not run inside a testable function. A budget in
// .core-surface-budget.yaml pins this file at its current size, exactly, so the
// package cannot re-accrete — and cmd_llz_imports_test.go refuses any import here
// but internal/cli.
//
// See docs/quickstart.md for the end-to-end flow.
package main

import (
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
)

func main() { os.Exit(cli.Main()) }
