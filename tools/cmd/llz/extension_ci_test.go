package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── scheduled trigger (the WHEN axis) ────────────────────────────────────────

// A schedule: cron makes a ci step a standalone scheduled job in its own workflow,
// with on: schedule (distinct crons unioned) + workflow_dispatch and dependency order.
func TestRenderScheduledWorkflow(t *testing.T) {
	jobs := []extCIJob{
		{Ext: "mon", Name: "expiry", Schedule: "0 0 * * 0", Argv: []string{"llz", "ci", "expiry-audit"}},
		{Ext: "mon", Name: "report", Schedule: "0 0 * * 0", Argv: []string{"llz", "status"}, DependsOn: []string{"x-mon-expiry"}},
	}
	out, err := renderScheduledWorkflow(jobs)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out,
		"name: llz extensions (scheduled)",
		"  schedule:\n    - cron: \"0 0 * * 0\"", // the two identical crons collapse to one
		"workflow_dispatch: {}",
		"x-mon-expiry:",
		"needs: [x-mon-expiry]", // dependsOn among scheduled steps rides needs:
	)
}

// The CI tool-supply: a ci step's digest-pinned image renders into the job's container:,
// for both the converge-anchored and the scheduled workflow.
func TestCIStepImageRendersContainer(t *testing.T) {
	const img = "ghcr.io/org/spin-toolchain@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	anchored := []extCIJob{{Ext: "fn", Name: "deploy", Anchor: anchorPostConverge, Image: img, Argv: []string{"spin", "deploy"}}}
	out, err := renderExtensionsWorkflow(anchored)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "container: "+img, "run: spin deploy")

	scheduled := []extCIJob{{Ext: "fn", Name: "scan", Schedule: "0 0 * * 0", Image: img, Argv: []string{"trivy", "fs", "."}}}
	sout, err := renderScheduledWorkflow(scheduled)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, sout, "container: "+img)
}

// A mutable tag (not digest-pinned) is rejected at generation — a remote extension's CI
// image is trust surface, so :latest can't be swapped under you after review.
func TestCIStepImageMustBeDigestPinned(t *testing.T) {
	for _, bad := range []string{"ghcr.io/org/img:latest", "alpine", "alpine:3.20"} {
		jobs := []extCIJob{{Ext: "a", Name: "x", Anchor: anchorPostConverge, Image: bad, Argv: []string{"y"}}}
		if _, err := renderExtensionsWorkflow(jobs); err == nil {
			t.Errorf("image %q must be rejected (not digest-pinned)", bad)
		}
	}
}

// A scheduled step may depend only on another scheduled step — a cross-trigger edge
// (to a converge-anchored job in the other workflow) can't be a needs: edge.
func TestScheduledRejectsCrossTriggerDep(t *testing.T) {
	jobs := []extCIJob{{Ext: "mon", Name: "x", Schedule: "0 0 * * 0", Argv: []string{"y"}, DependsOn: []string{"x-other-anchored"}}}
	if _, err := renderScheduledWorkflow(jobs); err == nil {
		t.Fatal("a scheduled step must not depend on a non-scheduled job")
	}
}

// partitionCIJobs routes by trigger, and runExtensionCIWorkflow emits BOTH files,
// removing the scheduled file when its last scheduled step is dropped.
func TestCIWorkflowGeneratesBothFilesAndCleansUp(t *testing.T) {
	root := t.TempDir()
	jobs := []extCIJob{
		{Ext: "a", Name: "anchored", Anchor: anchorPostConverge, Argv: []string{"llz", "x"}},
		{Ext: "a", Name: "cron", Schedule: "0 0 1 * *", Argv: []string{"llz", "y"}},
	}
	if anchored, scheduled := partitionCIJobs(jobs); len(anchored) != 1 || len(scheduled) != 1 {
		t.Fatalf("partition = %d anchored, %d scheduled", len(anchored), len(scheduled))
	}
	if err := runExtensionCIWorkflow(globalOpts{}, jobs, root, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{extensionsWorkflowPath, extensionsScheduledWorkflowPath} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Fatalf("%s should exist: %v", p, err)
		}
	}
	if err := runExtensionCIWorkflow(globalOpts{}, jobs, root, true); err != nil {
		t.Fatalf("--check should pass when up to date: %v", err)
	}
	// drop the scheduled job → --check flags drift, regen removes the scheduled file
	if err := runExtensionCIWorkflow(globalOpts{}, jobs[:1], root, true); err == nil {
		t.Fatal("--check should fail after dropping the scheduled step")
	}
	if err := runExtensionCIWorkflow(globalOpts{}, jobs[:1], root, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, extensionsScheduledWorkflowPath)); !os.IsNotExist(err) {
		t.Fatal("scheduled workflow should be removed once no scheduled steps remain")
	}
}

func TestCIJobID(t *testing.T) {
	cases := []struct{ ext, name, want string }{
		{"renovate", "validate", "x-renovate-validate"},
		{"akamai-functions", "deploy lab", "x-akamai-functions-deploy-lab"},
		{"My_Guard", "Check!!", "x-my-guard-check"},
	}
	for _, c := range cases {
		if got := ciJobID(c.ext, c.name); got != c.want {
			t.Errorf("ciJobID(%q,%q) = %q, want %q", c.ext, c.name, got, c.want)
		}
	}
}

// post-converge / operate jobs need the converge pivot; the inter-extension
// dependsOn edge rides GitHub `needs:` for free.
func TestRenderNeedsWiring(t *testing.T) {
	jobs := []extCIJob{
		{Ext: "obs", Name: "dash", Anchor: anchorPostConverge, Argv: []string{"llz", "x"}},
		{Ext: "fn", Name: "deploy", Anchor: anchorPostConverge, Argv: []string{"spin", "deploy"},
			DependsOn: []string{"x-obs-dash"}}, // deploy after the dashboards land
	}
	out, err := renderExtensionsWorkflow(jobs)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out,
		"x-obs-dash:",
		"needs: [converge]",             // post-converge → pivot
		"needs: [converge, x-obs-dash]", // pivot + inter-extension DAG edge
		"run: llz ci converge",          // the pivot job exists
	)
}

// The finding: a pre-converge job is bidirectional — the CORE converge job gains
// a needs: on the extension. Only expressible because llz generates the converge
// caller job.
func TestRenderPreConvergeIsBidirectional(t *testing.T) {
	jobs := []extCIJob{
		{Ext: "seed", Name: "warm", Anchor: anchorPreConverge, Argv: []string{"llz", "warm"}},
	}
	out, err := renderExtensionsWorkflow(jobs)
	if err != nil {
		t.Fatal(err)
	}
	// the converge job must declare it needs the pre-converge extension job
	if !strings.Contains(out, "converge:\n    name: converge (core bootstrap pivot)\n    needs: [x-seed-warm]") {
		t.Fatalf("converge job should need the pre-converge job; got:\n%s", out)
	}
	// and the pre-converge job itself has no needs on converge (would be a cycle)
	if strings.Contains(out, "x-seed-warm:\n    name: seed:warm (pre-converge)\n    needs:") {
		t.Fatalf("pre-converge job should not need converge:\n%s", out)
	}
}

func TestRenderRejectsCycle(t *testing.T) {
	// a pre-converge job that dependsOn a post-converge job: post needs converge
	// needs pre needs post → cycle, caught at generation.
	jobs := []extCIJob{
		{Ext: "a", Name: "one", Anchor: anchorPreConverge, Argv: []string{"x"}, DependsOn: []string{"x-b-two"}},
		{Ext: "b", Name: "two", Anchor: anchorPostConverge, Argv: []string{"y"}},
	}
	if _, err := renderExtensionsWorkflow(jobs); err == nil {
		t.Fatal("expected a cycle error")
	}
}

func TestRenderRejectsUnknownAnchorAndDep(t *testing.T) {
	if _, err := renderExtensionsWorkflow([]extCIJob{{Ext: "a", Name: "x", Anchor: "mid-converge", Argv: []string{"z"}}}); err == nil {
		t.Fatal("expected unknown-anchor error (the WHERE ceiling)")
	}
	if _, err := renderExtensionsWorkflow([]extCIJob{{Ext: "a", Name: "x", Anchor: anchorPostConverge, Argv: []string{"z"}, DependsOn: []string{"x-nope-nope"}}}); err == nil {
		t.Fatal("expected unknown-dependsOn error")
	}
}

// Determinism: same input → byte-identical output (the --check drift gate needs this).
func TestRenderDeterministic(t *testing.T) {
	jobs := []extCIJob{
		{Ext: "a", Name: "one", Anchor: anchorPreConverge, Argv: []string{"x"}},
		{Ext: "b", Name: "two", Anchor: anchorPostConverge, Argv: []string{"y"}},
		{Ext: "c", Name: "three", Anchor: anchorOperate, Argv: []string{"z"}},
	}
	a, _ := renderExtensionsWorkflow(jobs)
	b, _ := renderExtensionsWorkflow(jobs)
	if a != b {
		t.Fatal("render is not deterministic")
	}
}

// The Anchor interface + registry: names match, and each binding installs the
// correct edge direction (the behavior that used to be a switch).
func TestAnchorRegistryBindings(t *testing.T) {
	if got := strings.Join(ciAnchorNames(), ","); got != "pre-converge,post-converge,operate" {
		t.Fatalf("anchor names = %q", got)
	}
	d := newDAG()
	d.addNode(&dagNode{id: convergeJob})
	d.addNode(&dagNode{id: "job"})

	pre, ok := lookupAnchor(anchorPreConverge)
	if !ok {
		t.Fatal("pre-converge should resolve")
	}
	pre.Bind(d, "job")
	if !d.preds[convergeJob]["job"] {
		t.Fatal("pre-converge should make converge need job (phase→job)")
	}

	post, _ := lookupAnchor(anchorPostConverge)
	post.Bind(d, "job")
	if !d.preds["job"][convergeJob] {
		t.Fatal("post-converge should make job need converge (job→phase)")
	}

	if _, ok := lookupAnchor("mid-converge"); ok {
		t.Fatal("unknown anchor must not resolve")
	}
}

// coreSpine is DERIVED from the lifecycle registry's anchorable phases — same count,
// same ids, same order. Adding an anchorable phase (or a spine node) on only one side
// fails here.
func TestCoreSpineMatchesAnchorablePhases(t *testing.T) {
	anchorable := anchorablePhases()
	if len(coreSpine) != len(anchorable) {
		t.Fatalf("coreSpine has %d nodes but %d lifecycle phases are Anchorable", len(coreSpine), len(anchorable))
	}
	for i, p := range anchorable {
		if coreSpine[i].id != p.CoreJobID {
			t.Errorf("coreSpine[%d].id = %q, want phase %q CoreJobID %q", i, coreSpine[i].id, p.ID, p.CoreJobID)
		}
	}
}

// ── DAG unit tests (the formalism itself) ────────────────────────────────────

func TestDAGTopoOrderAndNeeds(t *testing.T) {
	d := newDAG()
	d.addNode(&dagNode{id: "a"})
	d.addNode(&dagNode{id: "b"})
	d.addNode(&dagNode{id: "c"})
	d.need("b", "a") // b needs a
	d.need("c", "b") // c needs b
	d.need("c", "a") // c needs a (out-of-order edge)
	got, err := d.topo()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("topo = %v, want [a b c]", got)
	}
	// needsOf is deterministic (insertion order), regardless of edge-add order.
	if n := strings.Join(d.needsOf("c"), ","); n != "a,b" {
		t.Fatalf("needsOf(c) = %q, want a,b", n)
	}
}

func TestDAGRejectsCycleAndDanglingEdge(t *testing.T) {
	cyc := newDAG()
	cyc.addNode(&dagNode{id: "a"})
	cyc.addNode(&dagNode{id: "b"})
	cyc.need("a", "b")
	cyc.need("b", "a")
	if _, err := cyc.topo(); err == nil {
		t.Fatal("expected a cycle error")
	}

	dangling := newDAG()
	dangling.addNode(&dagNode{id: "a"})
	dangling.need("a", "ghost")
	if _, err := dangling.topo(); err == nil {
		t.Fatal("expected a dangling-edge error")
	}
}

// operate jobs land after the converge pivot AND the operate phase node, proving
// the chained core spine orders multi-phase work through the DAG.
func TestRenderOperateAfterConverge(t *testing.T) {
	jobs := []extCIJob{{Ext: "rot", Name: "creds", Anchor: anchorOperate, Argv: []string{"llz", "ci", "rotate"}}}
	out, err := renderExtensionsWorkflow(jobs)
	if err != nil {
		t.Fatal(err)
	}
	ci, op, job := strings.Index(out, "  converge:"), strings.Index(out, "  operate:"), strings.Index(out, "  x-rot-creds:")
	if !(ci >= 0 && ci < op && op < job) {
		t.Fatalf("expected converge < operate < job order; got %d %d %d", ci, op, job)
	}
	mustContain(t, out, "needs: [operate]")
}

func mustContain(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("output missing %q\n---\n%s", sub, s)
		}
	}
}
