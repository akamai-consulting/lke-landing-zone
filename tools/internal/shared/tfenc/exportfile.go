package tfenc

// exportfile.go — hand the environment to a shell WITHOUT putting it on stdout.
//
// A live instance's passphrase has been disclosed by writing these values to
// stdout, which is captured by scrollback, script(1), CI logs and `set -x`. A TTY
// guard cannot catch that: a pipe is the intended destination. So the values go
// to a private file and stdout carries only the commands that consume it —
// everything a recorder sees is a path.
//
// This protects the TRANSPORT, not the destination: after the `eval` the value is
// in the shell's environment, inherited by every child. `--shell-init` avoids both
// and is what the docs lead with. A FIFO would keep the bytes off disk but cannot
// work here — `eval "$(…)"` drains stdout to completion first, so llz has exited
// before the shell sources anything.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// exportDirPattern is also what Sweep matches, so it must stay distinctive enough
// that a sweep can never delete something that is not ours.
const exportDirPattern = "llz-tofu-env-"

// exportMaxAge bounds how long an unconsumed file may sit before the next `llz
// tofu` removes it. Not zero, because concurrent invocations must not delete each
// other's file mid-handoff; not longer, because it is how long a passphrase sits
// on disk. It is a user-visible deadline for a snippet captured now and evaluated
// later — the documented one-liner evaluates immediately and cannot hit it.
const exportMaxAge = time.Minute

// WriteExports writes the exports to a private file and returns the shell snippet
// that sources and then deletes it.
//
// The DIRECTORY carries the guarantee, not the file mode: a 0600 file created
// inside a world-readable /tmp is visible to every local user between create and
// chmod. MkdirTemp makes the private directory first, so there is no such instant.
func (l Local) WriteExports() (snippet string, err error) {
	dir, err := os.MkdirTemp("", exportDirPattern+"*")
	if err != nil {
		return "", fmt.Errorf("creating a private directory for the exports: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	path := filepath.Join(dir, "env.sh")
	if err := os.WriteFile(path, []byte(l.Exports()), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("writing the exports: %w", err)
	}
	// `.` rather than `source`: POSIX, and it is what dash/sh accept too.
	// Separated by `;` and not `&&` on purpose — the removal must run even when
	// sourcing fails, or a syntax error leaves the secret on disk.
	return fmt.Sprintf(". %s; rm -f %s; rmdir %s 2>/dev/null\n",
		shellQuote(path), shellQuote(path), shellQuote(dir)), nil
}

// SweepStaleExports removes export files older than exportMaxAge.
//
// `--export` run without an `eval` writes a passphrase nobody consumes, and
// nothing else would ever remove it. Every `llz tofu` sweeps first, so the
// exposure is bounded by the next use of the command rather than by the operator
// remembering. Best-effort: a directory another user owns is skipped rather than
// failing the command actually asked for.
func SweepStaleExports() int {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), exportDirPattern) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < exportMaxAge {
			continue
		}
		if os.RemoveAll(filepath.Join(os.TempDir(), e.Name())) == nil {
			removed++
		}
	}
	return removed
}

// shellQuote wraps s in single quotes, the one form in which the content is
// verbatim. A temp path is unlikely to contain a quote; "unlikely" is not a
// reason to interpolate it into a command the shell will run.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
