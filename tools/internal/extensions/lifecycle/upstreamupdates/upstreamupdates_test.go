package upstreamupdates

import (
	"strings"
	"testing"
)

// ── Classify ────────────────────────────────────────────────────────────────

func TestClassifyMatchesSubtreesAndWholePathsDifferently(t *testing.T) {
	// The distinction is load-bearing: "environments/" is a tree, while
	// "landingzone.yaml" is one file. A plain HasPrefix would let
	// landingzone.yaml.example — a template artifact no plan reads — select the
	// state-writing import.
	prefixes := []string{"terraform-iac-bootstrap/", "landingzone.yaml", "environments/"}

	for _, tc := range []struct {
		name  string
		files []string
		want  bool
	}{
		{"tf root", []string{"terraform-iac-bootstrap/cluster/main.tf"}, true},
		{"spec file, exact", []string{"landingzone.yaml"}, true},
		{"env subtree", []string{"environments/primary.yaml"}, true},
		{"example is NOT the spec", []string{"landingzone.yaml.example"}, false},
		{"workflow-only upgrade PR", []string{".github/workflows/llz-terraform.yml", ".copier-answers.yml"}, false},
		{"docs only", []string{"docs/quickstart.md"}, false},
		{"mixed selects", []string{"README.md", "environments/lab.yaml"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Classify(tc.files, prefixes)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Touches != tc.want {
				t.Errorf("Touches = %v, want %v (matched %v)", c.Touches, tc.want, c.Matched)
			}
		})
	}
}

func TestClassifyFailsClosedOnAnEmptyFileList(t *testing.T) {
	// A PR always changes at least one file. An empty list means the query broke,
	// and answering "touches nothing" for it would skip the import on every PR
	// while looking exactly like a clean tree.
	if _, err := Classify(nil, []string{"terraform-iac-bootstrap/"}); err == nil {
		t.Error("an empty file list must be an error, not a `false`")
	}
}

func TestClassifyFailsClosedOnNoPrefixes(t *testing.T) {
	// A gate that always answers the same way is not a gate.
	if _, err := Classify([]string{"a.tf"}, nil); err == nil {
		t.Error("no prefixes must be an error rather than a permanent `false`")
	}
}

func TestClassifyReportNamesWhatMatched(t *testing.T) {
	c, err := Classify([]string{"README.md", "environments/lab.yaml"}, []string{"environments/"})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	c.Report(&b)
	if !strings.Contains(b.String(), "environments/lab.yaml") {
		t.Errorf("report must name the file that selected the import, got: %s", b.String())
	}
}

func TestClassifyReportSaysWhatSkippingCosts(t *testing.T) {
	// The skipped import means the plan can show a pre-existing VPC as "to be
	// created". A reader who is not told that reads it as a real diff.
	c, err := Classify([]string{"docs/x.md"}, []string{"environments/"})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	c.Report(&b)
	if !strings.Contains(b.String(), "to be created") {
		t.Errorf("report must warn that the plan may show a pre-existing resource as new, got: %s", b.String())
	}
}

// ── Decide ──────────────────────────────────────────────────────────────────

func TestDecideOpensAPROnlyWhenHEADMoved(t *testing.T) {
	// `llz upgrade` exits 0 whether or not it changed anything, so the commit is
	// the only honest signal. Treating "already current" as an upgrade would open
	// an empty PR every month.
	same := Decide(State{BeforeSHA: "abc", AfterSHA: "abc", Version: "v1.2.3"})
	if same.OpenPR {
		t.Error("an unchanged HEAD must not open a pull request")
	}
	if !strings.Contains(same.Reason, "already on the target release") {
		t.Errorf("reason must say the instance was already current, got %q", same.Reason)
	}

	moved := Decide(State{BeforeSHA: "abc", AfterSHA: "def", Version: "v1.2.3"})
	if !moved.OpenPR {
		t.Error("a moved HEAD must open a pull request")
	}
	if moved.Branch != "chore/template-upgrade-v1.2.3" {
		t.Errorf("branch = %q", moved.Branch)
	}
}

func TestDecideRefusesToClobberAnOpenUpgradePR(t *testing.T) {
	// The branch already on the remote is an earlier run's unmerged PR. Pushing
	// over it would replace a diff someone may be halfway through reviewing, with
	// nothing in the PR saying it changed underneath them.
	d := Decide(State{BeforeSHA: "abc", AfterSHA: "def", Version: "v1.2.3", RemoteHas: true})
	if d.OpenPR {
		t.Error("an existing remote branch must not be force-pushed")
	}
	if !strings.Contains(d.Reason, "not been merged") {
		t.Errorf("reason must explain the branch is an unmerged earlier run, got %q", d.Reason)
	}
}

func TestDecideWarnsAboutResidueWithoutDiscardingTheUpgrade(t *testing.T) {
	// A dirty tree after a committing upgrade means files changed that the commit
	// did not capture. Refusing the PR would discard a good upgrade over residue;
	// staying silent would ship a PR whose diff is not the whole change.
	d := Decide(State{BeforeSHA: "abc", AfterSHA: "def", Version: "v1", Dirty: " M apl-values/x.yaml"})
	if !d.OpenPR {
		t.Error("residue must not block an otherwise good upgrade")
	}
	if !strings.Contains(d.Warning, "NOT in this PR") {
		t.Errorf("residue must be reported as excluded, got %q", d.Warning)
	}
	if !strings.Contains(d.Summary(State{Dirty: " M apl-values/x.yaml", Version: "v1"}), "Uncommitted residue") {
		t.Error("the step summary must carry the residue warning too")
	}
}

func TestBranchNameIsARefAndCarriesTheVersion(t *testing.T) {
	// Version-named so a later release opens its own PR instead of retargeting an
	// older open one. Sanitised because a slash would nest the ref and collide.
	for in, want := range map[string]string{
		"v1.2.3":      "chore/template-upgrade-v1.2.3",
		" v1.2.3\n":   "chore/template-upgrade-v1.2.3",
		"":            "chore/template-upgrade-unknown",
		"feat/x v2":   "chore/template-upgrade-feat-xv2",
		"release/9.9": "chore/template-upgrade-release-9.9",
	} {
		if got := BranchName(in); got != want {
			t.Errorf("BranchName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPRBodyCarriesNoBacktickedCLICommand(t *testing.T) {
	// The body used to live in a workflow heredoc, where
	// TestDeliveredWorkflowCommands resolves every CLI invocation in a `run:`
	// script against the real cobra tree — prose included — so a backticked
	// command tokenised with its closing backtick and failed to resolve. It lives
	// in Go now, but the body is still copied into workflows by people who reach
	// for the nearest example, so the shape is pinned here.
	if strings.Contains(prBody, "`llz ") {
		t.Error("prBody contains a backticked llz command; use a fenced block so it tokenises clean " +
			"if this text is ever moved back into a run: script")
	}
	if !strings.Contains(prBody, "repo-readiness") {
		t.Error("the body must tell a reviewer which check actually gates the upgrade")
	}
}
