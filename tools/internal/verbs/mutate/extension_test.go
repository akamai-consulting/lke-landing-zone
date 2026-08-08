package mutate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "dev-mutation-testing" {
		t.Errorf("identity drifted: %q", e.Name)
	}
}

// A mutation run takes minutes. It must never ship enabled: `Always` is a default
// rather than a constant, so an instance can still turn it on, but the default
// keeps the slowest thing in the catalog out of every instance's critical path —
// which is the only reason it can call itself a gate at all.
func TestStaysOptIn(t *testing.T) {
	if Extension().Always {
		t.Error("dev-mutation-testing became always-on — a mutation run is minutes where every " +
			"other gate is milliseconds, and a gate is defined partly by being cheap enough to " +
			"run before you attempt the state")
	}
}

// Files only. `go test` and `gremlins` are local processes over local source;
// neither reaches a network, which is why read-repo is the whole grant.
func TestStaysFilesOnly(t *testing.T) {
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
		for _, bad := range []string{"net/http", "k8s.io/", "linodego", "kubectlprobe"} {
			if strings.Contains(string(b), `"`+bad) {
				t.Errorf("%s reaches %s — a gate may hold read-repo and nothing else", f, bad)
			}
		}
	}
	for _, g := range Extension().Grants() {
		if g != extension.ReadRepo {
			t.Errorf("declared %q — this runs local processes over local source", g)
		}
	}
}
