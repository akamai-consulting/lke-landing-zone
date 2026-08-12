package branchpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// THE INCIDENT. `Deployment-branch-policies` — capital D — at all three call
// sites. GitHub's REST paths are case-sensitive, so every call answered 404 Not
// Found, and the 404 read as "the environment is missing" in a run that had just
// finished writing secrets to that same environment. It reached a live adopter.
func TestBranchPoliciesPathIsTheRouteGitHubActuallyServes(t *testing.T) {
	got := branchPoliciesPath("akamai/gsap-apl", "infra-prod")
	// Spelled out rather than assembled from the same pieces the function uses:
	// a test that rebuilds the path the way the code does agrees with the code by
	// construction, including when both are wrong.
	want := "repos/akamai/gsap-apl/environments/infra-prod/deployment-branch-policies"
	if got != want {
		t.Errorf("branchPoliciesPath =\n  %q\nwant\n  %q", got, want)
	}
}

// stringLiteral pulls every "..." out of a line.
var stringLiteral = regexp.MustCompile(`"([^"\\]*)"`)

// A CAPITALISED SEGMENT IS NOT A ROUTE. Resource names in GitHub's REST API are
// lowercase kebab-case, so an uppercase letter in a static path segment is a 404
// waiting for whoever runs it — and a 404 is the least diagnosable answer the API
// gives, because it is indistinguishable from "the thing you named is gone".
//
// SCOPED TO THE STATEMENT, NOT THE LITERAL, because the bug was never IN the
// `repos/...` literal. A REST path is built by concatenation —
// `"repos/"+repo+"/environments/"+envName+"/Deployment-branch-policies"` — so the
// misspelling lived in its own fragment that a scan anchored on `repos/` cannot
// see. The first cut of this test was anchored that way and stayed GREEN when the
// capital D was reintroduced: a guard for a bug it could not have caught.
//
// Widening it to every path-shaped literal instead flags seven honest ones
// (`/etc/harbor-admin/...`, `/Namespace`, `/Chart.yaml`), and a gate that cries
// wolf gets switched off. So: find lines that build a `repos/` path, then check
// EVERY literal on that line. Non-test sources only — fixtures carry `repos/o/r`,
// and owner/repo are case-insensitive to GitHub regardless.
func TestNoRESTPathStatementCarriesAnUppercaseSegment(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, `"repos/`) || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, m := range stringLiteral.FindAllStringSubmatch(line, -1) {
				lit := m[1]
				if strings.ContainsAny(lit, "/") && lit != strings.ToLower(lit) {
					offenders = append(offenders, fmt.Sprintf("%s:%d  %q", path, i+1, lit))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, o := range offenders {
		t.Errorf("a GitHub REST path is built with an uppercase segment — GitHub paths are "+
			"case-sensitive, so this 404s and the 404 reads as \"not found\" rather than "+
			"\"misspelled\":\n  %s", o)
	}
}
