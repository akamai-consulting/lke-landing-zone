package registry

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
