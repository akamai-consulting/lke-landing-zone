package main

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertsuite"
)

// The one test that asserts the suite command is REGISTERED in the ci tree.
// That is a fact about the tree, not about the suite, so it stays with the tree.

func TestAssertSuiteLanesNameRealVerbs(t *testing.T) {
	registered := map[string]bool{}
	for _, c := range ciCmd().Commands() {
		registered[c.Name()] = true
	}
	for _, l := range assertsuite.Lanes("e2e") {
		if len(l.Steps) == 0 {
			t.Errorf("lane %s has no steps — it would pass having run nothing", l.Name)
		}
		for _, s := range l.Steps {
			if !registered[s[0]] {
				t.Errorf("lane %s invokes `llz ci %s`, which is not a registered verb", l.Name, s[0])
			}
		}
	}
}

// Every lane needs a rationale. The old YAML carried per-lane comments explaining
// what each proved; losing that in the move to Go would be a real regression in
// reviewability, so it is enforced rather than hoped for.
