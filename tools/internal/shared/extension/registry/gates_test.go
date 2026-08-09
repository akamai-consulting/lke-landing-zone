package registry

import (
	"bytes"
	"errors"
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
		{"a", failingCmd("one"), nil, nil},
		{"b", failingCmd("two"), nil, nil},
		{"c", okCmd(), nil, nil},
	}

	var out, errOut bytes.Buffer
	err := RunGates(nil, &out, &errOut, nil)
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
	gates = []Gate{{"a", okCmd(), nil, nil}, {"b", okCmd(), nil, nil}}

	var out, errOut bytes.Buffer
	if err := RunGates(nil, &out, &errOut, nil); err != nil {
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
func failingCmd(msg string) func() *cobra.Command {
	return func() *cobra.Command {
		return &cobra.Command{
			Use:  "gate-" + msg,
			RunE: func(*cobra.Command, []string) error { return errors.New(msg) },
		}
	}
}

func okCmd() func() *cobra.Command {
	return func() *cobra.Command {
		return &cobra.Command{Use: "ok", RunE: func(*cobra.Command, []string) error { return nil }}
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
	gates = []Gate{{"obj-encryption", okCmd(), nil, nil}, {"guard-docs", okCmd(), nil, nil}}

	off := false
	toggles := map[string]clusterspec.ComponentToggle{"objProxy": {Enabled: &off}}

	var out, errOut bytes.Buffer
	if err := RunGates(nil, &out, &errOut, toggles); err != nil {
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
	gates = []Gate{{"guard-docs", okCmd(), nil, nil}}

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
		Args:      nil,
		NewWithTree: func(tree *cobra.Command) *cobra.Command {
			got = tree
			return okCmd()()
		},
	}}

	var out, errOut bytes.Buffer
	if err := RunGates(want, &out, &errOut, nil); err != nil {
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
