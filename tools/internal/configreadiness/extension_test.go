package configreadiness

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("config-readiness does not validate: %v", err)
		}
	}
}

// It reads the repo AND asks GitHub and Linode what is actually configured, so it
// is an assertion rather than a gate. The line matters: a gate may hold read-repo
// and nothing else, and declaring one here would be claiming this runs in the fast
// pre-commit path over files alone.
func TestConfigReadinessIsTheConfiguredPredicate(t *testing.T) {
	e := Extension()
	if len(e.Bindings) != 1 {
		t.Fatalf("want one binding — one question, one moment; got %v", e.Bindings)
	}
	b := e.Bindings[0]
	if b.Kind != extension.Assertion {
		t.Errorf("kind = %s, want assertion — it queries GitHub and Linode, so it is not a "+
			"gate however local its other half is", b.Kind)
	}
	if b.State != extension.Configured {
		t.Errorf("state = %s, want configured — this is the predicate that state is named "+
			"for, and every later state presumes its answer", b.State)
	}
	for _, g := range b.Grants {
		switch g {
		case extension.ReadRepo, extension.CloudRead, extension.SecretRead:
		default:
			t.Errorf("holds %q — readiness REPORTS, it does not repair. A mutating grant here "+
				"means something started fixing what it found", g)
		}
	}
}
