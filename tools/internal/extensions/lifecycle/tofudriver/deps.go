package tofudriver

import (
	"os"
	"strings"
)

// Deps carries what this package cannot reach for itself.
//
// ONE FIELD. The three verbs here — tfplan, tfoutput, tfdestroy — drive OpenTofu
// through internal/terraform and internal/tfbin, both of which own their own
// seams. What is left is the step-summary sink, and that is genuinely package
// main's: it knows this binary runs inside GitHub Actions.
type Deps struct {
	// Summary appends to a GitHub Actions file (GITHUB_STEP_SUMMARY /
	// GITHUB_OUTPUT). The plan verb's whole contract is the summary it writes plus
	// the changed/unchanged output the calling job gates on — so this must do real
	// work or every assertion about either runs against nothing.
	Summary func(envVar string, lines ...string) error
}

// caps is the installed capability set, defaulting to a REAL GHA append.
//
// Not `func(...) error { return nil }`: an installed default is a fixture too,
// and defaulting this one to a no-op has now broken a test in four separate
// extractions (teardown, objenc, converge, envtopology). The pattern is settled
// enough to be a rule — hand zero values that work.
var caps = Deps{Summary: realGHAAppend}

// Install wires the capabilities main owns. Call once, before any verb runs.
func Install(d Deps) { caps = d }

func realGHAAppend(envVar string, lines ...string) error {
	path := os.Getenv(envVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}
