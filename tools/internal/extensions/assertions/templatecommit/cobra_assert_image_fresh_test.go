package templatecommit

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const (
	// The verdict lines. Asserted as literals rather than by calling the code that
	// prints them: the bug (#428) was a run emitting BOTH, so a test that reuses the
	// producer would agree with whichever it reused.
	okVerdict   = "assert-image-fresh: OK"
	skipVerdict = "assert-image-fresh: SKIPPED"
)

// captureOutput runs fn with both streams redirected and returns what each got.
// The verdict is a printed LINE, so nothing short of reading the streams tests it —
// asserting on the returned error only ever saw nil for both a pass and a skip,
// which is exactly how the two came to be indistinguishable.
func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	fn()
	wOut.Close()
	wErr.Close()
	o, _ := io.ReadAll(rOut)
	e, _ := io.ReadAll(rErr)
	return string(o), string(e)
}

func TestRunAssertImageFresh(t *testing.T) {
	const sha = "0d634d7d54a138314be21d0891c376fbae99519a"
	const other = "7ec07dc7929384cf393bbde98002d7089097e673"
	// Nothing resolves: the guard must behave exactly as it did before the tag
	// resolution existed, i.e. skip + pass rather than fail on an unanswered question.
	unresolvable := func(string, string) (string, bool) { return "", false }
	cases := []struct {
		name, baked, ref string
		resolve          func(repo, ref string) (string, bool)
		wantErr          string // substring; "" means no error
		wantVerdict      string // the line the run must print; "" for the error cases
	}{
		{"dev matches full sha", "dev-" + sha, sha, unresolvable, "", okVerdict},
		{"dev matches short ref", "dev-" + sha, sha[:12], unresolvable, "", okVerdict},
		{"dev short build matches full ref", "dev-" + sha[:12], sha, unresolvable, "", okVerdict},
		{"dev sha mismatch", "dev-" + sha, other, unresolvable, "image/template skew", ""},
		{"dev vs unresolvable ref skips", "dev-" + sha, "main", unresolvable, "", skipVerdict},
		{"unstamped dev skips", "dev", sha, unresolvable, "", skipVerdict},
		{"empty Version skips", "", sha, unresolvable, "", skipVerdict},
		{"release tag matches", "v1.2.3", "v1.2.3", unresolvable, "", okVerdict},
		{"release tag mismatch", "v1.2.3", "v1.2.4", unresolvable, "image/template skew", ""},
		{"release vs sha skips", "v1.2.3", sha, unresolvable, "", skipVerdict},
		{"unresolvable ref errors", "dev-" + sha, "", unresolvable, "cannot resolve the template ref", ""},

		// The case this guard used to skip — a release-tag pin against a dev image.
		// It is the DEFAULT shape of a fresh instance, so both outcomes must be right.
		{"tag resolving to the baked commit passes", "dev-" + sha, "v0.0.39",
			func(string, string) (string, bool) { return sha, true }, "", okVerdict},
		{"tag resolving to a different commit fails", "dev-" + sha, "v0.0.39",
			func(string, string) (string, bool) { return other, true }, "image/template skew", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The unstamped cases are fatal in CI and skipped outside it, and this
			// suite itself runs under GITHUB_ACTIONS — so the local and CI runs of the
			// same test would disagree unless the arm is pinned here.
			t.Setenv("GITHUB_ACTIONS", "")
			stubTemplateCommit(t, tc.resolve)
			var err error
			out, _ := captureOutput(t, func() { err = runAssertImageFresh(tc.baked, tc.ref, "acme/tmpl") })
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("runAssertImageFresh(%q,%q) = %v, want nil", tc.baked, tc.ref, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runAssertImageFresh(%q,%q) = %v, want error containing %q", tc.baked, tc.ref, err, tc.wantErr)
			}
			if tc.wantVerdict == "" {
				if strings.Contains(out, okVerdict) {
					t.Errorf("a failing run printed an OK verdict:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tc.wantVerdict) {
				t.Errorf("runAssertImageFresh(%q,%q) printed %q, want a %q line", tc.baked, tc.ref, out, tc.wantVerdict)
			}
			// THE BUG: a skipped run also printed the OK line, and the OK line came
			// second, so it read as the verdict.
			if tc.wantVerdict == skipVerdict && strings.Contains(out, okVerdict) {
				t.Errorf("a SKIPPED run also claimed OK — the two cannot both be true:\n%s", out)
			}
		})
	}
}

// The three inputs the guard cannot compare, each asserted to produce a SKIP and
// NOT an OK — the direct pin of #428, where an unstamped "dev" build was warned
// about as unusable and then reported as matching a commit.
func TestSkippedRunNeverClaimsOK(t *testing.T) {
	const sha = "0d634d7d54a138314be21d0891c376fbae99519a"
	cases := []struct{ name, baked, ref string }{
		// Both directions of the unstamped case: against a SHA pin and against a tag
		// pin, which take different arms (the tag pin resolves first).
		{"unstamped against a sha pin", "dev", sha},
		{"unstamped against a tag pin", "dev", "v0.0.44"},
		{"empty stamp against a sha pin", "", sha},
		{"unresolvable ref", "dev-" + sha, "main"},
		{"release build against a sha pin", "v1.2.3", sha},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", "")
			stubTemplateCommit(t, func(string, string) (string, bool) { return "", false })
			var err error
			out, errOut := captureOutput(t, func() { err = runAssertImageFresh(tc.baked, tc.ref, "acme/tmpl") })
			if err != nil {
				t.Fatalf("want a skip (pass), got %v", err)
			}
			if strings.Contains(out, okVerdict) {
				t.Errorf("skipped run printed an OK verdict:\n%s", out)
			}
			if !strings.Contains(out, skipVerdict) {
				t.Errorf("skipped run printed no SKIPPED verdict on stdout:\n%s", out)
			}
			// The annotation is what surfaces in the Actions UI; losing it would make a
			// skip invisible to everyone not reading the log.
			if !strings.Contains(errOut, "::warning::assert-image-fresh:") {
				t.Errorf("skipped run emitted no ::warning:: annotation:\n%s", errOut)
			}
		})
	}
}

// In CI the binary comes from the pinned ci image, and every published image stamps
// its version — so an unstamped build there is a stamping regression (it has
// happened: PR #433) and the guard is OFF. Skipping would report that as a pass.
func TestUnstampedIsFatalInCI(t *testing.T) {
	const sha = "0d634d7d54a138314be21d0891c376fbae99519a"
	// Whitespace included: the stamp arrives from an ldflag and a padded one is
	// still unstamped, so trimming must not be the difference between fatal and skip.
	for _, tc := range []struct{ name, baked string }{
		{"literal dev", "dev"},
		{"empty stamp", ""},
		{"padded dev", "  dev  "},
	} {
		baked := tc.baked
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", "true")
			stubTemplateCommit(t, func(string, string) (string, bool) { return "", false })
			var err error
			out, _ := captureOutput(t, func() { err = runAssertImageFresh(baked, sha, "acme/tmpl") })
			if err == nil {
				t.Fatalf("unstamped %q in CI must FAIL, got nil (output: %q)", baked, out)
			}
			if strings.Contains(out, okVerdict) {
				t.Errorf("a failing run printed an OK verdict:\n%s", out)
			}
			// Both causes and both remediations, because the wrong one wastes a day.
			for _, want := range []string{
				"no version stamp",
				"llz tokens --env <deployment> --yes",
				"dockerfiles/Dockerfile",
				"internal/cli.Version",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("unstamped-in-CI message missing %q:\n%v", want, err)
				}
			}
		})
	}
}

// Outside CI the same input is a local `go build`, which must still pass — the
// guard does not block a developer on a stamp only the release build sets.
func TestUnstampedOutsideCISkips(t *testing.T) {
	const sha = "0d634d7d54a138314be21d0891c376fbae99519a"
	t.Setenv("GITHUB_ACTIONS", "")
	stubTemplateCommit(t, func(string, string) (string, bool) { return "", false })
	var err error
	out, _ := captureOutput(t, func() { err = runAssertImageFresh("dev", sha, "acme/tmpl") })
	if err != nil {
		t.Fatalf("local dev build must not fail the guard: %v", err)
	}
	if !strings.Contains(out, skipVerdict) || strings.Contains(out, okVerdict) {
		t.Errorf("want a SKIPPED verdict and no OK, got:\n%s", out)
	}
}

// The message is the whole product here: the operator who hits this has just been
// told by `llz render --check` to run a command that cannot help them. It has to
// carry the resolved commit (so the tag and the sha can be reconciled by eye) and
// the exact re-pin, or they are left guessing again.
func TestAssertImageFreshSkewMessageIsActionable(t *testing.T) {
	const baked = "0d634d7d54a138314be21d0891c376fbae99519a"
	const pin = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"
	stubTemplateCommit(t, func(string, string) (string, bool) { return pin, true })

	err := runAssertImageFresh("dev-"+baked, "v0.0.39", "acme/tmpl")
	if err == nil {
		t.Fatal("want skew error")
	}
	for _, want := range []string{
		"v0.0.39 (= " + pin + ")", // the tag AND what it resolves to
		"gh variable set TF_IMAGE",
		"gh variable set KUBE_IMAGE",
		"ci-tofu:sha-" + pin,
		"ci-kubernetes:sha-" + pin,
		"llz render --check", // names the symptom they actually saw
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("skew message missing %q:\n%v", want, err)
		}
	}
}

func TestAssertImageFreshCmdWiring(t *testing.T) {
	c := AssertImageFreshCmd()
	if c.Use != "assert-image-fresh" {
		t.Errorf("Use = %q", c.Use)
	}
	// The flag survives as an escape hatch, but must NOT be required: the workflow
	// now runs the verb bare and the ref comes from the instance's own pin.
	f := c.Flags().Lookup("template-ref")
	if f == nil {
		t.Fatal("missing --template-ref override flag")
	}
	if a := f.Annotations[cobra.BashCompOneRequiredFlag]; len(a) > 0 && a[0] == "true" {
		t.Error("--template-ref must not be required — the pin is resolved from the instance")
	}
}
