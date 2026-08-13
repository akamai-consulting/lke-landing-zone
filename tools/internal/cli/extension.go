package cli

// extension.go — `llz extension list`, the read-only window onto the declarations
// compiled into this binary.
//
// WHY A COMMAND AT ALL. The declaration model was built and validated with no way
// to look at it, which makes "the registry says X" a claim only a test can check.
// One listing makes the model's two hard-won facts legible to a reader instead:
// WHERE an extension attaches, and WHAT that attachment may touch — per binding,
// because scoping grants to the extension instead is the mistake that produced a
// one-line bypass (docs/designs/internal-extension-model.md).
//
// IT IS A VIEW, NOT THE SET AN INSTANCE RUNS. The ENABLED column shows the
// declared DEFAULT (`Always`), which an instance overrides in both directions
// through spec.components — registry.EnabledFor is what resolves that, and it is
// what `llz ci gates` and the assert battery actually dispatch on. Listing the
// resolved set would need a spec, and this command must work in a checkout that
// has none.
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
		// SHORTER THAN IT WANTS TO BE, on purpose. The spine belongs in ONE place —
		// the package doc of internal/shared/extension — and a second telling here
		// is a copy that drifts. It also costs core-surface budget (ADR 0014) to
		// restate in package main what a library package already says.
		Long: "Extensions declare WHERE they attach to the platform lifecycle\n" +
			"(bindings) and, per binding, WHAT that attachment may touch (grants).\n" +
			"They are load-bearing: `llz ci gates` dispatches gate bindings, enablement\n" +
			"resolves from them, and capability handles are built from these grants.\n" +
			"The model is one page: the package doc of internal/shared/extension.",
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
	if verbose {
		return listVerbose(out, exts)
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tENABLED\tBINDINGS\tGRANTS\tSUMMARY")
	for _, e := range exts {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			e.Name, enabledLabel(e), bindingSummary(e), grantSummary(e), e.Short)
	}
	return w.Flush()
}

// listVerbose renders one BLOCK per extension instead of continuation rows.
//
// ────────────────────────────────────────────────────────────────────────────
// THE TABLE COULD NOT CARRY THE DETAIL, AND PRETENDING IT COULD MADE IT
// UNREADABLE.
//
// Verbose used to add continuation rows to the same tabwriter. `Incomplete` is
// deliberately PROSE, so one long note sized the BINDINGS column for the entire
// table and every summary row rendered past 1,600 characters. Moving the content
// to the last column fixed the padding and pushed the detail off the right-hand
// edge instead — the two failure modes of putting a paragraph in a column.
//
// A listing whose whole job is to make the model legible cannot be the thing that
// hides it, so verbose stops being a table. The summary view keeps its alignment,
// which is what a skimmer needs; verbose gets a block, which is what a reader
// following one extension needs. They are different questions.
// ────────────────────────────────────────────────────────────────────────────
func listVerbose(out io.Writer, exts []extension.Extension) error {
	for i, e := range exts {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "%s  (%s)\n", e.Name, enabledLabel(e))
		// WHERE THE CODE IS, which the name does not tell you: thirty-four of the
		// sixty-five extensions — half of them — live in a package with a different name
		// (assert-storage in assertions/volumes, posture-at-rest in
		// lifecycle/atrest, import-brownfield in lifecycle/brownfield). Every error
		// message, gate exemption and ratchet entry in this tree names the
		// EXTENSION, so without this a reader holding a failure has no route to the
		// code. Derived from the declaration's constructor rather than transcribed —
		// see registry.Package.
		if pkg, ok := registry.Package(e.Name); ok {
			fmt.Fprintf(out, "  package  internal/extensions/%s\n", pkg)
		}
		if e.Component != "" {
			fmt.Fprintf(out, "  follows  spec.components.%s\n", e.Component)
		}
		fmt.Fprintf(out, "  %s\n", e.Short)
		for _, b := range e.Bindings {
			fmt.Fprintf(out, "    %s\n", b)
		}
		for _, note := range e.Incomplete {
			fmt.Fprintf(out, "    NOT DECLARED: %s\n", note)
		}
	}
	return nil
}

// enabledLabel is the ENABLED cell, and the ◐ marker beside it.
//
// A PARTIAL extension must not read as a complete one. The marker rides the
// ENABLED column because that is what a skimmer reads, and "always" beside a
// four-binding extension that has eight is the misreading Incomplete exists to
// prevent.
func enabledLabel(e extension.Extension) string {
	label := "opt-in"
	if e.Always {
		label = "always"
	}
	if len(e.Incomplete) > 0 {
		label += " ◐"
	}
	return label
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
