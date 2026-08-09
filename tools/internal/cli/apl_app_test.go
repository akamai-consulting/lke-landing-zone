package cli

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

func TestFindComponent(t *testing.T) {
	if len(clusterspec.Components) == 0 {
		t.Fatal("component registry is empty")
	}
	first := clusterspec.Components[0].Name
	if c, ok := findComponent(first); !ok || c.Name != first {
		t.Errorf("findComponent(%q) = (%v, %v), want the registry entry", first, c, ok)
	}
	if _, ok := findComponent("definitely-not-a-component"); ok {
		t.Error("findComponent must return ok=false for an unknown name")
	}
}

// The validation guards fire BEFORE any spec file is touched, so they are testable
// without an instance on disk.
func TestRunAppToggle_Guards(t *testing.T) {
	if err := runAppToggle("", "argocd", true); err == nil {
		t.Error("missing --env must error")
	}
	if err := runAppToggle("e2e", "definitely-not-a-component", true); err == nil ||
		!strings.Contains(err.Error(), "unknown app") {
		t.Errorf("unknown app must error with a hint, got %v", err)
	}

	// Disabling a mandatory component must be refused before any file edit.
	var mandatory string
	for _, c := range clusterspec.Components {
		if c.Mandatory {
			mandatory = c.Name
			break
		}
	}
	if mandatory == "" {
		t.Skip("no mandatory component in the registry to exercise the guard")
	}
	if err := runAppToggle("e2e", mandatory, false); err == nil ||
		!strings.Contains(err.Error(), "cannot be disabled") {
		t.Errorf("disabling mandatory %q must be refused, got %v", mandatory, err)
	}
}
