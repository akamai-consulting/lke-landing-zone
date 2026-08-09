package phasetiming

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
	if e.Name != "phase-timing" {
		t.Errorf("identity drifted: %q", e.Name)
	}
}

// The binding is a placement rather than a fit, and the note is the only thing
// saying so. Dropping it would make an instrumentation package read as a
// contributing assertion at `operating` — which is precisely the ban-by-omission
// failure Incomplete exists to prevent.
func TestPlacementStaysMarked(t *testing.T) {
	inc := strings.ToLower(strings.Join(Extension().Incomplete, " "))
	if inc == "" {
		t.Fatal("Incomplete was emptied — this extension contributes no verdict and cannot fail; " +
			"declared as a bare assertion it is a free vote for a state it knows nothing about")
	}
	for _, want := range []string{"instrumentation", "diagnostic", "argocd-diagnostics"} {
		if !strings.Contains(inc, want) {
			t.Errorf("the note no longer mentions %q — it is the record of why no fifth kind was invented", want)
		}
	}
}

// cluster-read and nothing else. The phase log is a write, and deliberately needs
// no grant: it goes to $RUNNER_TEMP, and write-repo means the instance repo's
// TRACKED files. This is the first extension to lean on that boundary.
func TestGrantsAreReadOnly(t *testing.T) {
	for _, g := range Extension().Grants() {
		if g != extension.ClusterRead {
			t.Errorf("declared %q — this reads cluster Events and writes only a temp-dir log", g)
		}
	}
	if Extension().HasGrant(extension.WriteRepo) {
		t.Error("write-repo declared — the phase log lives under $RUNNER_TEMP, not in the instance repo")
	}
}
