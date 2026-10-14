package sourceref

// maketargets.go — the third vocabulary this guard checks.
//
// THE VERB IS NARROWER THAN THE CHECK, and renaming it would cost more than the
// inaccuracy. `symbol-ref-guard` began as `pkg.Symbol` only, took test-function
// citations when those turned out to be the same claim, and takes make targets
// here for the same reason: all three are NAMES THIS REPO DECLARES, cited in
// prose, where the only thing keeping the citation true is that somebody
// remembered. What differs is the vocabulary a name is drawn from, not the
// question being asked of it.
//
// WHY MAKE TARGETS ARE WORTH A VOCABULARY. `make lint` is cited 33 times and
// `make coverage` 10; the runbooks, the skills and the AGENTS files all tell a
// reader — often an agent — to run one. A renamed target turns every one of
// those into an instruction that fails at the point of use, and the Makefile is
// exactly the kind of file that gets reorganised.
//
// ALL 119 CITATIONS RESOLVE TODAY. This ships green and is purely a ratchet, which
// is the cheapest moment to add one: nothing to fix, and the next rename cannot
// land quietly.
//
// BACKTICKED FORM ONLY, and that is a real constraint rather than laziness. `make`
// is an ordinary English verb, and an unanchored scan finds "make sure", "make it
// impossible", "make everything" — measured, and it swamps the signal. Inside
// backticks the word is unambiguously the command.

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// makeCiteExpr matches a backticked make invocation naming one target.
var makeCiteExpr = regexp.MustCompile("`make ([a-z][a-z0-9-]*)`")

// makeTargetExpr matches a rule's target at the start of a line. Multi-target
// rules (`a b:`) do not occur here and are deliberately not guessed at; the
// pattern below would simply not index them, which fails CLOSED — an unindexed
// target reports its citations as stale rather than passing them silently.
var makeTargetExpr = regexp.MustCompile(`^([a-z][a-z0-9-]*)\s*:`)

// makeTargets indexes the rules the Makefile declares.
//
// `.PHONY` and friends are excluded by the leading-lowercase-letter rule rather
// than by name: a special target starts with a dot, and no citation names one.
// A TREE WITH NO MAKEFILE INDEXES NOTHING RATHER THAN FAILING, and the caller
// then judges no citations at all — the same shape as the test axis. An instance
// repo carries this binary's docs and none of its build surface, and refusing to
// run there would make the guard useless on the second tree it is pointed at. The
// risk this trades away is small and bounded: with no Makefile there is nothing
// for a `make` citation to be checked against either way.
func makeTargets(repo capability.Repo) (map[string]bool, error) {
	if _, err := repo.Stat("Makefile"); err != nil {
		return nil, nil
	}
	data, err := repo.ReadFile("Makefile")
	if err != nil {
		return nil, fmt.Errorf("read Makefile: %w", err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		// A recipe line is TAB-indented and can contain a colon; only a rule
		// starts at column zero.
		if strings.HasPrefix(line, "\t") {
			continue
		}
		if m := makeTargetExpr.FindStringSubmatch(line); m != nil {
			// `target: export VAR := x` is a target-specific variable, and its
			// left-hand side is still the target — indexing it is correct.
			out[m[1]] = true
		}
	}
	return out, nil
}
