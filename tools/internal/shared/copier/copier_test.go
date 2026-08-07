package copier_test

// Ref is the function with a RULE in it, and the rule is load-bearing: a scaffold
// must never float on `main`. These arrived from cmd/llz with their coverage
// spread across commands_test.go; the fallback ORDER now has its own test.

import (
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/copier"
)

func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := copier.Version
	copier.Version = v
	t.Cleanup(func() { copier.Version = prev })
}

func withLatestRelease(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	prev := copier.LatestReleaseFn
	copier.LatestReleaseFn = fn
	t.Cleanup(func() { copier.LatestReleaseFn = prev })
}

func TestRefPrefersAnExplicitFlag(t *testing.T) {
	withVersion(t, "v1.2.3")
	got, err := copier.Ref("v9.9.9", "acme/tpl")
	if err != nil || got != "v9.9.9" {
		t.Fatalf("= (%q, %v); an explicit --ref must win", got, err)
	}
}

func TestRefAnchorsToThisBinaryWhenItIsARelease(t *testing.T) {
	withVersion(t, "v1.2.3")
	withLatestRelease(t, func(string) (string, error) {
		t.Fatal("must not ask the registry when this binary is itself a release")
		return "", nil
	})
	got, err := copier.Ref("", "acme/tpl")
	if err != nil || got != "v1.2.3" {
		t.Fatalf("= (%q, %v), want v1.2.3", got, err)
	}
}

func TestRefFallsBackToTheLatestReleaseOnADevBuild(t *testing.T) {
	withVersion(t, "dev")
	withLatestRelease(t, func(repo string) (string, error) {
		if repo != "acme/tpl" {
			t.Errorf("asked about %q", repo)
		}
		return "v0.9.0", nil
	})
	got, err := copier.Ref("", "acme/tpl")
	if err != nil || got != "v0.9.0" {
		t.Fatalf("= (%q, %v), want v0.9.0", got, err)
	}
}

// The whole point: a dev build that cannot reach the registry must ERROR, never
// fall through to an unpinned scaffold. tflint rejects an unpinned module source,
// Renovate cannot bump it, and copier's own validator refuses it — so an empty ref
// fails much later and somewhere unrecognisable.
func TestRefRefusesRatherThanFloatingOnMain(t *testing.T) {
	withVersion(t, "dev")
	withLatestRelease(t, func(string) (string, error) { return "", errors.New("no network") })
	got, err := copier.Ref("", "acme/tpl")
	if err == nil {
		t.Fatalf("expected an error, got ref %q", got)
	}
	if !strings.Contains(err.Error(), "--ref") {
		t.Errorf("the error must name the remedy, got %q", err)
	}
}

func TestCopyAndUpdateArgvCarryTheRef(t *testing.T) {
	cp := strings.Join(copier.CopyArgv("acme", "v1.0.0", "dir"), " ")
	if !strings.Contains(cp, "v1.0.0") || !strings.Contains(cp, "copier") {
		t.Errorf("CopyArgv = %q", cp)
	}
	up := strings.Join(copier.UpdateArgv("v2.0.0"), " ")
	if !strings.Contains(up, "v2.0.0") {
		t.Errorf("UpdateArgv = %q", up)
	}
}
