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

func TestCopierRenderArgvUsesAnswersAndSkipsTasks(t *testing.T) {
	got := copierRenderArgv(&answers.File{SrcPath: "gh:my-org/lke-landing-zone", UpstreamOrg: "my-org", InstanceRepo: "my-org/inst"}, "v1.2.3", "/tmp/render")
	joined := strings.Join(got, " ")
	for _, want := range []string{"copier copy", "--skip-tasks", "--force", "--vcs-ref v1.2.3", "upstream_org=my-org", "instance_repo=my-org/inst", "llz_version=v1.2.3", "gh:my-org/lke-landing-zone", "/tmp/render"} {
		if !strings.Contains(joined, want) {
			t.Errorf("copierRenderArgv missing %q in %q", want, joined)
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
