package llzver

// LatestRelease came down from selfupgrade at 0% covered, which is how it stayed
// uncovered for as long as it did: it shells out to `gh`, so the obvious reading
// is "not unit-testable". It is — kubectlprobe.Exec is a swappable seam, and every
// decision in the function is made on the BYTES that come back, not on the
// process. The filtering rule is the part worth pinning: a draft has no usable tag
// and a pre-release is an unpromoted e2e candidate (RELEASING.md), so serving
// either to `llz new` would pin a scaffold to a ref that can move or vanish.

import (
	"errors"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func withExec(t *testing.T, fn func(string, ...string) ([]byte, error)) {
	t.Helper()
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = orig })
}

func TestLatestReleaseSkipsDraftsPrereleasesAndTheCLITagTrack(t *testing.T) {
	var gotName string
	var gotArgs []string
	withExec(t, func(name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return []byte(`[
			{"tagName":"v0.0.50","isDraft":true,"isPrerelease":false},
			{"tagName":"v0.0.49","isDraft":false,"isPrerelease":true},
			{"tagName":"llz/v9.9.9","isDraft":false,"isPrerelease":false},
			{"tagName":"v0.0.40","isDraft":false,"isPrerelease":false},
			{"tagName":"v0.0.9","isDraft":false,"isPrerelease":false}
		]`), nil
	})

	got, err := LatestRelease("akamai-consulting/lke-landing-zone")
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	// v0.0.50 is a draft, v0.0.49 a pre-release, llz/v9.9.9 the CLI tag track;
	// v0.0.40 beats v0.0.9 numerically, which lexical ordering would get wrong.
	if got != "v0.0.40" {
		t.Errorf("LatestRelease = %q, want v0.0.40", got)
	}
	if gotName != "gh" {
		t.Errorf("shelled out to %q, want gh", gotName)
	}
	if len(gotArgs) == 0 || gotArgs[0] != "release" {
		t.Errorf("argv = %v, want a `gh release list …`", gotArgs)
	}
}

func TestLatestReleaseErrors(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		out        string
		err        error
	}{
		// The `gh` hint matters: an unauthenticated gh is by far the likeliest
		// cause, and without it the failure reads as "the repo has no releases".
		{name: "exec fails", err: errors.New("boom"), want: "is `gh` authenticated?"},
		{name: "unparseable json", out: "not json", want: "parse release list"},
		// Distinct from an empty list only in cause, not in effect: either way there
		// is nothing safe to pin, so the message must point at --ref.
		{name: "no full releases", out: `[{"tagName":"v1.0.0","isDraft":true,"isPrerelease":false}]`, want: "--ref"},
		{name: "empty list", out: `[]`, want: "--ref"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withExec(t, func(string, ...string) ([]byte, error) { return []byte(tc.out), tc.err })
			got, err := LatestRelease("o/r")
			if err == nil {
				t.Fatalf("want an error, got %q", got)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// execOutput is a one-line closure over the seam, and a closure is exactly the
// thing that is easy to get wrong by writing `var execOutput = kubectlprobe.Exec`
// — an assignment snapshots the seam at init and silently ignores every later
// swap, including the ones the tests above depend on.
func TestExecOutputReadsTheSeamAtCallTime(t *testing.T) {
	withExec(t, func(name string, args ...string) ([]byte, error) {
		return []byte("swapped"), nil
	})
	out, err := execOutput("anything")
	if err != nil || string(out) != "swapped" {
		t.Errorf("execOutput = %q, %v — want the swapped seam's output", out, err)
	}
}
