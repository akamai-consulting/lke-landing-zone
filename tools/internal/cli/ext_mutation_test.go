package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// An extension command's own `short:` is what shows up in `llz --help`; the
// generic "operator-defined command" text is the fallback for an entry that
// omitted it, not a replacement for one that supplied it.
func TestAddExtCommandsKeepsAnOperatorSuppliedShort(t *testing.T) {
	root := &cobra.Command{Use: "llz"}
	addExtCommands(root, []extCommand{
		{Name: "smoke", Short: "run the smoke suite", Argv: []string{"bash", "hack/smoke.sh"}},
		{Name: "psql", Argv: []string{"./hack/psql.sh"}}, // no short → the fallback
	})

	find := func(name string) *cobra.Command {
		for _, c := range root.Commands() {
			if c.Name() == name {
				return c
			}
		}
		return nil
	}

	c := find("smoke")
	if c == nil {
		t.Fatal("smoke should be registered")
	}
	if c.Short != "run the smoke suite" {
		t.Errorf("smoke Short = %q, want the operator's own text", c.Short)
	}

	c = find("psql")
	if c == nil {
		t.Fatal("psql should be registered")
	}
	if !strings.Contains(c.Short, "operator-defined command") || !strings.Contains(c.Short, extCommandsFile) {
		t.Errorf("psql Short = %q, want the generated fallback naming %s", c.Short, extCommandsFile)
	}
}
