package credcoverage

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("posture-credential-coverage does not validate: %v", err)
		}
	}
}

// A GATE IS THE STRICTEST CLAIM IN THE MODEL, so check it rather than assert it.
//
// A gate is defined by cost and reach — fast, local, files only — and the
// validator cannot see either: it checks what a binding DECLARES, not what its
// code does. `token-inventory`'s validate-tokens lane looked like a gate and was
// not, because it probes GitHub, Linode and S3 over the network. This package
// passes only while it stays on os.ReadFile.
//
// The catalog filed this extension under `invariant: operating`, which would mean
// continuous drift-detection against a live platform. It reaches nothing. If that
// ever changes, this test is what says so.
func TestPackageStaysFilesOnly(t *testing.T) {
	for _, forbidden := range []string{
		// no cluster
		"kubectlprobe.", "exec.Command", "exec.CommandContext",
		// no network
		"http.Get", "http.NewRequest", "http.DefaultClient",
		// no writes — a gate reports, it does not repair
		"os.WriteFile", "os.Create", "os.Remove", "os.RemoveAll", "os.Rename", "os.Mkdir", "os.MkdirAll",
	} {
		for _, f := range nonTestSources(t) {
			if containsCall(t, f, forbidden) {
				t.Errorf("%s calls %s — this package declares gate:scaffolded[read-repo], which "+
					"says fast, local, files only. If it now needs a cluster or a network it is an "+
					"ASSERTION, not a gate (see token-inventory's validate-tokens); if it needs to "+
					"write, that half belongs in cmd/llz (see promote-pipeline).", f, forbidden)
			}
		}
	}
}

// One binding, per the guard-charts rule: a split needs divergent CAPABILITY, not
// divergent subject matter. These two checks are the two ends of one question —
// does every ExternalSecret path resolve, and is every credential measured — and
// they hold the same grant at the same moment.
func TestOneQuestionOneBinding(t *testing.T) {
	e := Extension()
	if len(e.Bindings) != 1 {
		t.Fatalf("want exactly one binding — two checks, one question, one moment; got %v", e.Bindings)
	}
	b := e.Bindings[0]
	if b.Kind != extension.Gate || b.State != extension.Scaffolded {
		t.Errorf("binding = %s, want gate:scaffolded. The catalog filed this under "+
			"invariant:operating, which is wrong on its face: it reaches no cluster", b)
	}
	if len(b.Grants) != 1 || b.Grants[0] != extension.ReadRepo {
		t.Errorf("grants = %v, want [read-repo] only — a gate may hold nothing else", b.Grants)
	}
}
