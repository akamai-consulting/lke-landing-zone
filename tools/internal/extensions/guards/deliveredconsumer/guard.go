package deliveredconsumer

// guard.go implements `llz ci delivered-consumer-guard` — every file this
// template DELIVERS as `managed` must name something that consumes it, and that
// consumer must still exist.
//
// THE WEDGE IT COMES FROM. `apl-values/values.yaml` was a 425-line `managed` file
// shipped to every instance and re-delivered by every `llz upgrade`. Its consumer
// was the clusterspec values-render pipeline, RETIRED when LLZ moved to the
// managed App Platform — Linode owns apl-core's values there, so nothing renders
// a per-env values.yaml any more (clusterspec/values.go records what went). The
// renderer went; the delivered file stayed.
//
// For the next year that file was, from an adopter's point of view,
// indistinguishable from a live one: it shipped, `llz upgrade` overwrote local
// edits to it, four docs sent operators there to change things, and its own header
// said `llz render` consumed it. An instance carried a 3Gi Loki WAL-replay
// override in it, believed it applied, and ran a 1Gi ingester in an OOM crashloop
// for 16 days with log ingestion down.
//
// WHY NOTHING CAUGHT IT, AND WHAT THIS CAN HONESTLY CHECK. No static analysis can
// decide "this file reaches a cluster" — the delivered surface is data, and its
// consumers are spread across renderers, CI verbs, external tools and human
// procedure. What IS decidable is the relation between two things this repo
// already writes down: the manifest says a file is delivered, and this registry
// says who reads it. Removing a consumer then becomes a RED GATE rather than
// silence, which is the entire difference between the two outcomes above.
//
// So the guard is deliberately not clever. It asserts two things:
//
//  1. COMPLETENESS — every `managed` entry in .template-manifest has a row here.
//     Adding a delivered file forces the author to answer "who reads this?", which
//     is the question nobody asked about values.yaml for a year.
//  2. LIVENESS — every row whose consumer is a code symbol or a repo path must
//     name one that STILL EXISTS. This is the arm that fires on the retirement:
//     delete RenderValues, and the row pointing at it goes red in the same commit.
//
// Rows whose consumer is a human (docs, README) are unchecked by construction and
// say so. That is a real limit, stated rather than papered over: this guard cannot
// tell a doc someone reads from a doc nobody has opened in two years. It is aimed
// at the machine-consumed half, which is where the silent failure lives.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/guardkit"
)

// ConsumerKind says what sort of thing reads a delivered file, which decides
// whether the guard can check it.
type ConsumerKind int

const (
	// ConsumerSymbol — a Go symbol in tools/ produces or reads the file. CHECKED:
	// the symbol must still be defined somewhere under tools/.
	ConsumerSymbol ConsumerKind = iota
	// ConsumerPath — a path in this repo (a workflow, a script, a Makefile target)
	// reads it. CHECKED: the path must still exist.
	ConsumerPath
	// ConsumerHuman — an operator reads it. NOT CHECKED, and cannot be: there is
	// no signal in the tree for whether a human still reads a page. Rows carry a
	// reason so the choice is visible rather than a default.
	ConsumerHuman
	// ConsumerExternal — an external tool reads it by convention at a fixed
	// filename (tflint, checkov, gitleaks). NOT CHECKED for the same reason: the
	// consumer is not in this repo. The reason names the tool.
	ConsumerExternal
)

// Consumer is one answer to "who reads this delivered file?".
type Consumer struct {
	Kind ConsumerKind
	// Ref is the Go symbol (ConsumerSymbol) or repo-relative path (ConsumerPath)
	// that must still exist. Empty for the unchecked kinds.
	Ref string
	// Why is the one-line justification, and it is required for every row: a
	// registry of names with no reasons is a list nobody can argue with.
	Why string
}

// Consumers maps each `managed` .template-manifest entry to what reads it.
//
// KEYED BY THE MANIFEST ENTRY VERBATIM, globs included, so a manifest edit that
// changes a pattern shows up here as a missing row rather than silently matching
// a different set of files.
var Consumers = map[string]Consumer{
	// The three template-mechanism files. Each names the DECLARATION that reads
	// it, not the verb an operator types: a verb is a string that can be renamed
	// without the code moving, and the code moving is what this guard watches.
	// QUALIFIED, pkg.Name. An earlier revision of this row said bare `Classify`
	// and passed — on `baoread.Classify`, which parses OpenBao stderr and has
	// nothing to do with the manifest. The symbol the row meant did not exist
	// under that name anywhere. That is this guard failing at its own job, and the
	// qualified form is what stops it: `manifest.Classify` would not have resolved.
	".template-manifest": {ConsumerSymbol, "manifest.LoadFrom",
		"the manifest is its own consumer — `llz upgrade` classifies every scaffold file by reading it"},
	".template-removals": {ConsumerSymbol, "sustain.ApplyTemplateRemovals",
		"`llz upgrade` deletes the paths it lists from an instance"},
	".template-managed.lock": {ConsumerSymbol, "deps.ManagedFreshCmd",
		"`llz ci managed-fresh` writes it and every instance's upgrade verifies against it"},
	".github/actions/**": {ConsumerPath, "instance-template/.github/workflows",
		"the delivered workflows call these composites by ./.github/actions/<name>"},
	".tflintrc.hcl":  {ConsumerExternal, "", "tflint reads it by fixed filename; `make tf-lint` runs it"},
	".checkov.yaml":  {ConsumerExternal, "", "checkov reads it by fixed filename; the delivered terraform workflow runs it"},
	".gitleaks.toml": {ConsumerExternal, "", "gitleaks reads it by fixed filename in the delivered scan job"},
	".claude/settings.json": {ConsumerExternal, "",
		"Claude Code reads it by fixed path; it configures the agent in an adopter's checkout"},
	".gitignore": {ConsumerExternal, "", "git reads it by fixed filename"},
	"landingzone.yaml.example": {ConsumerSymbol, "clusterspec.LoadInstance",
		"the worked example an operator copies to landingzone.yaml, which LoadInstance then reads"},
	"environments/*.yaml.example": {ConsumerSymbol, "clusterspec.LoadSplit",
		"the per-env worked example; LoadSplit assembles environments/*.yaml from the same shape"},
	"docs/**":   {ConsumerHuman, "", "the delivered operator docs — runbooks and playbooks read during an incident"},
	"README.md": {ConsumerHuman, "", "the instance's front door"},
	"AGENTS.md": {ConsumerHuman, "", "instructions for agents working in an adopter's checkout"},
	"terraform-iac-bootstrap/AGENTS.md": {ConsumerHuman, "",
		"the same, scoped to the terraform tree"},
	"terraform-iac-bootstrap/.gitignore": {ConsumerExternal, "",
		"git reads it by fixed filename; it ignores the per-env tfvars `llz render` regenerates"},
	"apl-values/README.md": {ConsumerHuman, "",
		"explains what LLZ owns in the apl-values tree and what Linode owns"},
	".github/workflows/llz-*.yml": {ConsumerExternal, "",
		"GitHub Actions runs them; the delivered caller stubs invoke them as reusable workflows"},
	"apl-values/_shared/apl-overlay/**": {ConsumerSymbol, "clusterspec.RenderAppValuesOverlayShared",
		"the apl-overlay reconciler merges this tree onto apl-core's machine branch; the shared " +
			"base is held byte-identical to its renderers by TestTemplateSharedOverlayMatchesRenderers"},
}

// Run is the gate. root is the repository root.
func Run(root string) error {
	repo := capability.RepoForGate(Extension(), root)
	manifestPath := guardkit.RepoPath(repo, filepath.Join("instance-template", ".template-manifest"))
	raw, err := repo.ReadFile(manifestPath)
	if err != nil {
		// UNREADABLE IS A FAILURE, not an empty manifest. An empty manifest has
		// zero managed entries, which this guard would otherwise call a clean pass
		// — the exact "examined nothing" green it exists to prevent.
		return fmt.Errorf("read %s: %w", manifestPath, err)
	}

	entries := ManagedEntries(string(raw))
	if len(entries) == 0 {
		return fmt.Errorf("delivered-consumer-guard: %s declared ZERO managed files. That is not a "+
			"clean pass — it is a manifest this guard could not read, and every delivered file is "+
			"unchecked", manifestPath)
	}

	symbols, err := repoSymbolCorpus(repo)
	if err != nil {
		return err
	}
	if len(symbols) == 0 {
		return fmt.Errorf("delivered-consumer-guard: read no Go source under tools/, so every " +
			"ConsumerSymbol row would report its symbol missing. Refusing to render that as findings")
	}

	var problems []string
	for _, e := range entries {
		c, ok := Consumers[e]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"::error file=%s::`managed %s` is delivered to every instance and no row in "+
					"deliveredconsumer.Consumers says what reads it. Add one naming the consumer. "+
					"A delivered file whose consumer nobody can name is how apl-values/values.yaml "+
					"shipped for a year after its renderer was retired.", manifestPath, e))
			continue
		}
		if strings.TrimSpace(c.Why) == "" {
			problems = append(problems, fmt.Sprintf(
				"::error file=%s::`managed %s` has a Consumers row with no reason. A registry of "+
					"names with no reasons is a list nobody can argue with.", manifestPath, e))
			continue
		}
		switch c.Kind {
		case ConsumerSymbol:
			if !symbols[c.Ref] {
				problems = append(problems, fmt.Sprintf(
					"::error file=%s::`managed %s` names %q as its consumer, and that symbol no "+
						"longer appears anywhere under tools/. Either the consumer was retired and "+
						"this file should stop being delivered, or it was renamed and this row is "+
						"stale. THIS IS THE apl-values/values.yaml CASE: its renderer was deleted, "+
						"the file kept shipping, and an instance's Loki fix silently did nothing "+
						"for 16 days.", manifestPath, e, c.Ref))
			}
		case ConsumerPath:
			if _, serr := repo.Stat(guardkit.RepoPath(repo, c.Ref)); serr != nil {
				problems = append(problems, fmt.Sprintf(
					"::error file=%s::`managed %s` names the path %q as its consumer, and that path "+
						"does not exist.", manifestPath, e, c.Ref))
			}
		}
	}

	sort.Strings(problems)
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		return fmt.Errorf("delivered-consumer-guard: %d delivered file(s) have no live consumer", len(problems))
	}
	fmt.Printf("delivered-consumer-guard: %d managed file(s) delivered, every one with a named consumer that still exists.\n", len(entries))
	return nil
}

// ManagedEntries returns the path patterns classified `managed` in a
// .template-manifest, in file order. Comments and blank lines are skipped; a line
// whose class is not `managed` is not this guard's business.
//
// PURE, so the parsing is testable without a repo — and so a manifest format
// change fails in a unit test rather than as a mysterious empty corpus.
func ManagedEntries(manifest string) []string {
	var out []string
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "managed" {
			continue
		}
		out = append(out, fields[1])
	}
	return out
}
