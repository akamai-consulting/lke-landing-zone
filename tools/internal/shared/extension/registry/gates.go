package registry

// gates.go — THE FIRST DISPATCH. A gate binding runs because the registry says it
// exists, not because someone wrote a call.
//
// This is issue #399's Phase 2 acceptance criterion, open since the declaration
// model landed: "a gate binding RUNS FROM THE REGISTRY, not from a hardcoded call.
// Without that this is a directory, not a framework." Sixty-one declarations had
// exactly one non-test consumer — `llz extension list` — so every binding was
// inert, and the model described a system that nothing consulted.
//
// WHY GATES FIRST. A gate is the one kind that needs no capability plumbing: the
// validator permits it `read-repo` and nothing else, so there is no client to
// scope, no credential to fence, and no argument about what a handle should look
// like. The guards/ bucket is 15 packages and 15-for-15 all-Gate, which is what
// makes it a clean target rather than a convenient one.
//
// ────────────────────────────────────────────────────────────────────────────
// IT NEEDS NO ACTION ABI, AND THAT IS THE DESIGN, not a shortcut.
//
// The obvious route was a `Run func(Handles) error` on Binding — the ABI the
// design doc has deferred since Phase 1 for want of a consumer. Two things ruled
// it out, and both were measured rather than assumed:
//
//   - THE GUARDS' ENTRY POINTS ARE NOT UNIFORM. Five are `Run(root string) error`;
//     the rest want a config path, a profile, an io.Writer pair, or the live cobra
//     tree (docs-guard resolves documented `llz …` invocations against it). Several
//     expose no exported run function at all. A single signature would have meant
//     rewriting fifteen packages to fit an ABI invented for them — the tail wagging
//     the dog, and the exact way a wrong ABI gets frozen.
//   - THE REGISTRY ALREADY HOLDS RUNNABLE THINGS. commands.go maps each extension
//     to its cobra constructors, by FUNCTION REFERENCE, so the compiler checks the
//     wiring. A *cobra.Command is an entry point that already exists, already takes
//     its own flags, and already works.
//
// So a gate is a BINDING plus the command that runs it, and the table below is the
// same shape commands.go established for the same reason: a function reference
// costs one line and renaming it breaks the build rather than a test three weeks
// later.
//
// WHAT THIS DEFERS, deliberately: nothing here delivers a capability to anything.
// A gate holds `read-repo`, and reading the repo is what a process does by
// existing. The first binding kind that needs `capability.Handles` delivered
// through dispatch is an assertion or a transition, and that is where the ABI
// question becomes real — with converge and import-brownfield as the cases the
// design doc nominates. This slice proves the registry can DRIVE; it does not
// claim to have answered how it hands anything over.
// ────────────────────────────────────────────────────────────────────────────

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/budget"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/chartguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/credcoverage"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/docsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/plaintext"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/wavehealth"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// Gate is one runnable gate: which extension declared it, and the command that IS
// it. Args are the fixed flags the driver supplies, because a gate reads a tree
// and the driver is what knows where the tree is.
type Gate struct {
	Extension string
	New       func() *cobra.Command
	Args      []string
}

// gates is every gate binding this binary can RUN, as opposed to merely describe.
//
// IT IS DELIBERATELY NOT THE WHOLE SET. Measured: 18 extensions declare a gate
// binding and 6 are driven here. The other 12 are still invoked only by the
// Makefile, one `llz ci <verb>` shell-out each:
//
//	guard-cosign-subject   guard-coverage        guard-manifests
//	guard-monitoring-labels guard-workflow-shells mesh-egress
//	mtls-wiring            pin-coherence         template-manifest
//	template-sustain       token-inventory       version-pins
//
// Converting them is mechanical, and listing them here before they are wired would
// be a table that lies. TestUndrivenGatesAreNamedInTheSource prints the live
// numbers on every run, so this comment cannot quietly drift away from the model —
// and it tells you to delete this note when the gap closes.
//
// The gap MATTERS because of how the driver reads when it is wrong: `gates: N ran,
// all clean` looks identical to a full pass. That is the vacuous-green shape every
// corpus guard in this tree already refuses, arriving one level up.
//
// AND ONE GATE IS NOT DRIVEABLE AT ALL, which the first run of this driver found
// by failing. `chart-lock-drift` takes a chart directory as a POSITIONAL argument
// — the Makefile passes $(OPENBAO_CHART) — and the registry has no way to know
// which charts an instance has. Supplying it here would put instance knowledge in
// the model.
//
// That is a constraint on what "driveable" means, not an oversight: a gate whose
// SUBJECT is chosen by the caller needs the caller. Whether the model should let a
// binding declare its own corpus is a real question and is not answered here — one
// case does not meet the two-case bar this repo uses for changing the vocabulary.
var gates = []Gate{
	{"guard-budgets", budget.CoreSurfaceCmd, []string{"--root", ".."}},
	{"guard-budgets", budget.UntestableLOCCmd, []string{"--root", ".."}},
	{"guard-charts", chartguard.ChartPinGuardCmd, []string{"--root", ".."}},
	{"posture-credential-coverage", credcoverage.CoverageGuardCmd, []string{"--root", ".."}},
	{"guard-docs", docsguard.DocsGuardCmd, []string{"--root", ".."}},
	{"posture-plaintext", plaintext.PlaintextGuardCmd, []string{"--root", ".."}},
	{"wave-health", wavehealth.DependencyGuardCmd, []string{"--root", ".."}},
	{"wave-health", wavehealth.HealthGuardCmd, []string{"--root", ".."}},
}

// Gates returns the runnable gates, sorted so output does not depend on the order
// someone happened to type the table in. Output stability is a CORRECTNESS
// property for a gate driver, for the reason guardwalk's SortFindings records: a
// gate whose output reorders between runs produces a diff every run, and a gate
// people stop reading is a gate that is not running.
func Gates() []Gate {
	out := append([]Gate(nil), gates...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Extension < out[j].Extension })
	return out
}

// GateBindings returns, for each extension in the registry, the gate bindings it
// declares. This is the DECLARED set — what the model says exists — as against
// Gates(), which is what this binary can run. The difference between them is the
// remaining work, and a test reports it rather than letting it drift.
func GateBindings() map[string][]extension.Binding {
	out := map[string][]extension.Binding{}
	for _, e := range All() {
		for _, b := range e.Bindings {
			if b.Kind == extension.Gate {
				out[e.Name] = append(out[e.Name], b)
			}
		}
	}
	return out
}

// RunGates runs every gate the registry can drive, in order, and reports which
// failed.
//
// IT DOES NOT STOP AT THE FIRST FAILURE. A gate driver that aborts makes a
// contributor fix one finding, re-run, and meet the next — which is the loop that
// teaches people to run the gates as late as possible. Collecting them costs
// nothing here because a gate reaches no cluster and cannot leave the tree in a
// half-changed state.
func RunGates(out, errOut io.Writer) error {
	var failed []string
	for _, g := range Gates() {
		c := g.New()
		c.SetArgs(g.Args)
		c.SetOut(out)
		c.SetErr(errOut)
		// A gate's cobra command prints its own findings; silencing usage keeps a
		// findings failure from printing a flag reference nobody wants.
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err != nil {
			failed = append(failed, fmt.Sprintf("%s/%s: %v", g.Extension, c.Name(), err))
			fmt.Fprintf(errOut, "::error::gate %s (%s) failed: %v\n", c.Name(), g.Extension, err)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d gate(s) failed:\n\t%s", len(failed), strings.Join(failed, "\n\t"))
	}
	fmt.Fprintf(out, "gates: %d ran, all clean\n", len(Gates()))
	return nil
}

// GatesCmd is `llz ci gates` — run every gate the registry declares AND can drive.
//
// It lives here rather than in cmd/llz for the reason the whole campaign exists:
// package main is the one package that cannot be imported or tested from outside,
// and a driver that only main can call is a driver only main can test.
func GatesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "gates",
		Short: "run every gate binding the registry declares (read-repo only, no cluster)",
		Long: "Runs each `gate` binding from the extension registry rather than from a\n" +
			"hardcoded list. A gate is file-in, findings-out: the validator permits it\n" +
			"`read-repo` and nothing else, so none of these reaches a cluster, a cloud\n" +
			"or a credential.\n\n" +
			"Not every declared gate is driven here yet — `llz extension list` shows the\n" +
			"declared set, and the ones still invoked only by the Makefile are named in\n" +
			"registry/gates.go.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return RunGates(c.OutOrStdout(), c.ErrOrStderr())
		},
	}
}
