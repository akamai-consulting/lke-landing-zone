package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// ext.go is the operator escape hatch: instance-specific subcommands declared in
// .llz/commands.yaml are registered as first-class cobra commands at startup.
// This replaces the old Makefile.local. See docs/extending-llz.md.

const extCommandsFile = ".llz/commands.yaml"

// extCommand is one operator-defined command. argv is run verbatim (extra args the
// operator passes on the CLI are appended), so it behaves like a shell alias.
//
// IT DOES NOT HAVE --dry-run SUPPORT, WHICH THIS COMMENT CLAIMED FOR A LONG TIME.
// The RunE below passes cliopts.Global.DryRun to proc.RunEcho, which reads like it
// works and cannot: DisableFlagParsing is set, so cobra hands this command EVERY
// remaining token — including a global flag typed BEFORE the subcommand — and root
// never parses one. `llz --dry-run smoke` therefore runs the command for real.
// Measured, not reasoned: the probe script wrote its file both ways.
//
// The dryRun argument is kept rather than replaced with a literal false, because
// the value is the honest source of truth for "did the operator ask for a dry run"
// and hard-coding false would make a future fix silently no-op. What is fixed is
// the CLAIM, which is what an operator was relying on.
//
// THE SECOND HALF IS THE ONE THAT BITES, and nothing documented it: the global flag
// is not ignored, it is APPENDED TO THE OPERATOR'S ARGV. `llz --dry-run smoke` runs
// `sh -c '…' --dry-run`. A script with strict flag parsing fails on an argument its
// author never passed; a lenient one runs for real while the operator believes they
// dry-ran it. docs/extending-llz.md says so now.
type extCommand struct {
	Name  string   `json:"name"`
	Short string   `json:"short"`
	Argv  []string `json:"argv"`
}

type extCommandsDoc struct {
	Commands []extCommand `json:"commands"`
}

// loadExtCommands reads dir/.llz/commands.yaml and returns the declared entries.
// A missing file is not an error (returns nil) — the file is optional.
func loadExtCommands(dir string) ([]extCommand, error) {
	b, err := os.ReadFile(filepath.Join(dir, extCommandsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc extCommandsDoc
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", extCommandsFile, err)
	}
	return doc.Commands, nil
}

// addExtCommands registers each valid extension command on root, warning (and
// skipping) on malformed entries or names that collide with a built-in.
func addExtCommands(root *cobra.Command, cmds []extCommand) {
	builtin := map[string]bool{}
	for _, c := range root.Commands() {
		builtin[c.Name()] = true
	}
	for _, ec := range cmds {
		ec := ec
		switch {
		case ec.Name == "" || len(ec.Argv) == 0:
			fmt.Fprintf(os.Stderr, "llz: skipping %s entry with empty name/argv\n", extCommandsFile)
			continue
		case builtin[ec.Name]:
			fmt.Fprintf(os.Stderr, "llz: skipping %s command %q (shadows a built-in)\n", extCommandsFile, ec.Name)
			continue
		}
		short := ec.Short
		if short == "" {
			short = "operator-defined command (" + extCommandsFile + ")"
		}
		root.AddCommand(&cobra.Command{
			Use:   ec.Name,
			Short: short,
			// Forward every arg to argv untouched — same convention as `drift` /
			// `env add`. (llz's own flags aren't parsed here, so they pass through
			// too; rely on your command's own flags rather than llz's.)
			DisableFlagParsing: true,
			RunE: func(_ *cobra.Command, args []string) error {
				return proc.RunEcho(cliopts.Global.DryRun, append(append([]string{}, ec.Argv...), args...)...)
			},
		})
	}
}
