package doctor

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
	if e.Name != "doctor-probes" || !e.Always {
		t.Errorf("identity drifted: name=%q always=%v", e.Name, e.Always)
	}
}

// The split is forced, not stylistic. A gate may hold read-repo and nothing else,
// so collapsing the halves would not merely over-grant the file scan — it would
// fail Validate outright.
func TestHalvesStaySplitByCapability(t *testing.T) {
	var gate, assertion *extension.Binding
	bs := Extension().Bindings
	for i := range bs {
		switch bs[i].Kind {
		case extension.Gate:
			gate = &bs[i]
		case extension.Assertion:
			assertion = &bs[i]
		}
	}
	if gate == nil || assertion == nil {
		t.Fatal("want one gate (cross-org reuse, files only) and one assertion (credential reach)")
	}
	for _, g := range gate.Grants {
		if g != extension.ReadRepo {
			t.Errorf("gate declared %q — the cross-org scan reads .github/workflows and nothing else", g)
		}
	}
	if !has(*assertion, extension.CloudRead) || !has(*assertion, extension.SecretRead) {
		t.Errorf("the credential-reach half must declare cloud-read and secret-read, got %v", assertion.Grants)
	}
	// It READS credentials and asks whether they still work. It places none.
	if has(*assertion, extension.SecretCustody) {
		t.Error("secret-custody declared — these probes read credential material, they do not place it")
	}
}

// The cross-org gate must not grow a network call: it is the only check here that
// runs before anything exists, and its value is that it needs nothing.
func TestCrossOrgGateStaysFilesOnly(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".", "crossorg.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"net/http", "os/exec", "linodego", "kubectlprobe"} {
		if strings.Contains(string(b), `"`+bad) || strings.Contains(string(b), bad+`"`) {
			t.Errorf("crossorg.go reaches %s — it backs the GATE binding, which may hold read-repo alone", bad)
		}
	}
}

func has(b extension.Binding, g extension.Grant) bool {
	for _, have := range b.Grants {
		if have == g {
			return true
		}
	}
	return false
}
