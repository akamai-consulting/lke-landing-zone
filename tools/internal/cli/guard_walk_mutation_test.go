package cli

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/guardwalk"
)

// gwFinding is a guard finding with a THIRD field outside the sort key, so a
// comparator that swaps records it considers equal is observable. Every real
// caller (mtlsFinding, wdInversion) carries such payload fields.
type gwFinding struct {
	file, secondary, payload string
}

func gwKey(f gwFinding) (string, string) { return f.file, f.secondary }

func gwOrder(in []gwFinding) []string {
	out := make([]string, len(in))
	for i, f := range in {
		out[i] = f.file + "|" + f.secondary + "|" + f.payload
	}
	return out
}

func gwWant(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// TestSortGuardFindingsOrdersByFileThenSecondary pins the primary/secondary
// split. Every tree-scanning guard emits findings in walk order, so this
// comparator is the only thing making a guard's annotations reproducible across
// machines — a reversed or collapsed key turns a stable diff into a noisy one.
func TestSortGuardFindingsOrdersByFileThenSecondary(t *testing.T) {
	// Deliberately shuffled, and the secondary key is DESCENDING within b.yaml
	// while ascending across files, so file-vs-secondary confusion shows up.
	in := []gwFinding{
		{"b.yaml", "z", "1"},
		{"a.yaml", "m", "2"},
		{"b.yaml", "a", "3"},
		{"a.yaml", "c", "4"},
	}
	guardwalk.SortFindings(in, gwKey)
	gwWant(t, gwOrder(in), []string{
		"a.yaml|c|4",
		"a.yaml|m|2",
		"b.yaml|a|3",
		"b.yaml|z|1",
	})

	// The file key must DOMINATE: a later file with an early secondary must not
	// jump ahead of an earlier file with a late secondary.
	in2 := []gwFinding{
		{"z.yaml", "aaa", "1"},
		{"a.yaml", "zzz", "2"},
	}
	guardwalk.SortFindings(in2, gwKey)
	gwWant(t, gwOrder(in2), []string{"a.yaml|zzz|2", "z.yaml|aaa|1"})
}

// TestSortGuardFindingsKeepsFullyTiedFindingsPut is the strict-weak-ordering
// half. Two findings with the SAME (file, secondary) are equal to this
// comparator, so it must report neither as less than the other — a comparator
// that answers "less" for a tie makes sort.Slice shuffle records it cannot
// distinguish, which reorders the payload the operator actually reads.
func TestSortGuardFindingsKeepsFullyTiedFindingsPut(t *testing.T) {
	in := []gwFinding{
		{"a.yaml", "same", "first"},
		{"a.yaml", "same", "second"},
	}
	guardwalk.SortFindings(in, gwKey)
	gwWant(t, gwOrder(in), []string{"a.yaml|same|first", "a.yaml|same|second"})
}
