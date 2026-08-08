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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/budget"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/chartguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/cosignguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/credcoverage"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/docsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/meshegress"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/monitoringlabel"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/mtlsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/plaintext"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/templatemanifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/versionpins"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/wavehealth"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/workflowshells"
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
// TWO GATES ARE NOT DRIVEABLE, both found by the driver failing rather than by
// anyone reasoning about it. `check-coverage` takes a coverprofile path and the
// whole per-package floor list as positionals — that is Makefile knowledge, and
// the floors live in COVERAGE_MINS precisely so they can be overridden per
// invocation. And `chart-lock-drift` takes a chart directory as a POSITIONAL
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

	// EIGHT MORE, converted from Makefile targets. Each was one `llz ci <verb>`
	// shell-out; the args are the ones the Makefile passed, carried over verbatim
	// rather than guessed.
	{"guard-cosign-subject", cosignguard.Cmd, []string{"--root", ".."}},
	{"guard-monitoring-labels", monitoringlabel.Cmd, []string{"--root", ".."}},
	{"guard-workflow-shells", workflowshells.Cmd, []string{"--dir", "../.github/workflows"}},
	{"mesh-egress", meshegress.Cmd, []string{"--root", ".."}},
	{"mtls-wiring", mtlsguard.Cmd, []string{"--root", ".."}},
	{"version-pins", versionpins.Cmd, []string{"--root", ".."}},
	// template-manifest scans the SCAFFOLD, not the repo — its subject is what an
	// instance receives, so its root is instance-template rather than `..`.
	{"template-manifest", templatemanifest.Cmd, []string{"--root", "../instance-template"}},
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
func RunGates(out, errOut io.Writer, toggles map[string]clusterspec.ComponentToggle) error {
	// ENABLEMENT IS LOAD-BEARING HERE, AND THIS IS WHERE IT STARTS. A gate is the
	// harmless case: skipping one runs fewer checks, which is visible in the
	// output and reversible by a toggle. Skipping an assert lane or a transition
	// would be a behaviour change hiding inside a config value, which is why the
	// resolver landed inert and is being made real one kind at a time.
	skip := map[string]string{}
	if toggles != nil {
		res, err := EnabledFor(toggles)
		if err != nil {
			return err
		}
		for _, e := range res {
			if !e.Enabled {
				skip[e.Extension.Name] = e.Reason
			}
		}
	}

	var failed, skipped []string
	for _, g := range Gates() {
		if why, off := skip[g.Extension]; off {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", g.Extension, why))
			continue
		}
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
	// SKIPS ARE PRINTED, ALWAYS, and on the clean path too. A driver that silently
	// runs fewer checks reads exactly like one that ran them all — the vacuous-green
	// shape this tree refuses everywhere else. An operator who disabled a component
	// should see the consequence named.
	for _, s := range skipped {
		fmt.Fprintf(out, "gates: skipped %s\n", s)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d gate(s) failed:\n\t%s", len(failed), strings.Join(failed, "\n\t"))
	}
	fmt.Fprintf(out, "gates: %d ran, %d skipped, all clean\n", len(Gates())-len(skipped), len(skipped))
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
			// NO SPEC MEANS RUN EVERYTHING, and that is not a fallback — it is the
			// template repo, where these gates guard the code that produces
			// instances and there is no instance whose components could excuse
			// one. Detected() reporting false is the normal case for `make lint`
			// here; treating it as "disable everything" would turn the whole gate
			// suite off in the one place it matters most.
			lz, ok, err := clusterspec.Detected()
			if err != nil {
				return fmt.Errorf("reading the instance spec to resolve enablement: %w", err)
			}
			// THE INSTANCE-WIDE DEFAULTS, NOT AN ENVIRONMENT'S, and the
			// distinction is a real seam between two models. Component toggles in
			// this spec are PER-ENVIRONMENT: one env can run Harbor and another
			// not. A gate is repo-scoped — it guards the code that produces every
			// environment — so there is no env to ask, and asking one would let a
			// single deployment's configuration silence a check protecting all of
			// them. spec.defaults.components is the instance-wide answer and the
			// only one a repo-scoped check can honestly use.
			var toggles map[string]clusterspec.ComponentToggle
			if ok && lz != nil {
				toggles = lz.Spec.Defaults.Components
			}
			return RunGates(c.OutOrStdout(), c.ErrOrStderr(), toggles)
		},
	}
}
