package registry

import (
	"bytes"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// EVERY DRIVEN GATE MUST FAIL ON AN EMPTY CORPUS.
//
// The rule is plaintext-guard's, in its own words: "a guard that had nothing to
// check reports the same green as one that checked everything, so this fails
// instead." It was a CONVENTION some guards kept, and the cost of that showed up
// as a real defect — docs-guard, run through this driver against a one-command
// cobra tree, resolved zero of 872 documented invocations, found nothing, returned
// nil, and the driver announced `8 ran, all clean`.
//
// A convention the next gate forgets is not a rule. This makes it checkable: point
// every gate at an EMPTY directory and require it to refuse.
//
// WHY THE WHOLE-DRIVER FORM RATHER THAN A TEST PER GUARD. The failure was not in a
// guard, it was in the SET — one member behaving differently from its siblings, in
// the direction of silence. Only a test over the set can see that, and only over
// the DRIVEN set, because a gate nobody runs cannot report a false green.
func TestEveryDrivenGateFailsOnAnEmptyCorpus(t *testing.T) {
	empty := t.TempDir()

	var passed []string
	for _, g := range Gates() {
		c := g.new(&cobra.Command{Use: "llz"})
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&out)
		c.SetArgs(emptyCorpusArgs(g, empty))
		c.SilenceUsage, c.SilenceErrors = true, true

		if err := c.Execute(); err == nil {
			passed = append(passed, g.Extension+"/"+c.Name())
		}
	}
	sort.Strings(passed)

	var unexpected []string
	for _, p := range passed {
		if _, exempt := emptyCorpusExempt[p]; !exempt {
			unexpected = append(unexpected, p)
		}
	}
	// An exemption without an argument is an exemption nobody weighed. The map
	// holds reasons rather than bools precisely so this can be checked.
	for name, why := range emptyCorpusExempt {
		if strings.TrimSpace(why) == "" {
			t.Errorf("emptyCorpusExempt[%q] has no reason — a gate registered as legitimately "+
				"silent is indistinguishable from one nobody fixed unless the argument is written down", name)
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("%d gate(s) reported CLEAN over an empty directory:\n\t%s\n"+
			"\tA guard that examined nothing must say so. Add the corpus check the way "+
			"plaintext-guard and docs-guard do, or register it in emptyCorpusExempt with "+
			"the reason an empty tree is a legitimate pass for it.",
			len(unexpected), strings.Join(unexpected, "\n\t"))
	}

	// The ratchet half: an exemption for a gate that now fails correctly is a
	// stale allowance, and a stale allowance is what the next gate is written to
	// match.
	var stale []string
	got := map[string]bool{}
	for _, p := range passed {
		got[p] = true
	}
	for name := range emptyCorpusExempt {
		if !got[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%d exemption(s) no longer needed — DELETE them from emptyCorpusExempt:\n\t%s",
			len(stale), strings.Join(stale, "\n\t"))
	}
}

// emptyCorpusArgs points the gate at an empty tree, on whatever flag it declares.
//
// IT IGNORES Subtree DELIBERATELY. A gate reading `instance-template` under an
// empty root would be handed a path that does not exist, which several guards
// report as a read error rather than as an empty corpus — a different failure, and
// not the one under test. The empty DIRECTORY is the subject here.
//
// It reads Gate.Flag rather than scanning a built argv for `--root`/`--dir`, which
// is what it used to do. That scan silently skipped any gate whose subject flag it
// did not recognise, so a third spelling would have been counted as an unexpected
// pass — the vacuous shape this test exists to catch, in the test itself.
func emptyCorpusArgs(g Gate, dir string) []string {
	flag := g.Flag
	if flag == "" {
		flag = "--root"
	}
	return []string{flag, dir}
}
