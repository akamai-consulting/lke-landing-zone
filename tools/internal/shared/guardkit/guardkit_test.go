package guardkit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// reader is the fenced reader RepoPath now takes. A bare read-repo binding is
// what every gate declares, so it is what these tests exercise.
func reader(root string) capability.Repo {
	return capability.RepoAt(extension.Binding{
		Kind:   extension.Gate,
		State:  extension.Scaffolded,
		Grants: []extension.Grant{extension.ReadRepo},
	}, root)
}

// RepoPath's whole job is that the SAME relative path resolves in two layouts:
// the template repo, where trees sit at the root, and a rendered instance, where
// the scaffold's copy sits under instance-template/. Getting it wrong is silent —
// a raw join returns a path that does not exist, the walk tolerates a missing
// tree, and the guard reports green over nothing.
func TestRepoPathResolvesEitherLayout(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	mk("terraform-modules")
	if got, want := RepoPath(reader(root), "terraform-modules"), "terraform-modules"; got != want {
		t.Errorf("template layout: RepoPath = %q, want %q", got, want)
	}

	mk("instance-template/platform-apl")
	if got, want := RepoPath(reader(root), "platform-apl"), filepath.Join("instance-template", "platform-apl"); got != want {
		t.Errorf("instance layout: RepoPath = %q, want %q", got, want)
	}

	// Neither exists: return the direct join so the CALLER's missing-tree handling
	// runs. Returning the nested path instead would make every "not found" message
	// name a directory the reader has never heard of.
	if got, want := RepoPath(reader(root), "nowhere"), "nowhere"; got != want {
		t.Errorf("absent: RepoPath = %q, want the direct join %q", got, want)
	}
}

// The direct path WINS when both exist. A rendered instance left inside the
// template repo (`make instance-test` leaves one at .instance-test/) must not
// redirect a template-repo guard into the rendered copy.
func TestRepoPathPrefersTheDirectTree(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"platform-apl", "instance-template/platform-apl"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := RepoPath(reader(root), "platform-apl"), "platform-apl"; got != want {
		t.Errorf("RepoPath = %q, want the direct tree %q", got, want)
	}
}

func TestRequireCorpusFailsClosedOnZero(t *testing.T) {
	dirs := []string{"a/b", "c/d"}
	err := RequireCorpus("some-guard", 0, dirs)
	if err == nil {
		t.Fatal("an empty corpus must fail — a guard that examined nothing reports the same green as one that examined everything")
	}
	// The message has to name the guard AND the roots, because the actionable
	// question is always "which tree was not rendered".
	for _, want := range []string{"some-guard", "a/b", "c/d"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}
	if err := RequireCorpus("some-guard", 1, dirs); err != nil {
		t.Errorf("one examined file is a corpus: %v", err)
	}
}

// A reader that may not read resolves NOTHING, and returns the direct path rather
// than the instance-template one — the layout probe cannot run, so the honest
// answer is the caller's own spelling, whose subsequent read will refuse and say
// why. Returning the nested guess would name a directory that was never checked.
func TestRepoPathWithoutTheGrantDoesNotGuessTheLayout(t *testing.T) {
	if got, want := RepoPath(capability.DeniedRepo(), "platform-apl"), "platform-apl"; got != want {
		t.Errorf("RepoPath = %q, want %q", got, want)
	}
}
