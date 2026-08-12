package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
)

// docs/local.md is the ONE documentation file an instance owns. The guarantee has
// three independent parts and every one of them is load-bearing on its own:
// delivered (or the adopter never gets it), classified `owned` (or the upgrade
// overwrites the thing that exists to survive upgrades), and ordered after
// `managed docs/**` in the manifest (which is last-match-wins, so a rule in the
// wrong place is silently inert).
//
// Two of the three fail SILENTLY. A file that is delivered but classed `managed`
// looks completely correct until the first upgrade eats it — which is the exact
// failure this seam was added for, reproduced one level up.

func TestLocalDocsIsInTheDeliveredKeepSet(t *testing.T) {
	if !DeliveredDocs["local.md"] {
		t.Error("docs/local.md is not in the keep-set — deliver-docs prunes it out and the " +
			"adopter never receives the one file they are told is theirs")
	}
}

func TestLocalDocsIsOwnedNotManaged(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "instance-template")
	m, err := manifest.Load(root)
	if err != nil {
		t.Fatalf("load .template-manifest: %v", err)
	}
	if got := m.Classify("docs/local.md"); got != "owned" {
		t.Errorf("docs/local.md classifies as %q, want \"owned\" — a `managed` local index is "+
			"overwritten by the upgrade it exists to survive", got)
	}
	// The sibling it must NOT disturb: everything else under docs/ stays managed,
	// so template docs keep refreshing on upgrade.
	if got := m.Classify("docs/quickstart.md"); got != "managed" {
		t.Errorf("docs/quickstart.md classifies as %q, want \"managed\" — the local-docs rule "+
			"widened past the one file it is for", got)
	}
}

// Last-match-wins: an `owned docs/local.md` line placed ABOVE `managed docs/**` is
// inert, and inert in the way that looks right in review.
func TestLocalDocsRuleComesAfterTheManagedDocsGlob(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "instance-template", ".template-manifest"))
	if err != nil {
		t.Fatal(err)
	}
	var managedAt, ownedAt = -1, -1
	for i, line := range strings.Split(string(b), "\n") {
		switch strings.Join(strings.Fields(line), " ") {
		case "managed docs/**":
			managedAt = i
		case "owned docs/local.md":
			ownedAt = i
		}
	}
	if managedAt < 0 || ownedAt < 0 {
		t.Fatalf("expected both rules present (managed docs/** at %d, owned docs/local.md at %d)", managedAt, ownedAt)
	}
	if ownedAt < managedAt {
		t.Errorf("`owned docs/local.md` (line %d) precedes `managed docs/**` (line %d) — "+
			"last-match-wins makes it inert", ownedAt+1, managedAt+1)
	}
}
