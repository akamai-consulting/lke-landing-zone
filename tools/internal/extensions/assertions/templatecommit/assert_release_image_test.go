package templatecommit

import (
	"strings"
	"testing"
	"time"
)

// stubRegistry makes ImagePublished answer from a script, one entry per look, and
// counts the sleeps so a test can prove the loop is bounded by ATTEMPTS rather
// than spinning.
func stubRegistry(t *testing.T, answers []struct{ published, asked bool }) (looks, sleeps *int) {
	t.Helper()
	var nLooks, nSleeps int
	prevPub, prevSleep := ImagePublished, sleepFor
	ImagePublished = func(string) (bool, bool) {
		i := nLooks
		nLooks++
		if i >= len(answers) {
			return answers[len(answers)-1].published, answers[len(answers)-1].asked
		}
		return answers[i].published, answers[i].asked
	}
	sleepFor = func(time.Duration) { nSleeps++ }
	t.Cleanup(func() { ImagePublished, sleepFor = prevPub, prevSleep })
	return &nLooks, &nSleeps
}

type answer = struct{ published, asked bool }

// THE INCIDENT, and it is a RACE. build-images for the release commit started
// 66s before the release workflow and was still in_progress when the retag ran.
// A check that failed on the first look would be correct and useless — it would
// turn every promptly cut release into a manual re-run.
func TestReleaseImageWaitsForABuildStillInFlight(t *testing.T) {
	looks, sleeps := stubRegistry(t, []answer{
		{false, true}, {false, true}, {false, true}, {true, true},
	})
	var out strings.Builder
	if err := RunAssertReleaseImage("akamai-consulting", "llz", "abc123", 10, time.Second, &out); err != nil {
		t.Fatalf("gave up on an image that appeared on the 4th look: %v", err)
	}
	if *looks != 4 {
		t.Errorf("looked %d time(s), want 4 — it should stop as soon as the image appears", *looks)
	}
	if *sleeps != 3 {
		t.Errorf("slept %d time(s), want 3 — one between each pair of looks and none after the last", *sleeps)
	}
	if !strings.Contains(out.String(), "appeared after 4 look(s)") {
		t.Errorf("did not report that it had to wait:\n%s", out.String())
	}
}

// The budget IS the count. A loop bounded by a deadline spins at full speed the
// moment its clock seam is a no-op — which is exactly the shape of these tests.
func TestReleaseImageIsBoundedByAttemptsNotAClock(t *testing.T) {
	looks, sleeps := stubRegistry(t, []answer{{false, true}})
	err := RunAssertReleaseImage("o", "llz", "abc123", 5, time.Hour, &strings.Builder{})
	if err == nil {
		t.Fatal("an image that never appears must fail")
	}
	if *looks != 5 {
		t.Errorf("looked %d time(s) for --attempts 5", *looks)
	}
	// Four, not five: sleeping after the final look delays the report by a whole
	// interval and buys no extra evidence.
	if *sleeps != 4 {
		t.Errorf("slept %d time(s), want 4 (none after the last look)", *sleeps)
	}
	if !strings.Contains(err.Error(), "never appeared") || !strings.Contains(err.Error(), "build-images") {
		t.Errorf("failure does not name the cause or where to look:\n%v", err)
	}
}

// "Not published" and "could not ask" are different facts and the operator acts on
// them differently — re-run the build, versus re-run this job.
func TestReleaseImageDistinguishesAnUnreachableRegistry(t *testing.T) {
	stubRegistry(t, []answer{{false, false}})
	err := RunAssertReleaseImage("o", "llz", "abc123", 3, time.Second, &strings.Builder{})
	if err == nil {
		t.Fatal("must fail closed when it could never ask — a wrong PASS publishes an unpullable tag")
	}
	if !strings.Contains(err.Error(), "could not ask") {
		t.Errorf("reported a missing image when the registry was unreachable:\n%v", err)
	}
}

func TestReleaseImagePassesImmediatelyWhenAlreadyPublished(t *testing.T) {
	looks, sleeps := stubRegistry(t, []answer{{true, true}})
	var out strings.Builder
	if err := RunAssertReleaseImage("akamai-consulting", "llz", "abc123", 60, time.Minute, &out); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if *looks != 1 || *sleeps != 0 {
		t.Errorf("looks=%d sleeps=%d — the normal case must not wait at all", *looks, *sleeps)
	}
	if !strings.Contains(out.String(), "is published") {
		t.Errorf("no confirmation line:\n%s", out.String())
	}
}

// A gate with nothing to look up must say so rather than pass.
func TestReleaseImageRefusesAnEmptySHA(t *testing.T) {
	stubRegistry(t, []answer{{true, true}})
	err := RunAssertReleaseImage("o", "llz", "   ", 3, time.Second, &strings.Builder{})
	if err == nil {
		t.Fatal("an empty --sha must be an error, not a vacuous pass")
	}
	if !strings.Contains(err.Error(), "examined nothing") {
		t.Errorf("error does not say why an empty sha is refused:\n%v", err)
	}
}

// The ref it looks up is the one the release retags FROM — a different spelling
// here would check an image nobody publishes and pass forever.
func TestReleaseImageLooksUpTheRefTheRetagUses(t *testing.T) {
	var got string
	prev := ImagePublished
	ImagePublished = func(ref string) (bool, bool) { got = ref; return true, true }
	t.Cleanup(func() { ImagePublished = prev })

	if err := RunAssertReleaseImage("Akamai-Consulting", "llz", "1276c08f", 1, time.Second, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	// Lowercased owner, `sha-` prefix — exactly what llz-release.yml's imagetools
	// create names as its source.
	if want := "ghcr.io/akamai-consulting/llz:sha-1276c08f"; got != want {
		t.Errorf("looked up %q, want %q", got, want)
	}
}

// THE DEFAULTS ARE THE GATE. The wait budget is what turns this from a check that
// fails on every promptly cut release into one that rides out the race — so the
// numbers are asserted, not left to whoever edits the flag line next. Sized from
// measurement: build-images completed in 6-9 minutes across the last eight runs,
// and 59 intervals x 15s = 14m45s covers a slow one with margin.
func TestAssertReleaseImageDefaultsCoverASlowBuildImagesRun(t *testing.T) {
	c := AssertReleaseImageCmd()
	attempts, err := c.Flags().GetInt("attempts")
	if err != nil {
		t.Fatal(err)
	}
	interval, err := c.Flags().GetDuration("interval")
	if err != nil {
		t.Fatal(err)
	}
	if budget := time.Duration(attempts-1) * interval; budget < 12*time.Minute {
		t.Errorf("wait budget is %s (attempts=%d interval=%s) — under the 6-9 min a build-images "+
			"run takes, so a release cut promptly after a merge still fails the way v0.0.44 did",
			budget, attempts, interval)
	}
	if got, _ := c.Flags().GetString("image"); got != "llz" {
		t.Errorf("default --image is %q — it must name the image llz-release.yml retags", got)
	}
	if got, _ := c.Flags().GetString("sha"); got != "" {
		t.Errorf("--sha defaults to %q; it must be empty so an unset one is refused rather than "+
			"silently looking up the wrong image", got)
	}
}

// The command must actually run the check it advertises — a RunE wired to nothing
// passes every release.
func TestAssertReleaseImageCommandRunsTheCheck(t *testing.T) {
	var asked string
	prev := ImagePublished
	ImagePublished = func(ref string) (bool, bool) { asked = ref; return true, true }
	t.Cleanup(func() { ImagePublished = prev })

	c := AssertReleaseImageCmd()
	c.SetOut(&strings.Builder{})
	c.SetErr(&strings.Builder{})
	c.SetArgs([]string{"--owner", "o", "--sha", "deadbeef"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if asked != "ghcr.io/o/llz:sha-deadbeef" {
		t.Errorf("command looked up %q", asked)
	}
}

func TestAssertReleaseImageCommandFailsWithoutASHA(t *testing.T) {
	c := AssertReleaseImageCmd()
	c.SetOut(&strings.Builder{})
	c.SetErr(&strings.Builder{})
	c.SetArgs(nil)
	if err := c.Execute(); err == nil {
		t.Error("running with no --sha must fail rather than pass having looked up nothing")
	}
}
