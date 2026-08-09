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
// WHAT THIS DEFERRED, AND WHAT HAPPENED INSTEAD.
//
// This paragraph used to say "nothing here delivers a capability to anything. A
// gate holds `read-repo`, and reading the repo is what a process does by
// existing" — and predicted that the first binding kind needing
// `capability.Handles` delivered through dispatch would be an assertion or a
// transition, which is where the ABI question would become real.
//
// BOTH HALVES ARE NOW FALSE, and the second is the interesting one.
//
// Reading the repo is no longer what a process does by existing: `read-repo` has
// a handle, and every gate here is fenced to a tree. So gates DO consume a
// capability. But they acquire it WITHOUT AN ABI — each binding looks itself up
// from its own declaration at the point of use (capability.RepoForGate, and the
// cloudBinding/repoBinding accessors beside it), and this driver still passes
// nothing but flags.
//
// Self-service turned out to have a property a dispatcher could not offer.
// teardown selects its binding from `--yes`/`--dry-run` at RUNTIME, narrowing
// itself to a read-only handle on a dry run; a `Run func(Handles) error` would
// have had to choose the handle before the flags were parsed. An ABI would have
// foreclosed that.
//
// So the open question is no longer "when will something need Handles delivered
// through dispatch" but "does anything, given self-service works". The honest
// candidates are the cases self-service demonstrably cannot serve: a lane that
// must be handed a NARROWED capability by something other than itself, an
// auditor that needs a central record of what was handed out, and
// `template-sustain`, whose command is undriveable because its Deps are
// assembled in package main (see undrivenGates).
//
// That is a smaller question than the one deferred here, and it should be
// answered before an ABI is built rather than by building one.
// ────────────────────────────────────────────────────────────────────────────

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/manifestguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/budget"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/chartguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/cosignguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/credcoverage"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/docsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/meshegress"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/monitoringlabel"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/mtlsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/pincoherence"
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
	// NewWithTree, when set, is used instead of New and receives the LIVE cobra
	// tree as a value. It exists for gates that inspect the command set.
	//
	// THE DRIVER SILENTLY DISABLED A CHECK WITHOUT IT. docs-guard validates every
	// documented `llz …` invocation against the tree; run through New() the command
	// is PARENTLESS, its Root() is itself, and 868 invocations resolved against a
	// tree of one — all skipped, reported clean. `gates: 8 ran, all clean` while one
	// of the eight checked nothing, which is the vacuous-green shape this driver
	// refuses everywhere else, introduced by the driver.
	//
	// PARENTING IT IS NOT THE FIX. Attaching the command to the real root before
	// Execute() recurses forever: cobra's Execute() on a parented command delegates
	// to Root().ExecuteC(), re-running `llz ci gates` from os.Args. The tree must
	// arrive as data.
	NewWithTree func(tree *cobra.Command) *cobra.Command
}

// new returns the runnable command, preferring the tree-aware constructor.
func (g Gate) new(tree *cobra.Command) *cobra.Command {
	if g.NewWithTree != nil {
		return g.NewWithTree(tree)
	}
	return g.New()
}

// undrivenGates is every declared gate this binary does NOT drive, and why.
//
// ────────────────────────────────────────────────────────────────────────────
// IT IS A DECLARED LIST BECAUSE THE PROSE VERSION DRIFTED.
//
// This was a paragraph naming twelve gates, above a comment asserting that
// "TestUndrivenGatesAreNamedInTheSource prints the live numbers on every run, so
// this comment cannot quietly drift away from the model". It drifted anyway: the
// prose still said "6 are driven" and listed seven gates as undriven that were
// sitting in the table immediately below it.
//
// The test could not have caught it. It only LOGGED the live numbers and asserted
// nothing about the source, so the property its own docstring claimed was never
// checked — and the direction it missed is the one that matters: a gate moving
// from undriven to DRIVEN leaves a stale name behind, and a stale name reads as
// remaining work that is already done.
//
// So the list is data, and TestUndrivenGatesMatchTheModel compares it to the live
// set in BOTH directions — the same shape as allowedRawKubectl and the in-degree
// ratchet. Driving a gate now fails the build until its entry is deleted, which is
// how the paydown gets banked instead of rotting.
// ────────────────────────────────────────────────────────────────────────────
//
// The gap MATTERS because of how the driver reads when it is wrong: `gates: N ran,
// all clean` looks identical to a full pass. That is the vacuous-green shape every
// corpus guard in this tree already refuses, arriving one level up.
var undrivenGates = map[string]string{
	// NOT DRIVEABLE, found by the driver failing rather than by anyone reasoning
	// about it: `check-coverage` takes a coverprofile path and the whole
	// per-package floor list as positionals. That is Makefile knowledge, and the
	// floors live in COVERAGE_MINS precisely so they can be overridden per
	// invocation.
	//
	// That is a constraint on what "driveable" means, not an oversight: a gate
	// whose SUBJECT is chosen by the caller needs the caller. Whether the model
	// should let a binding declare its own corpus is a real question and is not
	// answered here — one case does not meet the two-case bar this repo uses for
	// changing the vocabulary.
	"guard-coverage": "check-coverage takes the coverprofile and the per-package floor list as positionals — Makefile knowledge",

	// NOT A REPO-SCANNING GATE. token-inventory's gate binding is `rotation-plan`,
	// which reads EVENT/CRON/SCOPE/CONFIRM/REASON/*_APPLY from the environment and
	// maps a GitHub Actions dispatch onto the step outputs the rotation jobs gate
	// on. Driving it from a file-in/findings-out driver would run a workflow router
	// with no workflow around it — it would fail on the unknown scope, or worse,
	// pass having routed nothing.
	"token-inventory": "the rotation-plan gate routes a GitHub Actions dispatch from the environment, not the tree",

	// ITS COMMAND IS STILL IN PACKAGE MAIN. `llz ci managed-fresh` is built from
	// sustainDeps(), one of main's fifteen Deps assemblers, and cmd/llz's own
	// header records why it stays there: a command that needs main to assemble its
	// capability's Deps cannot live on the other side of that assembly. The
	// registry is in internal/shared and cannot import main, so this is blocked on
	// moving the DI layer, not on writing a flag set.
	"template-sustain": "llz ci managed-fresh is assembled from main's sustainDeps(), which internal/shared cannot reach",
}

// gates is every gate binding this binary can RUN, as opposed to merely describe.
//
// `chart-lock-drift` is absent for the same not-driveable reason as
// `guard-coverage` above — it takes a chart directory as a POSITIONAL (the
// Makefile passes $(OPENBAO_CHART)) and the registry has no way to know which
// charts an instance has. It is not in undrivenGates because its EXTENSION,
// guard-charts, is driven for its other binding; the gap is per-command, and the
// model's unit here is the extension.
var gates = []Gate{
	{"guard-budgets", budget.CoreSurfaceCmd, []string{"--root", ".."}, nil},
	{"guard-budgets", budget.UntestableLOCCmd, []string{"--root", ".."}, nil},
	{"guard-charts", chartguard.ChartPinGuardCmd, []string{"--root", ".."}, nil},
	{"posture-credential-coverage", credcoverage.CoverageGuardCmd, []string{"--root", ".."}, nil},
	// The SECOND command of the same gate binding, and its absence was a hole of
	// exactly the shape this driver exists to close: the extension counted as
	// "driven" on the strength of one of its two checks, so `gates: N ran, all
	// clean` covered credential COVERAGE while the ExternalSecret path
	// cross-validation ran only from the Makefile. guard-manifests contributes
	// three commands for the same reason — the unit the model names is the
	// binding, not the command.
	{"posture-credential-coverage", credcoverage.ExternalSecretPathsCmd, []string{"--root", ".."}, nil},
	{"guard-docs", nil, []string{"--root", ".."}, docsguard.DocsGuardCmdFor},
	{"posture-plaintext", plaintext.PlaintextGuardCmd, []string{"--root", ".."}, nil},
	{"wave-health", wavehealth.DependencyGuardCmd, []string{"--root", ".."}, nil},
	{"wave-health", wavehealth.HealthGuardCmd, []string{"--root", ".."}, nil},

	// EIGHT MORE, converted from Makefile targets. Each was one `llz ci <verb>`
	// shell-out; the args are the ones the Makefile passed, carried over verbatim
	// rather than guessed.
	{"guard-cosign-subject", cosignguard.Cmd, []string{"--root", ".."}, nil},
	{"guard-monitoring-labels", monitoringlabel.Cmd, []string{"--root", ".."}, nil},
	{"guard-workflow-shells", workflowshells.Cmd, []string{"--dir", "../.github/workflows"}, nil},
	{"mesh-egress", meshegress.Cmd, []string{"--root", ".."}, nil},
	{"mtls-wiring", mtlsguard.Cmd, []string{"--root", ".."}, nil},
	{"version-pins", versionpins.Cmd, []string{"--root", ".."}, nil},
	// template-manifest scans the SCAFFOLD, not the repo — its subject is what an
	// instance receives, so its root is instance-template rather than `..`.
	{"template-manifest", templatemanifest.Cmd, []string{"--root", "../instance-template"}, nil},

	// guard-manifests is THREE commands under one gate binding
	// (`gate:scaffolded[read-repo]`, named "rendered-manifests"). All three read
	// the rendered tree, which mesh-egress already requires, so the driver's
	// standing assumption that `make render-charts` has run is unchanged.
	{"guard-manifests", manifestguard.DroppedAPIVersionsCmd, []string{"--root", ".."}, nil},
	{"guard-manifests", manifestguard.ArgoCDRenderedAppsCmd, []string{"--root", ".."}, nil},
	{"guard-manifests", manifestguard.PlaceholderGuardCmd, []string{"--root", ".."}, nil},

	// pin-coherence had no *cobra.Command at all — only `Assert(dir)`, called from
	// verbs/lint and from assert-image-fresh. It was undriveable for want of a flag
	// set rather than for any reason of substance, which a prose list of "the other
	// twelve" hid by putting it beside the two that genuinely cannot be driven.
	//
	// The TEMPLATE repo has no .copier-answers.yml, so this is silent here by
	// design and only speaks in an instance. That is not vacuous-green: the gate's
	// whole subject is a fact that exists only in an instance, and Assert
	// distinguishes "no instance" from "pins disagree".
	{"pin-coherence", pincoherence.Cmd, []string{"--root", ".."}, nil},
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
func RunGates(tree *cobra.Command, out, errOut io.Writer, toggles map[string]clusterspec.ComponentToggle) error {
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
		c := g.new(tree)
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
			"or a credential — and that is now enforced at runtime rather than only\n" +
			"declared: each gate reads through a handle fenced to the tree it was\n" +
			"pointed at, so a path outside the repository is refused rather than read.\n\n" +
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
			return RunGates(c.Root(), c.OutOrStdout(), c.ErrOrStderr(), toggles)
		},
	}
}
