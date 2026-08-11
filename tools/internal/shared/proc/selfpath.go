package proc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// osExecutable is a seam. A test cannot re-exec itself under another name to
// prove the not-named-like-the-command case, which is the one that matters.
var osExecutable = os.Executable

// SelfOnPATH makes THIS PROCESS'S OWN BINARY resolvable to child processes under
// name, and returns a cleanup that puts PATH back.
//
// THE INCIDENT. copier's `_tasks` invoke `llz` BY NAME and fall back to a warning
// when `command -v llz` comes up empty — deliberately, so an adopter whose llz
// predates the template still gets a render. `llz upgrade` runs copier twice and
// both renders execute those tasks, so when the fallback is taken the docs prune
// and the root-link repoint silently do not happen. That was survivable while the
// clean render passed `--skip-tasks` (it delivered no docs/ at all, so the
// `managed` overwrite pass had nothing to copy). Dropping `--skip-tasks` — which
// is what makes the overwrite source match a fresh `llz new` — turned it into a
// live defect: an operator running `llz upgrade` by absolute path, with no llz on
// PATH, gets the UNPRUNED template docs tree copied into the instance (measured:
// 23 files to 86, including docs/adr/** and docs/designs/**). The files land
// untracked, so they do not appear in the upgrade's diffstat.
//
// NOT `PATH=$(dirname $self)`. That is what `llz ci upgrade-test` does, and it is
// only correct there because the gate is handed a binary that is already NAMED
// llz. This process's executable can be named anything — `llz-422` while
// bisecting, a `go run` temp binary, `llz.test` under a test — and prepending its
// directory would then publish the wrong thing or nothing at all. Publishing an
// explicit symlink under the name the child actually looks up is the only form
// that does not depend on what the operator called the file.
//
// It no-ops when PATH already resolves name to this same binary — the common case
// for an installed llz — so the normal upgrade allocates no temp dir and mutates
// no environment.
//
// Mutating this process's own PATH (rather than threading an env through every
// exec) matches proc's contract: these children are copier and git run attached to
// the operator's terminal, and the seam they share takes no env. The returned
// cleanup restores the previous value, so the mutation does not outlive the render.
func SelfOnPATH(name string) (func(), error) {
	noop := func() {}

	self, err := osExecutable()
	if err != nil {
		return noop, fmt.Errorf("resolve this process's own executable to publish it as %q: %w", name, err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return noop, fmt.Errorf("stat this process's own executable %s: %w", self, err)
	}

	if found, err := exec.LookPath(name); err == nil {
		if foundInfo, statErr := os.Stat(found); statErr == nil && os.SameFile(selfInfo, foundInfo) {
			return noop, nil
		}
	}

	dir, err := os.MkdirTemp("", "llz-selfpath-*")
	if err != nil {
		return noop, fmt.Errorf("stage a directory to publish %q on PATH: %w", name, err)
	}
	if err := os.Symlink(self, filepath.Join(dir, name)); err != nil {
		_ = os.RemoveAll(dir)
		return noop, fmt.Errorf("publish %s as %q: %w", self, name, err)
	}

	prev, had := os.LookupEnv("PATH")
	next := dir
	if prev != "" {
		next = dir + string(os.PathListSeparator) + prev
	}
	if err := os.Setenv("PATH", next); err != nil {
		_ = os.RemoveAll(dir)
		return noop, fmt.Errorf("prepend %s to PATH: %w", dir, err)
	}

	return func() {
		if had {
			_ = os.Setenv("PATH", prev)
		} else {
			_ = os.Unsetenv("PATH")
		}
		_ = os.RemoveAll(dir)
	}, nil
}
