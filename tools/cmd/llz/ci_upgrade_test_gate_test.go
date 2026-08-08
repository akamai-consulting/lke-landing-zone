package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/copier"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/upgrade"
)

// TestCopierUpdateArgvIsNonInteractive is the unit-speed twin of the gate's
// `update-is-noninteractive` check.
//
// It exists because the expensive check cannot run everywhere: `llz ci
// upgrade-test` needs copier, git tags, and ~90s, and self-skips without them.
// This one runs in microseconds on every `go test`, so the flag cannot be
// dropped and rediscovered by an adopter — which is how it was found the first
// time. Losing --defaults makes `llz upgrade` re-prompt, and with no terminal
// that is an unhandled prompt_toolkit exception rather than a prompt.
func TestCopierUpdateArgvIsNonInteractive(t *testing.T) {
	for _, ref := range []string{"v0.0.40", ""} {
		argv := copier.UpdateArgv(ref)
		if !containsArg(argv, "--defaults") {
			t.Errorf("copier.UpdateArgv(%q) = %v\n"+
				"missing --defaults: `copier update` re-asks every question without it, which is an\n"+
				"unhandled exception in CI/scripts/ssh and three silent re-answer prompts by hand.", ref, argv)
		}
		if !containsArg(argv, "--trust") {
			t.Errorf("copier.UpdateArgv(%q) lost --trust; the template's _tasks would not run: %v", ref, argv)
		}
	}
	// The ref is what the upgrade exists to move. A bare update floats the code to
	// the latest tag while leaving llz_version stale, which is the skew the
	// explicit --data pin prevents.
	if argv := copier.UpdateArgv("v0.0.40"); !containsArg(argv, "llz_version=v0.0.40") {
		t.Errorf("copier.UpdateArgv did not pin llz_version: %v", argv)
	}
}

// `llz new` legitimately prompts for its three answers — only the UPDATE path
// must be silent. Pinning that keeps a future "make everything non-interactive"
// sweep from turning the scaffold's questions into silent defaults.
func TestCopierCopyArgvStillPrompts(t *testing.T) {
	if argv := copier.CopyArgv("acme", "v0.0.40", "dest"); containsArg(argv, "--defaults") {
		t.Errorf("copier.CopyArgv gained --defaults: `llz new` must ASK for instance_repo, not "+
			"scaffold silently onto the placeholder: %v", argv)
	}
}

func TestCopierScaffoldArgv(t *testing.T) {
	argv := upgrade.CopierScaffoldArgv("/tmp/tmpl", "v0.0.39", "/tmp/out/instance",
		map[string]string{"instance_repo": "probe-org/probe-instance", "openbao_team": "probe-team"})

	// The template must be addressed as a PATH, so the gate runs offline and
	// against the tree under test rather than whatever is published on GitHub.
	if argv[len(argv)-2] != "/tmp/tmpl" || argv[len(argv)-1] != "/tmp/out/instance" {
		t.Errorf("template/dest are not the last two args: %v", argv)
	}
	// Every probe answer has to reach copier, or `answers-preserved` degrades into
	// asserting that the DEFAULTS were preserved — which they trivially are.
	for _, want := range []string{
		"instance_repo=probe-org/probe-instance",
		"openbao_team=probe-team",
		"llz_version=v0.0.39",
	} {
		if !containsArg(argv, want) {
			t.Errorf("scaffold argv is missing --data %s: %v", want, argv)
		}
	}
	// The harness itself must never block on a prompt.
	if !containsArg(argv, "--defaults") {
		t.Errorf("scaffold argv would prompt: %v", argv)
	}
	// Deterministic ordering, so a failure diff is stable across runs.
	if got := upgrade.CopierScaffoldArgv("/t", "v1.0.0", "/d",
		map[string]string{"b": "2", "a": "1"}); !containsArg(got, "a=1") ||
		indexOfArg(got, "a=1") > indexOfArg(got, "b=2") {
		t.Errorf("answers not emitted in sorted order: %v", got)
	}
}

func indexOfArg(argv []string, want string) int {
	for i, a := range argv {
		if a == want {
			return i
		}
	}
	return -1
}

func TestPreviousReleaseTag(t *testing.T) {
	// Numeric ordering, not string: "v0.0.9" > "v0.0.10" lexically, and picking the
	// wrong one quietly narrows what the gate covers.
	t.Run("picks the highest release numerically", func(t *testing.T) {
		got, ok := upgrade.PreviousReleaseTag([]string{"v0.0.9", "v0.0.10", "v0.0.2"}, nil)
		if !ok || got != "v0.0.10" {
			t.Errorf("upgrade.PreviousReleaseTag = %q,%v; want v0.0.10", got, ok)
		}
	})

	// THE case this argument exists for. Cutting a release tags the commit under
	// test, and "upgrade v0.0.40 → v0.0.40" is a no-op that passes while testing
	// nothing — a color.Green gate meaning least on the run that matters most.
	t.Run("skips a tag on the commit under test", func(t *testing.T) {
		got, ok := upgrade.PreviousReleaseTag(
			[]string{"v0.0.39", "v0.0.40"}, map[string]bool{"v0.0.40": true})
		if !ok || got != "v0.0.39" {
			t.Errorf("upgrade.PreviousReleaseTag = %q,%v; want v0.0.39 — the release being cut is not something to upgrade FROM", got, ok)
		}
	})

	// Delegated to llzver.LatestLLZTag, so pre-releases and the retired llz/v* track are
	// excluded by the same rule `llz self-update` and `llz new` apply.
	t.Run("ignores pre-releases and the legacy tag track", func(t *testing.T) {
		got, ok := upgrade.PreviousReleaseTag([]string{"v0.0.39", "v0.0.41-rc1", "llz/v9.9.9"}, nil)
		if !ok || got != "v0.0.39" {
			t.Errorf("upgrade.PreviousReleaseTag = %q,%v; want v0.0.39", got, ok)
		}
	})

	t.Run("reports not-ok on a clone with no release tags", func(t *testing.T) {
		if got, ok := upgrade.PreviousReleaseTag([]string{"main", ""}, nil); ok {
			t.Errorf("upgrade.PreviousReleaseTag = %q,%v; want not-ok so the gate skips rather than inventing a release", got, ok)
		}
	})
}

func TestMergeConflictArtifacts(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("clean.yaml", "a: 1\nb: 2\n")
	write("broken.yaml", "a: 1\n<<<<<<< before\nb: 2\n=======\nb: 3\n>>>>>>> after\n")
	write("values.yaml.rej", "@@ hunk @@\n")
	// A .git directory holds copier's own merge bookkeeping and is not the
	// instance's content; scanning it would report artifacts that ship nowhere.
	write(".git/MERGE_MSG", "<<<<<<< HEAD\n")

	markers, rejects, err := upgrade.MergeConflictArtifacts(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 1 || !strings.HasPrefix(markers[0], "broken.yaml:") {
		t.Errorf("markers = %v; want just broken.yaml", markers)
	}
	if len(rejects) != 1 || rejects[0] != "values.yaml.rej" {
		t.Errorf("rejects = %v; want just values.yaml.rej", rejects)
	}
}

func TestIndentedTail(t *testing.T) {
	// The tail, because copier's message and a traceback's exception line are LAST
	// while the head is a wall of file-creation noise.
	got := upgrade.IndentedTail("create a\ncreate b\nOSError: [Errno 22]", 2)
	if strings.Contains(got, "create a") {
		t.Errorf("upgrade.IndentedTail kept the head: %q", got)
	}
	if !strings.Contains(got, "OSError") {
		t.Errorf("upgrade.IndentedTail dropped the exception line: %q", got)
	}
	for _, ln := range strings.Split(got, "\n") {
		if !strings.HasPrefix(ln, "      ") {
			t.Errorf("line %q is not indented into the report", ln)
		}
	}
	if got := upgrade.IndentedTail("", 5); !strings.Contains(got, "no output") {
		t.Errorf("upgrade.IndentedTail(\"\") = %q; want it to say there was no output", got)
	}
}

func TestShortRef(t *testing.T) {
	const sha = "a8fc6a768bb16862eb2b4c5719b5c26b7ca82ce4"
	if got := upgrade.ShortRef(sha); got != "a8fc6a768bb1" {
		t.Errorf("upgrade.ShortRef(sha) = %q", got)
	}
	// A tag is already short and meaningful — truncating it would turn v0.0.40 into
	// noise.
	if got := upgrade.ShortRef("v0.0.40"); got != "v0.0.40" {
		t.Errorf("upgrade.ShortRef(tag) = %q; want it left alone", got)
	}
}
