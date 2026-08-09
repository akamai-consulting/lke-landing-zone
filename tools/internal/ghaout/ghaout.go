// Package ghaout appends to the GitHub Actions file-based output channels.
//
// One function, 25 callers, and it lived in cmd/llz because that is where every
// caller was. The OpenBao lifecycle extraction took the first caller OUT of
// package main, and the alternative was to give internal/baolifecycle a seam for
// it — a seam for a fifteen-line append with no decision in it, installed by
// main, so a test could stub what it already controls with an env var and a temp
// file.
//
// Moved VERBATIM, error strings included. Two tests assert on "open $%s"/
// "write $%s" and rewording them to read better would have been a behaviour
// change disguised as a move.
package ghaout

import (
	"fmt"
	"os"
)

// appendGHAFile appends lines to the GitHub Actions command file named by
// envVar (GITHUB_OUTPUT / GITHUB_ENV / GITHUB_STEP_SUMMARY). Outside Actions
// the variable is unset and the write is skipped, keeping the commands
// runnable from a workstation.
// appendGHAFile appends lines to the GitHub Actions command file named by
// envVar (GITHUB_OUTPUT / GITHUB_ENV / GITHUB_STEP_SUMMARY). Outside Actions
// the variable is unset and the write is skipped, keeping the commands
// runnable from a workstation.
func Append(envVar string, lines ...string) error {
	path := os.Getenv(envVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open $%s: %w", envVar, err)
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			f.Close()
			return fmt.Errorf("write $%s: %w", envVar, err)
		}
	}
	return f.Close()
}

// ── recovery keys ─────────────────────────────────────────────────────────────

// baoread.RecoveryKeysFromEnv reads the 3 quorum recovery keys from RECOVERY_K1/2/3.
// Under the chart's `seal "static"` auto-unseal, `operator init` yields recovery
// shares (not unseal keys): the seal mechanism unseals every pod at boot, so
// there is no submit-keys-to-unseal step. The recovery keys exist only to
// authorize the `operator generate-root` quorum that bao-regen-root runs.
