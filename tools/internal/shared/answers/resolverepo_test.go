package answers_test

// ResolveInstanceRepo arrived from cmd/llz/tokens.go untested — four callers, and
// every one of them relied on the fallback ORDER being right without anything
// pinning it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
)

func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return dir
}

func TestResolveInstanceRepoPrefersTheExplicitFlag(t *testing.T) {
	dir := chdirTemp(t)
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"),
		[]byte("instance_repo: from/answers\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := answers.ResolveInstanceRepo("explicit/flag", false)
	if err != nil || got != "explicit/flag" {
		t.Fatalf("= (%q, %v); an explicit --repo must win over the answers file", got, err)
	}
}

func TestResolveInstanceRepoFallsBackToTheAnswersFile(t *testing.T) {
	dir := chdirTemp(t)
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"),
		[]byte("instance_repo: from/answers\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := answers.ResolveInstanceRepo("", false)
	if err != nil || got != "from/answers" {
		t.Fatalf("= (%q, %v), want from/answers", got, err)
	}
}

func TestResolveInstanceRepoAdminFallbackIsTheTemplateExample(t *testing.T) {
	chdirTemp(t) // no answers file
	got, err := answers.ResolveInstanceRepo("", true)
	if err != nil {
		t.Fatal(err)
	}
	if want := templateid.ExampleRepo(); got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

// The non-admin miss must be an ERROR, not a silent guess. Resolving to the
// template's own example repo for an ordinary user would point their tokens and
// their build at somebody else's repository.
func TestResolveInstanceRepoRefusesRatherThanGuessing(t *testing.T) {
	chdirTemp(t)
	_, err := answers.ResolveInstanceRepo("", false)
	if err == nil {
		t.Fatal("expected an error when the repo cannot be determined")
	}
	if !strings.Contains(err.Error(), "--repo") {
		t.Errorf("the error must name the remedy, got %q", err)
	}
}
