package sustain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemplateVersion drops a .template-version into the current dir.
func writeTemplateVersion(t *testing.T, sha string) {
	t.Helper()
	b, err := json.Marshal(TemplateVersion{
		Schema: 1, TemplateRepo: "akamai/lke-landing-zone", TemplateRef: "main", TemplateSHA: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".template-version", b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// chdirTemp moves into a fresh temp dir for the duration of the test.

// driftStub dispatches the git shell-outs runDrift makes: ls-remote returns the
// remote head, cat-file reports reachable, rev-list returns the behind-count.
//
// TAG LOOKUPS RESOLVE TO NOTHING unless the caller supplies tags. Answering every
// ls-remote with the branch head — which this fake used to do — would make a pin
// resolve to the head no matter what it named, i.e. "up to date" always, hiding
// the drift the strict case asserts. A fake that says yes to every ref cannot
// tell the two apart.
func driftStub(latest string, tags ...map[string]string) func(string, ...string) ([]byte, error) {
	tagRefs := map[string]string{}
	if len(tags) > 0 {
		tagRefs = tags[0]
	}
	return func(_ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "ls-remote") && strings.Contains(joined, "refs/tags/"):
			ref := args[len(args)-1]
			name := strings.TrimSuffix(strings.TrimPrefix(ref, "refs/tags/"), "^{}")
			if sha, ok := tagRefs[name]; ok {
				return []byte(sha + "\t" + ref + "\n"), nil
			}
			return nil, nil // no such tag, as git reports it
		case strings.Contains(joined, "ls-remote"):
			return []byte(latest + "\trefs/heads/main\n"), nil
		case strings.Contains(joined, "rev-list"):
			return []byte("5\n"), nil
		default: // cat-file -e (commitReachable)
			return nil, nil
		}
	}
}

func TestRunDriftUpToDate(t *testing.T) {
	chdirTemp(t)
	writeTemplateVersion(t, "abcd1234")
	d := testDeps(t)
	d.Exec = driftStub("abcd1234") // remote head == stamped sha

	var err error
	captureStdout(t, func() { err = RunDrift(d, "main", "", false) })
	if err != nil {
		t.Errorf("RunDrift(d, up to date) = %v, want nil", err)
	}
}

func TestRunDriftDrifted(t *testing.T) {
	chdirTemp(t)
	writeTemplateVersion(t, "oldsha00")
	d := testDeps(t)
	d.Exec = driftStub("newsha99") // remote head moved ahead

	// Non-strict: drift is reported but not an error.
	var err error
	captureStdout(t, func() { err = RunDrift(d, "main", "", false) })
	if err != nil {
		t.Errorf("RunDrift(d, drifted, non-strict) = %v, want nil", err)
	}
	// Strict: drift is a hard failure.
	captureStdout(t, func() { err = RunDrift(d, "main", "", true) })
	if err == nil {
		t.Error("RunDrift(d, drifted, strict) = nil, want error")
	}
}

func TestRunDriftMissingFile(t *testing.T) {
	d := testDeps(t)
	chdirTemp(t)
	// No .template-version present.
	if err := RunDrift(d, "main", "", false); err == nil {
		t.Error("RunDrift(d, no .template-version) = nil, want error")
	}
}

func TestRunDriftMalformed(t *testing.T) {
	d := testDeps(t)
	chdirTemp(t)
	if err := os.WriteFile(filepath.Join(".", ".template-version"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunDrift(d, "main", "", false); err == nil {
		t.Error("RunDrift(d, malformed) = nil, want error")
	}
}

// TestRunDriftTagPinnedOnHead is the regression: copier records `_commit:` as a
// release TAG on the normal adopter path, so the recorded pin is `v0.0.42` and
// the branch head is a 40-hex sha. Comparing those as strings can only be
// unequal, and every tag-pinned instance sitting exactly on main's head was told
// it was behind — with a compare link showing no commits. A permanent warning is
// one nobody reads, and --strict turned it into a failing scheduled gate.
func TestRunDriftTagPinnedOnHead(t *testing.T) {
	chdirTemp(t)
	const head = "e5b8c5354874ad7d8a35caf664a5efd94e6ed3cc"
	writeTemplateVersion(t, "v0.0.42")
	d := testDeps(t)
	d.Exec = driftStub(head, map[string]string{"v0.0.42": head}) // the tag IS the head

	var err error
	out := captureStdout(t, func() { err = RunDrift(d, "main", "", true) })
	if err != nil {
		t.Errorf("RunDrift(tag pinned at head, strict) = %v, want nil — the instance is current", err)
	}
	if !strings.Contains(out, "Up to date") {
		t.Errorf("output = %q, want it to report up to date", out)
	}
}

// TestRunDriftTagPinnedBehind pins the other direction, so the fix above cannot
// be "everything is up to date": a tag that resolves to an OLDER commit than the
// branch head is still drift, and still fails --strict.
func TestRunDriftTagPinnedBehind(t *testing.T) {
	chdirTemp(t)
	writeTemplateVersion(t, "v0.0.41")
	d := testDeps(t)
	d.Exec = driftStub("e5b8c5354874ad7d8a35caf664a5efd94e6ed3cc",
		map[string]string{"v0.0.41": "1111111111111111111111111111111111111111"})

	var err error
	captureStdout(t, func() { err = RunDrift(d, "main", "", true) })
	if err == nil {
		t.Error("RunDrift(tag pinned behind head, strict) = nil, want error")
	}
}
