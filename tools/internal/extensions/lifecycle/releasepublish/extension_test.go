package releasepublish

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "release-publish" {
		t.Errorf("identity drifted: %q", e.Name)
	}
}

// Two of the three verbs in this package have a lifecycle state. Dropping the note
// would make the declaration read as complete while publish-charts sits here
// undeclared — the ban-by-omission failure Incomplete exists to prevent, and a
// novel one: this marks moved code that is not an extension, rather than code that
// has not moved.
//
// THE BINDINGS ARE CHECKED BY NAME, NOT BY COUNT. This asserted `len(Bindings) == 1`
// until assert-pr-gates arrived, and a count could only say "something changed" —
// it failed on the ARRIVAL of a correctly-declared binding while being unable to
// notice publish-charts quietly acquiring one, which is the single thing it was
// written to catch. The named set says exactly that and stays true as verbs are
// added.
func TestUnbindableHalfStaysMarked(t *testing.T) {
	inc := strings.ToLower(strings.Join(Extension().Incomplete, " "))
	if inc == "" {
		t.Fatal("Incomplete was emptied — publish-charts is in this package with no binding")
	}
	if !strings.Contains(inc, "publish-charts") {
		t.Error("the note no longer names the half that cannot be declared")
	}
	want := map[string]bool{"pin-images": true, "assert-pr-gates": true}
	for _, b := range Extension().Bindings {
		if !want[b.Name] {
			t.Errorf("undeclared binding %q — if publish-charts gained one, it must have acquired "+
				"an instance lifecycle state, which would be news", b.Name)
		}
		delete(want, b.Name)
	}
	for name := range want {
		t.Errorf("binding %q disappeared from the declaration", name)
	}
}

// It clones, commits and pushes a branch to the instance repo to produce the
// pull_request its gates are gated on. An assertion may hold read grants only, so
// despite the name this is a transition — see extension.go's header.
func TestAssertPRGatesDeclaresItsWrite(t *testing.T) {
	e := Extension()
	if !e.HasGrant(extension.WriteRepo) {
		t.Error("write-repo dropped — assert-instance-pr-gates pushes a branch to the instance repo")
	}
	for _, b := range e.Bindings {
		if b.Name != "assert-pr-gates" {
			continue
		}
		if b.Kind != extension.Transition {
			t.Errorf("assert-pr-gates is %q — a binding that pushes a commit cannot be an assertion", b.Kind)
		}
		return
	}
	t.Error("the assert-pr-gates binding is gone")
}

// It writes GitHub repository variables. An assertion may hold read grants only,
// so the kind follows from the grant rather than from taste.
func TestPinImagesDeclaresItsWrite(t *testing.T) {
	e := Extension()
	if !e.HasGrant(extension.CloudMutate) {
		t.Error("cloud-mutate dropped — pin-instance-images sets TF_IMAGE/KUBE_IMAGE on the instance repo")
	}
	if !e.Binds(extension.Transition) {
		t.Error("a binding that mutates cannot be an assertion")
	}
	if e.Always {
		t.Error("release-publish became always-on — it is release-lane machinery, not instance lifecycle")
	}
}
