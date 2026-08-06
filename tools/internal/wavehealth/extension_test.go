package wavehealth

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("wave-health does not validate: %v", err)
		}
	}
}

// A gate is the strictest claim in the model — fast, local, files only — and the
// validator cannot see any of those properties. The catalog filed this extension
// under `invariant: operating`, which would mean continuous drift-detection
// against a live platform; it reaches nothing. This is what says so.
func TestPackageStaysFilesOnly(t *testing.T) {
	for _, forbidden := range []string{
		"kubectlprobe.", "exec.Command", "exec.CommandContext",
		"http.Get", "http.NewRequest", "http.DefaultClient",
		"os.WriteFile", "os.Create", "os.Remove", "os.RemoveAll", "os.Rename", "os.Mkdir", "os.MkdirAll",
	} {
		for _, f := range nonTestSources(t) {
			if containsCall(t, f, forbidden) {
				t.Errorf("%s calls %s — this package declares gate:scaffolded[read-repo]: fast, "+
					"local, files only. A cluster or network reach makes it an ASSERTION; a write "+
					"makes it a transition. Either way the declaration is then false.", f, forbidden)
			}
		}
	}
}

func TestOneQuestionOneBinding(t *testing.T) {
	e := Extension()
	if len(e.Bindings) != 1 {
		t.Fatalf("want one binding — both checks ask whether the sync waves are coherent; got %v", e.Bindings)
	}
	b := e.Bindings[0]
	if b.Kind != extension.Gate || b.State != extension.Scaffolded {
		t.Errorf("binding = %s, want gate:scaffolded. The catalog filed this under "+
			"invariant:operating, which is wrong on its face: it reaches no cluster", b)
	}
	if len(b.Grants) != 1 || b.Grants[0] != extension.ReadRepo {
		t.Errorf("grants = %v, want [read-repo] only", b.Grants)
	}
}
