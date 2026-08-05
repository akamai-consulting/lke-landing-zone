package main

import (
	"os"
	"strings"
	"testing"
)

// image_repin_test.go covers the day-2 half of the #407 pin fix: pinning
// TF_IMAGE/KUBE_IMAGE to `sha-<the commit the template pin names>` is correct,
// and makes both variables stale the moment `llz upgrade` moves that pin. These
// pin the detector's two hard edges — do not touch what LLZ did not compute, and
// stay silent when the pin cannot be resolved — because getting either wrong
// costs an operator more than the staleness does.

const repinSHA = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"

// pinnedAt puts the test in an instance pinned at ref whose images all resolve.
func pinnedAt(t *testing.T, ref string) {
	t.Helper()
	writeInstanceDir(t, map[string]string{
		".copier-answers.yml": "_src_path: gh:acme/tmpl\nllz_version: " + ref + "\n",
	})
	stubTemplateCommit(t, func(string, string) (string, bool) { return repinSHA, true })
	stubImagePublished(t, func(string) (bool, bool) { return true, true })
}

func from(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestStaleCIImageVars(t *testing.T) {
	const (
		wantTF   = "ghcr.io/akamai-consulting/ci-tofu:sha-" + repinSHA
		wantKube = "ghcr.io/akamai-consulting/ci-kubernetes:sha-" + repinSHA
		oldSHA   = "0000000111111112222222333333344444445555"
	)

	// The regression itself: an upgrade rewrites the pin, and both image vars are
	// left naming the commit the PREVIOUS pin resolved to.
	t.Run("reports both variables when the pin moved out from under them", func(t *testing.T) {
		pinnedAt(t, "v0.0.40")
		got := staleCIImageVars("v0.0.40", from(map[string]string{
			"TF_IMAGE":   "ghcr.io/akamai-consulting/ci-tofu:sha-" + oldSHA,
			"KUBE_IMAGE": "ghcr.io/akamai-consulting/ci-kubernetes:sha-" + oldSHA,
		}))
		if len(got) != 2 {
			t.Fatalf("staleCIImageVars = %+v; want both variables", got)
		}
		if got[0].Name != "TF_IMAGE" || got[0].Want != wantTF {
			t.Errorf("TF_IMAGE want = %q; want %q", got[0].Want, wantTF)
		}
		if got[1].Name != "KUBE_IMAGE" || got[1].Want != wantKube {
			t.Errorf("KUBE_IMAGE want = %q; want %q", got[1].Want, wantKube)
		}
		// Have must carry the OLD value verbatim — it is the whole evidence an
		// operator gets that the two differ, and a report that only shows `want`
		// reads as an instruction with no reason attached.
		if !strings.Contains(got[0].Have, oldSHA) {
			t.Errorf("Have = %q; want the old commit preserved", got[0].Have)
		}
	})

	t.Run("says nothing when the recorded values already name the pin", func(t *testing.T) {
		pinnedAt(t, "v0.0.40")
		if got := staleCIImageVars("v0.0.40", from(map[string]string{
			"TF_IMAGE": wantTF, "KUBE_IMAGE": wantKube,
		})); len(got) != 0 {
			t.Errorf("staleCIImageVars = %+v; want none", got)
		}
	})

	// A fresh instance has nothing recorded. There is no skew to report — filling
	// them is computeAndReportImageVars' job, and reporting "stale" for a variable
	// that was never set would send a new adopter chasing a non-problem.
	//
	// It must also cost NOTHING to find that out: `llz upgrade` calls this on every
	// run, and resolving a pin nobody asked about is up to five network requests
	// spent to discover there was nothing to compare.
	t.Run("says nothing, and asks nothing, when nothing is recorded", func(t *testing.T) {
		pinnedAt(t, "v0.0.40")
		stubTemplateCommit(t, func(string, string) (string, bool) {
			t.Error("resolved the pin before checking whether anything was recorded")
			return "", false
		})
		stubImagePublished(t, func(string) (bool, bool) {
			t.Error("asked the registry before checking whether anything was recorded")
			return false, false
		})
		if got := staleCIImageVars("v0.0.40", from(map[string]string{})); len(got) != 0 {
			t.Errorf("staleCIImageVars = %+v; want none", got)
		}
	})

	// EDGE 1. A private fork or a hand-picked image is a decision someone made on
	// purpose. Overwriting it — or even calling it wrong — is not this function's
	// call, and `llz tokens` acts on what this returns.
	t.Run("leaves a value LLZ did not compute alone", func(t *testing.T) {
		pinnedAt(t, "v0.0.40")
		for _, foreign := range []string{
			"ghcr.io/my-fork/ci-tofu:sha-" + oldSHA, // another org
			"registry.example.com/ci-tofu:v1",       // another registry
			"ghcr.io/akamai-consulting/ci-other:x",  // another image
			"ghcr.io/akamai-consulting/ci-tofu:",    // no tag at all
		} {
			if got := staleCIImageVars("v0.0.40", from(map[string]string{"TF_IMAGE": foreign})); len(got) != 0 {
				t.Errorf("staleCIImageVars(%q) = %+v; want none — that value is the operator's", foreign, got)
			}
		}
	})

	// EDGE 2, and the expensive one to get wrong. !pinned means computeCIImageVars
	// has no commit-pinned answer and handed back the floating tags instead —
	// advising a re-pin onto those would talk a CORRECTLY pinned instance down to
	// the tags #407 exists to keep it off.
	t.Run("stays silent when the pin cannot be resolved", func(t *testing.T) {
		pinnedAt(t, "v0.0.40")
		stubTemplateCommit(t, func(string, string) (string, bool) { return "", false })
		if got := staleCIImageVars("v0.0.40", from(map[string]string{
			"TF_IMAGE": "ghcr.io/akamai-consulting/ci-tofu:sha-" + oldSHA,
		})); len(got) != 0 {
			t.Fatalf("staleCIImageVars = %+v; want none on an unresolvable pin", got)
		}
	})

	t.Run("stays silent when the pinned images were never published", func(t *testing.T) {
		pinnedAt(t, "v0.0.40")
		stubImagePublished(t, func(string) (bool, bool) { return false, true }) // a 404 IS an answer
		if got := staleCIImageVars("v0.0.40", from(map[string]string{
			"TF_IMAGE": "ghcr.io/akamai-consulting/ci-tofu:sha-" + oldSHA,
		})); len(got) != 0 {
			t.Fatalf("staleCIImageVars = %+v; want none — the floating fallback is not a re-pin target", got)
		}
	})

	// The mirror of the above, and the reason the two must be told apart: an
	// unreachable registry is NOT a 404. ciImageVarsForTag deliberately keeps the
	// pin when it could not ask, so the skew is still real and still reportable —
	// an offline operator gets the same correct advice as an online one.
	t.Run("still reports when the registry could not be asked", func(t *testing.T) {
		pinnedAt(t, "v0.0.40")
		stubImagePublished(t, func(string) (bool, bool) { return false, false }) // asked=false
		if got := staleCIImageVars("v0.0.40", from(map[string]string{
			"TF_IMAGE": "ghcr.io/akamai-consulting/ci-tofu:sha-" + oldSHA,
		})); len(got) != 1 || got[0].Want != wantTF {
			t.Fatalf("staleCIImageVars = %+v; want the TF_IMAGE skew reported anyway", got)
		}
	})

	t.Run("an empty ref is not an instance", func(t *testing.T) {
		pinnedAt(t, "v0.0.40")
		stubTemplateCommit(t, func(string, string) (string, bool) {
			t.Error("resolved a commit for an empty ref")
			return "", false
		})
		if got := staleCIImageVars("  ", from(map[string]string{"TF_IMAGE": "x"})); len(got) != 0 {
			t.Errorf("staleCIImageVars = %+v; want none", got)
		}
	})
}

func TestLLZComputedImageRef(t *testing.T) {
	for _, tc := range []struct {
		ref, image string
		want       bool
	}{
		{"ghcr.io/akamai-consulting/ci-tofu:sha-abc", "ci-tofu", true},
		{"ghcr.io/akamai-consulting/ci-tofu:1.12.5", "ci-tofu", true}, // the floating fallback is ours too
		{"ghcr.io/akamai-consulting/ci-kubernetes:sha-abc", "ci-kubernetes", true},
		{"ghcr.io/akamai-consulting/ci-tofu:sha-abc", "ci-kubernetes", false},
		{"ghcr.io/akamai-consulting/ci-tofu", "ci-tofu", false}, // no tag separator
		{"ghcr.io/akamai-consulting/ci-tofu:", "ci-tofu", false},
		{"ghcr.io/other/ci-tofu:sha-abc", "ci-tofu", false},
		{"", "ci-tofu", false},
	} {
		if got := llzComputedImageRef(tc.ref, tc.image); got != tc.want {
			t.Errorf("llzComputedImageRef(%q, %q) = %v; want %v", tc.ref, tc.image, got, tc.want)
		}
	}
}

// TestReportCIImageSkew pins the WARNING, which is the entire deliverable of the
// `llz upgrade` half: the command cannot fix these (they are GitHub repo
// variables and it pushes nothing), so what it prints is all the operator gets
// before CI tells them the same thing 20 minutes later.
func TestReportCIImageSkew(t *testing.T) {
	const oldSHA = "0000000111111112222222333333344444445555"

	setup := func(t *testing.T, tfImage string) {
		t.Helper()
		pinnedAt(t, "v0.0.41")
		if err := os.MkdirAll(".llz", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(".llz/vars.env", []byte("TF_IMAGE="+tfImage+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(".copier-answers.yml",
			[]byte("_src_path: gh:acme/tmpl\nllz_version: v0.0.41\ninstance_repo: ch-org/ch-instance\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("names both routes back, and the instance repo", func(t *testing.T) {
		setup(t, "ghcr.io/akamai-consulting/ci-tofu:sha-"+oldSHA)
		_, errOut := captureStdoutStderr(t, func() { reportCIImageSkew("v0.0.41") })

		// The remediation is useless without the repo it targets, and `<owner>/<name>`
		// left as a placeholder is the difference between copy-paste and a lookup.
		for _, want := range []string{
			"TF_IMAGE",
			oldSHA,               // what they have
			repinSHA,             // what they need
			"ch-org/ch-instance", // resolved, not a placeholder
			"llz tokens --env <env> --yes",
			"gh variable set TF_IMAGE",
			"assert-image-fresh", // names the guard that will otherwise say it in CI
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("warning is missing %q:\n%s", want, errOut)
			}
		}
	})

	// Silence is load-bearing: this runs on EVERY upgrade, and a warning that fires
	// when nothing is wrong is one operators learn to scroll past.
	t.Run("prints nothing when the images already match the new pin", func(t *testing.T) {
		setup(t, "ghcr.io/akamai-consulting/ci-tofu:sha-"+repinSHA)
		out, errOut := captureStdoutStderr(t, func() { reportCIImageSkew("v0.0.41") })
		if out != "" || errOut != "" {
			t.Errorf("warned with nothing to warn about:\nstdout=%q\nstderr=%q", out, errOut)
		}
	})

	t.Run("prints nothing when there is no vars.env to read", func(t *testing.T) {
		pinnedAt(t, "v0.0.41")
		out, errOut := captureStdoutStderr(t, func() { reportCIImageSkew("v0.0.41") })
		if out != "" || errOut != "" {
			t.Errorf("warned outside a provisioned instance:\nstdout=%q\nstderr=%q", out, errOut)
		}
	})
}

// TestPrintNextSteps guards the post-scaffold list, which is the quickstart an
// adopter actually reads — it is on their screen when the doc is not. It had
// drifted from docs/quickstart.md in both directions the drift can go: a step
// the doc gained (#405's `git push`) and a step the doc dropped (the deprecated
// `llz validate --env`). Neither gate that watches the other copy can see this
// one — docs-guard reads Markdown, and these are Go string literals — so the
// assertions live here.
func TestPrintNextSteps(t *testing.T) {
	out := captureStdout(t, func() { printNextSteps("my-instance", true) })

	// `llz env add` COMMITS and does not push, and the build renders from the
	// pushed tree — so omitting this walked every adopter into the exact state
	// #405 identified as one of two default-path slips.
	if !strings.Contains(out, "git push") {
		t.Errorf("next steps never tell the adopter to push:\n%s", out)
	}
	// Ordering is the whole point of a numbered path: pushing after the build was
	// dispatched is the same failure as not pushing at all.
	add, push := strings.Index(out, "llz env add"), strings.Index(out, "git push")
	build := strings.Index(out, "llz up")
	if !(add >= 0 && add < push && push < build) {
		t.Errorf("expected `llz env add` → `git push` → `llz up`; got indices %d, %d, %d:\n%s", add, push, build, out)
	}
	// Deprecated: prints a notice and points at `llz doctor --env`. Sending a
	// first-time adopter to it is how the CLI teaches its own retired surface.
	if strings.Contains(out, "llz validate --env") {
		t.Errorf("next steps still recommend the deprecated `llz validate --env`:\n%s", out)
	}
	// `llz up` chains tokens → doctor → build and stops at the first failure, but
	// it is interactive — an adopter who does not know that runs it unattended.
	if !strings.Contains(out, "interactive") {
		t.Errorf("next steps recommend `llz up` without saying it is interactive:\n%s", out)
	}
	// The individual gates have to stay reachable from here; they are what an
	// operator drops to when `llz up` stops on one of them.
	for _, want := range []string{"llz tokens --env", "llz doctor --env", "llz build <env>"} {
		if !strings.Contains(out, want) {
			t.Errorf("next steps no longer mention %q:\n%s", want, out)
		}
	}
}

func TestRepinPlanNote(t *testing.T) {
	if got := repinPlanNote(nil); got != "" {
		t.Errorf("repinPlanNote(nil) = %q; want empty", got)
	}
	// A dry run that reports "0 missing REQUIRED item(s)" and stops reads as "no
	// work" to the one operator who has some.
	got := repinPlanNote([]ciImageSkew{{Name: "TF_IMAGE"}, {Name: "KUBE_IMAGE"}})
	if !strings.Contains(got, "2") || !strings.Contains(got, "re-pin") {
		t.Errorf("repinPlanNote = %q; want it to count the re-pins", got)
	}
}
