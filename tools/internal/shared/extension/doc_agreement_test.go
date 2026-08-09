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

// THE GRANT-STATE TABLE MUST MATCH THE DOC TOO, and it did not.
//
// ────────────────────────────────────────────────────────────────────────────
// This file checked bindableStates and requirableStates and left grantStates
// alone — the table that has been WIDENED FOUR TIMES and gained a whole new row,
// making it by far the most-edited part of the ceiling and the one most likely to
// drift. It had drifted:
//
//	cloud-mutate   doc listed five states, code allowed six (`configured` missing)
//	cluster-write  doc said "at the same five", after the two rows had diverged
//	write-repo     absent from the doc entirely
//
// Three errors in the half of the ceiling that governs the DANGEROUS grants, in a
// document whose whole purpose is to say what the ceiling is. Nobody noticed
// because nothing looked.
//
// The check is deliberately the same shape as the binding-table one: parse the
// doc's own prose, compare against the live map, and report both directions. A
// widening that skips the doc now fails here, which is what makes the argued-row
// discipline enforceable rather than merely stated.
// ────────────────────────────────────────────────────────────────────────────
func TestDesignDocGrantStatesMatchesTheCode(t *testing.T) {
	body, err := os.ReadFile(filepath.FromSlash(modelDoc))
	if err != nil {
		t.Fatalf("the model doc is the spec for this package and must exist: %v", err)
	}
	doc := string(body)

	// THE CEILING ROW, LOCATED FIRST. Searching the whole document matched an
	// earlier mention of the same grant in a different rule and compared against
	// the wrong list — a scan that finds *a* match rather than *the* match is its
	// own kind of false confidence.
	var row string
	for _, line := range strings.Split(doc, "\n") {
		if strings.Contains(line, "the other half of the ceiling") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("the model doc no longer contains the restricted-grant ceiling row — either it " +
			"moved or the ceiling stopped being documented, and this check cannot tell which")
	}

	// Each grant's clause runs from its name to the next `;` or the cell end, so a
	// state listed for one grant cannot be read as belonging to its neighbour.
	states := regexp.MustCompile("`([a-z-]+)`")
	for _, g := range []extension.Grant{
		extension.SecretCustody, extension.CloudMutate,
		extension.ClusterWrite, extension.WriteRepo,
	} {
		want := extension.GrantStates(g)
		if len(want) == 0 {
			t.Errorf("%s is no longer a restricted grant but is still checked here", g)
			continue
		}
		marker := "`" + string(g) + "`"
		i := strings.Index(row, marker)
		if i < 0 {
			t.Errorf("the ceiling row does not mention %q. Every restricted grant must be "+
				"documented — an undocumented row is a ceiling only the code knows.", g)
			continue
		}
		clause := row[i+len(marker):]
		if j := strings.Index(clause, ";"); j >= 0 {
			clause = clause[:j]
		}
		if j := strings.Index(clause, "|"); j >= 0 {
			clause = clause[:j]
		}
		got := map[string]bool{}
		for _, m := range states.FindAllStringSubmatch(clause, -1) {
			// The prose names other grants parenthetically ("reading it is
			// `secret-read`"); only state names count.
			for _, st := range extension.States() {
				if string(st) == m[1] {
					got[m[1]] = true
				}
			}
		}
		for _, s := range want {
			if !got[string(s)] {
				t.Errorf("code allows %q at %q; the doc's clause for it does not list that state. "+
					"A widening that skips the doc leaves the spec describing a narrower ceiling "+
					"than the validator enforces.", g, s)
			}
		}
		for s := range got {
			var found bool
			for _, w := range want {
				if string(w) == s {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("the doc says %q is allowed at %q and the code does not — the doc "+
					"describes a wider ceiling than exists, which is the direction that gets "+
					"someone's declaration refused with no explanation.", g, s)
			}
		}
	}
}
