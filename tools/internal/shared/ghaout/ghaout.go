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
	"strings"
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

// ── THE OTHER FILE-BASED CHANNEL: ::add-mask:: ───────────────────────────────
//
// Mask and MaskLines came from internal/extensions/baoseed, filed there under
// "localised pure helpers: copies, not seams" -- which was the right call while
// baoseed was the only caller and stopped being true when objenc and openbao
// started importing the seeding extension to redact a secret from a log. Asking
// GitHub Actions to mask a value is the same kind of thing this package already
// does: write to a channel GHA reads, with no decision in it.
//
// MaskLines splits first because ::add-mask:: is per-line -- a multi-line secret
// masked whole leaves every individual line visible, which is the failure mode
// worth having a second function for.

// Mask asks GitHub Actions to redact a value from the log.
func Mask(v string) {
	if os.Getenv("GITHUB_ACTIONS") != "" && v != "" {
		fmt.Printf("::add-mask::%s\n", v)
	}
}

func MaskLines(v string) {
	for _, line := range strings.Split(v, "\n") {
		if strings.TrimSpace(line) != "" {
			Mask(line)
		}
	}
}
