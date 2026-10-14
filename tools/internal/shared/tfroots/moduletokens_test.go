package tfroots

// moduletokens_test.go — every `git::` module source in an embedded root must
// carry BOTH copier tokens.
//
// THE PROPERTY, NOT THE COUNT, IS THE POINT. A module source is written
//
//	git::ssh://git@github.com/<@ upstream_org @>/…//terraform-modules/llz-x?ref=<@ llz_version @>
//
// and the `?ref=` is the whole umbrella-tag contract: it is what pins the module
// to the same release as the scaffold that rendered it. Drop the token and the
// line is STILL VALID HCL and still a working source — Terraform just resolves
// the repository's default branch instead. So the instance silently tracks main
// while `.copier-answers.yml` says it is on vX.Y.Z, every `terraform init`
// succeeds, and nothing anywhere reports a version. That is the failure mode this
// pins: not a broken render, a render that works and is pinned to the wrong thing.
//
// Nothing else could see it. Substitute() is a string replace, so a root with no
// token is a no-op it reports success for; the render engine writes whatever it
// is handed; and `tofu validate` is happy either way. The tokens are only
// checkable HERE, against the embed, which is why the test lives beside it.
//
// It also pins the census in tfroots.go's header, which had already gone stale
// ("across the three roots" while there were four).

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// reGitSource matches a module `source =` line using a git:: reference.
var reGitSource = regexp.MustCompile(`(?m)^\s*source\s*=\s*"git::`)

// rootDirs returns the embedded root directory names (cluster, vpc, …).
func rootDirs(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(embedded, "roots")
	if err != nil {
		t.Fatalf("reading embedded roots: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func TestModuleSourcesCarryBothTokens(t *testing.T) {
	// Fail closed: an embed that produced no .tf files would make every loop below
	// vacuous, and "0 sources, all correct" is what a broken embed looks like.
	if len(tfRel) == 0 {
		t.Fatal("no embedded *.tf files — refusing to pass vacuously")
	}

	var orgTokens, refTokens, gitSources int
	rootsWithTokens := map[string]bool{}

	for _, rel := range tfRel {
		raw, err := embedded.ReadFile("roots/" + rel)
		if err != nil {
			t.Fatalf("reading roots/%s: %v", rel, err)
		}
		root := strings.SplitN(rel, "/", 2)[0]

		for _, line := range strings.Split(string(raw), "\n") {
			if !reGitSource.MatchString(line) {
				continue
			}
			gitSources++
			// A git:: source without the ref token resolves to the default branch:
			// the instance tracks main while claiming to be on its pinned release.
			if !strings.Contains(line, tokLLZVersion) {
				t.Errorf("roots/%s: git:: module source has no %s — it would resolve to the "+
					"default branch, so the instance tracks main while .copier-answers.yml says "+
					"otherwise:\n  %s", rel, tokLLZVersion, strings.TrimSpace(line))
			}
			// Without the org token the source names a hardcoded org, which breaks
			// every adopter fork — the no-org-identity-hardcoding rule in AGENTS.md.
			if !strings.Contains(line, tokUpstreamOrg) {
				t.Errorf("roots/%s: git:: module source has no %s — it hardcodes an org and "+
					"breaks every fork:\n  %s", rel, tokUpstreamOrg, strings.TrimSpace(line))
			}
			rootsWithTokens[root] = true
		}
		orgTokens += strings.Count(string(raw), tokUpstreamOrg)
		refTokens += strings.Count(string(raw), tokLLZVersion)
	}

	if gitSources == 0 {
		t.Fatal("no git:: module sources found in the embedded roots — the pinning contract " +
			"this test exists for would be unmeasurable, so this is a failure, not a pass")
	}

	// The census tfroots.go's header states. Bumping these when a root is added is
	// expected; updating that comment in the same commit is the point — it said
	// "the three roots" through the whole life of the fourth.
	const wantOrg, wantRef, wantRoots, wantTokenRoots = 3, 3, 4, 3
	if orgTokens != wantOrg || refTokens != wantRef {
		t.Errorf("embedded roots carry %d upstream_org / %d llz_version tokens; tfroots.go's "+
			"header says %d and %d. Update that comment in this commit.",
			orgTokens, refTokens, wantOrg, wantRef)
	}
	if got := len(rootDirs(t)); got != wantRoots {
		t.Errorf("%d embedded roots; tfroots.go's header says %d. Same rule — and check "+
			"instance-template/terraform-iac-bootstrap/AGENTS.md, which enumerates them for "+
			"adopters and has already been wrong about this.", got, wantRoots)
	}
	if got := len(rootsWithTokens); got != wantTokenRoots {
		t.Errorf("%d roots carry module tokens; the header says %d", got, wantTokenRoots)
	}
}
