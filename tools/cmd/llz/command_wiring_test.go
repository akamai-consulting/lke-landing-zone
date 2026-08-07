package main

// command_wiring_test.go — every command an extension exposes must be reachable
// in the tree.
//
// THIS IS THE ONE THING NOTHING CAUGHT. An extension can be declared, pass
// Validate(), appear in `llz extension list`, and still ship a verb nobody can
// run — because wiring it is a hand-written AddCommand in ci.go or main.go and
// forgetting one produces no error anywhere. `llz extension list` reads the
// registry; the CLI reads the tree; nothing compared them.
//
// It runs in package main on purpose: the tree is main's, built by newRootCmd,
// and this is the only place that can see both it and the registry.

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

// The reverse is NOT asserted, and the omission is deliberate. Package main
// legitimately owns commands no extension declares: the group constructors
// themselves, the deps-ABI verbs (ci_teardown, promote, ci_managedlock), the
// composition (import), and gen-toc, which docsguard's own read-only guard keeps
// out. Asserting "every leaf is claimed" would mean maintaining a list of those
// by hand — the 214-entry exercise this design replaced — to catch a failure that
// has never happened, since an unclaimed command in the tree still WORKS.
//
// What has happened, and what the test above catches, is the other direction.
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
