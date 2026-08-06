package plaintext

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "posture-plaintext" || !e.Always {
		t.Errorf("identity drifted: name=%q always=%v", e.Name, e.Always)
	}
}

// A gate is the strictest claim in the model: files only, no cluster, no cloud, no
// credential. Checked rather than asserted — the same pin posture-credential-coverage
// carries, and the reason a gate can be trusted without reading it.
func TestPackageStaysFilesOnly(t *testing.T) {
	banned := []string{"net/http", "os/exec", "k8s.io/", "linodego", "kubectlprobe"}
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
		for _, bad := range banned {
			if strings.Contains(string(b), `"`+bad) || strings.Contains(string(b), bad+`"`) {
				t.Errorf("%s reaches %s — a gate may hold read-repo and nothing else", f, bad)
			}
		}
	}
}

func TestGrantsAreReadRepoOnly(t *testing.T) {
	for _, g := range Extension().Grants() {
		if g != extension.ReadRepo {
			t.Errorf("declared %q — a gate may only read the repo", g)
		}
	}
}

// THE EXTRACTION'S SCAR. The self-exemption used to name two BASENAMES, so moving
// this guard out of package main silently re-enabled it against itself: it
// reported every example URL in its own registry as an unregistered hop, and
// simultaneously called the real entries stale, because the keys are file paths
// and the path had changed. A guard that scans the tree it lives in must survive
// being moved within that tree.
func TestGuardExemptsItselfByDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.ToSlash(wd)+"/", guardOwnDir) {
		t.Fatalf("this package now lives outside %q (%s) — the self-exemption no longer "+
			"matches its own source, so the guard will report its own registry as findings "+
			"and call every real entry stale. Update guardOwnDir with the move", guardOwnDir, wd)
	}
}
