package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func writeExtFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".llz"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, extCommandsFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadExtCommands(t *testing.T) {
	dir := t.TempDir()

	// Missing file is not an error.
	if cmds, err := loadExtCommands(dir); err != nil || cmds != nil {
		t.Fatalf("missing file: cmds=%v err=%v", cmds, err)
	}

	writeExtFile(t, dir, `commands:
  - name: smoke
    short: run smoke test
    argv: [bash, hack/smoke.sh]
  - name: psql
    argv: [./hack/psql.sh]
`)
	cmds, err := loadExtCommands(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 2 {
		t.Fatalf("got %d commands, want 2", len(cmds))
	}
	if cmds[0].Name != "smoke" || cmds[0].Short != "run smoke test" ||
		len(cmds[0].Argv) != 2 || cmds[0].Argv[0] != "bash" {
		t.Errorf("unexpected first command: %+v", cmds[0])
	}
}

func TestAddExtCommands(t *testing.T) {
	root := &cobra.Command{Use: "llz"}
	root.AddCommand(&cobra.Command{Use: "lint"}) // a built-in to shadow

	addExtCommands(root, []extCommand{
		{Name: "smoke", Short: "ok", Argv: []string{"bash", "x.sh"}},
		{Name: "lint", Argv: []string{"echo", "nope"}}, // collides -> skipped
		{Name: "", Argv: []string{"echo"}},             // malformed -> skipped
		{Name: "bad", Argv: nil},                       // malformed -> skipped
	})

	has := func(name string) *cobra.Command {
		for _, c := range root.Commands() {
			if c.Name() == name {
				return c
			}
		}
		return nil
	}
	if has("smoke") == nil {
		t.Error("smoke should be registered")
	}
	if has("bad") != nil {
		t.Error("malformed `bad` should be skipped")
	}
	// `lint` must still be the original built-in (unchanged Use/empty Short here).
	if c := has("lint"); c == nil || c.Short != "" {
		t.Error("collision should not replace the built-in lint command")
	}
}
