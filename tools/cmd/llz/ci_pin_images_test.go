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
	t.Cleanup(func() { pinGH, pinManifestExists, pinDockerLogin, pinSleep = pg, pm, pl, ps })

	pinDockerLogin = func(string, string) error { return nil }
	pinSleep = func(time.Duration) {}
	pinManifestExists = manifest
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
