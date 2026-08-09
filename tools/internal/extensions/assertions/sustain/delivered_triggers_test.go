package sustain

// delivered_triggers_test.go — a delivered caller stub must be TRIGGERED BY the
// pipeline it calls.
//
// ────────────────────────────────────────────────────────────────────────────
// instance-template/.github/workflows/ IS TWO KINDS OF FILE. The `llz-*.yml`
// bodies are `managed` reusables carrying the whole pipeline; the unprefixed
// stubs are `merge`, owning only the trigger surface an operator may tune. A stub
// therefore decides WHEN a body runs, and the two are separate files.
//
// terraform.yml listed `.github/workflows/terraform.yml` in its own `paths:` and
// not `llz-terraform.yml` — it watched the 90-line caller and ignored the ~1300
// lines that plan and apply, plus the composite actions those drive. A change to
// the pipeline produced no run of the pipeline.
//
// THE CASE THAT MATTERS IS `copier update`. The body and .github/actions/** are
// both `managed`, so they change on exactly the upgrades an operator most wants a
// plan for, and the instance's PR showed none. The stub's own header describes the
// split at length, so the omission was a listing that stopped one file short.
//
// This is the delivered-surface instance of a defect the template repo hit twice
// (lint.yml missing platform-apl/**; `make lint LINT_ALL=1` missing the gate
// suite). The template repo now has coupling tests for its own CI. An adopter's
// instance has none — this is that guard, run here so it holds before the files
// ship rather than after.
//
// SCOPE, STATED. Only stubs that actually FILTER are checked: a workflow with no
// `paths:` runs on every event of its type, which is the safe direction and needs
// nothing from this test. Dependencies are followed ONE level — the stub's own
// `uses: ./…` plus those of any local reusable it calls — because that is the
// depth the delivered tree has, and inventing a general resolver for a shape that
// does not exist would be the speculative widening this repo's earned-row rule
// refuses.
// ────────────────────────────────────────────────────────────────────────────

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const deliveredWorkflows = "../../../../../instance-template/.github/workflows"

// localUses finds `uses: ./<path>` references — a local reusable workflow or a
// vendored composite action, both of which decide what the caller does.
var localUses = regexp.MustCompile(`uses:\s*\./(\.github/(?:workflows/[A-Za-z0-9._-]+\.ya?ml|actions/[A-Za-z0-9._-]+))`)

// pathsBlock captures one `paths:` list: the entries are the following lines that
// are `- '…'` items, stopping at the first line that is not one.
func pathsOf(body string) []string {
	var out []string
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "paths:" {
			continue
		}
		for _, m := range lines[i+1:] {
			t := strings.TrimSpace(m)
			if t == "" || strings.HasPrefix(t, "#") {
				continue // blank and comment lines do not end the list
			}
			if !strings.HasPrefix(t, "- ") {
				break
			}
			out = append(out, strings.Trim(strings.TrimPrefix(t, "- "), "'\""))
		}
	}
	return out
}

// covers implements the subset of GitHub's path matching these filters use:
// exact paths, `**` (crosses directory separators) and `*` (does not).
//
// `*` IS NOT A LUXURY HERE. The first cut handled only exact matches and `dir/**`,
// which was enough for the filter as written — and the fix it drove was to replace
// three individually-named reusables with `.github/workflows/llz-*.yml`, because
// naming them one at a time is exactly what left the filter a file short twice. A
// matcher that cannot read the pattern the fix needs would have forced the rot-prone
// spelling to stay.
//
// The two wildcards differ in whether they cross `/`, which is GitHub's rule and
// the reason `**/*.md` misses root-level files while `**.md` does not — a trap this
// repo has already been bitten by, recorded in lint.yml's own filter comment.
func covers(pattern, path string) bool {
	var re strings.Builder
	re.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch {
		case strings.HasPrefix(pattern[i:], "**"):
			re.WriteString(".*")
			i++ // consume the second star
		case pattern[i] == '*':
			re.WriteString("[^/]*")
		default:
			re.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	re.WriteString("$")
	m, err := regexp.Compile(re.String())
	if err != nil {
		return false
	}
	return m.MatchString(path)
}

func TestDeliveredStubsAreTriggeredByWhatTheyCall(t *testing.T) {
	dir := filepath.FromSlash(deliveredWorkflows)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v — the delivered workflows are this test's subject", dir, err)
	}

	bodies := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		bodies[e.Name()] = string(b)
	}
	if len(bodies) == 0 {
		t.Fatal("no delivered workflow files found — the tree moved and this guard reads nothing")
	}

	// The caller/callee shape must exist at all, or the scan is measuring nothing.
	var anyLocalUses bool
	for _, b := range bodies {
		if localUses.MatchString(b) {
			anyLocalUses = true
			break
		}
	}
	if !anyLocalUses {
		t.Fatal("no `uses: ./.github/…` reference in any delivered workflow — the stub/reusable " +
			"split this checks is gone, or the pattern drifted from the YAML; either way every " +
			"stub below would read as depending on nothing")
	}

	var checked int
	for name, body := range bodies {
		filters := pathsOf(body)
		if len(filters) == 0 {
			continue // runs on every event of its type: the safe direction
		}
		checked++

		// Direct dependencies, plus those of any local reusable it calls.
		deps := map[string]bool{}
		var collect func(string, int)
		collect = func(src string, depth int) {
			for _, m := range localUses.FindAllStringSubmatch(src, -1) {
				deps[m[1]] = true
				callee := strings.TrimPrefix(m[1], ".github/workflows/")
				if depth > 0 && callee != m[1] {
					if b, ok := bodies[callee]; ok {
						collect(b, depth-1)
					}
				}
			}
		}
		collect(body, 1)

		var uncovered []string
		for dep := range deps {
			var ok bool
			for _, f := range filters {
				if covers(f, dep) {
					ok = true
					break
				}
			}
			if !ok {
				uncovered = append(uncovered, dep)
			}
		}
		sort.Strings(uncovered)
		if len(uncovered) > 0 {
			t.Errorf("%s filters on %v and invokes %v, which no filter covers.\n"+
				"\tThis file is VENDORED into every adopter's repo, and a stub's `paths:` is the "+
				"only thing deciding when the pipeline it calls runs. The reusable bodies and "+
				"vendored actions are `managed`, so they change on `copier update` — precisely the "+
				"PR an operator wants the pipeline to run against. Add the path, or drop the "+
				"`paths:` filter so it always runs.",
				name, filters, uncovered)
		}
	}
	t.Logf("checked %d path-filtered delivered stub(s) against what they invoke", checked)
}

// THE MATCHER IS THE GUARD, so it is tested against cases rather than trusted.
//
// If covers() were wrong the check above would still pass — it would compare a
// pattern against a path using a rule GitHub does not have, and report clean. That
// is the shared-assumption failure the docs work already paid for once, where a
// generator and a checker each carried the same wrong copy of an anchor rule and
// agreed with each other.
//
// The `**/*.md` vs `**.md` row is the one this repo has actually been bitten by:
// `**` crosses `/` and `*` does not, so `**/*.md` REQUIRES a separator and misses
// every root-level file. lint.yml's filter carries the same note.
func TestCoversMatchesGitHubsPathSemantics(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
		why           string
	}{
		{".github/workflows/terraform.yml", ".github/workflows/terraform.yml", true, "exact"},
		{".github/workflows/terraform.yml", ".github/workflows/llz-terraform.yml", false, "exact must not prefix-match"},
		{".github/actions/**", ".github/actions/terraform-init", true, "** covers a child"},
		{".github/actions/**", ".github/actions/a/b/action.yml", true, "** crosses separators"},
		{".github/actions/**", ".github/workflows/x.yml", false, "** is still anchored"},
		{".github/workflows/llz-*.yml", ".github/workflows/llz-terraform.yml", true, "* within a segment"},
		{".github/workflows/llz-*.yml", ".github/workflows/terraform.yml", false, "* must not match the missing prefix"},
		{".github/workflows/llz-*.yml", ".github/workflows/sub/llz-x.yml", false, "* must NOT cross a separator"},
		{"**.md", "README.md", true, "**.md catches a root-level file"},
		{"**/*.md", "README.md", false, "**/*.md requires a separator — the trap"},
		{"**/*.md", "docs/README.md", true, "**/*.md matches nested"},
		{"terraform-iac-bootstrap/**", "terraform-iac-bootstrap/cluster/main.tf", true, "tree prefix"},
	}
	for _, c := range cases {
		if got := covers(c.pattern, c.path); got != c.want {
			t.Errorf("covers(%q, %q) = %v, want %v — %s", c.pattern, c.path, got, c.want, c.why)
		}
	}
}
