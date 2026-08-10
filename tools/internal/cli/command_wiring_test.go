package cli

// command_wiring_test.go — every command an extension exposes must be reachable
// in the tree.
//
// THIS IS THE ONE THING NOTHING CAUGHT. An extension can be declared, pass
// Validate(), appear in `llz extension list`, and still ship a verb nobody can
// run — because wiring it is a hand-written AddCommand in ci.go or root.go and
// forgetting one produces no error anywhere. `llz extension list` reads the
// registry; the CLI reads the tree; nothing compared them.
//
// IT USED TO SAY "it runs in package main on purpose", which was true and was a
// constraint rather than a choice: the tree could only be built there, so this
// could only be written there. internal/cli is an ordinary package now, which is
// what let commands_claimed_test.go assert the REVERSE direction — the one this
// file's closing note explains was too expensive to maintain by hand.
//
// AND ITS COVERAGE WAS PARTIAL UNTIL RECENTLY. It can only fail for a constructor
// registry.Commands() names, and twelve extension packages were absent from that
// table — every one of them exporting a plainly-named `Cmd()` rather than
// `SomethingCmd()`, so the guard's reach tracked an accident of naming. See
// registry/commands_census_test.go, which derives the table's subject instead.

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension/registry"
)

// treeCommands returns every command name reachable from the root, at any depth.
//
// Names, not paths: a constructor knows what it is called but not where package
// main chose to hang it, and asking it to know would be the group-placement
// coupling this guard exists without.
func treeCommands(t *testing.T) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, k := range c.Commands() {
			found[k.Name()] = true
			walk(k)
		}
	}
	walk(newRootCmd())
	return found
}

func TestEveryExtensionCommandIsWiredIn(t *testing.T) {
	in := treeCommands(t)
	missing := map[string][]string{}
	for _, c := range registry.Commands() {
		name := c.New().Name()
		if !in[name] {
			missing[c.Extension] = append(missing[c.Extension], name)
		}
	}
	if len(missing) == 0 {
		return
	}
	exts := make([]string, 0, len(missing))
	for e := range missing {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	for _, e := range exts {
		sort.Strings(missing[e])
		t.Errorf("%s exposes %s, which no AddCommand reaches — the extension is declared "+
			"and listed, but the verb cannot be run", e, strings.Join(missing[e], ", "))
	}
}

// The reverse IS asserted now, in commands_claimed_test.go, and the argument that
// used to sit here is worth keeping because it explains the shape of what replaced
// it. It read: "Asserting `every leaf is claimed` would mean maintaining a list of
// those by hand — the 214-entry exercise this design replaced — to catch a failure
// that has never happened, since an unclaimed command in the tree still WORKS."
//
// Two of those three clauses still hold. A hand-typed list would indeed be a second
// copy of the tree, and an unclaimed command does still work. What changed is that
// the list no longer has to be hand-typed: with the tree in an importable package
// it is generated from the tree once and ratcheted, so it cannot drift, and the
// failure it catches is not a broken command but an unmeasured one — a verb outside
// `llz extension list`, enablement, the capability fence and the gate driver.
func TestRegistryCommandsAreDistinct(t *testing.T) {
	// A constructor registered twice would make the guard above pass vacuously for
	// whichever extension listed it second.
	seen := map[string]string{}
	for _, c := range registry.Commands() {
		name := c.New().Name()
		if prev, dup := seen[name]; dup && prev != c.Extension {
			t.Errorf("%q is registered by both %s and %s — one of them does not own it",
				name, prev, c.Extension)
		}
		seen[name] = c.Extension
	}
}
