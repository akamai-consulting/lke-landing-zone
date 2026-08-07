package main

// extension.go — `llz extension list`, the read-only window onto the declarations
// compiled into this binary.
//
// WHY A COMMAND AT ALL, WHEN NOTHING IS WIRED UP YET. The declaration model was
// built and validated with no way to look at it, which makes "the registry says
// X" a claim only a test can check. One listing makes the model's two hard-won
// facts legible to a reader instead: WHERE an extension attaches, and WHAT that
// attachment may touch — per binding, because scoping grants to the extension
// instead is the mistake that produced a one-line bypass
// (docs/designs/internal-extension-model.md).
//
// NOT `llz ext`. That name is already the operator escape hatch — instance-defined
// subcommands from .llz/commands.yaml (ext.go) — and the two are unrelated: one is
// what an operator adds to their instance, the other is what this binary is built
// from. Cobra would have accepted `ext` as a prefix of `extension` and resolved it
// to whichever it matched first, which is precisely the kind of collision that
// only shows up as a support question.

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension/registry"
)

func extensionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "extension",
		Short: "inspect the extension declarations compiled into this binary",
		Long: "Extensions declare WHERE they attach to the platform lifecycle\n" +
			"(bindings) and, per binding, WHAT that attachment may touch (grants).\n" +
			"Nothing is loaded or executed through this yet — the declaration model\n" +
			"is Phase 1 (docs/designs/internal-extension-model.md); this command is\n" +
			"how you read it.",
	}
	c.AddCommand(extensionListCmd())
	return c
}

func extensionListCmd() *cobra.Command {
	var verbose bool
	c := &cobra.Command{
		Use:   "list",
		Short: "list compiled-in extensions, their bindings and their grants",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return listExtensions(os.Stdout, registry.All(), verbose)
		},
	}
	c.Flags().BoolVar(&verbose, "verbose", false, "show every binding on its own line, with the grants it holds")
	return c
}

// listExtensions writes the listing. The writer is a parameter because the output
// IS the product — the same lesson the budget engine learned the expensive way,
// where two mutants survived the whole suite because nothing could read what the
// gate printed.
func listExtensions(out io.Writer, exts []extension.Extension, verbose bool) error {
	if len(exts) == 0 {
		fmt.Fprintln(out, "no extensions are compiled into this binary")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tENABLED\tBINDINGS\tGRANTS\tSUMMARY")
	for _, e := range exts {
		enabled := "opt-in"
		if e.Always {
			enabled = "always"
		}
		// A PARTIAL extension must not read as a complete one. The marker goes in
		// the ENABLED column rather than a footnote because that column is what a
		// skimmer reads, and "always" beside a four-binding extension that has
		// eight is the misreading Incomplete exists to prevent.
		if len(e.Incomplete) > 0 {
			enabled += " ◐"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			e.Name, enabled, bindingSummary(e), grantSummary(e), e.Short)
		if verbose {
			// Continuation rows leave NAME/ENABLED empty and put the full binding —
			// including the grants THAT binding holds — in the BINDINGS column. Not
			// indented under the name: a wider first cell would stretch the column
			// for every row, and the summary line is the one that has to stay
			// scannable.
			for _, b := range e.Bindings {
				fmt.Fprintf(w, "\t\t%s\t\t\n", b)
			}
			for _, note := range e.Incomplete {
				fmt.Fprintf(w, "\t\tNOT DECLARED: %s\t\t\n", note)
			}
		}
	}
	return w.Flush()
}

// bindingSummary collapses an extension's bindings to `kind:state` pairs. The
// per-binding grants are deliberately NOT folded in here — they are what --verbose
// exists for, because a union printed on one line is the exact misreading
// (extension-scoped grants) the model was corrected to avoid.
//
// A PRECONDITION IS SHOWN, because dropping it would make this line wrong rather
// than merely brief. `transition:seeded` and `transition:seeded<operating` are
// different claims — the second says the action runs against a platform that is
// already up — and this command's own help says it lists bindings. Collapsing two
// distinct declarations onto one string is the banning-by-omission shape the whole
// model exists to avoid, in the surface an operator actually reads. It is rendered
// `<state` rather than the Binding.String() spelling because this column holds
// several bindings side by side and has no room for six words; the arrow points
// from the binding to what it needs.
func bindingSummary(e extension.Extension) string {
	seen := map[string]bool{}
	var out []string
	for _, b := range e.Bindings {
		at := string(b.Kind) + ":" + string(b.State)
		if b.Requires != "" {
			at += "<" + string(b.Requires)
		}
		if !seen[at] {
			seen[at] = true
			out = append(out, at)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// grantSummary is Extension.Grants — the DERIVED union, labelled as a summary so
// nobody reads it as the thing any single binding holds.
func grantSummary(e extension.Extension) string {
	gs := e.Grants()
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = string(g)
	}
	return strings.Join(out, ",")
}
