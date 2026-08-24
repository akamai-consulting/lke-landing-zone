package database

// declares_test.go — "does this deployment declare a database" must be answered
// from the VALUE, not from the presence of the assignment.
//
// ── THE REGRESSION ────────────────────────────────────────────────────────────
//
// The guard shipped on 2026-08-22 reasoning that DatabasesTFVars omits the
// `databases` assignment for an empty map, so the line is present if and only if
// a cluster was declared. The premise is true; the conclusion does not follow.
// `llz render` applies assignments ON TOP OF the root's terraform.tfvars.example,
// and that example ships an uncommented `databases = {}`. Omitting the assignment
// leaves the example's empty map in place.
//
// So Declared came back true for a deployment with zero databases — the DEFAULT
// configuration — and seed-db-admin killed the bootstrap with "run the databases
// apply first", advice that could not help: the apply had already run in the same
// pipeline and correctly created nothing.
//
// ── WHY THE UNIT TESTS DID NOT CATCH IT ───────────────────────────────────────
//
// Because they fed the predicate hand-written strings that matched the belief.
// The producer's REAL output was never put through the consumer's REAL predicate,
// which is the split-contract archetype in docs/e2e-gates.md. TestTheShippedExampleDoesNotReadAsADeclaration
// below closes exactly that: it reads the embedded example this repo actually
// ships and requires the predicate to answer "none".

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// THE GATE. Feed the producer's real bytes to the consumer's real predicate: the
// databases tfvars as `llz render` actually writes it for a deployment declaring
// no databases. A hand-written fixture cannot catch this, because the bug WAS the
// gap between the fixture and the file.
func TestTheShippedExampleDoesNotReadAsADeclaration(t *testing.T) {
	raw, err := render.TfrootExample("databases")
	if err != nil {
		t.Fatalf("read the embedded databases tfvars example: %v", err)
	}
	// Fail closed on vacuity: if the example ever stops carrying the assignment,
	// this test would pass while exercising nothing.
	if !strings.Contains(raw, "databases") {
		t.Fatal("the databases example no longer mentions `databases` — this test would pass " +
			"having checked nothing")
	}

	// Exactly what render produces for a spec with no databases: the example, with
	// DatabasesTFVars' assignments applied. len(c.Databases)==0 omits the databases
	// assign, which is the case that broke.
	rendered := render.Tfvars(raw, clusterspec.DatabasesTFVars("llz", "e2e", clusterspec.Cluster{}))

	if declaresDatabases([]byte(rendered)) {
		t.Errorf("a deployment declaring NO databases reads as declaring some.\n"+
			"    This is the default configuration, and it makes seed-db-admin kill the bootstrap\n"+
			"    with \"run the databases apply first\" — after that apply already ran and correctly\n"+
			"    created nothing.\n\nrendered tfvars:\n%s", rendered)
	}
}

// The other half of the same contract: a deployment that DOES declare a cluster
// must still be detected, or the guard stops protecting the case it was built
// for — a live PROVISIONING password left in Terraform state.
func TestARealDeclarationIsStillDetected(t *testing.T) {
	raw, err := render.TfrootExample("databases")
	if err != nil {
		t.Fatal(err)
	}
	c := clusterspec.Cluster{Databases: map[string]clusterspec.Database{
		"harbor": {Region: "us-ord", VPCID: 575244, SubnetID: 12345},
	}}
	rendered := render.Tfvars(raw, clusterspec.DatabasesTFVars("llz", "e2e", c))

	if !declaresDatabases([]byte(rendered)) {
		t.Errorf("a declared database cluster was not detected — the guard would exit 0 and leave "+
			"the provisioning password live.\n\nrendered tfvars:\n%s", rendered)
	}
}

// The value forms, in isolation. The empty-map spellings are the bug; the rest
// pin the edges so a future rewrite cannot quietly re-widen the predicate.
func TestDeclaresDatabasesValueForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"the example's own empty map", "databases = {}\n", false},
		{"empty with a space", "databases = { }\n", false},
		{"empty across lines", "databases = {\n}\n", false},
		{"empty but commented inside", "databases = {\n  # harbor = { ... }\n}\n", false},
		{"no assignment at all", "region_suffix = \"e2e\"\n", false},
		{"one cluster", "databases = {\n  harbor = {\n    region = \"us-ord\"\n  }\n}\n", true},
		{"one cluster, same line", "databases = { harbor = { region = \"us-ord\" } }\n", true},
		// A commented-out assignment must not count — the example carries a fully
		// worked illustration above the real line, and matching it would put every
		// deployment back where this started.
		{"commented-out example block", "#   databases = {\n#     harbor = {}\n#   }\ndatabases = {}\n", false},
		// Cannot-tell resolves LOUD. Exiting 0 here leaves the provisioning
		// password live, which is strictly worse than a false alarm.
		{"unbalanced braces", "databases = {\n  harbor = {\n", true},
		{"not a map literal", "databases = var.databases\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := declaresDatabases([]byte(tc.body)); got != tc.want {
				t.Errorf("declaresDatabases(%q) = %t, want %t", tc.body, got, tc.want)
			}
		})
	}
}
