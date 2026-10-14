package sourceref

// vocabulary.go — one engine for every kind of NAME this guard checks.
//
// SIX VOCABULARIES ARRIVED ONE AT A TIME, each as its own loop, its own counter,
// its own fail-closed arm and its own findings branch. They are the same shape
// and always were: EXTRACT a candidate, decide whether it is a CLAIM at all,
// resolve it against an INDEX, report what did not resolve, and refuse to report
// green over an index or a corpus that turned out empty.
//
// THE DUPLICATION WAS NOT FREE, and the bill came in three ways rather than as
// line count:
//
//   - THE SAME BUG, THREE TIMES. A name broken across a line leaves a trailing
//     hyphen. Paths hit it, then test citations hit it, then `llz ci` verbs hit
//     it, and each was diagnosed and fixed from scratch because there was nowhere
//     for the first fix to live.
//   - THE SAME RULE, RE-DERIVED. A `*` means a family — for path globs, for
//     `pkg.CI*Tag`, for `llz ci assert-*`. Three implementations of one idea.
//   - THE SAME TEST FAILURE, EVERY TIME. Each new fail-closed arm broke every
//     existing fixture, because a fixture written for one vocabulary has nothing
//     to say in another. Five rounds of "add a resolving citation to the tree".
//
// So the shape is a table now, and adding the seventh is a row rather than a
// loop. This is the move `budget` already made for its two gates — one engine,
// the differing sentence travelling with the entry — and the reason it is worth
// repeating is the same: what differs between these is small and nameable, and
// everything else was being copied.

import (
	"fmt"
	"regexp"
	"strings"
)

// vocabulary is one kind of name, and the four things that distinguish it.
type vocabulary struct {
	// Name appears in the summary line: "%d <Name> reference(s)".
	Name string
	// Expr finds candidates. Submatch 1 is the name by convention; Judge is handed
	// the full index slice, so a vocabulary needing more than that can take it.
	Expr *regexp.Regexp
	// Size is how many names were indexed. ZERO DISABLES THE VOCABULARY, which is
	// not a shortcut: a tree with no Makefile, no `ci` group or no ADR index cannot
	// answer the question, and an instance repo is exactly such a tree. Judging
	// anyway would report every citation in it.
	Size int
	// Empty is the fail-closed message for "indexed something, matched nothing" —
	// the vacuous green that means the extraction broke rather than that the repo
	// stopped citing.
	Empty string
	// Judge decides one candidate. It returns the ready-to-print detail for a
	// finding, or "" when the candidate resolves; judged=false means the candidate
	// was never a claim (a third-party package, a wrapped name, a placeholder) and
	// must not be counted, because counting it would inflate the coverage number
	// the fail-closed arms are read from.
	Judge func(file, line string, m []int) (detail string, judged bool)

	refs int
}

// VocabStat is what one vocabulary indexed and matched, for the repo-level floor
// test. Exported for the same reason docs-guard exports its Scanned counters: the
// only thing separating a real green from a silently-disabled check is how much
// it looked at, and a number nobody can read is a number nobody can assert on.
type VocabStat struct {
	Name    string
	Indexed int // names in the index; 0 means the vocabulary was disabled
	Refs    int // citations judged
}

// Stats reports what each vocabulary indexed and matched.
func stats(vocabs []*vocabulary) []VocabStat {
	out := make([]VocabStat, 0, len(vocabs))
	for _, v := range vocabs {
		out = append(out, VocabStat{Name: v.Name, Indexed: v.Size, Refs: v.refs})
	}
	return out
}

// scanLine runs every enabled vocabulary over one line, appending findings.
func scanLine(vocabs []*vocabulary, file string, ln line, out *[]Finding) {
	for _, v := range vocabs {
		if v.Size == 0 {
			continue
		}
		for _, m := range v.Expr.FindAllStringSubmatchIndex(ln.text, -1) {
			detail, judged := v.Judge(file, ln.text, m)
			if !judged {
				continue
			}
			v.refs++
			if detail != "" {
				*out = append(*out, Finding{File: file, Line: ln.num, Ref: detail})
			}
		}
	}
}

// emptyVocabulary returns the fail-closed error for the first vocabulary that
// indexed names and then matched nothing, or nil.
//
// ONE ARM INSTEAD OF SIX. Each vocabulary used to carry its own copy, and the
// sixth was written by pattern-matching the fifth — which is how the gate on
// "did this actually look at anything" ends up being the least-examined code in
// a guard whose entire subject is unexamined claims.
func emptyVocabulary(vocabs []*vocabulary, scanned int) error {
	for _, v := range vocabs {
		if v.Size > 0 && v.refs == 0 {
			return fmt.Errorf("symbol-ref-guard: %d file(s) scanned against %d indexed %s "+
				"name(s) but not one citation matched — %s", scanned, v.Size, v.Name, v.Empty)
		}
	}
	return nil
}

// vocabularySummary renders the resolved counts, in table order.
func vocabularySummary(vocabs []*vocabulary) string {
	parts := make([]string, 0, len(vocabs))
	for _, v := range vocabs {
		if v.Size == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s", v.refs, v.Name))
	}
	return strings.Join(parts, ", ")
}

// indexedSummary renders what each vocabulary had to check against.
func indexedSummary(vocabs []*vocabulary) string {
	parts := make([]string, 0, len(vocabs))
	for _, v := range vocabs {
		if v.Size == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s", v.Size, v.Name))
	}
	return strings.Join(parts, ", ")
}

// wrappedName reports whether a citation is the first half of a name a paragraph
// broke across lines.
//
// SHARED, BECAUSE IT WAS DIAGNOSED THREE TIMES. Paths hit it first (a hyphenated
// filename), then test citations, then `llz ci` verbs; each fix was rediscovered
// from the symptom because there was no one place for it. The rule needs BOTH
// conditions — the line must END at the candidate, and a longer real name must
// start with it — since either alone is too loose: plenty of correct citations
// sit at a line break, and plenty of stale ones are prefixes of something real.
func wrappedName(line string, end int, name string, known map[string]bool) bool {
	rest := strings.TrimRight(line[end:], " \t")
	if rest != "" && rest != "-" {
		return false
	}
	for k := range known {
		if len(k) > len(name) && strings.HasPrefix(k, name) {
			return true
		}
	}
	return false
}
