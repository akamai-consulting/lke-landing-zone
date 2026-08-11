package selfupgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
)

func TestSnapshotUpgradeOwnedRestoresOwnedButNotCopierAnswers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".template-manifest"), "owned owned.txt\nowned .copier-answers.yml\nmanaged managed.txt\n")
	writeFile(t, filepath.Join(dir, "owned.txt"), "operator\n")
	writeFile(t, filepath.Join(dir, ".copier-answers.yml"), "llz_version: v0.0.38\n")
	writeFile(t, filepath.Join(dir, "managed.txt"), "old template\n")
	gitInitRepo(t, dir, ".template-manifest", "owned.txt", ".copier-answers.yml", "managed.txt")
	chdir(t, dir)

	m, err := manifest.Load(".")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := SnapshotUpgradeOwned(m)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Cleanup()

	writeFile(t, filepath.Join(dir, "owned.txt"), "copier clobbered\n")
	writeFile(t, filepath.Join(dir, ".copier-answers.yml"), "llz_version: v0.0.39\n")
	if err := snap.restore(); err != nil {
		t.Fatal(err)
	}

	if got := mustRead(t, filepath.Join(dir, "owned.txt")); got != "operator\n" {
		t.Errorf("owned restored = %q", got)
	}
	if got := mustRead(t, filepath.Join(dir, ".copier-answers.yml")); got != "llz_version: v0.0.39\n" {
		t.Errorf("copier answers should not be restored, got %q", got)
	}
}

func TestCopierRenderArgvUsesAnswers(t *testing.T) {
	got := copierRenderArgv(&answers.File{SrcPath: "gh:my-org/lke-landing-zone", UpstreamOrg: "my-org", InstanceRepo: "my-org/inst"}, "v1.2.3", "/tmp/render")
	joined := strings.Join(got, " ")
	for _, want := range []string{"copier copy", "--force", "--vcs-ref v1.2.3", "upstream_org=my-org", "instance_repo=my-org/inst", "llz_version=v1.2.3", "gh:my-org/lke-landing-zone", "/tmp/render"} {
		if !strings.Contains(joined, want) {
			t.Errorf("copierRenderArgv missing %q in %q", want, joined)
		}
	}
}

// THE RENDER MUST RUN copier's _tasks, and this test used to assert the reverse.
//
// This render is the SOURCE every `managed` file is copied from during an upgrade,
// so it has to be the same artifact a fresh `llz new` at this ref produces — and
// the tasks are part of producing it: they deliver docs/, prune it to the operator
// set, and repoint the root-Markdown links that target template-only paths.
//
// With --skip-tasks it was not that artifact. The overwrite pass took AGENTS.md
// from a render where the link repoint had never run and laid it over the correct
// copy `copier update` had just produced, so every upgraded instance carried a
// dead relative link to docs/adopter-guide.md — a file deliver-docs prunes out of
// an instance — while every freshly scaffolded one was right. It survived each
// subsequent upgrade because the same pass re-applied it. `llz ci upgrade-test`'s
// converges-with-fresh check compares the two instances and found it immediately.
func TestCopierRenderArgvRunsTasks(t *testing.T) {
	got := copierRenderArgv(&answers.File{SrcPath: "/tmp/tmpl"}, "v1.2.3", "/tmp/render")
	for _, a := range got {
		if a == "--skip-tasks" {
			t.Fatalf("copierRenderArgv passes --skip-tasks: %v\n"+
				"The clean render is what `managed` files are overwritten FROM, so anything copier's tasks\n"+
				"write is absent from it — and the overwrite pass then reverts that content in every\n"+
				"upgraded instance while a fresh scaffold keeps it.", got)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
