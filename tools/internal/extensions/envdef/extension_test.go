package envdef_test

// This package is now ONLY a declaration -- the writer moved to
// internal/shared/envdef when the in-degree sweep found three peers importing it.
// Like instance-resolve before it, that left a package whose coverage floor was
// measuring code that had gone, and no test of the one thing still here.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/envdef"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := envdef.Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "env-definition" {
		t.Errorf("identity drifted: %q", e.Name)
	}
	if len(e.Incomplete) != 0 {
		t.Errorf("an Incomplete note appeared (%v) — this package is a declaration and "+
			"nothing else, so a gap here is a gap in the declaration itself", e.Incomplete)
	}
}

// write-repo AT `scaffolded` IS THE LOAD-BEARING PART, and it is one of only two
// states that grant may be asked for. This extension writes landingzone.yaml and
// environments/<env>.yaml into the operator's checked-in tree — files they will
// read a diff of — which is exactly the case write-repo was eventually added for
// after three refusals.
func TestItDeclaresTheRepoWriteItActuallyPerforms(t *testing.T) {
	e := envdef.Extension()
	if len(e.Bindings) != 1 {
		t.Fatalf("want exactly one binding, got %v", e.Bindings)
	}
	b := e.Bindings[0]
	if b.Kind != extension.Transition || b.State != extension.Scaffolded {
		t.Errorf("binding is %s, want transition:scaffolded", b)
	}
	var writes bool
	for _, g := range b.Grants {
		if g == extension.WriteRepo {
			writes = true
		}
	}
	if !writes {
		t.Error("write-repo is gone — this package's whole job is writing two spec files " +
			"into the instance repo, and dropping the grant hides that from the reviewer")
	}

	// The grant is fenced to {scaffolded, upgraded}; asking elsewhere must still fail.
	elsewhere := extension.Extension{
		Name: "probe", Short: "x",
		Bindings: []extension.Binding{{
			Kind: extension.Transition, State: extension.Converged,
			Grants: []extension.Grant{extension.WriteRepo},
		}},
	}
	if errs := elsewhere.Validate(); len(errs) == 0 {
		t.Error("write-repo became legal at `converged` — the two-state fence is what makes " +
			"this declaration mean something specific")
	}
}
