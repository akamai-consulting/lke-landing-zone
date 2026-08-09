package chartpublish_test

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/chartpublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := chartpublish.Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "chart-publish" || !e.Always {
		t.Errorf("identity drifted: name=%q always=%v", e.Name, e.Always)
	}
}

// The split is forced, not stylistic: the default run reads the registry and
// reports, while --publish-if-missing dispatches a workflow. An assertion may hold
// READ grants only, so collapsing these into one binding would widen it to the
// union and claim the write on every run — the exact over-granting that scoping
// grants per binding exists to prevent.
func TestReadAndWriteHalvesStaySeparate(t *testing.T) {
	var assertion, transition *extension.Binding
	for i, b := range chartpublish.Extension().Bindings {
		bb := chartpublish.Extension().Bindings[i]
		switch b.Kind {
		case extension.Assertion:
			assertion = &bb
		case extension.Transition:
			transition = &bb
		}
	}
	if assertion == nil || transition == nil {
		t.Fatal("want one assertion (verify) and one transition (publish)")
	}
	if has(*assertion, extension.CloudMutate) {
		t.Error("the verify half claimed cloud-mutate — it only reads the registry")
	}
	if !has(*transition, extension.CloudMutate) {
		t.Error("the publish half must declare cloud-mutate: `gh workflow run` is a GitHub write")
	}
	for _, b := range []extension.Binding{*assertion, *transition} {
		if b.Name == "" {
			t.Errorf("%s: two bindings at the same state need names", b)
		}
		if b.State != extension.Configured {
			t.Errorf("%s: both halves attach at `configured` — the pins are an INPUT that must resolve", b)
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
