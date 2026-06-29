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
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

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
	image     string // digest-pinned image → the job runs in `container:` (CI tool-supply)
	core      bool
}

// reImageDigest matches a digest-pinned image reference (…@sha256:<64 hex>).
var reImageDigest = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// validateCIImage requires a ci: step's image to be digest-pinned, not a mutable tag. A
// remote extension's CI image runs with the workflow's permissions, so it is trust
// surface; a digest pin makes it immutable and reviewable (like the source SHA pin),
// where `:latest` could be swapped under you after review.
func validateCIImage(jobID, image string) error {
	if image == "" {
		return nil
	}
	if !reImageDigest.MatchString(image) {
		return fmt.Errorf("%s: ci image %q must be digest-pinned (…@sha256:<64hex>), not a mutable tag", jobID, image)
	}
	return nil
}

// reWorkflowImageLine captures the value of an `image:` or scalar `container:` key in a
// workflow YAML (the container the job runs in). The block form `container:` with nothing
// after the colon yields no match (the nested `image:` line is matched instead).
var reWorkflowImageLine = regexp.MustCompile(`(?m)^\s*(?:image|container)\s*:\s*(\S.*?)\s*$`)

// reVarsRef matches a GitHub Actions variable reference `${{ vars.NAME }}` and captures NAME.
var reVarsRef = regexp.MustCompile(`\$\{\{\s*vars\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// isWorkflowPath reports whether dst is a GitHub Actions workflow file.
func isWorkflowPath(dst string) bool {
	return strings.Contains(dst, ".github/workflows/") && (strings.HasSuffix(dst, ".yml") || strings.HasSuffix(dst, ".yaml"))
}

// lintWorkflowImages closes the digest-pin gap for SCAFFOLDED workflows (the app-kit
// pattern): a ci: step's image: is pin-checked (validateCIImage), but a workflow shipped
// as a files: blob escaped that check entirely, yet it runs with the workflow's
// permissions just the same. For every files: entry that targets .github/workflows/**,
// this reads the source bytes and requires each `image:`/`container:` reference to be
// either digest-pinned (…@sha256:<64hex>) or a `${{ vars.NAME }}` whose NAME is declared
// in ghVars: (an operator-owned, reviewable, doctor-checked image source). A mutable
// `:latest` or an undeclared vars ref is a finding. Reads raw bytes (image refs are
// literal, never <@ @>-templated), so it needs no instance root.
func lintWorkflowImages(ext Extension) []string {
	declared := map[string]bool{}
	imageVar := map[string]bool{}
	for _, gv := range ext.Manifest.GHVars {
		declared[gv.Name] = true
		imageVar[gv.Name] = gv.Image
	}
	var findings []string
	scan := func(dst string, body []byte) {
		for _, m := range reWorkflowImageLine.FindAllSubmatch(body, -1) {
			val := strings.Trim(string(m[1]), `"'`)
			if val == "" || strings.HasPrefix(val, "{") || strings.HasPrefix(val, "#") {
				continue // flow-map container: {…} (image handled on its own line) or a comment
			}
			if vm := reVarsRef.FindStringSubmatch(val); vm != nil {
				switch {
				case !declared[vm[1]]:
					findings = append(findings, fmt.Sprintf("%s: image uses ${{ vars.%s }} but %s is not declared in ghVars: — declare it so it is doctor-checked and operator-owned", dst, vm[1], vm[1]))
				case !imageVar[vm[1]]:
					// declared, but not flagged as an image: its default/value isn't digest-checked.
					findings = append(findings, fmt.Sprintf("%s: image uses ${{ vars.%s }} but ghVar %s is not marked `image: true` — flag it so its default is digest-pin-enforced", dst, vm[1], vm[1]))
				}
				continue
			}
			if reImageDigest.MatchString(val) {
				continue
			}
			findings = append(findings, fmt.Sprintf("%s: workflow image %q must be digest-pinned (…@sha256:<64hex>) or a declared ghVars: image variable, not a mutable tag", dst, val))
		}
	}
	for _, f := range ext.Manifest.Files {
		info, err := fs.Stat(ext.fsys, f.Src)
		if err != nil {
			continue // a bad src is already reported by render/apply; not this check's job
		}
		if !info.IsDir() {
			if isWorkflowPath(f.Dst) {
				if b, rerr := fs.ReadFile(ext.fsys, f.Src); rerr == nil {
					scan(f.Dst, b)
				}
			}
			continue
		}
		_ = fs.WalkDir(ext.fsys, f.Src, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil || d.IsDir() {
				return werr
			}
			rel := strings.TrimPrefix(strings.TrimPrefix(p, f.Src), "/")
			dst := path.Join(f.Dst, rel)
			if isWorkflowPath(dst) {
				if b, rerr := fs.ReadFile(ext.fsys, p); rerr == nil {
					scan(dst, b)
				}
			}
			return nil
		})
	}
	return findings
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
	// Target is the core spine job id this anchor binds against — which must be the
	// CoreJobID of an anchorable lifecycle phase (TestAnchorTargets... enforces).
	Target() string
	// Bind installs the edge the anchor's semantics imply, between a core spine
	// node and the extension job.
	Bind(d *dag, jobID string)
}

// afterAnchor runs the job AFTER a core phase node (job needs phase).
type afterAnchor struct{ name, phase string }

func (a afterAnchor) Name() string            { return a.name }
func (a afterAnchor) Target() string          { return a.phase }
func (a afterAnchor) Bind(d *dag, job string) { d.need(job, a.phase) }

// beforeAnchor runs the job BEFORE a core phase node (phase needs job) — the
// bidirectional case, expressible only because llz generates the phase's caller
// job (you can order around a reusable-workflow call, never a step inside it).
type beforeAnchor struct{ name, phase string }

func (a beforeAnchor) Name() string            { return a.name }
func (a beforeAnchor) Target() string          { return a.phase }
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

// core lifecycle phase node ids. These MUST equal the CoreJobID of the matching
// anchorable phase in the lifecycle registry (lifecycle.go) — TestCoreJobSpecsMatch
// enforces it. converge is the bootstrap pivot every anchor is relative to; operate
// is the day-2 steady-state gate. In production converge `uses:` llz-terraform.yml
// (the placeholder here runs `llz ci converge`).
const (
	convergeJob = "converge"
	operateJob  = "operate"
)

// coreJobSpec is the CI-rendering specifics — job title, step argv, trailing comment
// — for an anchorable phase's generated job. The lifecycle registry owns WHICH phases
// are jobs (Anchorable + CoreJobID); this owns only HOW each renders, so the workflow
// codegen detail does not pull the lifecycle table back into this file.
type coreJobSpec struct {
	title string
	run   []string
	note  string
}

var coreJobSpecs = map[string]coreJobSpec{
	convergeJob: {title: "converge (core bootstrap pivot)",
		run: []string{"llz", "ci", "converge"}, note: "placeholder; in prod: uses: …/llz-terraform.yml"},
	operateJob: {title: "operate (day-2 steady-state gate)",
		run: []string{"llz", "status", "--wait"}},
}

// coreSpine is the ordered phase nodes seeded into every graph: the Anchorable subset
// of the lifecycle, DERIVED from the registry (anchorablePhases) so it cannot drift
// from it. Only phases that run as a job in THIS generated workflow can be a DAG node
// an extension needs:-attaches to; the chain edge between them is wired in buildCIDAG.
// To make a phase anchorable, set its CoreJobID in the registry and add a coreJobSpec
// — never edit a parallel list here.
var coreSpine = buildCoreSpine()

func buildCoreSpine() []*dagNode {
	var spine []*dagNode
	for _, p := range anchorablePhases() {
		spec := coreJobSpecs[p.CoreJobID] // keyed by job id; validated by TestCoreJobSpecsMatch
		spine = append(spine, &dagNode{
			id: p.CoreJobID, title: spec.title, core: true, run: spec.run, note: spec.note,
		})
	}
	return spine
}

// extCIJob is one extension ci: step, flattened from a manifest, ready to graph.
type extCIJob struct {
	Ext       string   // owning extension name
	Name      string   // step name
	Anchor    string   // one of ciAnchors (ignored when Schedule is set)
	Schedule  string   // a cron expr → TriggerSchedule (a standalone scheduled job, not converge-anchored)
	Image     string   // a digest-pinned image the job runs in (container:) — the CI tool-supply
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
		if err := validateCIImage(id, j.Image); err != nil {
			return nil, err
		}
		d.addNode(&dagNode{id: id, title: fmt.Sprintf("%s:%s (%s)", j.Ext, j.Name, j.Anchor), run: j.Argv, image: j.Image})
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
		b.WriteString("    runs-on: ubuntu-latest\n")
		if n.image != "" { // the CI tool-supply: the step's tools live in this digest-pinned image
			b.WriteString(fmt.Sprintf("    container: %s\n", n.image))
		}
		b.WriteString("    steps:\n")
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
		jobs = append(jobs, extCIJob{Ext: m.Name, Name: name, Anchor: anchor, Schedule: s.Schedule, Image: s.Image, Argv: s.Argv, DependsOn: s.DependsOn})
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

const (
	extensionsWorkflowPath          = ".github/workflows/llz-extensions.yml"
	extensionsScheduledWorkflowPath = ".github/workflows/llz-extensions-scheduled.yml"
)

// partitionCIJobs splits jobs by trigger: a Schedule (cron) makes a job a standalone
// scheduled job (TriggerSchedule); the rest are converge-anchored (TriggerConverge).
// The two cannot share a workflow — a converge-anchored job needs: the converge pivot,
// which never runs on a cron — so they emit two files.
func partitionCIJobs(jobs []extCIJob) (anchored, scheduled []extCIJob) {
	for _, j := range jobs {
		if j.Schedule != "" {
			scheduled = append(scheduled, j)
		} else {
			anchored = append(anchored, j)
		}
	}
	return anchored, scheduled
}

// buildScheduledDAG graphs the scheduled jobs alone — no core spine, no anchors (a
// scheduled job runs independently of converge). dependsOn may reference only another
// scheduled job (a cross-trigger edge can't be expressed in needs:).
func buildScheduledDAG(jobs []extCIJob) (*dag, error) {
	d := newDAG()
	for _, j := range jobs {
		id := ciJobID(j.Ext, j.Name)
		if err := validateCIImage(id, j.Image); err != nil {
			return nil, err
		}
		d.addNode(&dagNode{id: id, title: fmt.Sprintf("%s:%s (scheduled)", j.Ext, j.Name), run: j.Argv, image: j.Image})
	}
	for _, j := range jobs {
		id := ciJobID(j.Ext, j.Name)
		for _, dep := range j.DependsOn {
			if _, ok := d.nodes[dep]; !ok {
				return nil, fmt.Errorf("%s: dependsOn %q — a scheduled step may only depend on another scheduled step", id, dep)
			}
			d.need(id, dep)
		}
	}
	return d, nil
}

// renderScheduledWorkflow emits llz-extensions-scheduled.yml: an `on: schedule` workflow
// (the union of the steps' distinct crons, plus workflow_dispatch) with each scheduled
// step a job in dependency order. Pure + deterministic for the --check drift gate, like
// renderExtensionsWorkflow. This is the trigger axis the anchor model lacked — what
// scheduled-checks / cluster-health / a rotation cadence ride.
func renderScheduledWorkflow(jobs []extCIJob) (string, error) {
	d, err := buildScheduledDAG(jobs)
	if err != nil {
		return "", err
	}
	order, err := d.topo()
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	var crons []string
	for _, j := range jobs {
		if !seen[j.Schedule] {
			seen[j.Schedule] = true
			crons = append(crons, j.Schedule)
		}
	}
	sort.Strings(crons) // deterministic emission for the drift gate

	var b strings.Builder
	b.WriteString("# GENERATED from each enabled extension's scheduled ci: steps (steps with a\n" +
		"# `schedule:` cron) by `llz extension ci-workflow`. DO NOT EDIT BY HAND. Separate\n" +
		"# from llz-extensions.yml because a cron-triggered job cannot share a workflow with\n" +
		"# the converge-anchored DAG. `--check` is the drift gate.\n\n" +
		"name: llz extensions (scheduled)\n\n" +
		"on:\n  schedule:\n")
	for _, c := range crons {
		b.WriteString(fmt.Sprintf("    - cron: %q\n", c))
	}
	b.WriteString("  workflow_dispatch: {}\n\n" +
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
		b.WriteString("    runs-on: ubuntu-latest\n")
		if n.image != "" { // the CI tool-supply: the step's tools live in this digest-pinned image
			b.WriteString(fmt.Sprintf("    container: %s\n", n.image))
		}
		b.WriteString("    steps:\n")
		b.WriteString("      - uses: actions/checkout@v4\n")
		b.WriteString("      - run: " + shellQuote(n.run) + "\n")
	}
	return b.String(), nil
}

// syncWorkflowFile writes / checks / removes one generated workflow file: up-to-date is
// a no-op, --check fails on drift, an empty job set removes a stale file. Shared by the
// anchored and scheduled paths so the drift semantics are identical.
func syncWorkflowFile(g globalOpts, out string, jobs []extCIJob, render func([]extCIJob) (string, error), check bool) error {
	if len(jobs) == 0 {
		if _, statErr := os.Stat(out); statErr != nil {
			return nil // nothing to generate, nothing stale
		}
		if check {
			return fmt.Errorf("%s is stale — no matching ci: steps but the workflow exists; run `llz extension ci-workflow`", out)
		}
		if g.dryRun {
			fmt.Fprintf(os.Stderr, "(dry-run) would remove %s (no matching ci: steps)\n", out)
			return nil
		}
		if err := os.Remove(out); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s: removed (no matching ci: steps)\n", out)
		return nil
	}
	want, err := render(jobs)
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

// runExtensionCIWorkflow generates BOTH the converge-anchored workflow and the scheduled
// workflow from the given jobs, partitioned by trigger. Either file is removed when its
// job set is empty, so disabling the last scheduled step cleans up its workflow.
func runExtensionCIWorkflow(g globalOpts, jobs []extCIJob, root string, check bool) error {
	anchored, scheduled := partitionCIJobs(jobs)
	if err := syncWorkflowFile(g, filepath.Join(root, extensionsWorkflowPath), anchored, renderExtensionsWorkflow, check); err != nil {
		return err
	}
	return syncWorkflowFile(g, filepath.Join(root, extensionsScheduledWorkflowPath), scheduled, renderScheduledWorkflow, check)
}
