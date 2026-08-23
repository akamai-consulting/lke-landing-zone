package registry

// gates.go — THE FIRST DISPATCH. A gate binding runs because the registry says it
// exists, not because someone wrote a call. Without that the declaration model is
// a directory rather than a framework.
//
// WHY GATES FIRST. A gate is the one kind that needs no capability plumbing: the
// validator permits it `read-repo` and nothing else, so there is no client to
// scope, no credential to fence, and no argument about what a handle should look
// like. Do NOT navigate by "the guards/ bucket is all gates" — it no longer is
// (guard-coverage carries a `transition:scaffolded` beside its gate).
// TestEveryDrivenGateIsReadRepoOnly holds the line, and it checks the BINDING
// rather than the bucket.
//
// IT NEEDS NO ACTION ABI, AND THAT IS THE DESIGN, not a shortcut. The obvious
// route was a `Run func(Handles) error` on Binding. Two things ruled it out:
//
//   - THE GUARDS' ENTRY POINTS ARE NOT UNIFORM. Five are `Run(root string) error`;
//     the rest want a config path, a profile, an io.Writer pair, or the live cobra
//     tree (docs-guard resolves documented `llz …` invocations against it). Several
//     expose no exported run function at all. A single signature would mean
//     rewriting fifteen packages to fit an ABI invented for them, which is how a
//     wrong ABI gets frozen.
//   - THE REGISTRY ALREADY HOLDS RUNNABLE THINGS. commands.go maps each extension
//     to its cobra constructors, by FUNCTION REFERENCE, so the compiler checks the
//     wiring. A *cobra.Command already exists, already takes its own flags, and
//     already works.
//
// So a gate is a BINDING plus the command that runs it, and the table below is the
// shape commands.go established for the same reason: a function reference costs one
// line and renaming it breaks the build rather than a test three weeks later.
//
// GATES DO CONSUME A CAPABILITY — `read-repo` has a handle and every gate here is
// fenced to a tree — but they acquire it WITHOUT AN ABI: each binding looks itself
// up from its own declaration at the point of use (capability.RepoForGate, and the
// cloudBinding/repoBinding accessors beside it), and this driver passes nothing but
// flags.
//
// SELF-SERVICE HAS A PROPERTY A DISPATCHER CANNOT OFFER. teardown selects its
// binding from `--yes`/`--dry-run` at RUNTIME, narrowing itself to a read-only
// handle on a dry run; a `Run func(Handles) error` would have to choose the handle
// before the flags are parsed. So the open question is not "when will something
// need Handles delivered through dispatch" but "does anything, given self-service
// works" — and the honest candidates are the cases self-service demonstrably
// cannot serve: a lane that must be handed a NARROWED capability by something
// other than itself, and an auditor needing a central record of what was handed
// out. Answer that before building an ABI.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	clideps "github.com/akamai-consulting/lke-landing-zone/tools/internal/cli/deps"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/manifestguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/budget"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/callerperms"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/chartguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/cosignguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/credcoverage"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/defaultdeny"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/docsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/k8sminorcoherence"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/meshegress"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/monitoringlabel"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/mtlsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/mutabletags"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/pincoherence"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/plaintext"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/providerlock"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/runinjection"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/secretscope"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/setupgosite"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/sourceref"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/templatemanifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/versionpins"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/wavehealth"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/workflowshells"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// Gate is one runnable gate: which extension declared it, and the command that IS
// it. The driver supplies the subject tree, because a gate reads files and the
// driver is what knows where the repository is.
//
// FLAG AND SUBTREE REPLACED A LITERAL `Args: []string{"--root", ".."}` ON EVERY
// ROW, and the change is not cosmetic — see repoRoot below for the defect that
// literal carried. What it also bought is that the table now states only what is
// UNUSUAL about a row: twenty-seven of the thirty gates read the repository root
// through `--root`, so twenty-seven rows say nothing about their subject at all, and
// the three that differ say so in the field that differs — `guard-workflow-shells`
// (a different flag AND a subtree), `template-manifest` and `template-sustain` (a
// subtree each).
//
// THOSE NUMBERS ARE PINNED by TestTheDefaultedMajorityIsStillTheMajority. A count
// written in a comment and compared by nothing is a footnote, not a measurement:
// it goes stale silently and can contradict itself on its face without anyone
// noticing.
type Gate struct {
	Extension string
	New       func() *cobra.Command
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
	// Flag is the flag this gate takes its subject tree on. Empty means `--root`,
	// which is what every gate but one uses.
	Flag string
	// Subtree is the path UNDER the repository root this gate is pointed at. Empty
	// — the usual case — means the repository root itself.
	Subtree string
}

// new returns the runnable command, preferring the tree-aware constructor.
func (g Gate) new(tree *cobra.Command) *cobra.Command {
	if g.NewWithTree != nil {
		return g.NewWithTree(tree)
	}
	return g.New()
}

// args points the gate at its subject, relative to the caller's working directory.
//
// RELATIVE, NOT ABSOLUTE, and deliberately. The guards print the paths they found
// findings in, and CI reads those paths; handing them an absolute root would
// re-spell every finding in the output and every `::error file=` annotation with
// it. From `tools/` this yields exactly the `..` the Makefile has always passed,
// so the conversion changes no output at all — it changes only what happens when
// the caller is standing somewhere else.
//
// The absolute path is the fallback for the case filepath.Rel cannot express (a
// different volume on Windows). A correct absolute root beats a relative one that
// does not exist.
func (g Gate) args(root string) []string {
	flag := g.Flag
	if flag == "" {
		flag = "--root"
	}
	dir := root
	if g.Subtree != "" {
		dir = filepath.Join(root, g.Subtree)
	}
	if cwd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(cwd, dir); err == nil {
			dir = rel
		}
	}
	return []string{flag, dir}
}

// undrivenGates is every declared gate this binary does NOT drive, and why.
//
// ────────────────────────────────────────────────────────────────────────────
// IT IS A DECLARED LIST BECAUSE THE PROSE VERSION DRIFTED.
//
// This was a paragraph naming twelve gates, above a comment asserting that
// "TestUndrivenGatesMatchTheModel prints the live numbers on every run, so
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
}

// gates is every gate binding this binary can RUN, as opposed to merely describe.
//
// `chart-lock-drift` USED TO BE ABSENT for what looked like guard-coverage's
// reason — it took a chart directory as a POSITIONAL and "the registry has no way
// to know which charts an instance has". Reading the two side by side, they were
// never the same shape, and the difference is the whole finding:
//
//	check-coverage's subject is a COVERPROFILE, a build artifact that does not
//	exist until `go test` has run, plus a floor list the Makefile keeps
//	overridable on purpose. It is not a repo-scanning gate at all.
//
//	chart-lock-drift's subject is `kubernetes-charts/*/`, which chart-pin-guard
//	in the very same package had been enumerating all along.
//
// So it was a missing DEFAULT, not a gap in the model, and it now discovers its own
// corpus (refusing an empty one). THAT MATTERS BEYOND ONE GATE: the two cases
// together were the apparent two-case bar for giving the model a way to let a
// binding declare its corpus. One of them dissolved, so the bar is NOT met and no
// word is invented — which is the rule every other vocabulary change here followed.
var gates = []Gate{
	{Extension: "guard-budgets", New: budget.CoreSurfaceCmd},
	{Extension: "guard-budgets", New: budget.UntestableLOCCmd},
	{Extension: "guard-charts", New: chartguard.ChartPinGuardCmd},
	{Extension: "guard-charts", New: chartguard.ChartLockDriftCmd},
	{Extension: "posture-credential-coverage", New: credcoverage.CoverageGuardCmd},
	// The SECOND command of the same gate binding, and its absence was a hole of
	// exactly the shape this driver exists to close: the extension counted as
	// "driven" on the strength of one of its two checks, so `gates: N ran, all
	// clean` covered credential COVERAGE while the ExternalSecret path
	// cross-validation ran only from the Makefile. guard-manifests contributes
	// three commands for the same reason — the unit the model names is the
	// binding, not the command.
	{Extension: "posture-credential-coverage", New: credcoverage.ExternalSecretPathsCmd},
	{Extension: "guard-docs", NewWithTree: docsguard.DocsGuardCmdFor},
	{Extension: "posture-plaintext", New: plaintext.PlaintextGuardCmd},
	{Extension: "wave-health", New: wavehealth.DependencyGuardCmd},
	{Extension: "wave-health", New: wavehealth.HealthGuardCmd},

	// EIGHT MORE, converted from Makefile targets. Each was one `llz ci <verb>`
	// shell-out; the flags are the ones the Makefile passed, carried over rather
	// than guessed.
	{Extension: "guard-cosign-subject", New: cosignguard.Cmd},
	{Extension: "guard-monitoring-labels", New: monitoringlabel.Cmd},
	{Extension: "guard-source-refs", New: sourceref.Cmd},
	{Extension: "guard-source-refs", NewWithTree: sourceref.SymbolsCmdFor},

	// Reads .github/ AND instance-template/.github/, so it takes the repo root on
	// --root like the majority — not a Subtree like guard-workflow-shells below,
	// whose corpus is one directory. The two scan roots are the gate's own
	// business; handing it a subtree would silently halve what it checks.
	{Extension: "setup-go-sole-site", New: setupgosite.Cmd},
	// Reads ONE workflow — the image publisher — so it takes the repo root like
	// the majority rather than a Subtree: its subject is a single named file, and
	// handing it .github/workflows would only shorten the path it opens.
	{Extension: "guard-mutable-tags", New: mutabletags.Cmd},
	// Takes the repo root rather than a Subtree: its subject spans three trees at
	// once — the embedded roots, terraform-modules/ and the delivered scaffold — and
	// the whole point is comparing across them.
	{Extension: "guard-provider-lock", New: providerlock.Cmd},
	{Extension: "reusable-workflow-caller-permissions", New: callerperms.Cmd},
	{Extension: "workflow-injection", New: runinjection.Cmd},
	// Beside workflow-injection because it is the same subject read a different
	// way: that one asks what an expression can EXECUTE, this one asks what a
	// secret reference can RESOLVE. Both fail silently at runtime — an injection
	// looks like a successful step, an unresolved secret looks like an empty
	// string — so the tree is the only place either is visible.
	{Extension: "workflow-secret-scope", New: secretscope.Cmd},

	{Extension: "guard-workflow-shells", New: workflowshells.Cmd, Flag: "--dir", Subtree: ".github/workflows"},
	{Extension: "mesh-egress", New: meshegress.Cmd},
	// Beside mesh-egress because it reads the same two trees for the same reason:
	// a pod that cannot reach something, where nothing in the cluster says so.
	{Extension: "default-deny-egress", New: defaultdeny.Cmd},
	{Extension: "mtls-wiring", New: mtlsguard.Cmd},
	{Extension: "version-pins", New: versionpins.Cmd},
	// Beside version-pins because it is the same class of defect one authority
	// over: a relation between two individually well-formed pins. Its own subject
	// is the fidelity of the dry-run job in the table's own workflow — the kind
	// cluster's Kubernetes minor against the LKE-Enterprise minor the cluster
	// Terraform root pins.
	{Extension: "k8s-minor-coherence", New: k8sminorcoherence.Cmd},
	// template-manifest scans the SCAFFOLD, not the repo — its subject is what an
	// instance receives, so its subtree is instance-template rather than the root.
	{Extension: "template-manifest", New: templatemanifest.Cmd, Subtree: "instance-template"},

	// guard-manifests is THREE commands under one gate binding
	// (`gate:scaffolded[read-repo]`, named "rendered-manifests"). All three read
	// the rendered tree, which mesh-egress already requires, so the driver's
	// standing assumption that `make render-charts` has run is unchanged.
	{Extension: "guard-manifests", New: manifestguard.DroppedAPIVersionsCmd},
	{Extension: "guard-manifests", New: manifestguard.ArgoCDRenderedAppsCmd},
	{Extension: "guard-manifests", New: manifestguard.PlaceholderGuardCmd},

	// pin-coherence had no *cobra.Command at all — only `Assert(dir)`, called from
	// verbs/lint and from assert-image-fresh. It was undriveable for want of a flag
	// set rather than for any reason of substance, which a prose list of "the other
	// twelve" hid by putting it beside the two that genuinely cannot be driven.
	//
	// The TEMPLATE repo has no .copier-answers.yml, so this is silent here by
	// design and only speaks in an instance. That is not vacuous-green: the gate's
	// whole subject is a fact that exists only in an instance, and Assert
	// distinguishes "no instance" from "pins disagree".
	{Extension: "pin-coherence", New: pincoherence.Cmd},

	// UNBLOCKED BY MOVING THE DI LAYER, not by writing a flag set. This entry sat
	// in undrivenGates reading "llz ci managed-fresh is assembled from main's
	// sustainDeps(), which internal/shared cannot reach" — the gate was ordinary,
	// the ASSEMBLY was in the one package nothing may import. sustainDeps is
	// cli/deps.Sustain now, so the driver assembles the same value the CLI does.
	//
	// Its subject is the SCAFFOLD, like template-manifest: managed-fresh compares
	// instance-template's digest-locked files against the lock the template shipped.
	{Extension: "template-sustain", New: clideps.ManagedFreshCmd, Subtree: "instance-template"},
}

// repoMarker is what makes a directory the repository root.
//
// `.git` AND NOTHING ELSE, because the driver must answer the same question in two
// different trees: this template repo, and an instance repo rendered from it. A
// template-repo file (`.core-surface-budget.yaml`, `instance-template/`) would find
// the root here and fail in an instance; an instance file (`.copier-answers.yml`)
// does the reverse. `.git` is the one marker both have, and it is also the marker
// that means what the fence needs it to mean — the boundary of the checkout the
// operator asked about.
//
// It is matched as a NAME, not as a directory: `git worktree` and submodules write
// `.git` as a FILE holding a gitdir pointer, and a worktree of this repo is exactly
// where someone runs the gates.
const repoMarker = ".git"

// ErrNoRepoRoot is the refusal when the walk reaches the filesystem root.
var ErrNoRepoRoot = fmt.Errorf("no %s found in any parent directory", repoMarker)

// repoRoot walks up from start looking for the repository root.
//
// ────────────────────────────────────────────────────────────────────────────
// IT REPLACED A HARDCODED `..`, WHICH WAS A DEFECT AND NOT A CWD ARTIFACT.
//
// Every row of the table above used to pass `--root ".."` literally. That is
// correct from `tools/` — which is where the Makefile's LLZ_CI macro puts you, so
// `make llz-gates` was always right — and it is wrong from the repository root,
// which is the natural place to type the command. There, `..` is the PARENT OF THE
// REPOSITORY, and the suite silently changes subject:
//
//	llz ci gates            # from the repo root, before this change
//	guard-docs/docs-guard: docs-guard: 388 finding(s) across 908 file(s)
//
// 908 Markdown files, spanning every sibling checkout on the machine. It reported
// findings against repositories it was never pointed at.
//
// THE EMPTY-CORPUS RULE ALREADY CAUGHT EIGHTEEN OF THE NINETEEN, which is worth
// stating because it is the reason this looked survivable. Pointed at the wrong
// tree the other gates find nothing to read and fail closed by design. docs-guard
// is the exception for a structural reason rather than an oversight: its corpus is
// `**/*.md`, which is non-empty in almost any directory, so a wrong root produces a
// full-looking run over the wrong subject. Fail-closed-on-vacuity cannot save a
// guard whose subject exists everywhere.
//
// AND IT IS THE FENCE'S BLIND SPOT, which is the sharper half. `read-repo` now has
// a handle and every gate reads through it, so a path outside the tree is refused
// rather than read — but the fence is rooted at whatever `--root` says. It cannot
// tell a repository from its parent. A correct fence around the wrong tree reads as
// protection and is not, so the root has to be resolved rather than assumed.
// ────────────────────────────────────────────────────────────────────────────
func repoRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", start, err)
	}
	for {
		if _, err := os.Lstat(filepath.Join(dir, repoMarker)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		// filepath.Dir is its own fixed point at the root, which is the only
		// termination condition that holds on every platform.
		if parent == dir {
			return "", fmt.Errorf("locating the repository root from %s: %w", start, ErrNoRepoRoot)
		}
		dir = parent
	}
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

// Run selects and points the gate suite.
//
// A STRUCT RATHER THAN FOUR MORE PARAMETERS, because Root and Only are both
// strings and adjacent: a caller that transposed them would point the whole suite
// at a directory named after a gate, and get a corpus failure rather than anything
// that reads like the mistake it was.
type Run struct {
	// Root is the repository every gate reads under. Required — see repoRoot for
	// why the driver resolves it rather than assuming a working directory.
	Root string
	// Only, when set, narrows the suite to the gates whose EXTENSION or COMMAND
	// name matches. Empty runs everything.
	//
	// IT EXISTS SO THE MAKEFILE NEVER HAS TO KNOW A GATE'S FLAGS AGAIN. Thirteen
	// single-guard targets used to restate the `llz ci <verb> --root ..` the table
	// above already holds, so every gate had two spellings of its own invocation
	// and a flag change had to find both. Routing them through `--only` leaves one.
	//
	// The affordance those targets provided is real and the driver could not
	// replace it: iterating on ONE guard means running one guard, not the whole
	// table (32 rows, 29 of them taking the default subject — both pinned by
	// TestTheDefaultedMajorityIsStillTheMajority).
	// This is that, with the flags coming from the model.
	Only string
	// Toggles are the instance's component toggles, for enablement.
	Toggles map[string]clusterspec.ComponentToggle
}

// selected returns the gates this run covers, or an error if Only matched none.
//
// A NON-MATCHING FILTER IS A FAILURE, NOT AN EMPTY RUN. `--only wave-helth` would
// otherwise run nothing and report `0 ran, all clean` — the vacuous-green shape
// this driver refuses everywhere else, reachable by a typo.
func (r Run) selected(tree *cobra.Command) ([]Gate, error) {
	all := Gates()
	if r.Only == "" {
		return all, nil
	}
	var kept []Gate
	for _, g := range all {
		if g.Extension == r.Only || g.new(tree).Name() == r.Only {
			kept = append(kept, g)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("--only %q matches no driven gate: name an extension (%s) or one of "+
			"its commands, which `llz extension list` and registry/gates.go both show",
			r.Only, strings.Join(gateExtensions(), ", "))
	}
	return kept, nil
}

// gateExtensions names the driven extensions once, sorted, for the error above.
func gateExtensions() []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range Gates() {
		if !seen[g.Extension] {
			seen[g.Extension] = true
			out = append(out, g.Extension)
		}
	}
	sort.Strings(out)
	return out
}

// RunGates runs the selected gates, in order, and reports which failed. Every gate
// reads under r.Root (or under its declared Subtree), and nothing reads outside it.
//
// IT DOES NOT STOP AT THE FIRST FAILURE. A gate driver that aborts makes a
// contributor fix one finding, re-run, and meet the next — which is the loop that
// teaches people to run the gates as late as possible. Collecting them costs
// nothing here because a gate reaches no cluster and cannot leave the tree in a
// half-changed state.
func RunGates(tree *cobra.Command, r Run, out, errOut io.Writer) error {
	selected, err := r.selected(tree)
	if err != nil {
		return err
	}
	// ENABLEMENT IS LOAD-BEARING HERE, AND THIS IS WHERE IT STARTS. A gate is the
	// harmless case: skipping one runs fewer checks, which is visible in the
	// output and reversible by a toggle. Skipping an assert lane or a transition
	// would be a behaviour change hiding inside a config value, which is why the
	// resolver landed inert and is being made real one kind at a time.
	skip := map[string]string{}
	if r.Toggles != nil {
		res, err := EnabledFor(r.Toggles)
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
	for _, g := range selected {
		if why, off := skip[g.Extension]; off {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", g.Extension, why))
			continue
		}
		c := g.new(tree)
		c.SetArgs(g.args(r.Root))
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
	fmt.Fprintf(out, "gates: %d ran, %d skipped, all clean\n", len(selected)-len(skipped), len(skipped))
	return nil
}

// GatesCmd is `llz ci gates` — run every gate the registry declares AND can drive.
//
// It lives here rather than in the CLI layer, and the reason has CHANGED since it
// was written. It used to say "package main is the one package that cannot be
// imported or tested from outside, and a driver that only main can call is a driver
// only main can test". That was ADR 0014's ground 1, and ADR 0014's own amendment
// retires it: the tree moved to internal/cli, which is an ordinary importable,
// testable package, so untestability is no longer what is being avoided.
//
// What survives is ground 2, and it is enough. internal/cli is the WIRING layer and
// wiring is all it may hold; a driver that reads the registry, resolves enablement
// and decides what runs is not wiring. Saying so is weaker than the untestability
// argument and is written down as weaker, for the reason the ADR gives — a ratchet
// whose reason has quietly changed is a ratchet people stop believing.
func GatesCmd() *cobra.Command {
	var root, only string
	c := &cobra.Command{
		Use:   "gates",
		Short: "run every gate binding the registry declares (read-repo only, no cluster)",
		Long: "Runs each `gate` binding from the extension registry rather than from a\n" +
			"hardcoded list. A gate is file-in, findings-out: the validator permits it\n" +
			"`read-repo` and nothing else, so none of these reaches a cluster, a cloud\n" +
			"or a credential — and that is now enforced at runtime rather than only\n" +
			"declared: each gate reads through a handle fenced to the tree it was\n" +
			"pointed at, so a path outside the repository is refused rather than read.\n\n" +
			"The tree is the repository this is run from, located by walking up for a\n" +
			"`.git`, so the suite reads the same files from any working directory.\n" +
			"Pass --root to point it somewhere else.\n\n" +
			"--only <extension|command> runs a single guard while iterating on it,\n" +
			"with the same flags the whole suite would have given it. It is an error\n" +
			"for --only to match nothing, so a typo cannot report a clean run.\n\n" +
			"Not every declared gate is driven here yet — `llz extension list` shows the\n" +
			"declared set, and the ones still invoked only by the Makefile are named in\n" +
			"registry/gates.go.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			// RESOLVED, NOT ASSUMED, and it FAILS rather than falling back to the
			// working directory. A gate suite that silently changes its subject
			// based on where it was invoked is worse than one that refuses to run:
			// the fallback's failure mode is a full-looking run over the wrong
			// tree, which is precisely what the hardcoded `..` produced.
			if root == "" {
				found, err := repoRoot(".")
				if err != nil {
					return fmt.Errorf("%w — run this inside a checkout, or pass --root", err)
				}
				root = found
			}
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
			return RunGates(c.Root(), Run{Root: root, Only: only, Toggles: toggles},
				c.OutOrStdout(), c.ErrOrStderr())
		},
	}
	c.Flags().StringVar(&root, "root", "", "repository root to run the gates against (default: the enclosing checkout)")
	c.Flags().StringVar(&only, "only", "", "run just this gate, by extension or command name")
	return c
}
