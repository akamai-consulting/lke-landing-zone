package releasepublish

// waitabort_test.go — a wait for an image whose build already FAILED must abort,
// not poll out its budget.
//
// THE INCIDENT: a GHCR secondary rate limit ("You have exceeded a secondary rate
// limit") failed the ci-kubernetes push about four minutes in. The e2e lane's
// instantiate job then waited ~20 more minutes for an image that was never
// coming, and from the outside that is indistinguishable from a hang — it was
// reported as "seems stuck". The old error even asked the question the API could
// answer: "did Build Container Images succeed for <sha>?".

import (
	"strings"
	"testing"
	"time"
)

// stubWait installs the three seams waitForManifest touches.
func stubWait(t *testing.T, exists func(string) bool, failed func(string, string, string, string) (string, bool)) *int {
	t.Helper()
	slept := 0
	oe, of, os_ := pinManifestExists, pinBuildFailed, pinSleep
	pinManifestExists = exists
	pinBuildFailed = failed
	pinSleep = func(time.Duration) { slept++ }
	t.Cleanup(func() { pinManifestExists, pinBuildFailed, pinSleep = oe, of, os_ })
	return &slept
}

func TestWaitAbortsOnAFailedBuild(t *testing.T) {
	slept := stubWait(t,
		func(string) bool { return false },
		func(string, string, string, string) (string, bool) { return "https://gh/run/1", true })

	ok, url := waitForManifest("ghcr.io/o/ci-kubernetes:sha-abc", "tok", "o/repo", "abc123", "", 60, time.Second)
	if ok {
		t.Fatal("the image never published — must not report success")
	}
	if url != "https://gh/run/1" {
		t.Errorf("the failed run's URL must be returned so the error can name it, got %q", url)
	}
	if *slept != 0 {
		t.Errorf("aborted after %d sleeps — a known-failed build must not be waited on at all", *slept)
	}
}

// TestWaitChecksTheImageBeforeTheBuild — ordering matters. A matrix build can fail
// on a LATER image after the one we need was already pushed; in that case the
// artifact exists and the wait must succeed rather than abort on its sibling.
func TestWaitChecksTheImageBeforeTheBuild(t *testing.T) {
	stubWait(t,
		func(string) bool { return true },
		func(string, string, string, string) (string, bool) {
			t.Fatal("must not consult the build once the image exists")
			return "", false
		})

	if ok, _ := waitForManifest("img", "tok", "o/repo", "abc", "", 3, time.Second); !ok {
		t.Fatal("a published image must win immediately")
	}
}

// TestWaitStillPollsWhileTheBuildIsHealthy — the abort must not make the normal
// path impatient: a build that is merely slow still gets the full budget.
func TestWaitStillPollsWhileTheBuildIsHealthy(t *testing.T) {
	calls := 0
	slept := stubWait(t,
		func(string) bool { calls++; return calls > 3 }, // appears on the 4th check
		func(string, string, string, string) (string, bool) { return "", false })

	if ok, _ := waitForManifest("img", "tok", "o/repo", "abc", "", 10, time.Second); !ok {
		t.Fatal("a slow-but-healthy build must still be waited for")
	}
	if *slept != 3 {
		t.Errorf("want 3 sleeps before the image appeared, got %d", *slept)
	}
}

// TestWaitKeepsWaitingWhenTheBuildStateIsUnknowable — an API error is "could not
// tell", which is NOT "it failed". Treating a transient 403 as a failure would
// abort a perfectly good build.
func TestWaitKeepsWaitingWhenTheBuildStateIsUnknowable(t *testing.T) {
	slept := stubWait(t,
		func(string) bool { return false },
		func(string, string, string, string) (string, bool) { return "", false }) // err path returns this

	if ok, url := waitForManifest("img", "tok", "o/repo", "abc", "", 2, time.Second); ok || url != "" {
		t.Fatalf("want a plain budget timeout, got ok=%v url=%q", ok, url)
	}
	if *slept != 2 {
		t.Errorf("want the full budget (2 sleeps), got %d", *slept)
	}
	_ = strings.TrimSpace
}

// TestPinBuildFailedParsesTheRunURL covers the real seam body (the stubs above
// replace it, so without this the query itself is never exercised).
//
// The three answers it must keep distinct are the whole point: a URL means the
// build FAILED, empty-string means no failed run, and an API error means COULD
// NOT TELL — which must read as "keep waiting", never as "it failed". Collapsing
// the third into the first would abort a healthy build on one transient 403.
func TestPinBuildFailedParsesTheRunURL(t *testing.T) {
	prev := pinGH
	t.Cleanup(func() { pinGH = prev })

	for _, tc := range []struct {
		name, out string
		err       error
		wantURL   string
		wantFail  bool
	}{
		{name: "a failed run yields its URL", out: "https://github.com/o/r/actions/runs/9\n", wantURL: "https://github.com/o/r/actions/runs/9", wantFail: true},
		{name: "no failed run", out: "\n"},
		// The jq filter ends in `// ""`, so "no failed run" can arrive as a literal
		// two-character `""`. Reading that as a URL would abort a healthy build on
		// the very emptiness it signals. This case is here because the first cut of
		// the seam did exactly that, and the first cut of THIS TEST asserted the
		// bug — written to match the implementation instead of the requirement.
		{name: "jq's empty default is NOT a failure", out: `""` + "\n"},
		{name: "a non-URL answer is not a failure", out: "null\n"},
		{name: "an API error is NOT a failure", out: "", err: errBoom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinGH = func(string, ...string) ([]byte, error) { return []byte(tc.out), tc.err }
			url, failed := pinBuildFailed("tok", "o/r", "abc", "")
			if failed != tc.wantFail || url != tc.wantURL {
				t.Errorf("got (%q, %v), want (%q, %v)", url, failed, tc.wantURL, tc.wantFail)
			}
		})
	}
}

var errBoom = errBoomT{}

type errBoomT struct{}

func (errBoomT) Error() string { return "boom" }

// TestPinBuildFailedIsScopedToRunsSinceTheWatermark.
//
// Unbounded, this probe returned the first failed build for the sha EVER — so
// once any build for a commit had failed, `--build-if-missing` would dispatch a
// fresh one and then abort on poll zero against the corpse of the old one. The
// self-heal could never work for exactly the sha it exists for; the GHCR
// secondary rate-limit failure the seam's own header describes is a per-sha,
// permanent gravestone under the old query.
//
// The filter runs inside `gh api --jq`, so the assertion is on the QUERY: this
// test cannot host jq, and re-implementing the filter here would be testing a
// copy of it.
func TestPinBuildFailedIsScopedToRunsSinceTheWatermark(t *testing.T) {
	prev := pinGH
	t.Cleanup(func() { pinGH = prev })
	var jq string
	pinGH = func(_ string, args ...string) ([]byte, error) {
		for i, a := range args {
			if a == "--jq" && i+1 < len(args) {
				jq = args[i+1]
			}
		}
		return []byte("\n"), nil
	}

	pinBuildFailed("tok", "o/r", "abc", "2026-08-22T12:00:00Z")
	if !strings.Contains(jq, `created_at > "2026-08-22T12:00:00Z"`) {
		t.Errorf("the failure probe must exclude runs older than this invocation's dispatch; jq was:\n%s", jq)
	}

	// And an EMPTY watermark keeps the unbounded form, for callers with no
	// dispatch to anchor on. Silently filtering on the zero time would exclude
	// every run ever and turn the probe off.
	jq = ""
	pinBuildFailed("tok", "o/r", "abc", "")
	if strings.Contains(jq, "created_at") {
		t.Errorf("with no watermark the probe must stay unbounded; jq was:\n%s", jq)
	}
}

// TestTheWatermarkIsAnchoredToOurOwnDispatch.
//
// THE TRIGGER AND THE WAIT ARE ROUTINELY DIFFERENT PROCESSES. release-e2e's
// instantiate job dispatches the build with --trigger-only and a separate full
// invocation does the waiting, minutes afterwards. A watermark stamped at
// process start therefore sits AFTER the run being waited on, and the failure
// probe — whose entire job is to notice a build that died at its push step —
// filters that run straight out. The wait then burns its full budget on a corpse
// and returns the generic "may still be queued", which is the ~20-minute hang
// this file's header is about, reintroduced by the fix for it.
//
// The fake below implements the real jq semantics (`created_at > since`) rather
// than asserting on the query string: the point is not that a watermark is
// passed, it is that the failed run stays VISIBLE on the paths that did not
// dispatch it.
func TestTheWatermarkIsAnchoredToOurOwnDispatch(t *testing.T) {
	const runCreated = "2026-08-22T12:00:00Z"

	// A build ran for this sha and FAILED; this process starts an hour later and
	// dispatches nothing (`built && !BuildIfMissing` — the main/release path).
	setVars := stubPinSeams(t, 1, func(string) bool { return false })
	prevNow := pinNow
	t.Cleanup(func() { pinNow = prevNow })
	pinNow = func() time.Time { return mustTime(t, "2026-08-22T13:00:00Z") }

	var sawSince string
	pinBuildFailed = func(_, _, _, since string) (string, bool) {
		sawSince = since
		if since != "" && since > runCreated { // exactly what jq's filter does
			return "", false
		}
		return "https://gh/run/7", true
	}

	err := RunPinInstanceImages(baseOpts())
	if err == nil {
		t.Fatal("the image never published and its build failed — want an error")
	}
	if !strings.Contains(err.Error(), "https://gh/run/7") {
		t.Errorf("the failed build was invisible to the probe (watermark %q), so the wait fell through to the "+
			"generic timeout instead of naming the dead run: %v", sawSince, err)
	}
	if len(*setVars) != 0 {
		t.Errorf("nothing may be pinned when the build failed, got %v", *setVars)
	}
}

// TestADispatchWeMadeGetsAWatermarkBeforeIt — the other half. Self-heal only
// works if the corpse of an EARLIER failed build for the same sha is excluded,
// so a build this invocation dispatched must carry a watermark, and that
// watermark must sit before the dispatch (created_at is whole seconds, so an
// exact stamp loses to its own run under a strict `>`).
func TestADispatchWeMadeGetsAWatermarkBeforeIt(t *testing.T) {
	const now = "2026-08-22T13:00:00Z"
	calls := 0
	stubPinSeams(t, 1, func(string) bool { calls++; return calls > 2 })
	prevNow := pinNow
	t.Cleanup(func() { pinNow = prevNow })
	pinNow = func() time.Time { return mustTime(t, now) }

	triggered := false
	pinTriggerBuild = func(string, string, string, string) error { triggered = true; return nil }
	var sawSince string
	pinBuildFailed = func(_, _, _, since string) (string, bool) { sawSince = since; return "", false }

	o := baseOpts()
	o.BuildIfMissing, o.Ref = true, "main"
	if err := RunPinInstanceImages(o); err != nil {
		t.Fatalf("build-if-missing flow: %v", err)
	}
	if !triggered {
		t.Fatal("no dispatch happened — this test is not exercising the watermark")
	}
	if sawSince == "" {
		t.Fatal("a build WE dispatched must be watermarked, or an older failed run for the same sha " +
			"aborts the wait on poll zero and the self-heal can never work for the sha it exists for")
	}
	if sawSince >= now {
		t.Errorf("the watermark must sit strictly before the dispatch (created_at is whole seconds, so an "+
			"exact stamp fails jq's `>` against our own run); got %q with now=%q", sawSince, now)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}
