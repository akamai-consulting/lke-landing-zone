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

// Three of four lanes moved. Dropping the note would make the extension read as
// complete, which is the ban-by-omission failure Incomplete exists to prevent.
func TestPartialExtractionStaysMarked(t *testing.T) {
	inc := strings.Join(Extension().Incomplete, " ")
	if inc == "" {
		t.Fatal("Incomplete was emptied — the dropped-apiversions lane is still in package main")
	}
	if !strings.Contains(inc, "dropped-apiversions") {
		t.Error("the note no longer names the lane that is missing")
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
