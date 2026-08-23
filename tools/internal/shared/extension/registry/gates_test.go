package registry

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// THE ACCEPTANCE CRITERION, ASSERTED DIRECTLY. Issue #399: "a gate binding RUNS
// FROM THE REGISTRY, not from a hardcoded call in runLint. Without that this is a
// directory, not a framework."
//
// This is what says it does. Every gate the driver runs is reachable purely from
// registry data — no import of the guard packages here, no list to keep in step.
func TestGatesAreDrivenByTheRegistryNotAList(t *testing.T) {
	gs := Gates()
	if len(gs) == 0 {
		t.Fatal("the gate table is empty — the driver would report `all clean` having run " +
			"nothing, which is the vacuous-pass shape every corpus guard in this tree exists " +
			"to refuse")
	}
	declared := GateBindings()
	for _, g := range gs {
		if _, ok := declared[g.Extension]; !ok {
			t.Errorf("gates names %q, which declares no gate binding in the registry. The "+
				"table must reference the MODEL: an entry for an extension that does not "+
				"declare a gate is a hardcoded call wearing the registry's name.", g.Extension)
		}
		if g.New == nil && g.NewWithTree == nil {
			t.Errorf("%s has a nil constructor", g.Extension)
			continue
		}
		if c := g.new(nil); c == nil || c.Name() == "" {
			t.Errorf("%s's constructor produced no runnable command", g.Extension)
		}
	}
}

// A gate may hold `read-repo` and nothing else — the validator enforces it on the
// declaration, and this enforces that the DRIVER only ever runs such bindings. It
// is what keeps `llz ci gates` safe to run anywhere: no cluster, no cloud, no
// credential, whatever anyone adds to the table later.
func TestEveryDrivenGateIsReadRepoOnly(t *testing.T) {
	declared := GateBindings()
	for _, g := range Gates() {
		for _, b := range declared[g.Extension] {
			for _, gr := range b.Grants {
				if gr != extension.ReadRepo {
					t.Errorf("%s's gate binding holds %q — the driver promises `llz ci gates` "+
						"touches nothing but files, so a gate that grew a grant must leave "+
						"the table before it grows one", g.Extension, gr)
				}
			}
		}
	}
}

// THE GAP IS REPORTED, NOT HIDDEN. Not every declared gate is driven yet, and a
// driver that quietly covers half its subject is worse than one that says so —
// the reader sees `gates: N ran, all clean` and concludes the gates are green.
//
// THIS TEST REPLACES ONE THAT CHECKED NOTHING. Its predecessor logged the live
// numbers and asserted nothing, while its docstring claimed the source comment
// "cannot drift away from the model". The comment drifted: it still said six
// gates were driven when thirteen were, and named seven as undriven that were in
// the table directly below it.
//
// So this compares undrivenGates to the live set in BOTH directions. The second
// direction is the one the old test could never have had: a gate that becomes
// DRIVEN leaves a stale entry behind, and a stale entry reads as remaining work
// that is already done — which is how the previous drift went unnoticed through
// eight conversions.
func TestUndrivenGatesMatchTheModel(t *testing.T) {
	driven := map[string]bool{}
	for _, g := range Gates() {
		driven[g.Extension] = true
	}
	live := map[string]bool{}
	for name := range GateBindings() {
		if !driven[name] {
			live[name] = true
		}
	}

	var missing, stale []string
	for name := range live {
		if _, ok := undrivenGates[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name, why := range undrivenGates {
		if !live[name] {
			stale = append(stale, name)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("undrivenGates[%q] has no reason — an entry without one is indistinguishable "+
				"from an oversight, which is exactly what this list exists to tell apart", name)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	if len(missing) > 0 {
		t.Errorf("declared gate(s) neither driven nor listed: %s. Either add them to the gates "+
			"table, or add them to undrivenGates with the reason they cannot be driven — an "+
			"undriven gate nobody wrote down is one `gates: N ran, all clean` silently omits",
			strings.Join(missing, ", "))
	}
	if len(stale) > 0 {
		t.Errorf("undrivenGates still names %s, but %s driven now — DELETE the entr%s in this "+
			"commit. A stale name reads as work still to do and is how the prose version of this "+
			"list came to claim six driven gates when there were thirteen",
			strings.Join(stale, ", "),
			map[bool]string{true: "they are", false: "it is"}[len(stale) > 1],
			map[bool]string{true: "ies", false: "y"}[len(stale) > 1])
	}

	t.Logf("declared gate extensions: %d, driven: %d, undriven: %v",
		len(GateBindings()), len(driven), keysOf(live))

	if len(live) == 0 {
		t.Log("every declared gate is now driven — delete undrivenGates, the note above it " +
			"in gates.go, and this test with them")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RunGates must COLLECT failures rather than stop at the first, and must say how
// many ran. A driver that aborts on the first finding makes a contributor fix one,
// re-run, meet the next — the loop that teaches people to run gates as late as
// possible.
func TestRunGatesReportsEveryFailureNotJustTheFirst(t *testing.T) {
	orig := gates
	t.Cleanup(func() { gates = orig })

	gates = []Gate{
		{Extension: "a", New: failingCmd("one")},
		{Extension: "b", New: failingCmd("two")},
		{Extension: "c", New: okCmd()},
	}

	var out, errOut bytes.Buffer
	err := RunGates(nil, Run{Root: "."}, &out, &errOut)
	if err == nil {
		t.Fatal("RunGates returned nil with two failing gates")
	}
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention the %q failure — it stopped early, or it "+
				"reported only the last", err, want)
		}
	}
	if !strings.Contains(err.Error(), "2 gate(s) failed") {
		t.Errorf("error %q does not count the failures", err)
	}
}

// The clean path must say how many ran. `all clean` with no number is how a driver
// that ran nothing reads exactly like one that ran everything.
func TestRunGatesReportsTheCountOnSuccess(t *testing.T) {
	orig := gates
	t.Cleanup(func() { gates = orig })
	gates = []Gate{{Extension: "a", New: okCmd()}, {Extension: "b", New: okCmd()}}

	var out, errOut bytes.Buffer
	if err := RunGates(nil, Run{Root: "."}, &out, &errOut); err != nil {
		t.Fatalf("RunGates failed on clean gates: %v", err)
	}
	if !strings.Contains(out.String(), "2 ran") {
		t.Errorf("success output %q does not say how many ran", out.String())
	}
}

// failingCmd and okCmd are the synthetic gates the driver tests run against.
// Synthetic rather than real, because a driver test that runs the actual guards
// measures the guards; what is under test here is the DRIVER's behaviour when a
// gate fails, which no real guard can be made to do on demand without breaking the
// tree it scans.
//
// BOTH DECLARE `--root`, because the driver now points every gate at the resolved
// repository root and a synthetic gate that accepted no flags would make the whole
// driver suite pass against a code path the real gates never take.
func failingCmd(msg string) func() *cobra.Command {
	return func() *cobra.Command {
		c := &cobra.Command{
			Use:  "gate-" + msg,
			RunE: func(*cobra.Command, []string) error { return errors.New(msg) },
		}
		c.Flags().String("root", "", "repository root")
		return c
	}
}

func okCmd() func() *cobra.Command {
	return func() *cobra.Command {
		c := &cobra.Command{Use: "ok", RunE: func(*cobra.Command, []string) error { return nil }}
		c.Flags().String("root", "", "repository root")
		return c
	}
}

// ENABLEMENT IS LOAD-BEARING HERE, and this is the assertion that says so. Every
// other enablement test checks the RESOLVER; this checks that the driver acts on
// it — the difference between knowing an extension is off and not running it.
func TestRunGatesSkipsADisabledExtension(t *testing.T) {
	orig := gates
	t.Cleanup(func() { gates = orig })
	// wave-health follows no component, so pick one that does. obj-encryption is
	// not a gate, so borrow its NAME for a synthetic entry: what is under test is
	// the driver's skip logic keyed on the resolver, not the guard behind it.
	gates = []Gate{{Extension: "obj-encryption", New: okCmd()}, {Extension: "guard-docs", New: okCmd()}}

	off := false
	toggles := map[string]clusterspec.ComponentToggle{"objProxy": {Enabled: &off}}

	var out, errOut bytes.Buffer
	if err := RunGates(nil, Run{Root: ".", Toggles: toggles}, &out, &errOut); err != nil {
		t.Fatalf("RunGates failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "skipped obj-encryption") {
		t.Errorf("output %q does not skip the disabled extension — the resolver said off and "+
			"the driver ran it anyway", got)
	}
	if !strings.Contains(got, "objProxy") {
		t.Errorf("output %q does not name the component that disabled it — an operator "+
			"needs to know WHICH toggle did it", got)
	}
	if !strings.Contains(got, "1 ran, 1 skipped") {
		t.Errorf("output %q does not count runs and skips separately — `all clean` over a "+
			"silently reduced set is the vacuous-green shape this tree refuses", got)
	}
}

// GatesCmd's own wiring, which nothing else reaches: the flags it declares, and
// that its RunE resolves a spec rather than assuming one. Executed against a
// synthetic table so this measures the COMMAND, not the fifteen real guards.
func TestGatesCmdRunsTheDriver(t *testing.T) {
	orig := gates
	t.Cleanup(func() { gates = orig })
	gates = []Gate{{Extension: "guard-docs", New: okCmd()}}

	c := GatesCmd()
	if c.Use != "gates" || c.Short == "" {
		t.Errorf("command identity drifted: use=%q short=%q", c.Use, c.Short)
	}

	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(nil)
	// Runs in the tools/ tree, where clusterspec.Detected() finds no instance —
	// which is the TEMPLATE REPO case, and must run everything rather than
	// resolving to "disable all".
	if err := c.Execute(); err != nil {
		t.Fatalf("GatesCmd failed: %v", err)
	}
	if !strings.Contains(out.String(), "1 ran") {
		t.Errorf("output %q — with no instance spec every gate must still run; treating a "+
			"missing spec as `nothing enabled` would silence the whole suite in the one "+
			"place these gates matter most", out.String())
	}
}

// THE DRIVER MUST GIVE A TREE-INSPECTING GATE THE REAL TREE.
//
// docs-guard validates every documented `llz …` invocation against the cobra
// tree. Run through a plain constructor the command is PARENTLESS and its Root()
// is itself, so it resolved 868 invocations against a tree of one, skipped all of
// them, and reported clean — `gates: 8 ran, all clean` with one of the eight
// checking nothing.
//
// Asserted on the CONSTRUCTOR rather than by running docs-guard, because running
// it here would scan the whole repo in a unit test. What can go wrong is the
// wiring: a Gate that needs the tree losing NewWithTree, or the driver stopping
// passing it.
func TestATreeInspectingGateReceivesTheTree(t *testing.T) {
	var got *cobra.Command
	orig := gates
	t.Cleanup(func() { gates = orig })

	want := &cobra.Command{Use: "llz"}
	gates = []Gate{{
		Extension: "guard-docs",
		NewWithTree: func(tree *cobra.Command) *cobra.Command {
			got = tree
			return okCmd()()
		},
	}}

	var out, errOut bytes.Buffer
	if err := RunGates(want, Run{Root: "."}, &out, &errOut); err != nil {
		t.Fatalf("RunGates failed: %v", err)
	}
	if got == nil {
		t.Fatal("the gate was built with no tree — docs-guard would fall back to its own " +
			"Root(), which is itself, and silently check nothing")
	}
	if got != want {
		t.Errorf("the gate received %v, not the tree the driver was given", got)
	}
}

// Every gate must be buildable. A table entry with neither constructor would
// panic at run time, and the driver is the last place that should discover it.
func TestEveryGateHasAConstructor(t *testing.T) {
	for _, g := range Gates() {
		if g.New == nil && g.NewWithTree == nil {
			t.Errorf("%s has neither New nor NewWithTree", g.Extension)
		}
	}
}

// ── the repository root ───────────────────────────────────────────────────────

// THE DEFECT THIS REPLACED, PINNED. Every gate row used to pass `--root ".."`
// literally, which is the repository root from `tools/` (where the Makefile's
// LLZ_CI macro puts you) and the PARENT OF THE REPOSITORY from anywhere else. Run
// from the repo root, docs-guard reported 388 findings across 908 Markdown files
// spanning every sibling checkout on the machine.
//
// So the property is: the same tree, from any working directory.
func TestTheSubjectIsTheRepositoryFromAnyDirectory(t *testing.T) {
	root := t.TempDir()
	// Lstat, not Stat, is what the walk uses — so a plain file is a legal marker
	// here, which is also what `git worktree` and submodules actually write.
	if err := os.WriteFile(filepath.Join(root, repoMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "tools", "internal", "shared")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, start := range []string{root, filepath.Join(root, "tools"), deep} {
		got, err := repoRoot(start)
		if err != nil {
			t.Fatalf("repoRoot(%s): %v", start, err)
		}
		// EvalSymlinks because t.TempDir is under /var on darwin, which is a
		// symlink to /private/var — comparing the raw strings would fail for a
		// reason that has nothing to do with the walk.
		want, _ := filepath.EvalSymlinks(root)
		gotResolved, _ := filepath.EvalSymlinks(got)
		if gotResolved != want {
			t.Errorf("from %s the driver would run against %s, not the repository %s — "+
				"a gate suite that changes subject with the working directory reports "+
				"findings against trees it was never pointed at", start, got, root)
		}
	}
}

// IT FAILS RATHER THAN FALLING BACK. A fallback to the working directory has
// exactly the failure mode the hardcoded `..` had: a full-looking run over the
// wrong tree. Refusing is the only answer that cannot be mistaken for a pass.
func TestNoRepositoryIsRefusedNotDefaulted(t *testing.T) {
	// A temp dir with no marker, and t.TempDir is not inside a checkout.
	if _, err := repoRoot(t.TempDir()); !errors.Is(err, ErrNoRepoRoot) {
		t.Errorf("repoRoot outside a checkout returned %v — it must refuse, because the "+
			"alternative is a gate suite that silently scans whatever it happens to "+
			"be standing in", err)
	}
}

// The Makefile has always run these from `tools/` with `--root ..`, and the
// conversion must not re-spell a single path: the guards print the paths they
// found findings in, and CI reads them. From `tools/` the driver must still say
// exactly `..`.
func TestTheMakefileInvocationIsByteIdentical(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := repoRoot(cwd)
	if err != nil {
		t.Fatalf("this test must run inside the checkout: %v", err)
	}
	tools := filepath.Join(root, "tools")
	t.Chdir(tools)

	for _, tc := range []struct {
		name string
		gate Gate
		want []string
	}{
		{"the usual gate", Gate{Extension: "guard-budgets"}, []string{"--root", ".."}},
		{"a subtree gate", Gate{Extension: "template-manifest", Subtree: "instance-template"},
			[]string{"--root", "../instance-template"}},
		{"a gate with its own flag", Gate{Extension: "guard-workflow-shells", Flag: "--dir", Subtree: ".github/workflows"},
			[]string{"--dir", "../.github/workflows"}},
	} {
		got := tc.gate.args(root)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: args = %v, want %v — the Makefile passed the second spelling for "+
				"years and the guards' output is relative to it", tc.name, got, tc.want)
		}
	}
}

// Every row must be expressible without a literal path. A row that grew one would
// be the hardcoded `..` coming back under another name.
func TestNoGateCarriesAnAbsoluteOrEscapingSubtree(t *testing.T) {
	for _, g := range Gates() {
		if filepath.IsAbs(g.Subtree) {
			t.Errorf("%s declares an absolute subtree %q — a subtree is a path UNDER the "+
				"repository root, and an absolute one escapes the fence the root exists to set",
				g.Extension, g.Subtree)
		}
		if g.Subtree != "" && strings.Contains(filepath.Clean(g.Subtree), "..") {
			t.Errorf("%s declares subtree %q, which leaves the repository", g.Extension, g.Subtree)
		}
	}
}

// THE DEFAULTED MAJORITY IS THE WHOLE ARGUMENT FOR Flag/Subtree, so it is pinned
// rather than described. gates.go says the table states only what is UNUSUAL about
// a row — that claim is only true while the overwhelming majority of rows state
// nothing at all, and its previous telling ("eighteen of nineteen … and the two
// that differ") was stale in both numbers and self-contradictory besides: eighteen
// of nineteen leaves one, not two.
//
// Nothing compared it, so it rotted silently. Bumping these is expected as gates
// are added; updating gates.go's prose in the same commit is the point.
func TestTheDefaultedMajorityIsStillTheMajority(t *testing.T) {
	var defaulted, custom []string
	for _, g := range Gates() {
		if g.Flag == "" && g.Subtree == "" {
			defaulted = append(defaulted, g.Extension)
			continue
		}
		custom = append(custom, g.Extension)
	}
	const wantDefaulted, wantCustom = 27, 3
	if len(defaulted) != wantDefaulted || len(custom) != wantCustom {
		t.Errorf("%d gates take the default subject and %d differ; gates.go's header says %d and %d.\n"+
			"\tThe rows that differ are %v. Update that comment in this commit — a count nothing "+
			"compares is a footnote, not a measurement.",
			len(defaulted), len(custom), wantDefaulted, wantCustom, custom)
	}
	// The claim is comparative, not just arithmetic: "only what is unusual" stops
	// being true long before the counts are equal.
	if len(custom)*4 > len(defaulted) {
		t.Errorf("%d of %d gates now carry a custom subject — the table no longer states only "+
			"what is unusual, and gates.go's justification for Flag/Subtree needs rewriting "+
			"rather than renumbering", len(custom), len(Gates()))
	}
}

// ── --only ───────────────────────────────────────────────────────────────────

// THE MAKEFILE USED TO HOLD THE SECOND COPY. Thirteen single-guard targets each
// restated the `llz ci <verb> --root ..` the gate table already holds, so a flag
// change had two places to land and one of them was found by hand. `--only` is how
// those targets keep their affordance — run ONE guard while iterating on it —
// without keeping their own spelling of it.
//
// So the property is: selecting one gate runs that gate and no others, whether the
// caller names the extension or the command.
func TestOnlyNarrowsTheSuiteByEitherName(t *testing.T) {
	orig := gates
	t.Cleanup(func() { gates = orig })
	gates = []Gate{
		{Extension: "guard-docs", New: namedCmd("docs-guard")},
		{Extension: "wave-health", New: namedCmd("wave-health-guard")},
		{Extension: "wave-health", New: namedCmd("wave-dependency-guard")},
	}

	for _, tc := range []struct {
		only string
		want int
	}{
		{"guard-docs", 1},        // by extension, one command
		{"wave-health", 2},       // by extension, both its commands
		{"wave-health-guard", 1}, // by command, just that one
		{"", 3},                  // unfiltered
	} {
		var out, errOut bytes.Buffer
		if err := RunGates(nil, Run{Root: ".", Only: tc.only}, &out, &errOut); err != nil {
			t.Fatalf("--only %q: %v", tc.only, err)
		}
		if want := fmt.Sprintf("%d ran", tc.want); !strings.Contains(out.String(), want) {
			t.Errorf("--only %q ran %q, want %q — the filter selected the wrong set, and a "+
				"Makefile target routed through it would silently check something else",
				tc.only, strings.TrimSpace(out.String()), want)
		}
	}
}

// A TYPO MUST NOT REPORT A CLEAN RUN. `--only wave-helth` selecting nothing and
// printing `0 ran, all clean` is the vacuous-green shape this driver refuses
// everywhere else, reachable from a Makefile target with a stale name — which is
// exactly the drift routing them through here is meant to end.
func TestOnlyMatchingNothingIsAnError(t *testing.T) {
	var out, errOut bytes.Buffer
	err := RunGates(nil, Run{Root: ".", Only: "wave-helth"}, &out, &errOut)
	if err == nil {
		t.Fatal("--only with a name that matches no gate returned nil — a mistyped target " +
			"would report a clean run having checked nothing")
	}
	if !strings.Contains(err.Error(), "wave-helth") {
		t.Errorf("error %q does not name what failed to match", err)
	}
	if strings.Contains(out.String(), "all clean") {
		t.Errorf("output %q announced a clean run for a selection that matched nothing", out.String())
	}
}

// namedCmd is a synthetic gate with a chosen command name, for the selector tests.
func namedCmd(name string) func() *cobra.Command {
	return func() *cobra.Command {
		c := &cobra.Command{Use: name, RunE: func(*cobra.Command, []string) error { return nil }}
		c.Flags().String("root", "", "repository root")
		return c
	}
}

// EVERY ENTRY POINT THAT CLAIMS TO RUN EVERYTHING MUST REACH THIS SUITE.
//
// ────────────────────────────────────────────────────────────────────────────
// `make lint LINT_ALL=1` DID NOT, AND IT IS THE MODE THAT ADVERTISES ITSELF AS
// EXHAUSTIVE — "runs the full local mirror", "every check unconditionally", "run
// all checks", in three separate places in the Makefile.
//
// The recipe has two branches. LINT_ALL runs a fixed target list and then
// `exit 0`; the changed-file path falls through to `llz-gates` at the bottom. The
// gate call existed only at the bottom, so the narrower mode ran the whole suite
// and the exhaustive one ran none of it. Roughly half the gates have no equivalent
// in the LINT_ALL list — posture-plaintext, mesh-egress, mtls-wiring,
// guard-source-refs, guard-cosign-subject, guard-monitoring-labels,
// guard-manifests, wave-health, pin-coherence — so a contributor running the
// "everything" target before pushing got a clean pass over none of them.
//
// The comment above that recipe said "`llz-gates` now runs unconditionally". It
// was true of one branch. This test is what makes the word mean both.
//
// IT READS THE MAKEFILE AS TEXT, which is coarse, and that is the right trade: the
// alternative is running `make lint LINT_ALL=1` in a unit test, which takes minutes
// and needs tflint, checkov, kube-linter and a rendered chart tree. What can go
// wrong here is a rename, and a rename breaks this loudly rather than quietly.
// ────────────────────────────────────────────────────────────────────────────
func TestEveryLintBranchReachesTheGateSuite(t *testing.T) {
	const makefile = "../../../../../Makefile"
	b, err := os.ReadFile(filepath.FromSlash(makefile))
	if err != nil {
		t.Fatalf("reading %s: %v — this test's whole subject is that file", makefile, err)
	}
	src := string(b)

	// The recipe, from `lint:` at column 0 to the next column-0 line that is not
	// part of it. Recipe lines are tab-indented or continuations.
	i := strings.Index(src, "\nlint:\n")
	if i < 0 {
		t.Fatal("no `lint:` target in the Makefile — it was renamed, and this guard cannot " +
			"tell that from a Makefile that stopped linting")
	}
	recipe := src[i+len("\nlint:\n"):]
	if j := strings.Index(recipe, "\n\n"); j >= 0 {
		recipe = recipe[:j]
	}
	if !strings.Contains(recipe, "LINT_ALL") {
		t.Fatal("the lint recipe no longer mentions LINT_ALL — the two-branch shape this " +
			"checks is gone, so re-derive what the entry points are before deleting this")
	}

	// The LINT_ALL branch is everything up to its `exit 0`; the changed-file path is
	// what follows. Both must invoke the suite.
	k := strings.Index(recipe, "exit 0")
	if k < 0 {
		t.Fatal("the LINT_ALL branch no longer exits early — re-read the recipe; this test " +
			"assumes the two branches are separated by that exit")
	}
	lintAll, changed := recipe[:k], recipe[k:]

	if !strings.Contains(lintAll, "llz-gates") {
		t.Error("`make lint LINT_ALL=1` does not run `llz-gates`. That branch is documented as " +
			"running every check, and roughly half the gate suite is reachable no other way — a " +
			"contributor running it before pushing would get a clean pass over gates that never " +
			"ran, then meet them in CI. Add `$(MAKE) --no-print-directory llz-gates;` before its " +
			"`exit 0`.")
	}
	if !strings.Contains(changed, "llz-gates") {
		t.Error("the changed-file lint path does not run `llz-gates`. It runs unconditionally " +
			"by design: the whole suite is ~4s, so per-gate trigger filters were deleted rather " +
			"than maintained, and dropping the call reinstates the problem they had.")
	}
}

// EVERY CHANGE CLASS THE LOCAL MIRROR KNOWS MUST BE ABLE TO TRIGGER CI.
//
// ────────────────────────────────────────────────────────────────────────────
// `platform-apl/` WAS A CHANGE CLASS IN THE MAKEFILE AND NOT A PATH IN lint.yml.
//
// The Makefile's changed-file lint keys on `^platform-apl/` and routes it to
// wave-health-guard — it has known that tree is a change class all along. The
// workflow's `paths:` filters listed neither it nor instance-template/apl-values,
// so a PR touching only those started NO run of lint.yml, and with it none of the
// 24 gates. wave-health's entire subject is platform-apl/manifest and
// platform-apl/components; seven other guards read the tree; prom-rules-check
// lints platform-apl/components/observability/prometheus-rules by path.
//
// That is precisely the defect lint.yml's own `**.md` entry documents at length —
// "the gate existed, was tested, was wired into `make lint`, and nothing in CI
// could invoke it" — recurring one tree over, with the diagnosis written directly
// above the line that was missing.
//
// SO THE COUPLING IS ASSERTED RATHER THAN REMEMBERED. The Makefile is the local
// mirror of CI; where it recognises a top-level tree as worth linting, the workflow
// must be able to see a change to it. Both directions are NOT checked: a workflow
// may legitimately watch more than the Makefile branches on (`.tflintrc.hcl`,
// budget files), and demanding symmetry there would be inventing a rule.
// ────────────────────────────────────────────────────────────────────────────
func TestEveryLocalLintTreeCanTriggerCI(t *testing.T) {
	root := filepath.FromSlash("../../../../../")
	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("reading Makefile: %v", err)
	}
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "lint.yml"))
	if err != nil {
		t.Fatalf("reading lint.yml: %v — it is the CI half of this coupling", err)
	}

	recipe := string(mk)
	i := strings.Index(recipe, "\nlint:\n")
	if i < 0 {
		t.Fatal("no `lint:` target — see TestEveryLintBranchReachesTheGateSuite")
	}
	recipe = recipe[i:]
	if j := strings.Index(recipe, "\n\n"); j >= 0 {
		recipe = recipe[:j]
	}

	// Top-level trees named in the recipe's changed-file greps.
	trees := map[string]bool{}
	for _, pat := range regexp.MustCompile(`grep -qE '([^']+)'`).FindAllStringSubmatch(recipe, -1) {
		for _, m := range regexp.MustCompile(`\^([a-z][a-z0-9._-]*)/`).FindAllStringSubmatch(pat[1], -1) {
			trees[m[1]] = true
		}
	}
	if len(trees) == 0 {
		t.Fatal("extracted no change-class trees from the lint recipe — the greps were " +
			"restructured, and this guard would pass over anything")
	}

	var watched []string
	for _, m := range regexp.MustCompile(`(?m)^\s+- '([^']+)'`).FindAllStringSubmatch(string(wf), -1) {
		watched = append(watched, m[1])
	}
	if len(watched) == 0 {
		t.Fatal("lint.yml declares no path filters — either it now runs on everything " +
			"(delete this guard and say so) or the parse broke")
	}

	var missing []string
	for tree := range trees {
		var seen bool
		for _, p := range watched {
			if p == tree+"/**" || strings.HasPrefix(p, tree+"/") {
				seen = true
				break
			}
		}
		if !seen {
			missing = append(missing, tree)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the Makefile lints %v on change, and no lint.yml path filter matches "+
			"them — a PR touching only such a tree starts no run of that workflow, so every "+
			"gate reading it is unreachable from CI while passing locally. Add '<tree>/**' to "+
			"BOTH the pull_request and push filters.", missing)
	}
}

// coveredElsewhere are the LINT_ALL targets no CI job reaches by `make <target>`,
// each with the mechanism that does cover it. It is a ratchet, in both directions.
//
// Every entry is a target whose CI coverage is real but indirect, so a reader
// cannot confirm it by grepping for `make <name>` and neither can the check below.
var coveredElsewhere = map[string]string{
	"chart-version-guard": "its own workflow runs `llz ci chart-version-guard` directly (chart-version-guard.yml)",
	"instance-test":       "lint.yml's instantiate job runs template-scripts/ci/instance-test.sh",
	"version-pins-check":  "the `version-pins` gate is driven by `llz ci gates` (registry/gates.go)",
	"k8s-minor-coherence": "the `k8s-minor-coherence` gate is driven by `llz ci gates` (registry/gates.go)",
	"vet":                 "lint.yml's go-tests job runs `gofmt + go vet` as its own step",
}

// EVERY CHECK THE LOCAL "RUN EVERYTHING" MODE RUNS MUST BE REACHABLE FROM CI.
//
// ────────────────────────────────────────────────────────────────────────────
// THIS CLASS HAS NOW BEEN FOUND FOUR TIMES, ONE INSTANCE PER PASS, and each time
// by someone reading rather than by anything failing:
//
//	`make lint LINT_ALL=1`   skipped the whole gate suite — the mode documented
//	                         in three places as exhaustive ran none of the 24
//	lint.yml `paths:`        had no platform-apl/** trigger, so a PR touching
//	                         only that tree started no run at all
//	`actions-lint`           lints this repo's own workflows and no CI job called
//	                         it; the only actionlint in CI was over the RENDERED
//	                         instance's workflows, a different tree
//	`tf-fmt-check`           LINT_TF is every terraform check except formatting,
//	                         so `tofu fmt -check` ran nowhere in CI
//
// The shared shape is that a check is REAL, TESTED and WIRED INTO `make lint`,
// and no CI entry point names it — so it passes locally for whoever runs the full
// target and is absent from the only run that gates a merge. The pre-commit hook
// hides it further: it lives in .git/hooks, per-clone and uncommitted, so the
// author who added the check keeps seeing it pass.
//
// So the coupling is asserted. `make lint LINT_ALL=1` is the local mirror of CI by
// its own documentation; a target in it that no CI job can reach is either a gap
// or an entry in coveredElsewhere saying which mechanism covers it.
// ────────────────────────────────────────────────────────────────────────────
func TestEveryLocalLintTargetIsReachableFromCI(t *testing.T) {
	root := filepath.FromSlash("../../../../../")
	mkRaw, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("reading Makefile: %v", err)
	}
	mk := string(mkRaw)
	flat := regexp.MustCompile(`\\\n\s*`).ReplaceAllString(mk, " ")

	varOf := func(name string) []string {
		m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` :?= (.*)$`).FindStringSubmatch(flat)
		if m == nil {
			return nil
		}
		return strings.Fields(m[1])
	}
	expand := func(toks []string) []string {
		var out []string
		for _, tok := range toks {
			if strings.HasPrefix(tok, "$(") && strings.HasSuffix(tok, ")") {
				out = append(out, varOf(tok[2:len(tok)-1])...)
				continue
			}
			out = append(out, tok)
		}
		return out
	}

	// The LINT_ALL branch's target list.
	i := strings.Index(mk, "\nlint:\n")
	if i < 0 {
		t.Fatal("no `lint:` target — see TestEveryLintBranchReachesTheGateSuite")
	}
	rec := mk[i:]
	if j := strings.Index(rec, "\n\n"); j >= 0 {
		rec = rec[:j]
	}
	k := strings.Index(rec, "exit 0")
	if k < 0 {
		t.Fatal("the LINT_ALL branch no longer exits early — re-read the recipe")
	}
	m := regexp.MustCompile(`--no-print-directory ([^;]+);`).FindStringSubmatch(rec[:k])
	if m == nil {
		t.Fatal("could not find the LINT_ALL target list — the recipe was restructured, and " +
			"this guard would pass over an empty set")
	}
	lintAll := expand(strings.Fields(m[1]))
	if len(lintAll) < 5 {
		t.Fatalf("parsed only %d LINT_ALL targets (%v) — too few to be the full-lint list, so "+
			"the parse broke rather than the Makefile shrinking", len(lintAll), lintAll)
	}

	// Everything CI reaches with `make <target>`, plus those targets' prerequisites.
	reach := map[string]bool{}
	wfDir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(wfDir)
	if err != nil {
		t.Fatalf("reading %s: %v", wfDir, err)
	}
	makeCall := regexp.MustCompile(`make (?:--?\S+ )*([a-z0-9-]+)`)
	var direct []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(wfDir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		for _, mm := range makeCall.FindAllStringSubmatch(string(b), -1) {
			direct = append(direct, mm[1])
		}
	}
	if len(direct) == 0 {
		t.Fatal("no `make <target>` call found in any workflow — the parse broke; every target " +
			"would read as unreachable")
	}
	for _, tgt := range direct {
		reach[tgt] = true
		pm := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(tgt) + `: (.*)$`).FindStringSubmatch(flat)
		if pm == nil {
			continue
		}
		for _, p := range expand(strings.Fields(pm[1])) {
			reach[p] = true
		}
	}

	var gaps []string
	for _, tgt := range lintAll {
		if reach[tgt] || coveredElsewhere[tgt] != "" {
			continue
		}
		gaps = append(gaps, tgt)
	}
	sort.Strings(gaps)
	if len(gaps) > 0 {
		t.Errorf("`make lint LINT_ALL=1` runs %v, and no CI job reaches them by `make <target>` "+
			"nor does coveredElsewhere name a mechanism. Either add the target to a CI entry "+
			"point (lint-k8s / lint-tf) or record how it IS covered — a check that runs only "+
			"in the full local target passes for its author and gates nothing.", gaps)
	}

	// And the ratchet's other direction: an entry that has since become directly
	// reachable is a stale exemption, which reads as remaining work already done.
	var stale []string
	for tgt := range coveredElsewhere {
		if reach[tgt] {
			stale = append(stale, tgt)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("coveredElsewhere still exempts %v, which CI now reaches by `make <target>` "+
			"directly — delete the entries; a stale exemption hides the next real gap.", stale)
	}
}

// A CI JOB THAT DOES NOT REBUILD llz MUST RUN ONLY FORCED-SOURCE GATES.
//
// ────────────────────────────────────────────────────────────────────────────
// THE MACRO PREFERS A BINARY THAT IS NOT THE ONE UNDER REVIEW.
//
// LLZ_CI takes `llz` from PATH whenever the working tree is clean and one is
// installed, and only builds from source when LLZ_FORCE_SOURCE is set. That is
// right for a developer — it is fast, and the banner says which binary answered.
// In CI it is a trap: the ci-tofu and ci-kubernetes images BAKE an llz at
// image-build time, so a clean checkout plus a baked binary means the PATH branch
// runs the MERGE-BASE llz against the PR's tree.
//
// Both container jobs run .github/actions/setup-llz. The kubernetes job passes
// install-path: /usr/local/bin/llz, which overwrites the baked binary with one
// built from the PR — so its gates are safe whichever branch they take. The
// terraform job passes NO install-path: it installs the Go toolchain only, and
// `llz` on PATH stays the image's. Everything it runs must therefore force source.
//
// `template-manifest-check` did not, sitting one paragraph above
// managed-lock-check's comment stating the rule verbatim: "LLZ_CI's PATH-first
// default would use the prebuilt image binary — which is built from the merge-base
// and therefore doesn't even have this verb on the PR that introduces it". On a PR
// changing the classification logic it validated the new scaffold with the old
// rules, and said nothing.
//
// This asserts the rule instead of relying on the next author reading the
// neighbouring comment.
// ────────────────────────────────────────────────────────────────────────────
func TestContainerJobsRunThePRsLlz(t *testing.T) {
	root := filepath.FromSlash("../../../../../")
	mkRaw, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("reading Makefile: %v", err)
	}
	wfRaw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "lint.yml"))
	if err != nil {
		t.Fatalf("reading lint.yml: %v", err)
	}
	mk, wf := string(mkRaw), string(wfRaw)
	flat := regexp.MustCompile(`\\\n\s*`).ReplaceAllString(mk, " ")

	forced := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^([a-z0-9-]+): export LLZ_FORCE_SOURCE`).FindAllStringSubmatch(mk, -1) {
		forced[m[1]] = true
	}
	if len(forced) == 0 {
		t.Fatal("no target exports LLZ_FORCE_SOURCE — the convention this checks is gone, or " +
			"the parse is wrong; either way this guard would pass over everything")
	}

	// Targets whose recipe calls the LLZ_CI macro.
	usesMacro := map[string]bool{}
	var cur string
	for _, line := range strings.Split(mk, "\n") {
		if m := regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9._-]*):(?:[^=]|$)`).FindStringSubmatch(line); m != nil {
			cur = m[1]
		}
		if strings.Contains(line, "$(call LLZ_CI") && cur != "" {
			usesMacro[cur] = true
		}
	}
	if len(usesMacro) == 0 {
		t.Fatal("found no `$(call LLZ_CI` targets — the macro was renamed and this guard is vacuous")
	}

	varOf := func(n string) []string {
		m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(n) + ` :?= (.*)$`).FindStringSubmatch(flat)
		if m == nil {
			return nil
		}
		return strings.Fields(m[1])
	}
	var closure func(string, map[string]bool)
	closure = func(t string, seen map[string]bool) {
		if seen[t] {
			return
		}
		seen[t] = true
		m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(t) + `: (.*)$`).FindStringSubmatch(flat)
		if m == nil {
			return
		}
		for _, tok := range strings.Fields(m[1]) {
			if strings.HasPrefix(tok, "$(") && strings.HasSuffix(tok, ")") {
				for _, v := range varOf(tok[2 : len(tok)-1]) {
					closure(v, seen)
				}
				continue
			}
			closure(tok, seen)
		}
	}

	// Each job block, and whether it rebuilds llz onto PATH. Split line-wise: Go's
	// RE2 has no lookahead, and a job header is simply a 2-space-indented key.
	jobHeader := regexp.MustCompile(`^  ([a-z0-9][a-z0-9_-]*):\s*$`)
	var blocks []string
	var curBlk []string
	for _, line := range strings.Split(wf, "\n") {
		if jobHeader.MatchString(line) {
			if len(curBlk) > 0 {
				blocks = append(blocks, strings.Join(curBlk, "\n"))
			}
			curBlk = []string{line}
			continue
		}
		if len(curBlk) > 0 {
			curBlk = append(curBlk, line)
		}
	}
	if len(curBlk) > 0 {
		blocks = append(blocks, strings.Join(curBlk, "\n"))
	}

	var examined int
	for _, b := range blocks {
		name := jobHeader.FindStringSubmatch(strings.SplitN(b, "\n", 2)[0])
		if name == nil || !strings.Contains(b, "runs-on") {
			continue
		}
		// ONLY CONTAINER JOBS. The hazard is a BAKED llz on PATH, and only the
		// ci-tofu / ci-kubernetes images carry one. A plain ubuntu-latest runner has
		// no llz at all, so LLZ_CI's `command -v llz` fails there and the macro
		// builds from source whatever the target declares — flagging those would be
		// crying wolf, and a guard people learn to ignore stops being one. (The
		// first cut did exactly that, on go-tests' two budget targets.)
		if !strings.Contains(b, "container:") {
			continue
		}
		// COMMENTS STRIPPED BEFORE MATCHING, and that is not tidiness. The first cut
		// tested the raw block for "install-path" — and the comment added to the
		// terraform job EXPLAINING that it passes no install-path contains the
		// phrase, so the job read as one that rebuilds llz and was skipped. A
		// matcher that answers to prose about the code instead of the code is the
		// `.Cloud` vs `.Cloud.` defect in unbacked_test.go, one file over.
		code := b
		if lines := strings.Split(b, "\n"); true {
			kept := lines[:0]
			for _, l := range lines {
				if !strings.HasPrefix(strings.TrimSpace(l), "#") {
					kept = append(kept, l)
				}
			}
			code = strings.Join(kept, "\n")
		}
		if !strings.Contains(code, "setup-llz") || strings.Contains(code, "install-path:") {
			continue // no llz, or it rebuilt one from this PR
		}
		b = code
		for _, m := range regexp.MustCompile(`run: make ([a-z0-9-]+)`).FindAllStringSubmatch(b, -1) {
			examined++
			seen := map[string]bool{}
			closure(m[1], seen)
			var unforced []string
			for tgt := range seen {
				if usesMacro[tgt] && !forced[tgt] {
					unforced = append(unforced, tgt)
				}
			}
			sort.Strings(unforced)
			if len(unforced) > 0 {
				t.Errorf("lint.yml job %q runs `make %s` and does NOT rebuild llz (no install-path "+
					"on setup-llz), so `llz` on PATH is the one baked into the container image at "+
					"image-build time. These targets reach the LLZ_CI macro without exporting "+
					"LLZ_FORCE_SOURCE, so they validate this PR's tree with the MERGE-BASE binary: "+
					"%v.\n\tEither export LLZ_FORCE_SOURCE on each, or give the job's setup-llz an "+
					"install-path so the binary is this PR's.", name[1], m[1], unforced)
			}
		}
	}
	if examined == 0 {
		t.Fatal("no lint.yml job matched `setup-llz without install-path` + `run: make …` — the " +
			"workflow was restructured and this guard now checks nothing")
	}
	t.Logf("checked %d `make` entry point(s) in jobs that do not rebuild llz", examined)
}

// A TARGET `make help` ADVERTISES MUST ACTUALLY DO SOMETHING.
//
// ────────────────────────────────────────────────────────────────────────────
// FOUR OF THEM DID NOT, AND EXITED 0 SAYING SO QUIETLY.
//
// When thirteen single-guard targets collapsed into `llz-gates`, their recipes
// went and two things stayed: their names on the `.PHONY` line, and their
// descriptions in `make help`. A phony target with no recipe and no prerequisites
// is not an error in GNU make — it is "Nothing to be done", exit 0.
//
//	$ make monitoring-label-guard
//	make: Nothing to be done for `monitoring-label-guard'.  # exit 0
//
// So a contributor who read help, ran the guard by name and saw a clean exit had
// run nothing at all. That is worse than a missing target, which exits 2 and says
// so. It is the vacuous-green shape this tree refuses everywhere — reached, here,
// by typing the name the Makefile told you to type.
//
// The four were wave-dependency-guard, monitoring-label-guard,
// dropped-apiversions-check and placeholder-guard. All four still RUN, inside
// `llz ci gates`; only the Makefile spelling died.
//
// TWO INVARIANTS, ONE CHECK. Every name in help must resolve to a target with a
// recipe or prerequisites, and no `.PHONY` name may be recipe-less — the second
// catches a target that was never advertised, which is how placeholder-guard hid
// (it was on the .PHONY line and in no help line at all).
// ────────────────────────────────────────────────────────────────────────────
func TestEveryAdvertisedMakeTargetDoesSomething(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(filepath.FromSlash("../../../../../"), "Makefile"))
	if err != nil {
		t.Fatalf("reading Makefile: %v", err)
	}
	mk := string(raw)
	lines := strings.Split(mk, "\n")

	// Targets that would actually run: a rule with prerequisites or a recipe.
	runnable := map[string]bool{}
	ruleRE := regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9._-]*):([^=].*)?$`)
	for i, l := range lines {
		m := ruleRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		hasBody := i+1 < len(lines) && strings.HasPrefix(lines[i+1], "\t")
		if strings.TrimSpace(m[2]) != "" || hasBody {
			runnable[m[1]] = true
		}
	}
	if len(runnable) == 0 {
		t.Fatal("parsed no runnable targets from the Makefile — the parse broke and every name " +
			"below would read as dead")
	}

	flat := regexp.MustCompile(`\\\n\s*`).ReplaceAllString(mk, " ")
	phony := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\.PHONY:(.*)$`).FindAllStringSubmatch(flat, -1) {
		for _, n := range strings.Fields(m[1]) {
			phony[n] = true
		}
	}
	if len(phony) == 0 {
		t.Fatal("no .PHONY names parsed — the declaration moved and half this check is vacuous")
	}

	var deadPhony []string
	for n := range phony {
		if !runnable[n] {
			deadPhony = append(deadPhony, n)
		}
	}
	sort.Strings(deadPhony)
	if len(deadPhony) > 0 {
		t.Errorf(".PHONY declares %v with no recipe and no prerequisites. GNU make answers "+
			"`Nothing to be done` and EXITS 0, so anyone running one of these by name gets a "+
			"clean result having run nothing. Delete the name, or give it a rule.", deadPhony)
	}

	// The help recipe's advertised names.
	i := strings.Index(mk, "\nhelp:")
	if i < 0 {
		t.Fatal("no `help:` target — it was renamed, and this check cannot tell that from a " +
			"Makefile that stopped advertising anything")
	}
	helpRec := mk[i:]
	if j := strings.Index(helpRec, "\n\n"); j >= 0 {
		helpRec = helpRec[:j]
	}
	named := map[string]bool{}
	for _, m := range regexp.MustCompile(`@echo\s+"\s{2,}([a-z][a-z0-9._-]*)\s{2,}`).FindAllStringSubmatch(helpRec, -1) {
		named[m[1]] = true
	}
	if len(named) == 0 {
		t.Fatal("parsed no target names out of the help recipe — its echo shape changed and this " +
			"half of the check now reads nothing")
	}

	var advertisedDead []string
	for n := range named {
		if !runnable[n] {
			advertisedDead = append(advertisedDead, n)
		}
	}
	sort.Strings(advertisedDead)
	if len(advertisedDead) > 0 {
		t.Errorf("`make help` advertises %v, which no rule defines. A contributor who types one "+
			"gets either `Nothing to be done` (exit 0, having run nothing) or `No rule to make "+
			"target`. Remove the line, or point it at what actually runs the check now.",
			advertisedDead)
	}
	t.Logf("help advertises %d target(s); .PHONY names %d; %d runnable", len(named), len(phony), len(runnable))
}

// notInTheLocalMirror is every target a CI job runs that `make lint LINT_ALL=1`
// deliberately does not, and why. Ratcheted in both directions.
var notInTheLocalMirror = map[string]string{
	"coverage":  "runs the whole test suite with a coverage profile — `make coverage` is its own command, and folding it into a lint target would make the lint target minutes long",
	"test-race": "the -race suite, same reason; it is a TEST gate rather than a linter",
	"lint-k8s":  "the CI job entry point, whose contents LINT_ALL already runs individually",
	"lint-tf":   "the CI job entry point, whose contents LINT_ALL already runs individually",
	// COVERED BY ANOTHER ROUTE rather than skipped: the CHECK runs in LINT_ALL via
	// `llz-gates` (guard-manifests drives ArgoCDRenderedAppsCmd), so the standalone
	// target would be a second execution of the same guard. lint.yml's dry-run job
	// calls it directly because that job already has the rendered tree in hand.
	"argocd-rendered-apps-check": "its check runs in LINT_ALL through llz-gates as guard-manifests; the standalone target is CI's dry-run job and local iteration",
}

// The first cut of this map also listed `llz` and `build` as exemptions. lint.yml
// runs neither, and the ratchet's reverse direction said so on its first run —
// which is the argument for having that direction at all: an exemption nobody
// needs reads as a decision that was made.

// THE OTHER DIRECTION: A CHECK CI RUNS THAT NOTHING LOCAL DOES.
//
// ────────────────────────────────────────────────────────────────────────────
// `staticcheck` WAS RED, AND HAD BEEN FOR THE WHOLE OF AN AUDIT CAMPAIGN.
//
// TestEveryLocalLintTargetIsReachableFromCI asserts that everything the local
// full-lint runs is reachable from CI. The reverse was unguarded, and that is the
// direction that rots quietly: a check CI runs and no local target does is one an
// author cannot see fail until they push.
//
// staticcheck ran ONLY in lint.yml's go-tests job. It was not in LINT_ALL, and
// the pre-commit hook runs gofmt/actionlint/gitleaks rather than `make lint`. So
// two ST1005 findings sat in internal/shared/capability for the whole campaign —
// the working tree was red against a CI gate through twenty-two commits, and
// every local check said green. It is now in the LINT_ALL list.
//
// The exemptions are the genuinely-not-lint targets. `coverage` and `test-race`
// run the suite and belong to `make coverage` / `make test-race`; folding them in
// would make the lint target minutes long and is a real reason rather than an
// excuse. They are listed so the distinction is a decision on the record.
// ────────────────────────────────────────────────────────────────────────────
func TestEveryCITargetIsInTheLocalMirror(t *testing.T) {
	root := filepath.FromSlash("../../../../../")
	mkRaw, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("reading Makefile: %v", err)
	}
	mk := string(mkRaw)
	flat := regexp.MustCompile(`\\\n\s*`).ReplaceAllString(mk, " ")

	varOf := func(n string) []string {
		m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(n) + ` :?= (.*)$`).FindStringSubmatch(flat)
		if m == nil {
			return nil
		}
		return strings.Fields(m[1])
	}
	expand := func(toks []string) []string {
		var out []string
		for _, tok := range toks {
			if strings.HasPrefix(tok, "$(") && strings.HasSuffix(tok, ")") {
				out = append(out, varOf(tok[2:len(tok)-1])...)
				continue
			}
			out = append(out, tok)
		}
		return out
	}

	i := strings.Index(mk, "\nlint:\n")
	if i < 0 {
		t.Fatal("no `lint:` target — see TestEveryLintBranchReachesTheGateSuite")
	}
	rec := mk[i:]
	if j := strings.Index(rec, "\n\n"); j >= 0 {
		rec = rec[:j]
	}
	k := strings.Index(rec, "exit 0")
	if k < 0 {
		t.Fatal("the LINT_ALL branch no longer exits early")
	}
	local := map[string]bool{}
	for _, m := range regexp.MustCompile(`--no-print-directory ([^;]+);`).FindAllStringSubmatch(rec, -1) {
		for _, tgt := range expand(strings.Fields(m[1])) {
			local[tgt] = true
			// A CI entry point is satisfied by its contents running locally.
			for _, p := range expand(strings.Fields(func() string {
				pm := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(tgt) + `: (.*)$`).FindStringSubmatch(flat)
				if pm == nil {
					return ""
				}
				return pm[1]
			}())) {
				local[p] = true
			}
		}
	}
	if len(local) < 10 {
		t.Fatalf("parsed only %d targets from the lint recipe (%v) — the parse broke", len(local), local)
	}

	// Everything a lint.yml job invokes as `make <target>`.
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "lint.yml"))
	if err != nil {
		t.Fatalf("reading lint.yml: %v", err)
	}
	ci := map[string]bool{}
	for _, m := range regexp.MustCompile(`run: make ([a-z0-9-]+)`).FindAllStringSubmatch(string(wf), -1) {
		ci[m[1]] = true
	}
	if len(ci) == 0 {
		t.Fatal("no `run: make <target>` in lint.yml — the parse broke and nothing would be checked")
	}

	var missing, stale []string
	for tgt := range ci {
		if !local[tgt] && notInTheLocalMirror[tgt] == "" {
			missing = append(missing, tgt)
		}
	}
	for tgt, why := range notInTheLocalMirror {
		if !ci[tgt] {
			stale = append(stale, tgt)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("notInTheLocalMirror[%q] has no reason", tgt)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	if len(missing) > 0 {
		t.Errorf("lint.yml runs %v and `make lint LINT_ALL=1` does not — an author cannot see "+
			"these fail before pushing, which is how staticcheck stayed red through an entire "+
			"campaign while every local check said green. Add it to the LINT_ALL list, or record "+
			"it in notInTheLocalMirror with the reason it is not a linter.", missing)
	}
	if len(stale) > 0 {
		t.Errorf("notInTheLocalMirror names %v, which lint.yml no longer runs — delete the "+
			"entries; a stale exemption hides the next gap.", stale)
	}
}
