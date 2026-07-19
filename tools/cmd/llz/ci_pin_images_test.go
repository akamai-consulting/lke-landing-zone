package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestImageRef(t *testing.T) {
	const base, sha = "ghcr.io/akamai-consulting/ci-terraform", "abc123"
	if got := imageRef(base, sha, true); got != base+":sha-abc123" {
		t.Errorf("built: imageRef = %q, want sha- tag", got)
	}
	if got := imageRef(base, sha, false); got != base+":latest" {
		t.Errorf("not-built: imageRef = %q, want :latest", got)
	}
}

func TestParseBuildCount(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"0\n", 0}, {"3", 3}, {"  2 \n", 2}, {"", 0}, {"not-a-number", 0},
	} {
		if got := parseBuildCount([]byte(c.in)); got != c.want {
			t.Errorf("parseBuildCount(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestWaitForManifest(t *testing.T) {
	prevExists, prevSleep := pinManifestExists, pinSleep
	t.Cleanup(func() { pinManifestExists, pinSleep = prevExists, prevSleep })
	pinSleep = func(time.Duration) {} // no real sleeps

	// Appears on the 3rd poll.
	n := 0
	pinManifestExists = func(string) bool { n++; return n == 3 }
	if !waitForManifest("img", 5, 0) || n != 3 {
		t.Errorf("appears-on-3rd: got false or polls=%d", n)
	}

	// Never appears within the budget → false after retries+1 checks.
	n = 0
	pinManifestExists = func(string) bool { n++; return false }
	if waitForManifest("img", 4, 0) {
		t.Error("never-appears: want false")
	}
	if n != 5 { // first immediate check + 4 retries
		t.Errorf("never-appears: polled %d times, want 5", n)
	}
}

// stubPinSeams wires the gh / manifest / login / sleep seams for a flow test and
// records the `gh variable set` calls. builds is the value commitBuiltImages sees.
func stubPinSeams(t *testing.T, builds int, manifest func(string) bool) *[]string {
	t.Helper()
	var setVars []string
	pg, pm, pl, ps := pinGH, pinManifestExists, pinDockerLogin, pinSleep
	pbip, ptb := pinBuildInProgress, pinTriggerBuild
	t.Cleanup(func() {
		pinGH, pinManifestExists, pinDockerLogin, pinSleep = pg, pm, pl, ps
		pinBuildInProgress, pinTriggerBuild = pbip, ptb
	})

	pinDockerLogin = func(string, string) error { return nil }
	pinSleep = func(time.Duration) {}
	pinManifestExists = manifest
	// Defaults for the build-ensure seams; flow tests that exercise
	// --build-if-missing override these.
	pinBuildInProgress = func(string, string, string) bool { return false }
	pinTriggerBuild = func(string, string, string, string) error { return nil }
	pinGH = func(_ string, args ...string) ([]byte, error) {
		a := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(a, "api "): // build-run count query
			return []byte(strconv.Itoa(builds) + "\n"), nil
		case strings.HasPrefix(a, "variable set "):
			setVars = append(setVars, a)
			return nil, nil
		}
		return nil, errors.New("unexpected gh call: " + a)
	}
	return &setVars
}

func baseOpts() pinOpts {
	return pinOpts{
		instance: "akamai-consulting/lke-landing-zone-example", owner: "akamai-consulting",
		templateRepo: "akamai-consulting/lke-landing-zone", sha: "deadbeef", actor: "bot",
		templateToken: "tt", instanceToken: "it", interval: 0, retries: 3,
	}
}

func TestRunPinInstanceImages(t *testing.T) {
	// Commit built images → both vars pinned to the sha tag (after the image publishes).
	setVars := stubPinSeams(t, 1, func(string) bool { return true })
	if err := runPinInstanceImages(baseOpts()); err != nil {
		t.Fatalf("built flow: %v", err)
	}
	joined := strings.Join(*setVars, "\n")
	for _, want := range []string{
		"variable set TF_IMAGE --repo akamai-consulting/lke-landing-zone-example --body ghcr.io/akamai-consulting/ci-terraform:sha-deadbeef",
		"variable set KUBE_IMAGE --repo akamai-consulting/lke-landing-zone-example --body ghcr.io/akamai-consulting/ci-kubernetes:sha-deadbeef",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("built flow missing pin %q:\n%s", want, joined)
		}
	}

	// No build for this commit → pin :latest.
	setVars = stubPinSeams(t, 0, func(string) bool { return true })
	if err := runPinInstanceImages(baseOpts()); err != nil {
		t.Fatalf("latest flow: %v", err)
	}
	if !strings.Contains(strings.Join(*setVars, "\n"), "ci-terraform:latest") {
		t.Errorf("no-build flow should pin :latest, got %v", *setVars)
	}

	// Built but the sha image never publishes within the budget → error, no var set.
	setVars = stubPinSeams(t, 1, func(string) bool { return false })
	if err := runPinInstanceImages(baseOpts()); err == nil {
		t.Error("expected error when sha image never publishes")
	} else if len(*setVars) != 0 {
		t.Errorf("should not set a variable on publish timeout, got %v", *setVars)
	}

	// Missing required input → clear error.
	o := baseOpts()
	o.instanceToken = ""
	if err := runPinInstanceImages(o); err == nil || !strings.Contains(err.Error(), "GH_TOKEN_INSTANCE") {
		t.Errorf("missing token: err=%v, want a GH_TOKEN_INSTANCE required error", err)
	}
}

func TestRunPinInstanceImagesBuildIfMissing(t *testing.T) {
	// Built, but the sha image is missing on first check and a build is NOT in
	// progress → --build-if-missing triggers a fresh build, then the image
	// publishes and both vars pin to the sha tag.
	publishedAfter := 2
	calls := 0
	setVars := stubPinSeams(t, 1, func(string) bool {
		calls++
		// First two checks (anyShaImageMissing + TF wait) miss; then publishes.
		return calls > publishedAfter
	})
	var triggeredRef, triggeredSha string
	pinTriggerBuild = func(_, _, ref, sha string) error { triggeredRef, triggeredSha = ref, sha; return nil }
	pinBuildInProgress = func(string, string, string) bool { return false }

	o := baseOpts()
	o.buildIfMissing = true
	o.ref = "main"
	if err := runPinInstanceImages(o); err != nil {
		t.Fatalf("build-if-missing flow: %v", err)
	}
	if triggeredRef != "main" {
		t.Errorf("expected a build triggered on ref main, got %q", triggeredRef)
	}
	if triggeredSha != "deadbeef" {
		t.Errorf("build must target the exact sha, got %q", triggeredSha)
	}
	if !strings.Contains(strings.Join(*setVars, "\n"), "ci-terraform:sha-deadbeef") {
		t.Errorf("should pin the sha image after the triggered build, got %v", *setVars)
	}

	// A build already in progress → do NOT trigger a duplicate; just wait.
	stubPinSeams(t, 1, func(string) bool { return true })
	triggered := false
	pinTriggerBuild = func(string, string, string, string) error { triggered = true; return nil }
	pinBuildInProgress = func(string, string, string) bool { return true }
	o2 := baseOpts()
	o2.buildIfMissing = true
	o2.ref = "main"
	if err := runPinInstanceImages(o2); err != nil {
		t.Fatalf("in-progress flow: %v", err)
	}
	if triggered {
		t.Error("must NOT trigger a build when one is already in progress")
	}

	// --build-if-missing without --ref → clear error.
	stubPinSeams(t, 1, func(string) bool { return false })
	o3 := baseOpts()
	o3.buildIfMissing = true
	if err := runPinInstanceImages(o3); err == nil || !strings.Contains(err.Error(), "--ref is required") {
		t.Errorf("missing --ref: err=%v, want a --ref required error", err)
	}

	// Branch case: NO build ran for this commit (build-images doesn't auto-run off
	// main → commitBuiltImages sees 0) and the sha image is missing. --build-if-missing
	// must still trigger a build on the branch ref and pin the sha — NOT pin a stale
	// :latest. This is the gap that let branch e2es trip assert-image-fresh.
	calls2 := 0
	setVars2 := stubPinSeams(t, 0 /* no prior build for this commit */, func(string) bool {
		calls2++
		return calls2 > 2 // missing on the anyShaImageMissing + first wait check, then publishes
	})
	var branchRef string
	pinTriggerBuild = func(_, _, ref, _ string) error { branchRef = ref; return nil }
	pinBuildInProgress = func(string, string, string) bool { return false }
	o4 := baseOpts()
	o4.buildIfMissing = true
	o4.ref = "feat/x"
	if err := runPinInstanceImages(o4); err != nil {
		t.Fatalf("branch build-if-missing flow: %v", err)
	}
	if branchRef != "feat/x" {
		t.Errorf("branch: expected a build triggered on ref feat/x, got %q", branchRef)
	}
	if !strings.Contains(strings.Join(*setVars2, "\n"), "ci-terraform:sha-deadbeef") {
		t.Errorf("branch: should pin the sha image (not :latest), got %v", *setVars2)
	}
	if strings.Contains(strings.Join(*setVars2, "\n"), ":latest") {
		t.Errorf("branch: must not pin a stale :latest, got %v", *setVars2)
	}
}

func TestRunPinInstanceImagesTriggerOnly(t *testing.T) {
	// trigger-only with a missing image: trigger the build, then return WITHOUT
	// waiting for the publish and WITHOUT setting any instance variable.
	setVars := stubPinSeams(t, 1, func(string) bool { return false /* never publishes */ })
	triggered := false
	pinTriggerBuild = func(string, string, string, string) error { triggered = true; return nil }
	pinBuildInProgress = func(string, string, string) bool { return false }

	o := baseOpts()
	o.buildIfMissing = true
	o.ref = "main"
	o.triggerOnly = true
	o.instanceToken = "" // not needed: trigger-only never writes instance variables
	if err := runPinInstanceImages(o); err != nil {
		t.Fatalf("trigger-only flow: %v", err)
	}
	if !triggered {
		t.Error("trigger-only must still trigger the missing build")
	}
	if len(*setVars) != 0 {
		t.Errorf("trigger-only must not pin variables, got %v", *setVars)
	}

	// A full (non-trigger-only) run still requires the instance token.
	o2 := baseOpts()
	o2.instanceToken = ""
	if err := runPinInstanceImages(o2); err == nil || !strings.Contains(err.Error(), "GH_TOKEN_INSTANCE") {
		t.Errorf("full run without GH_TOKEN_INSTANCE: err=%v, want required error", err)
	}
}

// pinGHRetry rides out transient GitHub API failures (a 503 on the first
// Instantiate query killed release-e2e run 29540787054 at minute one) but still
// surfaces a persistent error after 3 attempts.
func TestPinGHRetry(t *testing.T) {
	origGH, origSleep := pinGH, pinSleep
	t.Cleanup(func() { pinGH, pinSleep = origGH, origSleep })
	var slept []time.Duration
	pinSleep = func(d time.Duration) { slept = append(slept, d) }

	// Fails twice (transient 503), succeeds on the third try.
	calls := 0
	pinGH = func(_ string, _ ...string) ([]byte, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("HTTP 503")
		}
		return []byte("1"), nil
	}
	out, err := pinGHRetry("tok", "api", "x")
	if err != nil || string(out) != "1" {
		t.Fatalf("retry should succeed on attempt 3: out=%q err=%v", out, err)
	}
	if calls != 3 || len(slept) != 2 {
		t.Errorf("calls=%d slept=%d, want 3/2", calls, len(slept))
	}

	// Persistent failure → error surfaces after exactly 3 attempts.
	calls = 0
	pinGH = func(_ string, _ ...string) ([]byte, error) { calls++; return nil, errors.New("boom") }
	if _, err := pinGHRetry("tok", "api", "x"); err == nil {
		t.Fatal("persistent failure must surface")
	}
	if calls != 3 {
		t.Errorf("calls=%d, want 3", calls)
	}
}
