package main

// drift_mutation_test.go pins the parts of `llz drift` that decide whether the
// instance is reported as behind — and which upstream it was compared against.
// Mutation testing found these unguarded: the branch/repo-URL defaulting (compare
// the wrong ref and the answer is meaningless), the ls-remote line split, the
// short-SHA truncation, and the three conditional report lines (compare link,
// behind-count, GitHub Actions ::warning) that carry the finding to the operator.

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// writeDriftStamp drops a .template-version naming an arbitrary template repo,
// so the non-github (no compare link) shape is reachable too.
func writeDriftStamp(t *testing.T, repo, sha string) {
	t.Helper()
	b, err := json.Marshal(templateVersion{
		Schema: 1, TemplateRepo: repo, TemplateRef: "main", TemplateSHA: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".template-version", b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// driftGitRecorder is driftStub plus argument capture (so a test can assert WHICH
// remote and ref were queried) and a switch for commit reachability (which decides
// whether a behind-count is available at all).
func driftGitRecorder(latest string, reachable bool, calls *[][]string) func(string, ...string) ([]byte, error) {
	return func(_ string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "ls-remote"):
			return []byte(latest + "\trefs/heads/whatever\n"), nil
		case strings.Contains(joined, "rev-list"):
			return []byte("5\n"), nil
		default: // cat-file -e (commitReachable)
			if !reachable {
				return nil, errors.New("bad object")
			}
			return nil, nil
		}
	}
}

// lsRemoteArgs returns the args of the ls-remote call, which carry the two things
// that decide what "drift" even means: the repo URL and the branch ref.
func lsRemoteArgs(t *testing.T, calls [][]string) []string {
	t.Helper()
	for _, c := range calls {
		if len(c) > 0 && c[0] == "ls-remote" {
			return c
		}
	}
	t.Fatalf("no ls-remote call was made; calls=%v", calls)
	return nil
}

// noGHAEnv neutralizes the two Actions env vars so a workstation/CI difference
// can't decide the assertions.
func noGHAEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
}

// TestRunDriftComparesAgainstTheRequestedRefAndRepo: drift is only meaningful
// relative to the ref/remote asked for. Defaulting must fill the blanks WITHOUT
// overriding an explicit --branch / --repo-url.
func TestRunDriftComparesAgainstTheRequestedRefAndRepo(t *testing.T) {
	const sha = "aabbccdd11223344556677889900aabbccddeeff"
	cases := []struct {
		name     string
		branch   string
		repoURL  string
		wantRef  string
		wantRepo string
	}{
		{"empty branch defaults to main", "", "", "refs/heads/main", "https://github.com/akamai/lke-landing-zone.git"},
		{"explicit branch is honored", "release-2.0", "", "refs/heads/release-2.0", "https://github.com/akamai/lke-landing-zone.git"},
		{"explicit repo URL is honored", "main", "https://git.example.invalid/tpl.git", "refs/heads/main", "https://git.example.invalid/tpl.git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chdirTemp(t)
			noGHAEnv(t)
			writeTemplateVersion(t, sha)
			var calls [][]string
			withExecOutput(t, driftGitRecorder(sha, true, &calls))

			var err error
			captureStdout(t, func() { err = runDrift(c.branch, c.repoURL, true) })
			if err != nil {
				t.Fatalf("runDrift(up to date) = %v, want nil", err)
			}
			args := lsRemoteArgs(t, calls)
			if got := args[len(args)-1]; got != c.wantRef {
				t.Errorf("compared against ref %q, want %q", got, c.wantRef)
			}
			if got := args[1]; got != c.wantRepo {
				t.Errorf("compared against repo %q, want %q", got, c.wantRepo)
			}
		})
	}
}

// TestRunDriftUpToDateStrict: the ls-remote line is "<sha>\t<ref>", so the head
// SHA is only what precedes the first blank. Failing to split it makes every
// instance look drifted (and --strict fail) forever; the SHA is also reported in
// short form. The already-short case is the truncation boundary: a SHA at the
// short length is reported whole, never re-cut.
func TestRunDriftUpToDateStrict(t *testing.T) {
	for _, c := range []struct{ name, sha, want string }{
		{"full SHA is shortened", "aabbccdd11223344556677889900aabbccddeeff", "(aabbccdd)"},
		{"SHA already at short length is unchanged", "abcd1234", "(abcd1234)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			chdirTemp(t)
			noGHAEnv(t)
			writeTemplateVersion(t, c.sha)
			var calls [][]string
			withExecOutput(t, driftGitRecorder(c.sha, true, &calls))

			var err error
			out := captureStdout(t, func() { err = runDrift("main", "", true) })
			if err != nil {
				t.Fatalf("runDrift(up to date, strict) = %v, want nil — the ls-remote line must be split at the tab", err)
			}
			if want := "Up to date with akamai/lke-landing-zone@main " + c.want + "."; !strings.Contains(out, want) {
				t.Errorf("stdout missing %q:\n%s", want, out)
			}
		})
	}
}

// TestRunDriftDriftedReportsEveryChannel walks the drifted path with everything
// available: a github slug (compare link), reachable commits (behind-count), the
// Actions annotation, and the step summary.
func TestRunDriftDriftedReportsEveryChannel(t *testing.T) {
	const (
		old    = "1111111122223333444455556666777788889999"
		latest = "9999999988887777666655554444333322221111"
	)
	chdirTemp(t)
	writeTemplateVersion(t, old)
	t.Setenv("GITHUB_ACTIONS", "true")
	sumFile := "summary.md"
	if err := os.WriteFile(sumFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_STEP_SUMMARY", sumFile)

	var calls [][]string
	withExecOutput(t, driftGitRecorder(latest, true, &calls))

	var err error
	out := captureStdout(t, func() { err = runDrift("main", "", false) })
	if err != nil {
		t.Fatalf("runDrift(drifted, non-strict) = %v, want nil", err)
	}
	for _, want := range []string{
		"instance at 11111111,",                           // short form, not the full 40
		"head at 99999999 —",                              // ditto for the remote head
		"5 commit(s) behind akamai/lke-landing-zone@main", // the behind-count is folded in
		"Compare: https://github.com/akamai/lke-landing-zone/compare/" + old + "..." + latest,
		"::warning title=Template drift::Instance is 5 commit(s) behind",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}

	b, rerr := os.ReadFile(sumFile)
	if rerr != nil {
		t.Fatalf("step summary not written: %v", rerr)
	}
	for _, want := range []string{
		"| Instance template ref | `main` (11111111) |",
		"| Template main head | 99999999 |",
		"| Commits behind | 5 |",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("step summary missing %q:\n%s", want, string(b))
		}
	}
}

// TestRunDriftDriftedOmitsUnavailableChannels is the other side: a non-github
// template repo has no compare link, unreachable commits have no behind-count, and
// outside Actions there is no ::warning. Reporting them anyway (empty URL, bare
// "commit(s)", an annotation nothing consumes) is noise that reads as data.
func TestRunDriftDriftedOmitsUnavailableChannels(t *testing.T) {
	const (
		old    = "1111111122223333444455556666777788889999"
		latest = "9999999988887777666655554444333322221111"
	)
	chdirTemp(t)
	noGHAEnv(t)
	writeDriftStamp(t, "https://gitlab.example.com/acme/tpl.git", old)

	var calls [][]string
	withExecOutput(t, driftGitRecorder(latest, false, &calls)) // commits not reachable locally

	var err error
	out := captureStdout(t, func() { err = runDrift("main", "", false) })
	if err != nil {
		t.Fatalf("runDrift(drifted, non-strict) = %v, want nil", err)
	}
	if got := lsRemoteArgs(t, calls)[1]; got != "https://gitlab.example.com/acme/tpl.git" {
		t.Errorf("fetch URL = %q, want the template repo URL verbatim", got)
	}
	for _, unwanted := range []string{"Compare:", "commit(s)", "::warning"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("stdout should not mention %q when it is unavailable:\n%s", unwanted, out)
		}
	}
}
