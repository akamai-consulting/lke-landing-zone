package registry

import (
	"sort"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// The registry's one job today is that everything compiled in is legal. Asserting
// it here rather than in each extension's own package is the point of having a
// registry at all: a declaration added later cannot skip the check by forgetting
// to write its own test.
func TestBuiltInSetValidates(t *testing.T) {
	if errs := Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("built-in extension set does not validate: %v", err)
		}
	}
}

// An empty registry would satisfy TestBuiltInSetValidates perfectly — ValidateSet
// over nothing returns nothing. So pin that the set is non-empty and that the
// extraction this package exists to demonstrate is actually in it.
func TestGuardBudgetsIsRegistered(t *testing.T) {
	e, ok := Lookup("guard-budgets")
	if !ok {
		t.Fatalf("guard-budgets is not registered; All() = %v", names(All()))
	}
	if !e.Binds(extension.Gate) {
		t.Errorf("guard-budgets should bind as a gate, got %v", e.Bindings)
	}
	// A gate reaches files and nothing else. If this ever widens, the extraction
	// stopped being the safe first one the catalog picked it for.
	if got := e.Grants(); len(got) != 1 || got[0] != extension.ReadRepo {
		t.Errorf("guard-budgets grants = %v, want [read-repo] only", got)
	}
}

func TestLookupMissesCleanly(t *testing.T) {
	if _, ok := Lookup("no-such-extension"); ok {
		t.Error("Lookup must report a miss, not an empty hit")
	}
}

// Callers render this set into help text and listings, so a stable order is part
// of the contract rather than a nicety.
//
// Asserted against sortByName rather than All, and see its comment for why: with
// one extension registered, All() is name-ordered and insertion-ordered at the
// same time, so a test over it proves nothing.
func TestSortByNameOrders(t *testing.T) {
	got := names(sortByName([]extension.Extension{
		{Name: "teardown"}, {Name: "assert-storage"}, {Name: "guard-budgets"},
	}))
	want := []string{"assert-storage", "guard-budgets", "teardown"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortByName = %v, want %v", got, want)
		}
	}
	// And that All actually routes through it, so the two cannot come apart.
	all := names(All())
	sorted := append([]string(nil), all...)
	sort.Strings(sorted)
	for i := range all {
		if all[i] != sorted[i] {
			t.Fatalf("All() is not name-ordered: got %v, want %v", all, sorted)
		}
	}
}

func names(exts []extension.Extension) []string {
	out := make([]string, len(exts))
	for i, e := range exts {
		out[i] = e.Name
	}
	return out
}
