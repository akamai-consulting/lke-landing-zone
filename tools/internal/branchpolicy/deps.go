package branchpolicy

// deps.go — the one edge this package could not bring with it.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"

// execOutput delegates to kubectlprobe.Exec through a CLOSURE, never by
// assignment — a direct assignment snapshots the seam before any test can swap it.
//
// The name is inherited and slightly wrong here: nothing in this package runs
// kubectl. It runs `gh api`. kubectlprobe.Exec has always taken the binary as its
// first argument, so it is the right seam; the package it lives in is named for
// its first ten callers rather than for what it does.
func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }
