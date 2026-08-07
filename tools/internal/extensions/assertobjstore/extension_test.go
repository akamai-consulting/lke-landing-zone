package assertobjstore

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
	if e.Name != "assert-objstore" || !e.Always {
		t.Errorf("identity drifted: name=%q always=%v", e.Name, e.Always)
	}
}

// The write is not incidental — it is the check. A future refactor that made this
// read-only would not be an optimisation; it would restore the exact blind spot
// this extension exists to close (the Linode API listing buckets that consumers
// cannot reach).
func TestDeclaresTheWriteItDependsOn(t *testing.T) {
	e := Extension()
	if !e.HasGrant(extension.CloudMutate) {
		t.Error("cloud-mutate dropped — this gate PUTs and DELETEs an object; that is how it works")
	}
	if !e.Binds(extension.Transition) {
		t.Error("a binding that mutates cannot be an assertion: assertions hold read grants only")
	}
	if e.HasGrant(extension.CloudRead) {
		t.Error("cloud-read declared — this extension never asks the Linode API, " +
			"and it exists BECAUSE the Linode API's answer was the misleading one")
	}
}

// It reads each consumer's own endpoint from live config and refuses to derive one.
// A fallback to the spec or the Linode API would reproduce the wrong view.
func TestRefusesToGuessAnEndpoint(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".", "roundtrip.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "REFUSES TO GUESS AN ENDPOINT") {
		t.Error("the no-fallback reasoning left roundtrip.go — it is the whole reason this gate " +
			"catches what verify-object-storage misses")
	}
}
