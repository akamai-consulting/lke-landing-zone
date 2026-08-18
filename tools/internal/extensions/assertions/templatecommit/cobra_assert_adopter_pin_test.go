package templatecommit

import (
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/versionpins"
)

const (
	pinSHA   = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"
	otherSHA = "1550263c84909350942bd1d0116088062f9a1c8b"
)

// stubPublishWait makes the publish wait instant and records how many times it
// slept. EVERY test in this file installs one — waitForCIImages otherwise sleeps
// its real 10x30s budget whenever a case stubs an image as unpublished, which is
// most of them, and a unit test that takes five minutes is a hung test.
func stubPublishWait(t *testing.T) *int {
	t.Helper()
	prevSleep, prevDelay := adopterPinSleep, adopterPinPublishDelay
	t.Cleanup(func() { adopterPinSleep, adopterPinPublishDelay = prevSleep, prevDelay })
	slept := 0
	adopterPinPublishDelay = time.Nanosecond
	adopterPinSleep = func(time.Duration) { slept++ }
	return &slept
}

// adopterPinStubs wires the happy path: the tag resolves, both images published.
func adopterPinStubs(t *testing.T) {
	t.Helper()
	stubPublishWait(t)
	stubTemplateCommit(t, func(string, string) (string, bool) { return pinSHA, true })
	stubImagePublished(t, func(string) (bool, bool) { return true, true })
	stubLatestRelease(t, func(string) (string, bool) { return "v0.0.39", true })
}

func stubLatestRelease(t *testing.T, fn func(repo string) (string, bool)) {
	t.Helper()
	prev := latestReleaseTag
	t.Cleanup(func() { latestReleaseTag = prev })
	latestReleaseTag = fn
}

func TestAssertAdopterPinHappyPath(t *testing.T) {
	adopterPinStubs(t)
	if err := runAssertAdopterPin("acme/tmpl", "v0.0.39"); err != nil {
		t.Fatalf("runAssertAdopterPin = %v, want nil", err)
	}
}

func TestAssertAdopterPinDefaultsToLatestRelease(t *testing.T) {
	adopterPinStubs(t)
	var askedFor string
	stubLatestRelease(t, func(repo string) (string, bool) { askedFor = repo; return "v0.0.39", true })
	if err := runAssertAdopterPin("acme/tmpl", ""); err != nil {
		t.Fatalf("runAssertAdopterPin = %v, want nil", err)
	}
	if askedFor != "acme/tmpl" {
		t.Errorf("latestReleaseTag asked about %q, want acme/tmpl", askedFor)
	}
}

// THE REGRESSION. This is the gate reproducing the pre-fix behaviour: `llz tokens`
// computes a floating Version tag for a release-pinned instance. It has to FAIL —
// this exact configuration shipped to a live adopter with e2e color.Green throughout.
func TestAssertAdopterPinRejectsAFloatingImagePin(t *testing.T) {
	stubPublishWait(t)
	stubTemplateCommit(t, func(string, string) (string, bool) { return pinSHA, true })
	// A commit with no published images is what makes computeCIImageVars fall back to
	// the floating tags — the same end state the pre-fix code produced unconditionally.
	stubImagePublished(t, func(string) (bool, bool) { return false, true })

	err := runAssertAdopterPin("acme/tmpl", "v0.0.39")
	if err == nil {
		t.Fatal("gate passed an instance that would run a floating, main-tracking image")
	}
	for _, want := range []string{"would not pin", "ci-tofu:" + versionpins.CITofuTag, "llz render --check"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message missing %q:\n%v", want, err)
		}
	}
}

// An unresolvable ref is a FAILED gate, not a skipped one. assert-image-fresh
// degrades to a warning because it cannot distinguish skew from unreachability
// mid-pipeline; this gate exists solely to answer the question, so not being able
// to ask it is the failure.
func TestAssertAdopterPinFailsOnAnUnresolvableRef(t *testing.T) {
	stubPublishWait(t)
	stubTemplateCommit(t, func(string, string) (string, bool) { return "", false })
	stubImagePublished(t, func(string) (bool, bool) { return true, true })
	if err := runAssertAdopterPin("acme/tmpl", "v9.9.9"); err == nil {
		t.Fatal("gate passed a ref it could not resolve")
	}
}

func TestAssertAdopterPinFailsWithNoReleaseAndNoRef(t *testing.T) {
	stubPublishWait(t)
	stubTemplateCommit(t, func(string, string) (string, bool) { return pinSHA, true })
	stubImagePublished(t, func(string) (bool, bool) { return true, true })
	stubLatestRelease(t, func(string) (string, bool) { return "", false })
	if err := runAssertAdopterPin("acme/tmpl", ""); err == nil {
		t.Fatal("gate passed with no release to check")
	}
}

// An unreachable registry must not fail the gate — the other three legs still
// answered, and the container pull is the backstop for an absent image.
//
// But it must not report OK either. The run used to warn "that leg is UNVERIFIED,
// not passed" and then close with "OK — an instance scaffolded at vX.Y.Z runs the
// llz that rendered it", a claim larger than the one the warning had withdrawn and
// the last line a release log shows. That is #428's shape one gate up.
func TestAssertAdopterPinToleratesAnUnreachableRegistry(t *testing.T) {
	stubPublishWait(t)
	stubTemplateCommit(t, func(string, string) (string, bool) { return pinSHA, true })
	stubImagePublished(t, func(string) (bool, bool) { return false, false })
	var err error
	out, errOut := captureOutput(t, func() { err = runAssertAdopterPin("acme/tmpl", "v0.0.39") })
	if err != nil {
		t.Fatalf("runAssertAdopterPin = %v, want nil (a GHCR blip must not block a release)", err)
	}
	if strings.Contains(out, "assert-adopter-pin: OK") {
		t.Errorf("a run that could not confirm publication still claimed OK:\n%s", out)
	}
	if !strings.Contains(out, "assert-adopter-pin: PARTIAL") {
		t.Errorf("want a PARTIAL verdict naming the unverified leg, got:\n%s", out)
	}
	if !strings.Contains(errOut, "UNVERIFIED, not passed") {
		t.Errorf("want the per-image annotation on stderr, got:\n%s", errOut)
	}
}

// The converse, so PARTIAL cannot creep onto a fully verified run: all four legs
// answering must still print OK and nothing else.
func TestAssertAdopterPinReportsOKOnlyWhenEveryLegAnswered(t *testing.T) {
	adopterPinStubs(t)
	var err error
	out, _ := captureOutput(t, func() { err = runAssertAdopterPin("acme/tmpl", "v0.0.39") })
	if err != nil {
		t.Fatalf("runAssertAdopterPin = %v, want nil", err)
	}
	if !strings.Contains(out, "assert-adopter-pin: OK") {
		t.Errorf("a fully verified run printed no OK verdict:\n%s", out)
	}
	if strings.Contains(out, "assert-adopter-pin: PARTIAL") {
		t.Errorf("a fully verified run also claimed PARTIAL:\n%s", out)
	}
}

// foreignCommit feeds the gate's negative half. If it ever returned the commit
// under test the negative check would be vacuous and the gate would pass a guard
// that accepts everything — which is the bug it was written to catch.
func TestForeignCommit(t *testing.T) {
	for _, sha := range []string{pinSHA, otherSHA, strings.Repeat("f", 40), strings.Repeat("0", 40)} {
		got := foreignCommit(sha)
		if got == sha {
			t.Errorf("foreignCommit(%q) returned the same commit", sha)
		}
		if len(got) != len(sha) {
			t.Errorf("foreignCommit(%q) = %q, want the same length", sha, got)
		}
		if !hexSHARe.MatchString(got) {
			t.Errorf("foreignCommit(%q) = %q, which is not a well-formed sha — "+
				"assert-image-fresh would take its not-a-SHA path and the negative check would test nothing", sha, got)
		}
	}
}

func TestAssertAdopterPinCmdWiring(t *testing.T) {
	c := AssertAdopterPinCmd()
	if c.Use != "assert-adopter-pin" {
		t.Errorf("Use = %q", c.Use)
	}
	for _, f := range []string{"ref", "template-repo"} {
		if c.Flags().Lookup(f) == nil {
			t.Errorf("missing --%s flag", f)
		}
	}
	// No --org: the GHCR owner comes from templateid.DefaultOrg via computeCIImageVars,
	// so a flag here would accept a value and silently ignore it.
	if c.Flags().Lookup("org") != nil {
		t.Error("--org is a no-op flag; it must not exist")
	}
}

// The gate runs from the TEMPLATE repo, which has no .copier-answers.yml — so a
// guard that reads the repo from the instance resolves against the first-party
// default instead of the repo it was handed. On a FORK that means resolving the
// fork's own release tag against upstream, where it does not exist.
func TestAssertAdopterPinResolvesAgainstTheRepoItWasGiven(t *testing.T) {
	stubPublishWait(t)
	writeInstanceDir(t, nil) // no answers file, exactly like a template checkout
	stubImagePublished(t, func(string) (bool, bool) { return true, true })

	var repos []string
	stubTemplateCommit(t, func(repo, _ string) (string, bool) {
		repos = append(repos, repo)
		return pinSHA, true
	})
	if err := runAssertAdopterPin("myfork/lke-landing-zone", "v0.0.39"); err != nil {
		t.Fatalf("runAssertAdopterPin = %v, want nil", err)
	}
	for _, got := range repos {
		if got != "myfork/lke-landing-zone" {
			t.Errorf("resolved against %q, want the repo the gate was given "+
				"(a fork's tag does not exist upstream, and the miss is reported as a guard failure)", got)
		}
	}
}

// Leg 4 must not depend on the network. It used to re-run the full command, whose
// resolve step degrades to warn-and-pass on a blip — and a passing NEGATIVE check
// is exactly the verdict the gate reports as "the skew guard is not guarding".
// A transient error must not be able to manufacture that.
func TestAssertAdopterPinLegFourIsNotNetworkDependent(t *testing.T) {
	stubPublishWait(t)
	stubImagePublished(t, func(string) (bool, bool) { return true, true })

	calls := 0
	stubTemplateCommit(t, func(string, string) (string, bool) {
		calls++
		// Resolve once (leg 1), then behave like a network that has gone away.
		return pinSHA, calls == 1
	})
	if err := runAssertAdopterPin("acme/tmpl", "v0.0.39"); err != nil {
		t.Fatalf("runAssertAdopterPin = %v; a blip after leg 1 must not change the verdict", err)
	}
	if calls != 1 {
		t.Errorf("resolved %d time(s), want exactly 1 — legs 2-4 must reuse leg 1's answer", calls)
	}
}

func TestWaitForCIImages(t *testing.T) {
	t.Run("already published costs nothing", func(t *testing.T) {
		slept := stubPublishWait(t)
		stubImagePublished(t, func(string) (bool, bool) { return true, true })
		waitForCIImages(pinSHA)
		if *slept != 0 {
			t.Errorf("slept %d time(s) for images that were already there", *slept)
		}
	})

	// The race this exists for: a candidate published minutes after the merge, with
	// build-images.yml still running. It must resolve into a pass, not a failed release.
	t.Run("waits for a build that is still running", func(t *testing.T) {
		slept := stubPublishWait(t)
		n := 0
		stubImagePublished(t, func(string) (bool, bool) { n++; return n > 3, true })
		waitForCIImages(pinSHA)
		if *slept == 0 {
			t.Error("did not wait for an image that was not published yet")
		}
	})

	t.Run("gives up inside the budget rather than hanging", func(t *testing.T) {
		slept := stubPublishWait(t)
		stubImagePublished(t, func(string) (bool, bool) { return false, true })
		waitForCIImages(pinSHA)
		if *slept != adopterPinPublishRetries {
			t.Errorf("slept %d time(s), want exactly the %d-retry budget", *slept, adopterPinPublishRetries)
		}
	})

	// Polling an endpoint that is not answering learns nothing, and the downstream
	// check treats "could not ask" as "do not downgrade" anyway.
	t.Run("does not wait on an unreachable registry", func(t *testing.T) {
		slept := stubPublishWait(t)
		stubImagePublished(t, func(string) (bool, bool) { return false, false })
		waitForCIImages(pinSHA)
		if *slept != 0 {
			t.Errorf("slept %d time(s) against a registry that never answered", *slept)
		}
	})
}
