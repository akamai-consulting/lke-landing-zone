package main

// extension_ci.go is a SPIKE (issue #10): tie extension ci: steps into the
// lifecycle by REUSING the promote.yml codegen pattern (promote_gen.go) instead
// of inventing an orchestration runtime. It builds a formal DAG of jobs and emits
// .github/workflows/llz-extensions.yml — a static, needs:-chained workflow that is
// 100% GitHub-native at runtime, with a --check drift gate.
//
// The DAG is the single source of truth:
//   - nodes are jobs (core lifecycle phases + each extension ci: step);
//   - an edge u→v means "v needs u" (u runs first);
//   - `anchor` and `dependsOn` are both lowered to edges;
//   - a deterministic topological sort gives the emission order and detects
//     cycles / dangling references at generation time (and at --check in CI).
//
// What the spike probes (see extension_ci_test.go):
//   - post-converge/operate → an edge to a core phase. Clean.
//   - dependsOn → an arbitrary inter-job edge; GitHub needs: IS the DAG at
//     runtime, so we only add generation-time validation, no scheduler.
//   - pre-converge → the edge points INTO the core converge node (converge needs
//     the job). Expressible only because llz generates the converge caller job —
//     you can order around a whole reusable-workflow call, never a step inside it.
//     That granularity limit IS the anchor ceiling.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"

	"sigs.k8s.io/yaml"
)

// ── the DAG ──────────────────────────────────────────────────────────────────

// dagNode is one workflow job. run is the step argv (empty for a pure marker);
// note is an optional trailing comment on the run line; core marks a lifecycle
// phase node (vs an extension job).
type dagNode struct {
	id, title string
	run       []string
	note      string
	core      bool
}

// dag is a tiny directed acyclic graph over job ids. Edges express `needs`: an
// edge u→v means v needs u (u runs first), stored as v's predecessor set.
// Insertion order is retained so topo() is deterministic — which the --check
// drift gate relies on.
type dag struct {
	order []string // node ids in insertion order (stable tie-break)
	nodes map[string]*dagNode
	preds map[string]map[string]bool // preds[v][u] = v needs u
}

func newDAG() *dag {
	return &dag{nodes: map[string]*dagNode{}, preds: map[string]map[string]bool{}}
}

func (d *dag) addNode(n *dagNode) {
	if _, ok := d.nodes[n.id]; ok {
		return
	}
	d.nodes[n.id] = n
	d.preds[n.id] = map[string]bool{}
	d.order = append(d.order, n.id)
}

// need records "after needs before" (edge before→after).
func (d *dag) need(after, before string) { d.preds[after][before] = true }

// needsOf returns after's predecessors in deterministic (insertion) order.
func (d *dag) needsOf(after string) []string {
	var out []string
	for _, id := range d.order {
		if d.preds[after][id] {
			out = append(out, id)
		}
	}
	return out
}

// topo returns the node ids in a deterministic topological order, or an error on
// a cycle or an edge to an unknown node. It repeatedly emits the first
// not-yet-emitted node (in insertion order) whose predecessors are all emitted —
// O(n²), fine for a handful of jobs, and stable.
func (d *dag) topo() ([]string, error) {
	for _, v := range d.order {
		for u := range d.preds[v] {
			if _, ok := d.nodes[u]; !ok {
				return nil, fmt.Errorf("job %q needs unknown job %q", v, u)
			}
		}
	}
	emitted := map[string]bool{}
	var out []string
	for len(out) < len(d.order) {
		progressed := false
		for _, v := range d.order {
			if emitted[v] {
				continue
			}
			ready := true
			for u := range d.preds[v] {
				if !emitted[u] {
					ready = false
					break
				}
			}
			if ready {
				emitted[v] = true
				out = append(out, v)
				progressed = true
			}
		}
		if !progressed {
			return nil, fmt.Errorf("extension ci graph has a cycle (a dependsOn/anchor loop)")
		}
	}
	return out, nil
}

// ── lifecycle anchors + core spine ───────────────────────────────────────────

// ci anchors — the small, ordered enum of lifecycle positions a ci: step may bind
// to. This is the WHERE ceiling (mirrors the kind ceiling): there is no
// "mid-converge" anchor, because that would need to splice into a step inside a
// reusable workflow, which a needs: edge cannot reach.
const (
	anchorPreConverge  = "pre-converge"
	anchorPostConverge = "post-converge"
	anchorOperate      = "operate"
)

// Anchor is the extension point a ci: step ties into. An extension names an
// anchor declaratively (anchor: post-converge); the framework resolves the name
// to one of these, which knows how to wire the job's edge into the DAG. Adding a
// lifecycle tie-in is one entry in anchorRegistry — there is no switch to touch.
type Anchor interface {
	Name() string
	// Bind installs the edge the anchor's semantics imply, between a core spine
	// node and the extension job.
	Bind(d *dag, jobID string)
}

// afterAnchor runs the job AFTER a core phase node (job needs phase).
type afterAnchor struct{ name, phase string }

func (a afterAnchor) Name() string            { return a.name }
func (a afterAnchor) Bind(d *dag, job string) { d.need(job, a.phase) }

// beforeAnchor runs the job BEFORE a core phase node (phase needs job) — the
// bidirectional case, expressible only because llz generates the phase's caller
// job (you can order around a reusable-workflow call, never a step inside it).
type beforeAnchor struct{ name, phase string }

func (a beforeAnchor) Name() string            { return a.name }
func (a beforeAnchor) Bind(d *dag, job string) { d.need(a.phase, job) }

// anchorRegistry is the single source of truth for the tie-in points: each binds
// an extension job to a core spine node. Extend it to add an anchor.
var anchorRegistry = []Anchor{
	beforeAnchor{anchorPreConverge, convergeJob},
	afterAnchor{anchorPostConverge, convergeJob},
	afterAnchor{anchorOperate, operateJob},
}

func lookupAnchor(name string) (Anchor, bool) {
	for _, a := range anchorRegistry {
		if a.Name() == name {
			return a, true
		}
	}
	return nil, false
}

// ciAnchorNames lists the registered anchor names (help text + error messages).
func ciAnchorNames() []string {
	names := make([]string, len(anchorRegistry))
	for i, a := range anchorRegistry {
		names[i] = a.Name()
	}
	return names
}

// ciAnchors is the registered anchor names, derived from the registry so the enum
// can't drift from the bindings.
var ciAnchors = ciAnchorNames()

func validAnchor(a string) bool {
	_, ok := lookupAnchor(a)
	return ok
}

// core lifecycle phase node ids. converge is the bootstrap pivot every anchor is
// relative to; operate is the day-2 steady-state gate. Chained (operate needs
// converge). In production converge `uses:` llz-terraform.yml (the placeholder
// here runs `llz ci converge`).
const (
	convergeJob = "converge"
	operateJob  = "operate"
)

// coreSpine is the ordered set of phase nodes seeded into every graph. It is the
// ANCHORABLE subset of the delivery lifecycle (lifecyclePhases below): only phases
// that run as a job in THIS generated workflow can be a DAG node an extension
// needs:-attaches to. Bootstrap (the converge pivot) and Operate qualify; the
// other six methodology phases run in other engines (copier, the spec renderer,
// promote.yml, `llz upgrade`, humans) and are therefore not nodes here — the same
// granularity ceiling the spike found. Extend this only when a phase becomes a
// generated job in this workflow; the chain edge is wired in buildCIDAG.
var coreSpine = []*dagNode{
	{id: convergeJob, title: "converge (core bootstrap pivot)", core: true,
		run: []string{"llz", "ci", "converge"}, note: "placeholder; in prod: uses: …/llz-terraform.yml"},
	{id: operateJob, title: "operate (day-2 steady-state gate)", core: true,
		run: []string{"llz", "status", "--wait"}},
}

// lifecyclePhase is one stage of the LLZ delivery methodology
// (docs/delivery-methodology.md). The methodology has EIGHT phases across four
// execution engines; only the two that run as jobs in this CI workflow are
// Anchorable here. The rest are recorded so the whole lifecycle is visible in code
// and each phase points at the engine that owns ITS extensibility — the same
// split-by-engine conclusion the rest of the extension vehicle follows. `llz
// extension anchors` prints this.
type lifecyclePhase struct {
	Num        int
	Name       string
	Engine     string // who runs the phase
	Anchorable bool   // is it a generated job in THIS workflow's DAG?
	HookVia    string // where an extension attaches at this phase
}

var lifecyclePhases = []lifecyclePhase{
	{0, "Entitle", "external (accounts / InfoSec)", false, "n/a — precondition"},
	{1, "Scaffold", "copier + laptop (llz new)", false, "files: rendered into the instance (llz extension apply)"},
	{2, "Configure", "laptop (llz render)", false, "vars:/secrets: declared; doctor checks, seed wires (OpenBao/GH)"},
	{3, "Bootstrap", "GitHub Actions (llz-terraform.yml → converge)", true, "ci: anchor pre-converge | post-converge"},
	{4, "Operate", "GitHub Actions (scheduled) + the llz CLI", true, "ci: anchor operate; commands: operator CLI (ext.go registration)"},
	{5, "Promote", "GitHub Actions (promote.yml — a separate generated workflow)", false, "llz env pipeline"},
	{6, "Sustain", "laptop + Renovate (llz upgrade / drift)", false, "copier migrations (llz extension upgrade + wiring)"},
	{7, "Handover", "human", false, "docs"},
}

// anchorablePhases is the count of lifecycle phases that are nodes in this DAG;
// TestCoreSpineMatchesAnchorablePhases keeps it in lockstep with coreSpine.
func anchorablePhases() int {
	n := 0
	for _, p := range lifecyclePhases {
		if p.Anchorable {
			n++
		}
	}
	return n
}

// runExtensionAnchors prints the full delivery lifecycle, which phases are
// anchorable in this workflow's DAG, and where every other phase's extension hook
// lives — so the answer to "where can an extension tie in?" is one table.
func runExtensionAnchors() error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PHASE\tNAME\tENGINE\tDAG NODE HERE?\tEXTENSION HOOK")
	for _, p := range lifecyclePhases {
		here := "—"
		if p.Anchorable {
			here = "yes"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", p.Num, p.Name, p.Engine, here, p.HookVia)
	}
	tw.Flush()
	fmt.Fprintf(os.Stderr, "\nci: anchors usable in this workflow: %s\n", strings.Join(ciAnchors, ", "))
	fmt.Fprintln(os.Stderr, "(other phases run in other engines — attach via the hook in the last column)")
	return nil
}

// extCIJob is one extension ci: step, flattened from a manifest, ready to graph.
type extCIJob struct {
	Ext       string   // owning extension name
	Name      string   // step name
	Anchor    string   // one of ciAnchors
	Argv      []string // the command to run
	DependsOn []string // other job ids this must follow (an inter-job edge)
}

var reJobIDUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// ciJobID is the GitHub-Actions-safe job id for an ext:step pair.
func ciJobID(ext, name string) string {
	clean := func(s string) string {
		return strings.Trim(reJobIDUnsafe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	}
	return "x-" + clean(ext) + "-" + clean(name)
}

// buildCIDAG lowers the core spine + every extension job into the graph, turning
// anchors and dependsOn into edges. It validates anchors and dependsOn targets;
// cycle detection happens in topo().
func buildCIDAG(jobs []extCIJob) (*dag, error) {
	d := newDAG()
	for i, n := range coreSpine {
		d.addNode(n)
		if i > 0 {
			d.need(n.id, coreSpine[i-1].id) // operate needs converge (the chain)
		}
	}
	for _, j := range jobs {
		if !validAnchor(j.Anchor) {
			return nil, fmt.Errorf("%s: unknown anchor %q (want %s)", ciJobID(j.Ext, j.Name), j.Anchor, strings.Join(ciAnchors, " | "))
		}
		id := ciJobID(j.Ext, j.Name)
		d.addNode(&dagNode{id: id, title: fmt.Sprintf("%s:%s (%s)", j.Ext, j.Name, j.Anchor), run: j.Argv})
	}
	for _, j := range jobs {
		id := ciJobID(j.Ext, j.Name)
		if a, ok := lookupAnchor(j.Anchor); ok { // validated in the loop above
			a.Bind(d, id) // the anchor knows its own edge direction
		}
		for _, dep := range j.DependsOn {
			if _, ok := d.nodes[dep]; !ok {
				return nil, fmt.Errorf("%s: dependsOn unknown job %q", id, dep)
			}
			d.need(id, dep)
		}
	}
	return d, nil
}

// renderExtensionsWorkflow renders llz-extensions.yml from the job DAG. Pure (no
// I/O) and deterministic (insertion-order topo sort) so it unit-tests and feeds a
// stable --check drift gate, exactly like renderPromoteWorkflow.
func renderExtensionsWorkflow(jobs []extCIJob) (string, error) {
	d, err := buildCIDAG(jobs)
	if err != nil {
		return "", err
	}
	order, err := d.topo()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("# GENERATED from each enabled extension's ci: steps by `llz extension\n" +
		"# ci-workflow`. DO NOT EDIT BY HAND — re-run it after enabling/disabling an\n" +
		"# extension, and wire `llz extension ci-workflow --check` into CI to fail on\n" +
		"# drift. Like promote.yml, the runtime is 100% GitHub-native: the job graph is\n" +
		"# emitted in topological order and `needs:` is the on-green gate. `converge` is\n" +
		"# the core bootstrap pivot every anchor is relative to.\n\n" +
		"name: llz extensions\n\n" +
		"on:\n  workflow_dispatch: {}\n\n" +
		"permissions:\n  contents: read\n\n" +
		"jobs:\n")

	for i, id := range order {
		n := d.nodes[id]
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(fmt.Sprintf("  %s:\n", id))
		b.WriteString(fmt.Sprintf("    name: %s\n", n.title))
		if needs := d.needsOf(id); len(needs) > 0 {
			b.WriteString(fmt.Sprintf("    needs: [%s]\n", strings.Join(needs, ", ")))
		}
		b.WriteString("    runs-on: ubuntu-latest\n    steps:\n")
		b.WriteString("      - uses: actions/checkout@v4\n")
		runLine := "      - run: " + shellQuote(n.run)
		if n.note != "" {
			runLine += "   # " + n.note
		}
		b.WriteString(runLine + "\n")
	}
	return b.String(), nil
}

// ── loader + command (thin glue, mirrors syncPromoteWorkflow) ────────────────

// manifestCIJobs flattens one manifest's ci: steps into jobs. Steps default to
// the post-converge anchor — the safe "after the cluster is up" position.
func manifestCIJobs(m extManifest) []extCIJob {
	var jobs []extCIJob
	for i, s := range m.CI {
		anchor := s.Anchor
		if anchor == "" {
			anchor = anchorPostConverge
		}
		name := s.Name
		if name == "" {
			name = fmt.Sprintf("step%d", i)
		}
		jobs = append(jobs, extCIJob{Ext: m.Name, Name: name, Anchor: anchor, Argv: s.Argv, DependsOn: s.DependsOn})
	}
	return jobs
}

// loadExtensionCIJobs collects ci: steps across every extension subdir of dir
// (the one-off, explicit-dir path; the system path is enabledCIJobs).
func loadExtensionCIJobs(dir string) ([]extCIJob, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var jobs []extCIJob
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), extensionManifest))
		if err != nil {
			continue // not an extension dir
		}
		var m extManifest
		if err := yaml.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", e.Name(), extensionManifest, err)
		}
		jobs = append(jobs, manifestCIJobs(m)...)
	}
	return jobs, nil
}

const extensionsWorkflowPath = ".github/workflows/llz-extensions.yml"

// runExtensionCIWorkflow generates the workflow under root from the given jobs
// (sourced from an explicit dir or the enabled set by the caller).
func runExtensionCIWorkflow(g globalOpts, jobs []extCIJob, root string, check bool) error {
	out := filepath.Join(root, extensionsWorkflowPath)
	if len(jobs) == 0 {
		if _, statErr := os.Stat(out); statErr == nil { // workflow exists but nothing needs it → stale
			if check {
				return fmt.Errorf("%s is stale — no enabled ci: steps but the workflow exists; run `llz extension ci-workflow`", out)
			}
			if g.dryRun {
				fmt.Fprintf(os.Stderr, "(dry-run) would remove %s (no enabled ci: steps)\n", out)
				return nil
			}
			if err := os.Remove(out); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "%s: removed (no enabled ci: steps)\n", out)
			return nil
		}
		fmt.Fprintln(os.Stderr, "no extension ci: steps — nothing to generate")
		return nil
	}
	want, err := renderExtensionsWorkflow(jobs)
	if err != nil {
		return err
	}
	got, _ := os.ReadFile(out)
	if string(got) == want {
		fmt.Fprintf(os.Stderr, "%s: up to date (%d job(s))\n", out, len(jobs))
		return nil
	}
	if check {
		return fmt.Errorf("%s is out of date with the enabled extensions — run `llz extension ci-workflow`", out)
	}
	if g.dryRun {
		fmt.Fprintf(os.Stderr, "(dry-run) would write %s (%d job(s))\n", out, len(jobs))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(out, []byte(want), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s: regenerated (%d job(s))\n", out, len(jobs))
	return nil
}
