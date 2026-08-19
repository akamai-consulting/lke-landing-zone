package upstreamupdates

import (
	"strings"
	"testing"
)

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

	moved := Decide(State{BeforeSHA: "abc", AfterSHA: "def0000", Version: "v1.2.3"})
	if !moved.OpenPR {
		t.Error("a moved HEAD must open a pull request")
	}
	if moved.Branch != "chore/template-upgrade-v1.2.3-def0000" {
		t.Errorf("branch = %q", moved.Branch)
	}
}

func TestDecideWillNotStackBehindAnOpenUpgrade(t *testing.T) {
	d := Decide(State{BeforeSHA: "abc", AfterSHA: "def", Version: "v1.2.3", OpenUpgradePR: true})
	if d.OpenPR {
		t.Error("a pending review must not get a second PR stacked behind it")
	}
	if !strings.Contains(d.Reason, "still open") {
		t.Errorf("reason must name the pending review, got %q", d.Reason)
	}
}

func TestDecideWillNotReproposeARejectedVersion(t *testing.T) {
	d := Decide(State{BeforeSHA: "abc", AfterSHA: "def", Version: "v1.2.3", RejectedThisVersion: true})
	if d.OpenPR {
		t.Error("a rejected version must not come back")
	}
	if !strings.Contains(d.Reason, "closed unmerged") {
		t.Errorf("reason must say it was rejected rather than shipped, got %q", d.Reason)
	}
}

func TestBranchNameIsARefAndCannotCollide(t *testing.T) {
	// Version-stemmed so the rejected-version guard can match it by prefix, and
	// SHA-suffixed so two runs never compute the same name. The second half is what
	// removed orphan recovery, the force-push and the spent-branch case.
	for in, want := range map[string]string{
		"v1.2.3":      "chore/template-upgrade-v1.2.3-",
		" v1.2.3\n":   "chore/template-upgrade-v1.2.3-",
		"":            "chore/template-upgrade-unknown-",
		"feat/x v2":   "chore/template-upgrade-feat-x-v2-",
		"release/9.9": "chore/template-upgrade-release-9.9-",
	} {
		if got := VersionStem(in); got != want {
			t.Errorf("VersionStem(%q) = %q, want %q", in, got, want)
		}
	}
	// git rejects far more than the three characters the first cut replaced, and
	// the failure surfaced at `git push` — AFTER the upgrade had been committed, so
	// the run did the work and threw it away. Allow-list, not deny-list.
	for _, bad := range []string{"v1~2", "v1^", "v1:x", "v1?", "v1*", "v1[a]", "v1\\x", "v1..2", "v1.", "v1.lock"} {
		got := VersionStem(bad)
		for _, ch := range []string{"~", "^", ":", "?", "*", "[", "]", "\\", ".."} {
			if strings.Contains(strings.TrimPrefix(got, branchStem), ch) {
				t.Errorf("VersionStem(%q) = %q still contains %q, which git rejects in a ref", bad, got, ch)
			}
		}
		if strings.HasSuffix(strings.TrimSuffix(got, "-"), ".lock") {
			t.Errorf("VersionStem(%q) = %q ends in .lock, which git rejects", bad, got)
		}
	}
	if BranchName("v1", "aaaaaaabbb") == BranchName("v1", "cccccccddd") {
		t.Error("two commits at one version must not share a branch")
	}
	if !strings.HasPrefix(BranchName("v1", "aaaaaaabbb"), VersionStem("v1")) {
		t.Error("the branch must sit under the stem the rejected-version guard matches")
	}
}

func TestPRBodyCarriesNoBacktickedCLICommand(t *testing.T) {
	// The body used to live in a workflow heredoc, where TestDeliveredWorkflow-
	// Commands resolves every CLI invocation in a `run:` script against the real
	// cobra tree — prose included — so a backticked command tokenised with its
	// closing backtick and failed to resolve. It lives in Go now, but the shape is
	// pinned in case it is ever moved back.
	//
	// BOTH VARIANTS, because there are two: prBody composes a different paragraph
	// for the non-draft fallback, and checking only the draft one is how a
	// backticked `llz ci tf-import` got into the other half unnoticed.
	for _, draft := range []bool{true, false} {
		body := prBody(draft)
		if strings.Contains(body, "`llz ") {
			t.Errorf("prBody(%t) contains a backticked llz command; use a fenced block so it tokenises "+
				"clean if this text is ever moved back into a run: script", draft)
		}
		if !strings.Contains(body, "repo-readiness") {
			t.Errorf("prBody(%t) must tell a reviewer which check actually gates the upgrade", draft)
		}
		if !strings.Contains(body, "gh variable set TF_IMAGE") {
			t.Errorf("prBody(%t) must carry the image bump, or repo-readiness fails on the image check "+
				"and never reaches the required-config check that catches a newly mandatory secret", draft)
		}
	}
}
