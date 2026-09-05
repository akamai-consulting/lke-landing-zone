package cli

// broadpat_workflow_scope_test.go — the SECOND place the broad PAT's scopes are
// written down, and the one TestBroadPATScopesSupersetInclusterPAT cannot see.
//
// credrotate.BroadPATScopes is the Go constant the in-cluster broadPatRotator
// mints with. The `create-linode-pat` job in the shipped instance-template
// workflow carries its own YAML literal for the same token — CI's path, used
// whenever broadPatRotator is not driving the rotation. Two sources, one
// meaning, and only one of them was under test: the literal had drifted to omit
// domains:read_write, which the in-cluster PAT requires, so on a
// broadPatRotator-disabled instance rotate-incluster-pat's mint would be
// rejected by Linode every month.
//
// A superset test that only reads the Go constant cannot catch that, so this
// reads the shipped YAML the way tfenc's gate reads its composite action.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
)

// workflowPath is the shipped rotation workflow whose scopes literal must agree
// with the Go constants.
const workflowPath = "instance-template/.github/workflows/llz-secret-rotation.yml"

// The gate must read the scope literal of the `create-linode-pat` step and no
// other. Two things make that harder than a regex:
//
//   - the workflow has TWO `operation: create` steps (the broad PAT and the
//     object-storage key), so anchoring on that string alone is not enough; and
//   - Go's RE2 has no lookahead, so a pattern cannot say "this `scopes:` with no
//     intervening `operation:`". A `(?s).*?` anchor spans the whole file and
//     only looks precise.
//
// So this scans instead: find each `scopes:` literal, walk BACK to the nearest
// `operation:` line, and keep the ones whose operation is `create`. That is
// exactly the association the gate claims to be testing, and it fails loudly if
// the file ever grows a second one rather than silently checking the wrong step.

// scopesLine matches a `scopes: "..."` key and captures its value.
var scopesLine = regexp.MustCompile(`^\s*scopes:\s*"([^"]+)"`)

// operationLine matches an `operation:` key and captures its value.
var operationLine = regexp.MustCompile(`^\s*operation:\s*(\S+)`)

// findScopes returns the scope literal belonging to the create step, failing
// rather than guessing if the file's shape has moved out from under it.
func findScopes(t *testing.T, b []byte) string {
	t.Helper()
	lines := strings.Split(string(b), "\n")

	var found []string
	var total int
	for i, ln := range lines {
		m := scopesLine.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		total++
		// Walk back to the operation this literal belongs to.
		op := ""
		for j := i - 1; j >= 0; j-- {
			if o := operationLine.FindStringSubmatch(lines[j]); o != nil {
				op = o[1]
				break
			}
		}
		if op == "create" {
			found = append(found, m[1])
		}
	}

	if total == 0 {
		t.Fatalf("no `scopes: \"...\"` literal in %s — the create-linode-pat step is where the "+
			"broad PAT's grant is declared; if it moved, point this gate at the new one", workflowPath)
	}
	if len(found) != 1 {
		t.Fatalf("%s has %d `scopes:` literal(s) under an `operation: create` step (%d overall), "+
			"expected exactly 1 — these gates assume one place declares the broad PAT's grant. "+
			"Check each literal explicitly rather than letting one go unchecked",
			workflowPath, len(found), total)
	}
	return found[0]
}

func workflowRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, workflowPath)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("%s not found above %s — this gate compares Go constants against the "+
				"SHIPPED workflow, so a moved or renamed workflow must fail here rather than "+
				"quietly stop being checked", workflowPath, dir)
		}
		dir = parent
	}
}

// TestWorkflowBroadPATScopesSupersetInclusterPAT asserts the workflow's own broad-PAT
// scopes literal covers every credrotate.InClusterPATScopes resource at >= its access.
// Same invariant as TestBroadPATScopesSupersetInclusterPAT, enforced against the
// other place the scope set is written down.
func TestWorkflowBroadPATScopesSupersetInclusterPAT(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(workflowRepoRoot(t), workflowPath))
	if err != nil {
		t.Fatal(err)
	}
	workflow := parseScopes(findScopes(t, b))
	for res, need := range parseScopes(credrotate.InClusterPATScopes) {
		got, ok := workflow[res]
		if !ok {
			t.Errorf("%s scopes literal is missing %q (the in-cluster PAT needs it) — "+
				"rotate-incluster-pat's mint 400s, because Linode refuses to create a token "+
				"with scopes greater than the requesting token's", workflowPath, res)
			continue
		}
		if got < need {
			t.Errorf("%s scopes literal has %q at level %d but the in-cluster PAT needs %d",
				workflowPath, res, got, need)
		}
	}
}

// TestWorkflowBroadPATScopesMatchConstant keeps the two broad-PAT declarations from
// drifting apart in either direction. They mint the same credential; a resource in
// one and not the other means the token you get depends on which path ran.
func TestWorkflowBroadPATScopesMatchConstant(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(workflowRepoRoot(t), workflowPath))
	if err != nil {
		t.Fatal(err)
	}
	workflow := parseScopes(findScopes(t, b))
	constant := parseScopes(credrotate.BroadPATScopes)
	for res, lvl := range constant {
		if got, ok := workflow[res]; !ok || got != lvl {
			t.Errorf("credrotate.BroadPATScopes has %q at level %d, %s has %v — the two broad-PAT "+
				"declarations must agree; whichever path mints the token should produce the same grant",
				res, lvl, workflowPath, workflow[res])
		}
	}
	for res, lvl := range workflow {
		if _, ok := constant[res]; !ok {
			t.Errorf("%s grants %q at level %d, credrotate.BroadPATScopes does not — the two "+
				"broad-PAT declarations must agree", workflowPath, res, lvl)
		}
	}
}
