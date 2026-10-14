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

// guard-manifests is a GATE-ONLY extension, and its single binding may hold
// read-repo and nothing else.
//
// It used to carry a second, `apl-schema` assertion binding with cloud-read —
// `llz ci validate-apl-values`, which resolved apl-core's chart from a registry
// to schema-check a rendered apl-core values.yaml. LLZ renders no such file on
// the managed App Platform, so that verb had no input and was retired; the
// binding went with it, and the package moved from assertions/ to guards/ under
// the mechanical bucket rule. What is pinned here is that the remaining binding
// stays a gate: a files-only lane that reaches the network is a different kind of
// thing and must be declared as one.
func TestTheOnlyBindingIsAFilesOnlyGate(t *testing.T) {
	bs := Extension().Bindings
	if len(bs) != 1 {
		t.Fatalf("want exactly one binding, got %d — a new one must declare its own grants", len(bs))
	}
	if bs[0].Kind != extension.Gate {
		t.Errorf("binding kind = %v, want Gate", bs[0].Kind)
	}
	for _, g := range bs[0].Grants {
		if g != extension.ReadRepo {
			t.Errorf("gate declared %q — a gate may only read the repo", g)
		}
	}
}
