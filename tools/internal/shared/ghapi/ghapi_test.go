package ghapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghcli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

type pageShape struct {
	TotalCount int      `json:"total_count"`
	Names      []string `json:"names"`
	//lint:ignore U1000 exercised by reflection, not by name: MergeJSONPages walks
	// every field and must skip this one (CanSet is false) rather than panic.
	unexported int
}

// The whole point of the helper: a second page must not be lost. Page-1
// truncation is the bug it exists to prevent, and the one it could most easily
// reintroduce.
func TestMergeJSONPagesKeepsEveryPage(t *testing.T) {
	pages := []json.RawMessage{
		json.RawMessage(`{"total_count":3,"names":["a","b"]}`),
		json.RawMessage(`{"total_count":3,"names":["c"]}`),
	}
	var got pageShape
	if err := MergeJSONPages(pages, &got); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if strings.Join(got.Names, ",") != "a,b,c" {
		t.Errorf("names = %v, want a,b,c", got.Names)
	}
	// The scalar is carried, not zeroed — it is the field you would use to CHECK
	// that pagination was complete, so dropping it defeats the audit.
	if got.TotalCount != 3 {
		t.Errorf("total_count = %d, want 3 (a dropped scalar hides an incomplete merge)", got.TotalCount)
	}
}

func TestMergeJSONPagesEdgeCases(t *testing.T) {
	t.Run("single page", func(t *testing.T) {
		var got pageShape
		if err := MergeJSONPages([]json.RawMessage{json.RawMessage(`{"total_count":1,"names":["x"]}`)}, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Names) != 1 || got.TotalCount != 1 {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("no pages leaves the zero value", func(t *testing.T) {
		var got pageShape
		if err := MergeJSONPages(nil, &got); err != nil {
			t.Fatal(err)
		}
		if got.Names != nil || got.TotalCount != 0 {
			t.Errorf("got %+v, want zero", got)
		}
	})
	t.Run("a malformed page errors rather than contributing nothing", func(t *testing.T) {
		var got pageShape
		if err := MergeJSONPages([]json.RawMessage{json.RawMessage(`{"names":`)}, &got); err == nil {
			t.Error("a truncated page must not merge silently")
		}
	})
	// The documented limitation. Asserted so a future caller hits a clear error
	// rather than a silent no-op.
	t.Run("a non-struct target is refused", func(t *testing.T) {
		var got []string
		err := MergeJSONPages([]json.RawMessage{json.RawMessage(`["a"]`)}, &got)
		if err == nil || !strings.Contains(err.Error(), "pointer to struct") {
			t.Errorf("err = %v, want a pointer-to-struct refusal", err)
		}
	})
	t.Run("unexported fields are skipped, not panicked on", func(t *testing.T) {
		var got pageShape
		if err := MergeJSONPages([]json.RawMessage{json.RawMessage(`{"names":["a"]}`)}, &got); err != nil {
			t.Fatalf("reflection must not panic or error on an unexported field: %v", err)
		}
	})
}

// An older `gh` rejects --slurp at parse time (exit 1, no stdout). That used to
// surface as "could not determine whether the passphrase exists" with no way
// forward; it must degrade to an unpaged read instead.
func TestGHAPIJSONPagedFallsBackWhenSlurpIsUnknown(t *testing.T) {
	var sawPaginate, sawPlain bool
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "--slurp" {
				sawPaginate = true
				return nil, errors.New("unknown flag: --slurp")
			}
		}
		sawPlain = true
		return []byte(`{"total_count":1,"names":["a"]}`), nil
	})
	var got pageShape
	if err := GHAPIJSONPaged("repos/o/r/environments", &got); err != nil {
		t.Fatalf("an older gh must degrade to one page, not hard-stop: %v", err)
	}
	if !sawPaginate || !sawPlain {
		t.Errorf("expected the --slurp attempt then the unpaged retry (paginate=%v plain=%v)", sawPaginate, sawPlain)
	}
	if len(got.Names) != 1 {
		t.Errorf("fallback lost the payload: %+v", got)
	}
}

// Any OTHER error is a real failure and must propagate — degrading a 403 to an
// unpaged read would turn an indefinite answer into a definite one.
func TestGHAPIJSONPagedPropagatesRealErrors(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("gh: Forbidden (HTTP 403)")
	})
	var got pageShape
	if err := GHAPIJSONPaged("repos/o/r/environments", &got); err == nil {
		t.Fatal("a 403 must propagate, not degrade to a single page")
	}
}

// withExecOutput swaps the capture seam. A LOCAL COPY, as everywhere else in this
// tree: a stream-swapping helper cannot live in a shared package without
// shipping `testing` into production code. It swaps kubectlprobe.Exec directly
// because that is what this package calls -- buildpreflight's version wrapped a
// package-local execOutput closure that did not come with the move.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = orig })
}

func withLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	orig := kubectlprobe.LookPathFn
	kubectlprobe.LookPathFn = fn
	t.Cleanup(func() { kubectlprobe.LookPathFn = orig })
}

// THE THREE-WAY ANSWER IS THE WHOLE POINT. "reachable", "definitively absent" and
// "could not tell" are different, and collapsing the third into the second is what
// makes a tool confidently tell an operator their repo does not exist when in fact
// `gh` was never authenticated.
func TestRepoStatusSeparatesAbsentFromIndeterminate(t *testing.T) {
	withLookPath(t, func(f string) (string, error) { return "/usr/bin/" + f, nil })

	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, nil })
	if found, err := RepoStatus("o/r"); !found || err != nil {
		t.Errorf("reachable = (%v, %v), want (true, nil)", found, err)
	}
	if !RepoExists("o/r") {
		t.Error("RepoExists = false for a reachable repo")
	}

	// A 404 is definitive: absent, no error.
	//
	// A REAL *exec.ExitError, not a fabricated one. ghcli.NotFound reads
	// ee.Stderr — the bytes the process actually wrote — so an error whose STRING
	// says "HTTP 404" does not match. That is the right design (a message can say
	// anything; the captured stderr is evidence) and it means the fixture has to
	// produce a genuine failed process.
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		_, err := exec.Command("sh", "-c", "echo 'gh: Not Found (HTTP 404)' >&2; exit 1").Output()
		return nil, err
	})
	if found, err := RepoStatus("o/r"); found || err != nil {
		t.Errorf("404 = (%v, %v), want (false, nil)", found, err)
	}
	if RepoExists("o/r") {
		t.Error("RepoExists = true for a 404")
	}

	// Anything else is indeterminate and MUST surface as an error rather than
	// false — an unauthenticated gh answering nothing is not proof of absence.
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("gh auth login required")
	})
	found, err := RepoStatus("o/r")
	if found || err == nil {
		t.Errorf("unauthenticated = (%v, %v), want (false, non-nil)", found, err)
	}
	if RepoExists("o/r") {
		t.Error("RepoExists must be false when the answer is unknown")
	}
}

func TestRepoStatusRefusesBeforeShellingOutToAnAbsentGh(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", fmt.Errorf("not found in $PATH") })
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		t.Error("shelled out to a gh that is not installed")
		return nil, nil
	})
	if _, err := RepoStatus("o/r"); err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Errorf("missing gh = %v, want a not-on-PATH error", err)
	}
}

// RequireInstanceRepo NO-OPS when gh is absent, which reads as backwards until you
// see why: it gates `llz tokens` and `llz doctor`, and refusing to run those on a
// machine without gh would block work that does not need GitHub at all.
func TestRequireInstanceRepoIsSilentWithoutGh(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", fmt.Errorf("nope") })
	if err := RequireInstanceRepo("o/r"); err != nil {
		t.Errorf("no gh on PATH = %v, want nil (nothing could be checked)", err)
	}
}

// The remediation text is the deliverable, not a detail: an operator hitting this
// is blocked, and the message decides whether their next move is the right one.
func TestRemediateMissingRepoDistinguishesTheThreeCauses(t *testing.T) {
	withLookPath(t, func(f string) (string, error) { return "/usr/bin/" + f, nil })

	origOwner := ghcli.OwnerKindFn
	t.Cleanup(func() { ghcli.OwnerKindFn = origOwner })

	// Owner EXISTS: the cause is visibility or spelling of the repo, and the
	// message must not tell them to create an org they already have.
	ghcli.OwnerKindFn = func(string) (string, error) { return "Organization", nil }
	out := captureStderr(t, func() { RemediateMissingRepo("o/r") })
	if !strings.Contains(out, "gh auth status") {
		t.Errorf("existing owner: %q should point at auth status — GitHub 404s a private "+
			"repo you cannot see exactly as it 404s one that is absent", out)
	}
	if strings.Contains(out, "organizations/new") {
		t.Errorf("existing owner: %q must not suggest creating the org", out)
	}

	// Owner ABSENT: `gh repo create` makes a repository and never the org that
	// holds it, so the create line alone dead-ends on a permissions error.
	ghcli.OwnerKindFn = func(string) (string, error) { return "", nil }
	out = captureStderr(t, func() { RemediateMissingRepo("nosuchorg/r") })
	for _, want := range []string{"OWNER", "nosuchorg", ".copier-answers.yml"} {
		if !strings.Contains(out, want) {
			t.Errorf("absent owner: %q missing %q", out, want)
		}
	}
}

func TestRequireInstanceRepoErrorsWhenTheRepoIsAbsent(t *testing.T) {
	withLookPath(t, func(f string) (string, error) { return "/usr/bin/" + f, nil })
	origOwner := ghcli.OwnerKindFn
	t.Cleanup(func() { ghcli.OwnerKindFn = origOwner })
	ghcli.OwnerKindFn = func(string) (string, error) { return "User", nil }
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		_, err := exec.Command("sh", "-c", "echo 'gh: Not Found (HTTP 404)' >&2; exit 1").Output()
		return nil, err
	})
	var err error
	_ = captureStderr(t, func() { err = RequireInstanceRepo("o/r") })
	if err == nil || !strings.Contains(err.Error(), "not visible to your `gh` login") {
		t.Errorf("absent repo = %v, want an error naming the gh login", err)
	}

	// An INDETERMINATE answer must not be reported as absence — "nothing was
	// checked" is its own outcome and the message says so.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("gh auth login") })
	err = RequireInstanceRepo("o/r")
	if err == nil || strings.Contains(err.Error(), "not visible") {
		t.Errorf("unreachable gh = %v, want an unreachable error rather than an absence claim", err)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	b, _ := io.ReadAll(r)
	return string(b)
}
