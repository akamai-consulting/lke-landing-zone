package gitcmd

// Five packages had written this and none of them had tested it. The two
// behaviours worth pinning are the ones the callers depend on without saying so:
// the `-C dir` placement, and the fact that Out SWALLOWS the error.

import (
	"errors"
	"reflect"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func withExec(t *testing.T, fn func(string, ...string) ([]byte, error)) {
	t.Helper()
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = orig })
}

func TestOutputPassesDirAsMinusC(t *testing.T) {
	var gotName string
	var gotArgs []string
	withExec(t, func(name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return []byte("  deadbeef\n"), nil
	})

	got, err := Output("/repo", "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	// Trimmed: every caller compares the result to a ref or a sha, and a trailing
	// newline makes all of those comparisons false.
	if got != "deadbeef" {
		t.Errorf("Output = %q, want it trimmed to deadbeef", got)
	}
	if gotName != "git" {
		t.Errorf("ran %q, want git", gotName)
	}
	// `-C dir` FIRST and before the subcommand. git only accepts it there, so the
	// ordering is load-bearing rather than stylistic.
	want := []string{"-C", "/repo", "rev-parse", "HEAD"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("argv = %v, want %v", gotArgs, want)
	}
}

func TestOutputPropagatesTheError(t *testing.T) {
	withExec(t, func(string, ...string) ([]byte, error) { return nil, errors.New("not a repo") })
	if _, err := Output("/nope", "status"); err == nil {
		t.Error("Output must return the error — its callers branch on it")
	}
}

func TestOutSwallowsTheErrorByDesign(t *testing.T) {
	// This is the difference between the two, and it is deliberate. Every caller of
	// Out is reading a stamp where "not a git checkout" and "no such ref" are both
	// simply unknown, so an error return would be discarded at all of them.
	withExec(t, func(string, ...string) ([]byte, error) { return nil, errors.New("boom") })
	if got := Out("rev-parse", "HEAD"); got != "" {
		t.Errorf("Out = %q on failure, want the empty string", got)
	}

	var gotArgs []string
	withExec(t, func(_ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("v1.2.3\n"), nil
	})
	if got := Out("describe", "--tags"); got != "v1.2.3" {
		t.Errorf("Out = %q, want v1.2.3", got)
	}
	// No -C: Out runs in the current directory, which is what separates it from
	// Output and why both exist.
	if !reflect.DeepEqual(gotArgs, []string{"describe", "--tags"}) {
		t.Errorf("Out argv = %v, want no -C prefix", gotArgs)
	}
}
