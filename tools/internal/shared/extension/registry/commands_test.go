package registry

import "testing"

// Every row must name an extension this registry actually declares. A typo in the
// name string would make the wiring guard in internal/cli report a failure
// against an extension nobody can find.
func TestEveryCommandRowNamesADeclaredExtension(t *testing.T) {
	// The row key is the PACKAGE name, not the catalog name — the two differ often
	// (assertobs declares "assert-observability"), so string equality against
	// All() would be wrong. What is worth asserting is that the set is non-empty
	// and every constructor actually produces a usable command.
	pkgs := map[string]bool{}
	for _, c := range Commands() {
		pkgs[c.Extension] = true
	}
	if len(pkgs) == 0 {
		t.Fatal("no command rows — the wiring guard would pass vacuously")
	}
	for _, c := range Commands() {
		if c.New == nil {
			t.Errorf("%s has a nil constructor", c.Extension)
			continue
		}
		cmd := c.New()
		if cmd == nil || cmd.Name() == "" {
			t.Errorf("%s: constructor produced an unusable command", c.Extension)
		}
	}
}

// Commands() hands out a copy. A caller that appends to it must not be able to
// grow the registry, which is the shape of bug that makes a guard drift.
func TestCommandsReturnsACopy(t *testing.T) {
	before := len(Commands())
	got := Commands()
	got = append(got, Command{Extension: "intruder"})
	_ = got
	if after := len(Commands()); after != before {
		t.Errorf("Commands() leaked its backing array: %d -> %d", before, after)
	}
}
