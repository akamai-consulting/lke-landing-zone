package extension_test

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func bind(k extension.BindingKind, s extension.State, g ...extension.Grant) extension.Binding {
	return extension.Binding{Kind: k, State: s, Grants: g}
}

// ok is a minimal valid extension the negative cases mutate one field of, so each
// test names exactly one reason for failure.
func ok() extension.Extension {
	return extension.Extension{
		Name:     "guard-budgets",
		Short:    "budget gates over repo files",
		Always:   true,
		Bindings: []extension.Binding{bind(extension.Gate, extension.Configured, extension.ReadRepo)},
	}
}

func errText(errs []error) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.Error())
		b.WriteString("\n")
	}
	return b.String()
}

func TestValidBaseline(t *testing.T) {
	if errs := ok().Validate(); len(errs) != 0 {
		t.Fatalf("baseline must validate, got:\n%s", errText(errs))
	}
}

func TestNameRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"guard-budgets", true}, {"assert-observability", true}, {"tofu-driver", true}, {"a1", true},
		{"", false}, {"-leading", false}, {"trailing-", false}, {"double--hyphen", false},
		{"Upper", false}, {"has space", false}, {"under_score", false},
	} {
		e := ok()
		e.Name = tc.name
		errs := e.Validate()
		if got := len(errs) == 0; got != tc.ok {
			t.Errorf("name %q: valid=%v, want %v (%s)", tc.name, got, tc.ok, errText(errs))
		}
	}
}

func TestBindingKindStateCoherence(t *testing.T) {
	for _, tc := range []struct {
		desc  string
		bind  extension.Binding
		valid bool
	}{
		{"transition to provisioned", bind(extension.Transition, extension.Provisioned, extension.CloudMutate), true},
		{"transition to destroyed", bind(extension.Transition, extension.Destroyed, extension.CloudMutate), true},
		// verified is the CONCLUSION of assertions and operating is a condition —
		// neither is somewhere an action moves the platform to.
		{"transition to verified", bind(extension.Transition, extension.Verified), false},
		{"transition to operating", bind(extension.Transition, extension.Operating), false},
		{"assertion on verified", bind(extension.Assertion, extension.Verified, extension.ClusterRead), true},
		// The catalog's most valuable split: ci_health.go fuses the converge ACTION
		// with the health PREDICATE. Separating them needs health to be a
		// `converged` assertion, so this must be expressible.
		{"assertion on converged", bind(extension.Assertion, extension.Converged, extension.ClusterRead), true},
		{"assertion on configured", bind(extension.Assertion, extension.Configured, extension.ReadRepo), true},
		{"invariant on operating", bind(extension.Invariant, extension.Operating, extension.ClusterWrite), true},
		{"invariant on converged", bind(extension.Invariant, extension.Converged), false},
		{"gate on scaffolded", bind(extension.Gate, extension.Scaffolded, extension.ReadRepo), true},
		{"gate on provisioned", bind(extension.Gate, extension.Provisioned, extension.ReadRepo), false},
		{"unknown kind", bind("hook", extension.Configured), false},
		{"unknown state", bind(extension.Gate, "warmed", extension.ReadRepo), false},
	} {
		e := ok()
		e.Bindings = []extension.Binding{tc.bind}
		errs := e.Validate()
		if got := len(errs) == 0; got != tc.valid {
			t.Errorf("%s: valid=%v, want %v (%s)", tc.desc, got, tc.valid, errText(errs))
		}
	}
}

func TestNoBindingsIsInvalid(t *testing.T) {
	e := ok()
	e.Bindings = nil
	if errs := e.Validate(); len(errs) == 0 {
		t.Error("an extension that attaches nowhere must not validate")
	}
}

func TestDuplicateBindingAndGrant(t *testing.T) {
	e := ok()
	e.Bindings = []extension.Binding{
		bind(extension.Gate, extension.Configured, extension.ReadRepo, extension.ReadRepo),
		bind(extension.Gate, extension.Configured, extension.ReadRepo),
	}
	got := errText(e.Validate())
	if !strings.Contains(got, "duplicate binding") {
		t.Errorf("want a duplicate-binding error, got: %s", got)
	}
	if !strings.Contains(got, "duplicate grant") {
		t.Errorf("want a duplicate-grant error, got: %s", got)
	}
}

// ── the ceiling, per binding ─────────────────────────────────────────────────

func TestGateMayHoldOnlyReadRepo(t *testing.T) {
	for _, g := range extension.Grants() {
		e := ok()
		e.Bindings = []extension.Binding{bind(extension.Gate, extension.Configured, g)}
		errs := e.Validate()
		if got := len(errs) == 0; got != (g == extension.ReadRepo) {
			t.Errorf("gate with grant %q: valid=%v (%s)", g, got, errText(errs))
		}
	}
}

func TestAssertionIsReadOnly(t *testing.T) {
	for _, g := range extension.Grants() {
		e := ok()
		e.Bindings = []extension.Binding{bind(extension.Assertion, extension.Verified, g)}
		errs := e.Validate()
		wantOK := g == extension.ReadRepo || g == extension.CloudRead || g == extension.ClusterRead
		if got := len(errs) == 0; got != wantOK {
			t.Errorf("assertion with grant %q: valid=%v, want %v (%s)", g, got, wantOK, errText(errs))
		}
	}
}

// REGRESSION. The read-only rule was once written `Binds(Assertion) &&
// !Binds(Transition)`, so bolting on ANY transition — even a completely unrelated
// one — switched it off, and an assertion could then hold every dangerous grant
// with zero errors. Grants live on the binding now, so a sibling binding lends it
// nothing.
func TestUnrelatedTransitionDoesNotUnlockTheAssertionCeiling(t *testing.T) {
	e := extension.Extension{
		Name: "sneaky", Short: "assertion smuggling write grants",
		Bindings: []extension.Binding{
			bind(extension.Assertion, extension.Verified,
				extension.SecretCustody, extension.CloudMutate, extension.ClusterWrite),
			bind(extension.Transition, extension.Scaffolded, extension.ReadRepo), // unrelated
		},
	}
	errs := e.Validate()
	if len(errs) == 0 {
		t.Fatal("an unrelated transition must not license the assertion's write grants")
	}
	for _, g := range []string{"secret-custody", "cloud-mutate", "cluster-write"} {
		if !strings.Contains(errText(errs), g) {
			t.Errorf("every offending grant should be reported; %q missing from:\n%s", g, errText(errs))
		}
	}
}

func TestTransitionToSeededRequiresSecretCustody(t *testing.T) {
	e := extension.Extension{Name: "openbao-seed", Short: "seed OpenBao paths",
		Bindings: []extension.Binding{bind(extension.Transition, extension.Seeded, extension.ClusterWrite)}}
	if errs := e.Validate(); len(errs) == 0 {
		t.Fatal("a transition to seeded without secret-custody must be rejected")
	}
	e.Bindings[0].Grants = append(e.Bindings[0].Grants, extension.SecretCustody)
	if errs := e.Validate(); len(errs) != 0 {
		t.Errorf("with secret-custody it must validate, got:\n%s", errText(errs))
	}
}

// REGRESSION. gate + transition:seeded was UNSATISFIABLE while grants were
// extension-scoped: one rule demanded secret-custody, the other forbade anything
// but read-repo, and no grant set satisfied both — the author got a loop with no
// reachable fix. Per-binding grants make it satisfiable exactly as it should be.
func TestGatePlusSeededTransitionIsSatisfiable(t *testing.T) {
	e := extension.Extension{
		Name: "seeder-with-a-gate", Short: "seeds credentials and lints its own config",
		Bindings: []extension.Binding{
			bind(extension.Gate, extension.Configured, extension.ReadRepo),
			bind(extension.Transition, extension.Seeded, extension.SecretCustody),
		},
	}
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("this combination must be satisfiable, got:\n%s", errText(errs))
	}
}

func TestOwnPathsOnlyWhereFilesAreWritten(t *testing.T) {
	mk := func(k extension.BindingKind, s extension.State) extension.Extension {
		return extension.Extension{Name: "template-sustain", Short: "upgrade policy and drift",
			Bindings: []extension.Binding{bind(k, s, extension.OwnPaths)}}
	}
	for _, s := range []extension.State{extension.Scaffolded, extension.Upgraded} {
		if errs := mk(extension.Transition, s).Validate(); len(errs) != 0 {
			t.Errorf("own-paths on a transition to %q must validate: %s", s, errText(errs))
		}
	}
	if errs := mk(extension.Transition, extension.Provisioned).Validate(); len(errs) == 0 {
		t.Error("own-paths on a transition to provisioned is meaningless and must be rejected")
	}
	if errs := mk(extension.Gate, extension.Scaffolded).Validate(); len(errs) == 0 {
		t.Error("own-paths on a gate must be rejected")
	}
}

// ── the whole-catalog check ──────────────────────────────────────────────────

func catalogSample() []extension.Extension {
	return []extension.Extension{{
		Name: "guard-budgets", Short: "untestable-loc, coverage and core-surface budgets", Always: true,
		Bindings: []extension.Binding{bind(extension.Gate, extension.Configured, extension.ReadRepo)},
	}, {
		// The acid test, post-split: converge is the action, health the predicate.
		Name: "converge", Short: "reconcile declared components", Always: true,
		Bindings: []extension.Binding{
			bind(extension.Transition, extension.Converged, extension.ClusterRead, extension.ClusterWrite),
			bind(extension.Assertion, extension.Converged, extension.ClusterRead),
		},
	}, {
		Name: "import-brownfield", Short: "adopt an existing cluster",
		Bindings: []extension.Binding{
			bind(extension.Transition, extension.Provisioned, extension.ReadRepo, extension.CloudRead, extension.CloudMutate)},
	}, {
		Name: "assert-observability", Short: "scrape, alerting and log-ingestion assertions", Always: true,
		Bindings: []extension.Binding{bind(extension.Assertion, extension.Verified, extension.ClusterRead)},
	}, {
		Name: "reconciler-runtime", Short: "in-cluster reconcile loop and leader election", Always: true,
		Bindings: []extension.Binding{
			bind(extension.Invariant, extension.Operating, extension.ClusterRead, extension.ClusterWrite)},
	}, {
		// The pairing pattern: one extension, two bindings, each scoped separately.
		Name: "harbor-provisioner", Short: "Harbor projects, robots and their assertion",
		Bindings: []extension.Binding{
			bind(extension.Transition, extension.Seeded, extension.SecretCustody),
			bind(extension.Assertion, extension.Verified, extension.ClusterRead),
		},
	}, {
		// The catalog's two flagged anomalies resolve the same way: the mutating
		// half is a transition, the observing half an assertion.
		Name: "assert-storage", Short: "volume encryption assertions plus the relabel action", Always: true,
		Bindings: []extension.Binding{
			bind(extension.Assertion, extension.Verified, extension.ClusterRead),
			bind(extension.Transition, extension.Converged, extension.CloudMutate),
		},
	}, {
		Name: "scaffold-instance", Short: "render a new instance repo", Always: true,
		Bindings: []extension.Binding{
			bind(extension.Transition, extension.Scaffolded, extension.ReadRepo, extension.OwnPaths)},
	}}
}

func TestCatalogSampleIsExpressible(t *testing.T) {
	if errs := extension.ValidateSet(catalogSample()); len(errs) != 0 {
		t.Fatalf("the catalog sample must be expressible, got:\n%s", errText(errs))
	}
}

func TestValidateSetRejectsDuplicateNames(t *testing.T) {
	e := ok()
	if got := errText(extension.ValidateSet([]extension.Extension{e, e})); !strings.Contains(got, "duplicate extension name") {
		t.Errorf("want a duplicate-name error, got: %s", got)
	}
}

// Grants() is derived from the bindings, so it can never disagree with them —
// which is the property that makes per-binding scoping safe to summarise.
func TestExtensionGrantsAreTheDerivedUnion(t *testing.T) {
	var converge extension.Extension
	for _, e := range catalogSample() {
		if e.Name == "converge" {
			converge = e
		}
	}
	got := converge.Grants()
	want := []extension.Grant{extension.ClusterRead, extension.ClusterWrite}
	if len(got) != len(want) {
		t.Fatalf("Grants() = %v, want %v (deduped union, vocabulary order)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Grants()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if !converge.HasGrant(extension.ClusterWrite) || converge.HasGrant(extension.SecretCustody) {
		t.Error("HasGrant must search every binding and nothing more")
	}
}

func TestVocabulariesAreStableAndClosed(t *testing.T) {
	if got := len(extension.Grants()); got != 7 {
		t.Errorf("the catalog measured 7 grants, got %d — if a grant was added deliberately, update this and the catalog together", got)
	}
	if got := len(extension.States()); got != 10 {
		t.Errorf("want 10 states, got %d", got)
	}
	for i := 0; i < 5; i++ {
		if extension.States()[0] != extension.Scaffolded || extension.Grants()[0] != extension.ReadRepo {
			t.Fatal("vocabulary order is not stable")
		}
	}
}

func TestHelpers(t *testing.T) {
	e := ok()
	if !e.Binds(extension.Gate) || e.Binds(extension.Transition) {
		t.Error("Binds is wrong")
	}
	if got := bind(extension.Gate, extension.Configured).String(); got != "gate:configured" {
		t.Errorf("Binding.String() = %q", got)
	}
	// grants appear in the string so an error names the offending scope, not just the kind
	if got := bind(extension.Gate, extension.Configured, extension.ReadRepo).String(); got != "gate:configured[read-repo]" {
		t.Errorf("Binding.String() with grants = %q", got)
	}
}
