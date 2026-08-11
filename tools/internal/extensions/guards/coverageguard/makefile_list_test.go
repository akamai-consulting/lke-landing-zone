package coverageguard

// makefile_list_test.go — the COVERAGE_MINS list in the root Makefile must not
// contain a comment.
//
// WHY THIS EXISTS. In GNU Make a `#` ENDS THE LOGICAL LINE, backslash
// continuations included. So a comment written between two entries of
//
//	COVERAGE_MINS := \
//		internal/a=70 \
//		# why internal/b is what it is
//		internal/b=69 \
//		internal/c=80 \
//
// does not annotate the list — it TRUNCATES it, and everything after the comment
// stops being gated. That happened: a note added beside one floor cut the list
// from 123 packages to 115, switching off eight coverage gates, while `make
// coverage` went on reporting "all gated packages meet their per-package
// thresholds" for the 115 that were left. A gate that silently covers less than
// it claims is the failure mode this whole tree is built to refuse, and this one
// was invisible from every direction: the build passed, the gate passed, and the
// only symptom was a number nobody was counting.
//
// The rule is therefore mechanical: annotate the list ABOVE the assignment.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const rootMakefile = "../../../../../Makefile"

// coverageMinsBlock returns the physical lines of the COVERAGE_MINS assignment,
// from the `:=` line to the first line that does not continue.
func coverageMinsBlock(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(rootMakefile)
	if err != nil {
		t.Fatalf("read %s: %v", rootMakefile, err)
	}
	lines := strings.Split(string(raw), "\n")
	start := -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, "COVERAGE_MINS :=") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("no COVERAGE_MINS assignment in the root Makefile — this gate would pass having read nothing")
	}
	var block []string
	for i := start; i < len(lines); i++ {
		block = append(block, lines[i])
		if !strings.HasSuffix(strings.TrimRight(lines[i], " \t"), "\\") {
			break
		}
	}
	return block
}

func TestCoverageMinsListCarriesNoComment(t *testing.T) {
	block := coverageMinsBlock(t)
	// Fail closed on vacuity: the real list is >100 entries, so a handful of lines
	// means the extractor stopped early rather than the list being short.
	if len(block) < 50 {
		t.Fatalf("COVERAGE_MINS block is only %d line(s) — either the list was gutted or this "+
			"extractor is broken; both need a human", len(block))
	}
	for i, ln := range block {
		if idx := strings.Index(ln, "#"); idx >= 0 {
			t.Errorf("COVERAGE_MINS line %d contains a `#`:\n    %s\n"+
				"In GNU Make a comment ENDS the logical line, continuations included — so this does not "+
				"annotate the list, it TRUNCATES it, and every package below stops being gated while "+
				"`make coverage` still reports success. Move the note ABOVE the assignment.",
				i+1, strings.TrimSpace(ln))
		}
	}
}

// And the entries have to be well formed, since a malformed one is dropped by the
// parser just as quietly as a truncated tail.
func TestCoverageMinsEntriesAreWellFormed(t *testing.T) {
	entry := regexp.MustCompile(`^\s*[\w./-]+=\d+\s*\\?\s*$`)
	block := coverageMinsBlock(t)
	n := 0
	for i, ln := range block {
		if i == 0 || strings.TrimSpace(ln) == "" || strings.TrimSpace(ln) == "\\" {
			continue
		}
		if !entry.MatchString(ln) {
			t.Errorf("COVERAGE_MINS line %d is not a `<pkg>=<int>` entry:\n    %s", i+1, strings.TrimSpace(ln))
			continue
		}
		n++
	}
	if n < 100 {
		t.Errorf("only %d well-formed COVERAGE_MINS entries — the list should gate every package "+
			"that has ever been banked; a sudden drop means entries were lost, not cleaned up", n)
	}
	t.Logf("%d packages gated by COVERAGE_MINS", n)
}
