package providerlock

// run.go — the reporting half. Kept out of providerlock.go so the judgement
// there stays a pure function over parsed input, testable with no filesystem.

import (
	"fmt"
	"io"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// Run scans repoRoot and reports. Returns an error when any lock entry violates
// a constraint.
func Run(repoRoot string, out, errOut io.Writer) error {
	repo := capability.RepoForGate(Extension(), repoRoot)

	// AGREEMENT FIRST, AND IT SHORT-CIRCUITS. When the declaration sites disagree
	// the intersection they form may be empty, and then NO lock can satisfy it —
	// so reporting the lock as the violation (which is what the loop below would
	// do) points the fix at the wrong file. See agreement.go.
	constraints, err := AllConstraints(repo)
	if err != nil {
		return err
	}
	if conflicts := CheckAgreement(constraints); len(conflicts) > 0 {
		return reportConflicts(conflicts, errOut)
	}

	results, err := Scan(repo)
	if err != nil {
		return err
	}
	var violations []Violation
	compared := 0
	for _, r := range results {
		compared += r.Compared
		violations = append(violations, r.Violations...)
		for _, n := range r.Notes {
			fmt.Fprintf(out, "  note: %s: %s\n", n.Root, n.Text)
		}
	}
	// FAIL CLOSED ON VACUITY. Locks were found and constraints were found, but if
	// none of them are for the SAME provider then nothing was actually compared —
	// which is what a normalisation bug looks like, and it would otherwise print
	// the same green line as a clean tree.
	if compared == 0 {
		return fmt.Errorf("provider-lock-guard: found locks and constraints but compared NOTHING — " +
			"no locked provider matched a constrained one. That is a provider-address normalisation " +
			"bug in this gate, not a clean tree")
	}
	if len(violations) == 0 {
		fmt.Fprintf(out, "provider-lock-guard: OK — %d provider pin(s) across %d root(s) satisfy the "+
			"constraints declared beside them.\n", compared, len(results))
		return nil
	}
	for _, v := range violations {
		fmt.Fprintf(errOut, "::error file=%s/%s/%s::%s is locked at %s and does not satisfy %s\n",
			rootsDir, v.Root, lockFile, v.Provider, v.Locked, v.Spec)
	}
	fmt.Fprintf(errOut, "\n%s %d provider pin(s) in a shipped lockfile cannot satisfy the "+
		"constraint declared beside them:\n", color.Red("\u2717"), len(violations))
	for _, v := range violations {
		fmt.Fprintf(errOut, "    %s\n", v)
	}
	fmt.Fprintf(errOut, `
A CONSTRAINT BUMP HAS TWO HALVES AND THIS CHANGE CARRIES ONE. versions.tf declares
what a provider must satisfy; the %s beside it records what will actually be
installed. Both are embedded in the llz binary and laid down together by
tfroots.Files at render time, so shipping them in disagreement means every render
produces a root whose own `+"`tofu init`"+` refuses it:

    Error: Failed to resolve provider packages
    ... locked provider ... does not match configured version constraints

TO FIX, regenerate the lock beside the constraint you moved:

    (cd %s/<root> && tofu init -backend=false -upgrade -input=false)

and commit the result. `+"`-backend=false`"+` is the correct scope rather than a
workaround for a tripwire: regenerating a lock is a provider resolution and has no
business reading state. The roots carry encryption.tf, so an init that DOES
configure the backend stops without $TF_ENCRYPTION on OpenTofu's own "Invalid
expression … A single static variable reference is required", which names neither
the passphrase nor the variable.

NOTHING IS OWED BY EXISTING INSTANCES. They commit no lockfile — `+"`llz render`"+`
writes this one into each root before every terraform op — so the fix reaches the
field with the release instead of as a per-instance chore. That is the difference
this gate's move bought: it now judges the exact bytes every instance renders.
`, lockFile, rootsDir)
	return fmt.Errorf("provider-lock-guard: %d shipped provider pin(s) violate a constraint declared beside them", len(violations))
}

// reportConflicts prints the disagreeing declaration sites and the remediation.
func reportConflicts(conflicts []Conflict, errOut io.Writer) error {
	for _, c := range conflicts {
		for _, spec := range c.Specs() {
			for _, file := range c.Sites[spec] {
				fmt.Fprintf(errOut, "::error file=%s::%s is constrained %q here, but differently elsewhere "+
					"in the template\n", file, c.Provider, spec)
			}
		}
	}
	fmt.Fprintf(errOut, "\n%s %d provider(s) constrained inconsistently across the template:\n",
		color.Red("✗"), len(conflicts))
	for _, c := range conflicts {
		fmt.Fprintf(errOut, "    %s\n", c)
	}
	fmt.Fprintf(errOut, `
WHY THIS BREAKS EVERY TERRAFORM OP. A root's constraint and those of the modules
it sources are INTERSECTED, not overridden. Two specs that do not overlap leave
no version to install and `+"`tofu init`"+` fails before it reaches the lock:

    Error: Failed to resolve provider packages
    Could not resolve provider <name>: no available releases match the given
    constraints <spec A>, <spec B>

This is the shape a dependency bot produces on its own. Dependabot scans
`+"`terraform-modules/*`"+` and NOT the generated roots (they carry copier tokens, so
they are not parseable HCL until rendered — see the `+"`terraform`"+` entry in
.github/dependabot.yml), so it moves the module half of the constraint and leaves
the root half behind.

TO FIX: set every site above to the SAME spec. The bot's PR is the module half;
the roots under tools/internal/shared/tfroots/roots/ are the half it cannot reach.
Then regenerate the delivered locks beside them, which is the check that runs next.
`)
	return fmt.Errorf("provider-lock-guard: %d provider(s) constrained inconsistently across the template", len(conflicts))
}
