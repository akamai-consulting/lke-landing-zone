package tfbin

// hydrate.go — every Terraform shell-out llz makes gets the instance's own
// credentials, without any call site asking for them.
//
// At the chokepoint for the same reason the binary name is: one answer, needed by
// everything, and wrong in a way that only shows up when a real command runs. It
// is what makes `llz ci tf-output` and `llz ci fetch-kubeconfig-state` work in a
// local checkout with no edit to either.
//
// Safe to do unconditionally because Hydrate never REPLACES, and contributes
// nothing outside an instance checkout — no flag to remember to unset. That is
// not the same as "it changes nothing": see tfenc/hydrate.go for why introducing
// an absent variable is a real change.
//
// A caller that assigns cmd.Env AFTER Command() returns still wins outright,
// which is the correct precedence: statepassphrase's verify pass does exactly
// that to pin TF_ENCRYPTION to the new key alone, and it must not be merged with
// anything.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfenc"
)

// localEnv is a package-var seam so tests can drive the hydration decision
// without a `.llz` cache on disk. Returns the additions and, separately, a
// malformed-material error worth telling the operator about.
var localEnv = func() ([]string, error) {
	l, err := tfenc.Hydrate(".")
	return l.Environ(), err
}

// noteWriter is where the hydration note goes. A package var so a test can read
// the sentence this package prints, rather than asserting on a copy of it.
var noteWriter io.Writer = os.Stderr

// noteOnce keeps the hydration report to one line per process. A `tofu init`
// followed by four `tofu output` calls is one decision, not five, and repeating
// it would bury the command's own output.
var noteOnce sync.Once

// hydrate attaches the instance environment to c, leaving c.Env nil when there
// is nothing to add so the command plainly inherits — the state every existing
// test and call site already expects.
func hydrate(c *exec.Cmd) *exec.Cmd {
	extra, err := localEnv()
	if err != nil {
		// Present and wrong is worth reporting: the operator believes it works.
		// Not fatal, because tfbin is on the path of commands that have nothing to
		// do with encryption.
		noteOnce.Do(func() { fmt.Fprintln(noteWriter, color.Yellow("llz:"), err) })
		return c
	}
	if len(extra) == 0 {
		return c
	}
	noteOnce.Do(func() {
		fmt.Fprintln(noteWriter, color.Dim("llz: "+tfenc.ResolvedNote(len(extra))))
	})
	c.Env = append(os.Environ(), extra...)
	return c
}

// CommandResolved is Command for a caller that has ALREADY resolved the instance
// environment — `llz tofu` needs the same answer for its diagnostics and its
// backend key, and resolving twice is two filesystem walks, two reports, and two
// chances to disagree. Passing nil is a plain inherit.
func CommandResolved(extra []string, args ...string) *exec.Cmd {
	c := exec.Command(Bin(), args...) // #nosec G204 -- binary is resolved from a fixed allowlist above
	if len(extra) > 0 {
		c.Env = append(os.Environ(), extra...)
	}
	return c
}

// resetNote re-arms the once-per-process note. Only tests call it; a running llz
// wants exactly one line however many Terraform commands it drives.
func resetNote() { noteOnce = sync.Once{} }
