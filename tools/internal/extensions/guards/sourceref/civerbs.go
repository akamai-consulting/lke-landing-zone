package sourceref

// civerbs.go — the fourth vocabulary, and the most operator-facing one.
//
// A doc that says "run `llz ci drain-obj-buckets`" fails at the point of use, and
// unlike a stale path it is an INSTRUCTION rather than a pointer: the reader is
// mid-incident, following a runbook, and the CLI answers `unknown command`. This
// is the same claim the make-target check makes, one interface over.
//
// ────────────────────────────────────────────────────────────────────────────
// WHY docs-guard DOES NOT ALREADY COVER THIS, and why the narrower check is
// legitimate where the broad one is not.
//
// docs-guard validates FLAGS for every `llz …` invocation whose command
// RESOLVES, and deliberately reports nothing when the command does not. Its
// header gives the measurement behind that: the only doc lines naming a
// non-existent top-level command are `llz smoke` and `llz psql` in
// extending-llz.md — USER-DEFINED commands from .llz/commands.yaml which by
// design never appear in the binary. Reporting unknown commands would flag the
// doc that teaches the extension mechanism.
//
// That reasoning is about TOP-LEVEL commands. `ci` is a built-in group and a
// user-defined command cannot land inside it, so within `llz ci …` the closed-set
// assumption docs-guard could not make is simply true.
//
// THE VERB LIST COMES FROM THE LIVE TREE, never a literal. A hardcoded list would
// be the hand-maintained copy this whole guard exists to argue against, and it
// would drift the same day a verb was renamed — which is the defect, not the
// check. That is why this half takes the cobra tree, the same way docs-guard
// does and for the same reason.

import (
	"path"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// ciCiteExpr matches a cited `llz ci <verb>`, with the trailing character kept so
// a family glob can be recognised.
var ciCiteExpr = regexp.MustCompile("llz ci ([a-z][a-z0-9-]*)")

// ciVerbs lists the subcommands of `llz ci` in the live tree.
//
// Returns nil when there is no `ci` group, which makes the caller judge nothing —
// the same shape as a missing Makefile. A tree without the group cannot answer
// the question, and guessing would report every citation in the repo.
func ciVerbs(tree *cobra.Command) map[string]bool {
	if tree == nil {
		return nil
	}
	var ci *cobra.Command
	for _, c := range tree.Commands() {
		if c.Name() == "ci" {
			ci = c
			break
		}
	}
	if ci == nil {
		return nil
	}
	out := map[string]bool{}
	for _, c := range ci.Commands() {
		out[c.Name()] = true
	}
	return out
}

// citedCIVerb reports the verb a citation names, as a pattern when it names a
// family.
//
// TWO SHAPES ARE NOT CLAIMS about a verb, and both were measured on the first
// run rather than anticipated:
//
//   - A FAMILY GLOB. `llz ci assert-*`, `bao-*`, `reap-*` and `teardown-*` name a
//     family, exactly as `versionpins.CI*Tag` does for symbols. The regex stops at
//     the `*` and leaves a trailing hyphen — the same truncation the path and
//     test-name rules already handle. Glob-matched against the live set, so a
//     family whose members have ALL been renamed still fails.
//
//   - A RETIRED VERB, which is the convention this check introduces:
//
//     llz ci <verb>   a LIVE invocation. It runs today.
//     `<verb>`        a NAME. What something used to be called.
//
//     Seven of the first run's thirty-one were sentences saying so outright —
//     "the retired `gen-bootstrap-tls` workflow step", "Replaces `llz ci
//     propagate-pat`", "NOT the `env-readiness` verb". Every one was
//     correct prose. Dropping the `llz ci` prefix costs nothing a reader wanted:
//     "the retired `gen-bootstrap-tls` verb" says the same thing and stops
//     looking like a command to run. Same trade the possessive made for symbols.
//
// Detecting "retired" from the prose was tried and rejected: a keyword regex over
// "retired|former|Replaces|no longer" is good enough to SIZE the work and far too
// loose to gate on, because the negation it is really looking for lives in the
// sentence's grammar rather than its vocabulary.
func citedCIVerb(line string, start, end int) string {
	verb := line[start:end]
	// `llz ci assert-*` — the regex stops at the `*`, so the family is recognised
	// either by the character that follows or by the hyphen it left behind.
	if (end < len(line) && line[end] == '*') || strings.HasSuffix(verb, "-") {
		return verb + "*"
	}
	return verb
}

// ciVerbKnown reports whether the citation names a verb that exists. A `*` makes
// it a family pattern that must match at least one.
func ciVerbKnown(verbs map[string]bool, cited string) bool {
	if !strings.Contains(cited, "*") {
		return verbs[cited]
	}
	for v := range verbs {
		if ok, err := path.Match(cited, v); err == nil && ok {
			return true
		}
	}
	return false
}
