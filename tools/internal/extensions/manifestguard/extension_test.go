package manifestguard

import (
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "guard-manifests" || !e.Always {
		t.Errorf("identity drifted: name=%q always=%v", e.Name, e.Always)
	}
}

// ALL FOUR LANES ARE HERE NOW, so the note that said otherwise must be gone —
// and this test flipped with it. It used to assert Incomplete stayed non-empty,
// guarding against someone quietly dropping the marker without moving the lane.
// The lane moved, so the guard now runs the other way: a note reappearing here
// means a lane left, and that should be argued rather than typed.
func TestNoLanesAreOutstanding(t *testing.T) {
	if inc := strings.Join(Extension().Incomplete, " "); inc != "" {
		t.Errorf("Incomplete came back (%q) — if a lane really left, say which and why", inc)
	}
	// The lane that was missing longest: prove its entry point is reachable here.
	if len(ScannedManifestTrees) == 0 {
		t.Error("dropped-apiversions lost its scan roots")
	}
}

// THE SPLIT THIS TEST FOUND. The first cut declared all three lanes as one gate;
// this failed on `apl_schema.go reaches os/exec` and was right — that lane shells
// out to helm to resolve the chart it validates against. A gate is defined by cost
// and reach, so the files-only lanes keep the gate and apl-schema became an
// assertion holding cloud-read. Scoped to the gate's own files for that reason.
func TestGateLanesStayFilesOnly(t *testing.T) {
	banned := []string{"net/http", "os/exec", "k8s.io/", "linodego", "kubectlprobe"}
	for _, f := range []string{"argocd_apps_guard.go", "placeholder_guard.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range banned {
			if strings.Contains(string(b), `"`+bad) || strings.Contains(string(b), bad+`"`) {
				t.Errorf("%s reaches %s — it backs the GATE binding, which may hold read-repo and nothing else", f, bad)
			}
		}
	}
}

// The two bindings must not collapse back into one: the gate may hold read-repo
// alone, and apl-schema needs the registry.
func TestBindingsStaySplitByCapability(t *testing.T) {
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
		t.Fatal("want one gate (files-only lanes) and one assertion (apl-schema, needs helm + a registry)")
	}
	for _, g := range gate.Grants {
		if g != extension.ReadRepo {
			t.Errorf("gate declared %q — a gate may only read the repo", g)
		}
	}
	if !hasGrant(*assertion, extension.CloudRead) {
		t.Error("apl-schema must declare cloud-read: it resolves the apl-core chart from a registry")
	}
}

func hasGrant(b extension.Binding, g extension.Grant) bool {
	for _, have := range b.Grants {
		if have == g {
			return true
		}
	}
	return false
}

// The placeholder set is defined HERE and imported by cmd/llz's bootstrap-cluster,
// not the other way round: a check that validates a set is meaningless if it runs
// against a different set than the producer ships. Same resolution as
// docsguard.DeliveredDocs.
func TestPlaceholderSetIsOwnedHere(t *testing.T) {
	if len(BootstrapValuePlaceholders) == 0 {
		t.Fatal("the placeholder set emptied — bootstrap-cluster substitutes these and this guard validates them")
	}
	for _, k := range BootstrapValuePlaceholders {
		if strings.TrimSpace(k) == "" {
			t.Errorf("blank placeholder key in the set")
		}
	}
}
