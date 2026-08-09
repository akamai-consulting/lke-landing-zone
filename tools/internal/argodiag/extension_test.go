package argodiag_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/argodiag"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := argodiag.Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "argocd-diagnostics" {
		t.Errorf("identity drifted: %q", e.Name)
	}
}

// The Incomplete note is the only thing standing between this declaration and a
// silent lie: the binding says `assertion`, and the command always exits 0. If the
// note is ever dropped, the declaration reads as complete and correct.
func TestKeepsTheWrongKindMarked(t *testing.T) {
	e := argodiag.Extension()
	if len(e.Incomplete) == 0 {
		t.Fatal("Incomplete was emptied — this extension's binding kind is knowingly wrong " +
			"(a diagnostic that never fails, declared as an assertion). Removing the note " +
			"makes an under-declaration invisible, which is the failure it exists to prevent")
	}
	if !strings.Contains(strings.ToLower(strings.Join(e.Incomplete, " ")), "diagnostic") {
		t.Error("the note no longer says what is missing")
	}
}

// cluster-read and nothing else — the one part of the declaration that is exactly
// true, and the part five consecutive `assert-` lanes got wrong.
func TestGrantsAreReadOnly(t *testing.T) {
	for _, g := range argodiag.Extension().Grants() {
		switch g {
		case extension.ClusterRead:
		default:
			t.Errorf("declared %q — this package only reads a cluster", g)
		}
	}
}

// The declaration's read-only claim is CHECKED, not asserted. A mutating kubectl
// verb reaching this package silently turns the grant line into a lie, which is
// exactly what happened to converge, assert-storage, assert-observability,
// assert-secrets and assert-identity in turn.
func TestPackageStaysReadOnly(t *testing.T) {
	mutating := regexp.MustCompile(`"(apply|patch|delete|create|replace|annotate|label|scale|edit|rollout|cordon|drain|taint)"`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := mutating.FindString(string(b)); m != "" {
			t.Errorf("%s passes a mutating kubectl verb (%s) — the declaration says cluster-read only", f, m)
		}
		if strings.Contains(string(b), "os.WriteFile") || strings.Contains(string(b), "os.RemoveAll") {
			t.Errorf("%s writes to disk — the declaration claims no write grant", f)
		}
	}
}
