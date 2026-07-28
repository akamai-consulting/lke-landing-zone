package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// databases is the only root whose spec mapping emits a MULTI-LINE right-hand
// side (`databases = { "<name>" = { … } }`, one entry per cluster — the 0-n
// fan-out the root does `for_each` over). Every other assignment is a scalar or a
// single-line list, so nothing else exercises applyAssigns/setHCLField with an
// RHS containing newlines: setHCLField is a single-line `^key\s*=.*$` rewrite, and
// the result then has to survive `tofu fmt`, which owns the `=` alignment the
// instance's own fmt-check enforces.
//
// The unit test in internal/clusterspec pins the mapping; this pins the seam.
func TestRenderDatabasesTfvars_MultilineAssign(t *testing.T) {
	base, err := tfrootExample("databases")
	if err != nil {
		t.Fatalf("read embedded databases tfvars.example: %v", err)
	}

	var c clusterspec.Cluster
	c.Databases = clusterspec.Databases{
		"shared":    {Region: "us-ord", VPCID: 575244, SubnetID: 12345, EngineVersion: "16", Type: "g6-dedicated-2", ClusterSize: 2},
		"analytics": {Region: "us-ord", VPCID: 575244, SubnetID: 12345, ClusterSize: 1},
	}
	out := renderTfvars(base, clusterspec.DatabasesTFVars("prod", c))

	// Exactly one REAL assignment: the example's `databases = {}` was replaced, not
	// appended past. (Anchored to column 0 — the file's commented example block
	// contains an indented `databases = {` that must not count.)
	if n := len(regexp.MustCompile(`(?m)^databases\s*=`).FindAllString(out, -1)); n != 1 {
		t.Fatalf("want exactly 1 top-level `databases =` assignment, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "databases = {}") {
		t.Error("the example's empty default survived instead of being replaced")
	}
	for _, want := range []string{`"analytics" = {`, `"shared" = {`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered tfvars missing %q:\n%s", want, out)
		}
	}
	// vpc_id is an HCL number: unquoted, or the number-typed object attribute
	// rejects it. `tofu fmt` pads the `=` per block, so match the value, not a
	// fixed column.
	if n := len(regexp.MustCompile(`(?m)^\s+vpc_id\s+= 575244$`).FindAllString(out, -1)); n != 2 {
		t.Errorf("want an unquoted vpc_id in each of the 2 entries, got %d:\n%s", n, out)
	}

	// `llz render --check` compares BYTES, so an unchanged spec must re-render
	// identically — this is what the sorted keys in hclDatabases buy (Go map
	// iteration order is randomized).
	for i := 0; i < 5; i++ {
		if again := renderTfvars(base, clusterspec.DatabasesTFVars("prod", c)); again != out {
			t.Fatalf("re-render %d drifted — `llz render --check` would report false drift", i)
		}
	}

	// Zero clusters: the example's `databases = {}` stands, so the root applies
	// cleanly and provisions nothing. This is the common case.
	stub := renderTfvars(base, clusterspec.DatabasesTFVars("dev", clusterspec.Cluster{}))
	if !strings.Contains(stub, "databases = {}") {
		t.Errorf("an unconfigured env must keep `databases = {}`:\n%s", stub)
	}
}
