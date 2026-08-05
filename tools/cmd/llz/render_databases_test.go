package main

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// databases is the only root whose spec mapping emits a MULTI-LINE right-hand
// side (`databases = { "<name>" = { … } }`, one entry per cluster — the 0-n
// fan-out the root does `for_each` over). Every other assignment is a scalar or a
// single-line list, so nothing else exercises applyAssigns/setHCLField with an
// RHS containing newlines — setHCLField being a single-line `^key\s*=.*$` rewrite.
//
// It must hold WITHOUT a tofu/terraform binary. renderTfvars pipes through
// `tofu fmt`, but fmtHCL is a best-effort pass-through when neither binary is
// installed — which is exactly the CI container. An earlier version of this test
// asserted the indentation `tofu fmt` adds and so passed locally and failed in CI;
// hclDatabases now emits already-formatted HCL, so the bytes are the same either
// way. Assert on bytes, not on layout the environment might supply.
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
	out := renderTfvars(base, clusterspec.DatabasesTFVars("acme", "prod", c))

	// Exactly one REAL assignment: the example's `databases = {}` was replaced, not
	// appended past. (Anchored to column 0 — the file's commented example block
	// contains an indented `databases = {` that must not count.)
	if n := len(regexp.MustCompile(`(?m)^databases\s*=`).FindAllString(out, -1)); n != 1 {
		t.Fatalf("want exactly 1 top-level `databases =` assignment, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "databases = {}") {
		t.Error("the example's empty default survived instead of being replaced")
	}
	// The exact block, byte for byte. This is what hclDatabases must produce with no
	// formatter available: two-space indent per level, `=` padded to the longest key
	// WITHIN each entry (so the two entries align differently — `cluster_size` vs
	// `engine_version`), keys sorted, numbers unquoted, and only the optionals the
	// spec set. It is also precisely what `tofu fmt` produces, which is the point:
	// with or without the binary, the rendered bytes are identical.
	wantBlock := `databases = {
  "analytics" = {
    region       = "us-ord"
    vpc_id       = 575244
    subnet_id    = 12345
    cluster_size = 1
  }
  "shared" = {
    region         = "us-ord"
    vpc_id         = 575244
    subnet_id      = 12345
    engine_version = "16"
    db_type        = "g6-dedicated-2"
    cluster_size   = 2
  }
}`
	if !strings.Contains(out, wantBlock) {
		t.Errorf("rendered databases block does not match.\nwant:\n%s\n\ngot:\n%s", wantBlock, out)
	}

	// `llz render --check` compares BYTES, so an unchanged spec must re-render
	// identically — this is what the sorted keys in hclDatabases buy (Go map
	// iteration order is randomized).
	for i := 0; i < 5; i++ {
		if again := renderTfvars(base, clusterspec.DatabasesTFVars("acme", "prod", c)); again != out {
			t.Fatalf("re-render %d drifted — `llz render --check` would report false drift", i)
		}
	}

	// Zero clusters: the example's `databases = {}` stands, so the root applies
	// cleanly and provisions nothing. This is the common case.
	stub := renderTfvars(base, clusterspec.DatabasesTFVars("acme", "dev", clusterspec.Cluster{}))
	if !strings.Contains(stub, "databases = {}") {
		t.Errorf("an unconfigured env must keep `databases = {}`:\n%s", stub)
	}

	// The environment must not change the output. With no tofu/terraform on PATH
	// fmtHCL returns its input untouched, so this run skips the formatter entirely —
	// and must still produce the same bytes as the run above, which (on a dev
	// machine) went through `tofu fmt`. This is the assertion that would have caught
	// the CI-only failure: locally both paths formatted, in CI neither did.
	withLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	if unformatted := renderTfvars(base, clusterspec.DatabasesTFVars("acme", "prod", c)); unformatted != out {
		t.Errorf("render differs with no tofu/terraform on PATH — the CI container has neither, "+
			"so hclDatabases must emit already-formatted HCL.\nwith formatter:\n%s\n\nwithout:\n%s", out, unformatted)
	}
}
