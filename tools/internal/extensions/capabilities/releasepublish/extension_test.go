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

// Only one of the two verbs in this package has a lifecycle state. Dropping the
// note would make the declaration read as complete while publish-charts sits here
// undeclared — the ban-by-omission failure Incomplete exists to prevent, and a
// novel one: this marks moved code that is not an extension, rather than code that
// has not moved.
func TestUnbindableHalfStaysMarked(t *testing.T) {
	inc := strings.ToLower(strings.Join(Extension().Incomplete, " "))
	if inc == "" {
		t.Fatal("Incomplete was emptied — publish-charts is in this package with no binding")
	}
	if !strings.Contains(inc, "publish-charts") {
		t.Error("the note no longer names the half that cannot be declared")
	}
	if len(Extension().Bindings) != 1 {
		t.Errorf("want exactly the pin-images binding, got %d — if publish-charts gained one, "+
			"it must have acquired an instance lifecycle state, which would be news",
			len(Extension().Bindings))
	}
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
