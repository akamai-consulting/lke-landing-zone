package extension_test

// THE IN-DEGREE INVARIANT: nothing in internal/extensions is imported by
// internal/extensions.
//
// This is the same rule as layering_test.go's, one level up, and it is the one the
// campaign had been breaking hardest -- 41 of the 75 extensions import at least one
// peer. The declarations describe a modular system; the import graph describes a
// monolith.
//
// WHY IT MATTERS, in one concrete failure. The model says `Always` is a default
// rather than a constant: an instance with no object storage must be able to turn
// `assert-objstore` off. But assert-objstore, assert-observability,
// credential-rotate and teardown all imported `objenc`, so turning object storage
// off was a COMPILE-TIME IMPOSSIBILITY rather than a configuration flag -- and
// nothing in any declaration recorded that the headline promise was undeliverable.
// Splitting objenc's wire layer out to shared/objstore took that edge count from
// four to one, and the one that remains is a real capability dependency worth
// arguing on its own terms.
//
// THE DIAGNOSIS IS ONE THIS CAMPAIGN ALREADY KNOWS HOW TO MAKE. It extracted a
// shared rule out of package main a dozen times -- guardwalk, kubectlprobe,
// pathglob, shquote, color, cigate -- each time on the argument that N callers of a
// private helper means a package. That argument had never been applied BETWEEN
// extensions. A package at in-degree 7 is not a capability seven other capabilities
// happen to want; it is substrate wearing an extension's declaration. The fix is
// always the same shape: the library half moves to shared/, the declaration and its
// verbs stay behind.
//
// THIS GUARD IS A RATCHET, NOT A CEILING. The list below was MEASURED, not chosen,
// and it fails in BOTH directions -- exactly like the core-surface budget, and for
// exactly the same reason. A new edge fails because the graph got worse. A
// DISAPPEARED edge also fails, because a stale allowance is slack the next change
// grows into unremarked, and because deleting the line is how the number is banked.
// The list may only shrink.

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// allowedEdges is the measured extension -> extension import set. Every line here
// is a package that should probably be split; none is an endorsement.
var allowedEdges = map[string]bool{
	"assertidentity -> healthsla":        true,
	"assertobs -> converge":              true,
	"assertobs -> objenc":                true,
	"assertsecrets -> assertobs":         true,
	"baoseed -> assertplatform":          true,
	"bootstrapcluster -> reconcilelanes": true,
	"configreadiness -> teardown":        true,
	"database -> tofudriver":             true,
	"deliverdocs -> docsguard":           true,
	"doctor -> tokeninv":                 true,
	"envadd -> configreadiness":          true,
	"envadd -> render":                   true,
	"envtopology -> promote":             true,
	"firewall -> reconciler":             true,
	"identityconfig -> kyverno":          true,
	"lint -> configreadiness":            true,
	"lint -> manifestguard":              true,
	"lint -> pincoherence":               true,
	"lint -> render":                     true,
	"lint -> sustain":                    true,
	"manifestguard -> brownfield":        true,
	"newinstance -> onboard":             true,
	"onboard -> branchpolicy":            true,
	"onboard -> configreadiness":         true,
	"onboard -> doctor":                  true,
	"onboard -> reachability":            true,
	"onboard -> statepassphrase":         true,
	"onboard -> templatecommit":          true,
	"openbao -> baolifecycle":            true,
	"reconciler -> credrotate":           true,
	"reconciler -> reconcilelanes":       true,
	"reconciler -> volumes":              true,
	"seedspecial -> identityconfig":      true,
	"teardown -> credrotate":             true,
	"templatecommit -> pincoherence":     true,
	"templatecommit -> sustain":          true,
	"templatecommit -> versionpins":      true,
	"upgrade -> onboard":                 true,
	"upgrade -> render":                  true,
	"upgrade -> selfupgrade":             true,
	"upgrade -> sustain":                 true,
	"upgrade -> templatecommit":          true,
}

// extensionEdges returns the DIRECT, non-test imports between extensions. Non-test
// is the meaningful scope: a test importing a peer as a realistic fixture does not
// put that peer in the binary, and it is the binary that decides whether a
// capability can be turned off.
func extensionEdges(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f",
		"{{.ImportPath}} {{join .Imports \" \"}}", "./../../extensions/...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	const pfix = modulePath + "/internal/extensions/"
	var edges []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], pfix) {
			continue
		}
		self := strings.TrimPrefix(f[0], pfix)
		for _, dep := range f[1:] {
			if !strings.HasPrefix(dep, pfix) {
				continue
			}
			if to := strings.TrimPrefix(dep, pfix); to != self {
				edges = append(edges, self+" -> "+to)
			}
		}
	}
	sort.Strings(edges)
	return edges
}

func TestNoNewExtensionToExtensionImports(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range extensionEdges(t) {
		if seen[e] {
			continue
		}
		seen[e] = true
		if !allowedEdges[e] {
			t.Errorf("NEW extension -> extension import: %s\n"+
				"\tinternal/extensions packages must not import each other. Split the "+
				"imported package: the library half moves to internal/shared, the "+
				"declaration and its verbs stay behind. If you genuinely believe this "+
				"edge is a capability dependency rather than borrowed substrate, say so "+
				"in the declaration and add the line to allowedEdges with that reason.", e)
		}
	}

	// The ratchet. An allowance nobody removed is slack, and slack is what the
	// core-surface budget's `exact: true` exists to prevent one file over.
	var stale []string
	for e := range allowedEdges {
		if !seen[e] {
			stale = append(stale, e)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%d allowed edge(s) no longer exist -- DELETE THEM from allowedEdges "+
			"in this commit, so the paydown is banked instead of left as room to regrow:\n\t%s",
			len(stale), strings.Join(stale, "\n\t"))
	}

	if testing.Verbose() {
		fmt.Printf("extension -> extension edges: %d (allowed: %d)\n", len(seen), len(allowedEdges))
	}
}
