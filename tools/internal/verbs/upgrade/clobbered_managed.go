package upgrade

// clobbered_managed.go — name the local edits the upgrade is about to throw away.
//
// WHAT THE UPGRADE DOES, AND WHY IT IS SILENT. The manifest policy overwrites
// every `managed` file from a clean render of the target ref (upgrade_policy.go).
// That is the class working as designed — the template owns those files. But the
// operator who had edited one gets a count: "overwrote 31 managed file(s)". Their
// change is in the diff, indistinguishable from the 30 the template moved, and
// the reason it vanished is nowhere in the output.
//
// WHAT ALREADY COVERS PART OF THIS, stated so the gap this closes is the real
// one. `llz lint` runs `managed-fresh` in every instance's CI and pre-commit hook
// (lint.go, stepVendoredFresh), so an edit that reaches a COMMIT is already
// reported, loudly, with the same remedy. What that cannot see is the edit that
// never got that far: a fix made in the working tree and then upgraded over,
// which is the ordinary shape of "I patched the workflow to unblock myself, then
// took the release". This says the thing at the moment the edit is destroyed
// rather than at the commit that never happened.
//
// ADVISORY, NEVER FATAL — the same rule reportCIImageSkew follows. The tree is
// already rewritten by the time this can be printed, and "your edit was
// overwritten" is not a reason to fail an upgrade that otherwise worked. The
// operator's copy is in git; what they lack is the knowledge that they need it.

import (
	"fmt"
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// managedEditsBefore is the digest-locked files whose bytes differ from the lock
// the instance carries — asked BEFORE copier runs, because that is the last
// moment an operator's edit is distinguishable from a hunk copier merged.
//
// ERRORS ARE SWALLOWED INTO "nothing to report", and that is a deliberate
// asymmetry rather than an oversight. This function is advisory decoration on a
// command that must keep working for a pre-lock instance, a template-repo
// checkout, and an operator with an unreadable tree. Failing the upgrade to
// deliver a warning would trade a real capability for a nicety. The cost is that
// a broken lock reports clean — which is why the GATE for this behavior is
// `managed-fresh` itself, where the same comparison DOES fail closed, and not
// this call site.
//
// Package var so the test can substitute it: the real one walks the scaffold.
var managedEditsBefore = func() []string {
	drift, _, err := sustain.DriftedManaged(SustainDeps(), "")
	if err != nil {
		return nil
	}
	return drift.Edited
}

// reportClobberedManaged prints the edits the overwrite pass discarded.
//
// It takes the list captured before copier ran rather than recomputing: after the
// overwrite every managed file matches the lock again by construction, so asking
// now would always answer "nothing was lost".
func reportClobberedManaged(edited []string, dryRun bool) {
	if len(edited) == 0 {
		return
	}
	verb, tense := "overwrote", "was"
	if dryRun {
		verb, tense = "would overwrite", "would be"
	}
	fmt.Fprintf(os.Stderr, "\n%s the upgrade %s %d template-owned file(s) you had edited locally — "+
		"%s change %s discarded:\n", color.Yellow("!"), verb, len(edited), pluralPossessive(len(edited)), tense)
	for _, rel := range edited {
		fmt.Fprintf(os.Stderr, "    %s\n", color.Bold(rel))
	}
	fmt.Fprintf(os.Stderr, "  These are `managed` in .template-manifest: the template owns them, so every\n"+
		"  upgrade re-renders them and a local edit cannot survive one. Recover yours with\n"+
		"    %s\n", color.Cyan("git diff HEAD -- "+edited[0]))
	fmt.Fprintf(os.Stderr, "  and then send the change upstream to the template, where it will persist.\n")
}

// pluralPossessive keeps the sentence grammatical for one file and for many
// without composing the message twice.
func pluralPossessive(n int) string {
	if n == 1 {
		return "its"
	}
	return "their"
}
