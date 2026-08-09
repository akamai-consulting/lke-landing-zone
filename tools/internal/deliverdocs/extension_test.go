package deliverdocs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/deliverdocs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := deliverdocs.Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "deliver-docs" || !e.Always {
		t.Errorf("identity drifted: name=%q always=%v", e.Name, e.Always)
	}
}

// The two bindings are the two moments copier runs, and the NAMES are load-bearing
// rather than decorative: without them the model permits only one transition per
// state, and these attach to different states for the same reason a name is needed
// when they do not.
func TestBindsBothCopierMoments(t *testing.T) {
	got := map[extension.State]extension.Binding{}
	for _, b := range deliverdocs.Extension().Bindings {
		if b.Kind != extension.Transition {
			t.Errorf("%s: this extension changes the tree; every binding is a transition", b)
		}
		if b.Name == "" {
			t.Errorf("%s: repeated attachments need names", b)
		}
		got[b.State] = b
	}
	for _, s := range []extension.State{extension.Scaffolded, extension.Upgraded} {
		b, ok := got[s]
		if !ok {
			t.Fatalf("no binding at %q — copier's _tasks fire at BOTH render and update, "+
				"so dropping one silently under-declares when this runs", s)
		}
		if !hasGrant(b, extension.WriteRepo) || !hasGrant(b, extension.ReadRepo) {
			t.Errorf("%s: wants read-repo and write-repo, got %v", b, b.Grants)
		}
	}
	if len(got) != 2 {
		t.Errorf("want exactly the two copier moments, got %d bindings", len(got))
	}
}

// own-paths is the grant this extension looks most like and must NOT hold: it is a
// fence against `copier update` re-rendering a file, and docs/ is classed `managed`
// — copier rewrites it wholesale every time, which is precisely what this verb runs
// after. Claiming the fence would be backwards, not merely over-granted.
func TestDoesNotClaimTheCopierFence(t *testing.T) {
	if deliverdocs.Extension().HasGrant(extension.OwnPaths) {
		t.Error("deliver-docs claimed own-paths — it PRUNES what copier renders and wants the re-render; " +
			"the fence would stop the thing it depends on")
	}
}

// The keep-set decides what an instance carries and lives in internal/docsguard,
// next to the guard that validates every link as it resolves inside the pruned
// tree. A second copy here would let the two drift, which is the drift that leaked
// cross-org-reuse-pattern.md into the e2e instance and broke instantiate.
func TestKeepSetIsNotDuplicatedHere(t *testing.T) {
	for _, name := range sourceFiles(t) {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "DeliveredDocs = ") {
			t.Errorf("%s defines its own keep-set — it belongs once, in internal/docsguard", name)
		}
	}
}

func sourceFiles(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range all {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	return out
}

func hasGrant(b extension.Binding, g extension.Grant) bool {
	for _, have := range b.Grants {
		if have == g {
			return true
		}
	}
	return false
}
