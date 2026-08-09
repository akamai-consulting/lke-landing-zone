package extension_test

// The design doc states the binding→state rules as a table and draws them twice.
// Prose drifts from code silently, and this PR has already had to correct one
// stale copy of a number. So the doc's table is checked against bindableStates
// here: if the model changes and docs/designs/internal-extension-model.md does
// not, this fails rather than leaving a diagram that quietly lies.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

const modelDoc = "../../../../docs/designs/internal-extension-model.md"

func TestDesignDocBindingTableMatchesTheCode(t *testing.T) {
	body, err := os.ReadFile(filepath.FromSlash(modelDoc))
	if err != nil {
		t.Fatalf("the model doc is the spec for this package and must exist: %v", err)
	}
	doc := string(body)

	// Every state the code permits, phrased as the doc phrases it.
	spine := []extension.State{extension.Scaffolded, extension.Configured, extension.Provisioned,
		extension.Seeded, extension.Converged, extension.Verified, extension.Operating}

	rowRE := regexp.MustCompile(`(?m)^\| ` + "`" + `(transition|assertion|invariant|gate)` + "`" + ` \| .*? \| (.*?) \|$`)
	rows := map[string]string{}
	for _, m := range rowRE.FindAllStringSubmatch(doc, -1) {
		rows[m[1]] = m[2]
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 binding rows in the doc table, found %d — the table moved or changed shape", len(rows))
	}

	// Each claim is checked by CONSTRUCTING the bindings and asking the validator,
	// so the doc is pinned to behaviour rather than to a second copy of the table.
	assertAttaches := func(k extension.BindingKind, s extension.State, want bool) {
		t.Helper()
		g := []extension.Grant{extension.ReadRepo}
		if k == extension.Transition && s == extension.Seeded {
			g = []extension.Grant{extension.SecretCustody}
		}
		e := extension.Extension{Name: "probe", Short: "x",
			Bindings: []extension.Binding{{Kind: k, State: s, Grants: g}}}
		if got := len(e.Validate()) == 0; got != want {
			t.Errorf("%s:%s attaches=%v, doc says %v", k, s, got, want)
		}
	}

	for _, s := range spine {
		attaches := s != extension.Verified && s != extension.Operating
		assertAttaches(extension.Transition, s, attaches)
		assertAttaches(extension.Assertion, s, true) // "any state"
		assertAttaches(extension.Invariant, s, s == extension.Operating)
		assertAttaches(extension.Gate, s, s == extension.Scaffolded || s == extension.Configured)
	}

	// The RECURRING states, checked for the same reason the spine is. Sweeping only
	// the spine was how the first cut shipped an Assertion row that excluded
	// `destroyed`: assert-no-orphans had nowhere to attach and no test looked.
	for _, s := range []extension.State{extension.Promoted, extension.Upgraded, extension.Destroyed} {
		assertAttaches(extension.Transition, s, true)
		assertAttaches(extension.Assertion, s, true)
		assertAttaches(extension.Invariant, s, false)
		assertAttaches(extension.Gate, s, false)
	}

	// The doc's exact wording, so a reworded row is noticed too.
	for kind, want := range map[string]string{
		"transition": "every state except",
		"assertion":  "any state",
		"invariant":  "`operating`",
		"gate":       "`scaffolded`, `configured`",
	} {
		if !strings.Contains(rows[kind], want) {
			t.Errorf("doc row %q reads %q, expected it to contain %q", kind, rows[kind], want)
		}
	}
}

// The precondition section, pinned the same way: by CONSTRUCTING the declarations
// the doc describes and asking the validator, so the prose is tied to behaviour
// rather than to a second copy of requirableStates.
func TestDesignDocPreconditionSectionMatchesTheCode(t *testing.T) {
	body, err := os.ReadFile(filepath.FromSlash(modelDoc))
	if err != nil {
		t.Fatalf("the model doc is the spec for this package and must exist: %v", err)
	}
	doc := string(body)

	if !strings.Contains(doc, "#### Preconditions") {
		t.Fatal("the Preconditions section is gone from the model doc — Requires is part of " +
			"the model's shape and the doc is its spec")
	}

	valid := func(b extension.Binding) bool {
		e := extension.Extension{Name: "probe", Short: "x", Bindings: []extension.Binding{b}}
		return len(e.Validate()) == 0
	}

	// "optional — the zero value means the binding makes no such claim"
	if !valid(extension.Binding{Kind: extension.Transition, State: extension.Seeded,
		Grants: []extension.Grant{extension.SecretCustody}}) {
		t.Error("doc says Requires is optional, but a binding without one does not validate")
	}
	// "`operating`, on a `transition` only"
	if !valid(extension.Binding{Kind: extension.Transition, State: extension.Seeded,
		Requires: extension.Operating, Grants: []extension.Grant{extension.SecretCustody}}) {
		t.Error("doc says a transition may require `operating`, but that does not validate")
	}
	if valid(extension.Binding{Kind: extension.Assertion, State: extension.Converged,
		Requires: extension.Operating, Grants: []extension.Grant{extension.ClusterRead}}) {
		t.Error("doc says `transition` only, but an assertion accepted a precondition")
	}
	if valid(extension.Binding{Kind: extension.Transition, State: extension.Seeded,
		Requires: extension.Converged, Grants: []extension.Grant{extension.SecretCustody}}) {
		t.Error("doc says `operating` only, but `converged` was accepted as a precondition")
	}
	// "the grant check runs at both states, never at `Requires` alone"
	if valid(extension.Binding{Kind: extension.Transition, State: extension.Configured,
		Requires: extension.Operating, Grants: []extension.Grant{extension.SecretCustody}}) {
		t.Error("doc says the grant check runs at BOTH states, but a grant illegal at the " +
			"declared State was accepted because it is legal at the precondition")
	}
}
