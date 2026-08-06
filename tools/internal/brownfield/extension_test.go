package brownfield

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("import-brownfield does not validate: %v", err)
		}
	}
}

// THE FIRST OPT-IN EXTENSION. Seven shipped `always` before this one, which left
// the field with only one observed value — a default nobody had ever set the other
// way. Adoption is the case that settles it: a greenfield instance has nothing to
// import, so this capability is needed once or never.
func TestImportIsOptIn(t *testing.T) {
	if Extension().Always {
		t.Error("import-brownfield must not ship enabled: a greenfield instance has nothing " +
			"to adopt, and this is the first extension that exercises alwaysEnabled: false")
	}
}

// `own-paths` IS REACHABLE — the correction to what template-sustain suggested.
//
// That extraction concluded own-paths might be the one grant no extension can
// hold, because the copier restore pass reads .template-manifest's class table and
// ADR 0014 pins that as core. Import shows the distinction: WRITING a file the
// manifest classes `owned` needs no access to the class table. The grant is a
// fence, and declaring a fence is not the same as enforcing one.
func TestOwnPathsIsHeldAtScaffolded(t *testing.T) {
	var found bool
	for _, b := range Extension().Bindings {
		for _, g := range b.Grants {
			if g != extension.OwnPaths {
				continue
			}
			found = true
			if b.Kind != extension.Transition || b.State != extension.Scaffolded {
				t.Errorf("own-paths is held by %s — the grant is only meaningful on a "+
					"transition to scaffolded or upgraded, where copier runs", b)
			}
		}
	}
	if !found {
		t.Error("import writes the instance repo; without own-paths, copier would re-render " +
			"and 3-way-merge the files it just authored")
	}
}

// TWO TRANSITIONS AT TWO MOMENTS. The catalog records that an earlier draft
// declared a single transition to `provisioned` holding own-paths, and that the
// validator refused it. Pin the shape so the collapse cannot come back: neither
// binding may hold the other's grant.
func TestTheTwoHalvesStaySeparate(t *testing.T) {
	e := Extension()
	if len(e.Bindings) != 2 {
		t.Fatalf("want two transitions, got %d", len(e.Bindings))
	}
	for _, b := range e.Bindings {
		if b.Kind != extension.Transition {
			t.Errorf("%s: import ACTS at both moments; both bindings are transitions", b)
		}
		var hasOwn, hasMutate bool
		for _, g := range b.Grants {
			switch g {
			case extension.OwnPaths:
				hasOwn = true
			case extension.CloudMutate:
				hasMutate = true
			}
		}
		if hasOwn && hasMutate {
			t.Errorf("%s holds both own-paths and cloud-mutate — that is the collapsed "+
				"single-transition shape the validator refused", b)
		}
	}

	// And the collapse is still refused, not merely absent.
	collapsed := extension.Extension{
		Name: "probe", Short: "x",
		Bindings: []extension.Binding{{
			Kind: extension.Transition, State: extension.Provisioned,
			Grants: []extension.Grant{extension.OwnPaths, extension.CloudMutate},
		}},
	}
	if errs := collapsed.Validate(); len(errs) == 0 {
		t.Error("own-paths at provisioned must still be refused — it is only meaningful " +
			"where files are written")
	}
}
