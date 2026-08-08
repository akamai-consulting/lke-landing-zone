package configreadiness

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("config-readiness does not validate: %v", err)
		}
	}
}

func bindings(t *testing.T) map[string]extension.Binding {
	t.Helper()
	byName := map[string]extension.Binding{}
	for _, b := range Extension().Bindings {
		byName[b.Name] = b
	}
	return byName
}

// It reads the repo AND asks GitHub and Linode what is actually configured, so it
// is an assertion rather than a gate. The line matters: a gate may hold read-repo
// and nothing else, and declaring one here would be claiming this runs in the fast
// pre-commit path over files alone.
func TestConfigReadinessIsTheConfiguredPredicate(t *testing.T) {
	b := bindings(t)["build-ready"]
	if b.Kind != extension.Assertion {
		t.Errorf("kind = %s, want assertion — it queries GitHub and Linode, so it is not a "+
			"gate however local its other half is", b.Kind)
	}
	if b.State != extension.Configured {
		t.Errorf("state = %s, want configured — this is the predicate that state is named "+
			"for, and every later state presumes its answer", b.State)
	}
}

// `inputs-resolve` ARRIVED HERE FROM A DELETED EXTENSION, and this pins why it is
// shaped the way it is.
//
// It was `instance-resolve`: a package holding a declaration and no code, after its
// four resolvers moved to internal/shared/instanceresolve. A declaration with no
// code is a label on a library, so the package went — but the CLAIM is true, and
// deleting it would have removed the record while leaving the behaviour. It came
// here because both bindings ask one question at two moments: does this instance's
// configuration resolve?
func TestInputsResolveIsAnAssertionAndNotAGate(t *testing.T) {
	b := bindings(t)["inputs-resolve"]
	if b.Kind != extension.Assertion {
		t.Errorf("kind = %s, want assertion. The original author tried `gate` and Validate() "+
			"refused it: a gate permits read-repo and NOTHING else, because it runs in the "+
			"fast pre-commit path over files alone. region_resolve and objcluster_resolve "+
			"call the Linode API — which is the point of them, since the hardcoded region "+
			"list they replaced went stale. Needing the network disqualifies a gate however "+
			"read-only it is", b.Kind)
	}
	if b.State != extension.Scaffolded {
		t.Errorf("state = %s, want scaffolded — the resolvers answer before anything is "+
			"rendered, which is earlier than `configured`", b.State)
	}
}

// Neither binding may repair what it measures, and they must not share a grant set.
// Folding them into one would hand the offline resolvers `secret-read`, a grant
// they never use — the over-granting argument reconcile-actions made when it split
// into four.
func TestNeitherBindingRepairsAndTheGrantsStayApart(t *testing.T) {
	byName := bindings(t)
	for _, b := range Extension().Bindings {
		for _, g := range b.Grants {
			switch g {
			case extension.ReadRepo, extension.CloudRead, extension.SecretRead:
			default:
				t.Errorf("%s holds %q — readiness REPORTS, it does not repair. A mutating grant "+
					"here means something started fixing what it found", b.Name, g)
			}
		}
	}
	for _, g := range byName["inputs-resolve"].Grants {
		if g == extension.SecretRead {
			t.Error("inputs-resolve holds secret-read — it reads a repo and a cloud, nothing " +
				"else. This is the union that folding the two bindings together would create")
		}
	}
}
