package clusterspec

// component_emits_test.go — the emit predicate, including the arm that is not a
// conjunction.

import "testing"

// THE PRODUCTION PATH, AGAINST A REAL SPEC ON DISK. Everything else here calls
// ComponentEmits directly; nothing exercised DisabledComponents, which is what
// the three preflight call sites actually use — it loads a spec through
// Detected() and swallows every failure by design.
//
// THAT SWALLOW IS THE RIGHT DEFAULT AND THE WHOLE RISK. A bad apiVersion, a
// schema bump, an instance-layout move: the load errors, the off-set comes back
// empty, every conditional check runs, and nothing anywhere says the spec went
// unread. The conditionality would be inert with the suite still green — the
// quiet-pass class this branch exists to abolish, reproduced inside the fix for
// it. This is the gate that makes "the loader stopped working" and "nothing is
// disabled" stop looking alike: the fixture DISABLES harbor in staging, so a
// spec that failed to load cannot produce the expected answer.
func TestDisabledComponentsReadsARealSpec(t *testing.T) {
	t.Chdir(writeInstance(t, splitFiles()))

	// splitStaging sets `components: { harbor: { enabled: false } }`.
	staging := DisabledComponents("staging")
	if !staging["harbor"] {
		t.Fatal("staging disables harbor in the spec — an empty or wrong off-set here means the spec was never read, " +
			"and every conditional capability check is silently unconditional")
	}

	// prod does not, so the same loader must report it ON — otherwise the gate
	// could pass by disabling everything.
	if DisabledComponents("prod")["harbor"] {
		t.Error("prod does not disable harbor — reporting it off would switch the check off where it IS needed")
	}

	// An env the spec does not carry yields no off-set, which means every check
	// still runs. Conservative, and asserted so it cannot quietly invert.
	if len(DisabledComponents("no-such-env")) != 0 {
		t.Error("an unknown env must yield an empty off-set (every check still applies)")
	}
}

// NO SPEC AT ALL → EMPTY OFF-SET, so a preflight in a bare tree still asks every
// question rather than silently skipping them.
func TestDisabledComponentsWithoutASpec(t *testing.T) {
	t.Chdir(t.TempDir())
	if n := len(DisabledComponents("prod")); n != 0 {
		t.Errorf("off-set = %d entries with no spec, want 0", n)
	}
}

// A MANDATORY COMPONENT IS PRESENT WHATEVER THE TOGGLES SAY. clusterFoundation
// is Mandatory AND ManagedSkip: the conjunction alone calls it "not emitted" on
// every managed cluster, so a preflight keyed on it would stop asking about a
// component the cluster does not converge without. The renderer skips Mandatory
// entries for the opposite reason — they are emitted by another path — and that
// asymmetry is exactly what this arm exists to keep out of the answer.
func TestMandatoryComponentsAlwaysEmit(t *testing.T) {
	var checked int
	for _, c := range Components {
		if !c.Mandatory {
			continue
		}
		checked++
		off := map[string]ComponentToggle{c.Name: {Enabled: boolPtr(false)}}
		if !ComponentEmits(off, Bootstrap{}, c.Name) {
			t.Errorf("%s is Mandatory — it must emit even with enabled:false and an empty managedApps list", c.Name)
		}
	}
	if checked == 0 {
		t.Fatal("no Mandatory component found — this gate examined nothing")
	}
}

// AND THE CONJUNCTION STILL HOLDS FOR EVERYONE ELSE, so the Mandatory arm cannot
// be widened into "always true".
func TestNonMandatoryComponentsHonourBothHalves(t *testing.T) {
	const name = "harbor"
	c, ok := componentByName[name]
	if !ok || c.Mandatory {
		t.Fatalf("fixture: %s must exist and be non-Mandatory", name)
	}
	full := Bootstrap{ManagedApps: DefaultManagedApps}
	if !ComponentEmits(map[string]ComponentToggle{name: {Enabled: boolPtr(true)}}, full, name) {
		t.Error("enabled + declared must emit")
	}
	// Half one: the env toggle.
	if ComponentEmits(map[string]ComponentToggle{name: {Enabled: boolPtr(false)}}, full, name) {
		t.Error("enabled:false must not emit")
	}
	// Half two: the managed app list (ManagedConditionalOn).
	if ComponentEmits(map[string]ComponentToggle{name: {Enabled: boolPtr(true)}}, Bootstrap{ManagedApps: []string{"loki"}}, name) {
		t.Error("absent from managedApps must not emit")
	}
}

// An unknown name emits nothing — a caller asking about a component that does
// not exist must not be told its content is present.
func TestUnknownComponentDoesNotEmit(t *testing.T) {
	if ComponentEmits(nil, Bootstrap{ManagedApps: DefaultManagedApps}, "noSuchComponent") {
		t.Error("an unknown component must not report as emitted")
	}
}
